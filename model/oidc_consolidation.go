package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// OIDCConsolidation identifies an operator-verified pair, never a name/email match.
type OIDCConsolidation struct {
	SurvivorID       int    `json:"survivor_id"`
	RetiredID        int    `json:"retired_id"`
	SurvivorSubject  string `json:"survivor_subject"`
	RetiredSubject   string `json:"retired_subject"`
	CanonicalSubject string `json:"canonical_subject"`
}

// ConsolidateIdleOIDCAccounts is an offline maintenance operation. The caller
// must stop gateway traffic and drain settlement writers first. Only unused
// duplicate accounts can be retired: balances and historical actors are not
// rewritten, and unused API keys retain every property except their owner.
// committed distinguishes a committed mutation from a subsequent cache error.
func ConsolidateIdleOIDCAccounts(ctx context.Context, pairs []OIDCConsolidation) (committed bool, err error) {
	if len(pairs) == 0 {
		return false, errors.New("OIDC consolidation requires explicit account pairs")
	}
	initCol()
	ids := make([]int, 0, len(pairs)*2)
	for _, pair := range pairs {
		if pair.SurvivorID <= 0 || pair.RetiredID <= 0 || pair.SurvivorID == pair.RetiredID ||
			pair.SurvivorSubject == "" || pair.RetiredSubject == "" ||
			pair.SurvivorSubject == pair.RetiredSubject ||
			(pair.CanonicalSubject != pair.SurvivorSubject && pair.CanonicalSubject != pair.RetiredSubject) ||
			slices.Contains(ids, pair.SurvivorID) || slices.Contains(ids, pair.RetiredID) {
			return false, errors.New("invalid or overlapping OIDC consolidation pairs")
		}
		ids = append(ids, pair.SurvivorID, pair.RetiredID)
	}
	slices.Sort(ids)
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var users []User
		if err := lockForUpdate(tx).Where("id IN ?", ids).Order("id").Find(&users).Error; err != nil {
			return err
		}
		if len(users) != len(ids) {
			return errors.New("OIDC consolidation account is missing or deleted")
		}
		byID := make(map[int]User, len(users))
		for _, user := range users {
			byID[user.Id] = user
		}
		for _, pair := range pairs {
			survivor, retired := byID[pair.SurvivorID], byID[pair.RetiredID]
			if survivor.Role != common.RoleCommonUser || retired.Role != common.RoleCommonUser ||
				survivor.Status != common.UserStatusEnabled || retired.Status != common.UserStatusEnabled ||
				survivor.OidcId != pair.SurvivorSubject || retired.OidcId != pair.RetiredSubject ||
				survivor.Group != retired.Group {
				return errors.New("OIDC consolidation identity or account-state precondition failed")
			}
			var subjectOwners []User
			if err := tx.Unscoped().Select("id", "oidc_id").
				Where("oidc_id IN ?", []string{pair.SurvivorSubject, pair.RetiredSubject}).Find(&subjectOwners).Error; err != nil {
				return err
			}
			if len(subjectOwners) != 2 {
				return errors.New("OIDC consolidation subjects do not have unique owners")
			}
			if retired.UsedQuota != 0 || retired.RequestCount != 0 || retired.AffCount != 0 ||
				retired.AffQuota != 0 || retired.AffHistoryQuota != 0 || retired.InviterId != 0 ||
				retired.StripeCustomer != "" || retired.GitHubId != "" || retired.WeChatId != "" ||
				retired.TelegramId != "" || retired.LinuxDOId != "" || retired.DiscordId != "" {
				return errors.New("OIDC duplicate has business history or additional identities; manual migration required")
			}
			for _, table := range []string{
				"checkins", "external_identity_claims", "midjourneys", "passkey_credentials",
				"quota_data", "redemptions", "subscription_orders", "subscription_pre_consume_records",
				"tasks", "top_ups", "two_fa_backup_codes", "two_fas", "user_access_tokens",
				"user_oauth_bindings", "user_subscriptions",
			} {
				var count int64
				if err := tx.Table(table).Where("user_id = ?", retired.Id).Count(&count).Error; err != nil {
					return err
				}
				if count != 0 {
					return fmt.Errorf("OIDC duplicate owns %s resources; manual migration required", table)
				}
			}
			var count int64
			if err := tx.Model(&User{}).Where("inviter_id = ?", retired.Id).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return errors.New("OIDC duplicate has invited users")
			}
			if err := tx.Table("integration_clients").Where("created_by = ?", retired.Id).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return errors.New("OIDC duplicate owns integration clients")
			}
			logDB := LOG_DB.WithContext(ctx)
			if LOG_DB == DB {
				logDB = tx
			}
			if err := logDB.Model(&Log{}).
				Where("user_id = ? AND type IN ?", retired.Id, []int{LogTypeConsume, LogTypeError, LogTypeRefund}).
				Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return errors.New("OIDC duplicate has relay history")
			}
			var tokens []Token
			if err := tx.Unscoped().Where("user_id = ?", retired.Id).Find(&tokens).Error; err != nil {
				return err
			}
			for _, token := range tokens {
				if token.UsedQuota != 0 {
					return errors.New("OIDC duplicate API key has usage history")
				}
			}
			if err := invalidateTokensCache(tokens); err != nil {
				return err
			}
			for _, userID := range []int{survivor.Id, retired.Id} {
				if _, err := IncrementUserAuthVersionWithTx(tx, userID); err != nil {
					return err
				}
				var sessions []UserSession
				if err := tx.Where("user_id = ? AND status = ?", userID, UserSessionStatusActive).
					Find(&sessions).Error; err != nil {
					return err
				}
				now := time.Now().Unix()
				for _, session := range sessions {
					if err := writeUserSessionDenyFence(&session, UserSessionStatusRevoking, now, "oidc_account_consolidated"); err != nil {
						return err
					}
				}
				if err := tx.Model(&UserSession{}).
					Where("user_id = ? AND status = ?", userID, UserSessionStatusActive).
					Updates(map[string]any{"status": UserSessionStatusRevoked, "revoked_at": now, "revoked_reason": "oidc_account_consolidated"}).Error; err != nil {
					return err
				}
				if err := tx.Where("user_id = ?", userID).Delete(&AuthFlow{}).Error; err != nil {
					return err
				}
			}
			// Release the canonical subject/email before assigning them to the
			// survivor. The retired balance stays frozen, not granted twice.
			if err := tx.Model(&User{}).Where("id = ?", retired.Id).
				Updates(map[string]any{"status": common.UserStatusDisabled, "oidc_id": "", "email": "", "access_token": nil}).Error; err != nil {
				return err
			}
			profile := survivor
			if pair.CanonicalSubject == pair.RetiredSubject {
				profile = retired
			}
			if err := lockNormalizedEmail(tx, profile.Email); err != nil {
				return err
			}
			if err := ensureEmailAvailableWithTx(tx, profile.Email, survivor.Id); err != nil {
				return err
			}
			if err := tx.Model(&User{}).Where("id = ?", survivor.Id).
				Updates(map[string]any{"oidc_id": pair.CanonicalSubject, "email": profile.Email, "display_name": profile.DisplayName}).Error; err != nil {
				return err
			}
			if err := tx.Unscoped().Model(&Token{}).Where("user_id = ?", retired.Id).Update("user_id", survivor.Id).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if err := invalidateUserCache(id); err != nil {
			return true, fmt.Errorf("OIDC consolidation committed; user cache invalidation failed: %w", err)
		}
		if err := PublishUserAuthCache(id); err != nil {
			return true, fmt.Errorf("OIDC consolidation committed; auth cache publication failed: %w", err)
		}
		if err := InvalidateUserTokensCache(id); err != nil {
			return true, fmt.Errorf("OIDC consolidation committed; API key cache invalidation failed: %w", err)
		}
	}
	return true, nil
}
