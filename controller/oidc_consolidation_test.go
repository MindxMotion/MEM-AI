package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupOIDCConsolidation(t *testing.T) (*gorm.DB, []model.OIDCConsolidation, []model.Token) {
	t.Helper()
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	// Only ownership is relevant for the resource guards; use real database
	// tables without coupling this fixture to unrelated provider schemas.
	for _, table := range []string{
		"checkins", "external_identity_claims", "midjourneys", "quota_data", "redemptions",
		"subscription_orders", "subscription_pre_consume_records", "tasks", "top_ups",
		"two_fa_backup_codes", "user_oauth_bindings", "user_subscriptions",
	} {
		require.NoError(t, db.Table(table).AutoMigrate(&struct {
			ID     int
			UserID int
		}{}))
	}
	require.NoError(t, db.Table("integration_clients").AutoMigrate(&struct {
		ID        int
		CreatedBy int
	}{}))
	users := []model.User{
		{Id: 101, Username: "canonical-a", OidcId: "primary-a", Email: "a@example.test", Quota: 7654321, UsedQuota: 2345679, RequestCount: 76, AffCode: "ca"},
		{Id: 102, Username: "obsolete-a", OidcId: "roster-a", Email: "old-a@example.test", Quota: 10000000, AffCode: "oa"},
		{Id: 103, Username: "established-b", OidcId: "roster-b", Email: "old-b@example.test", Quota: 9876543, UsedQuota: 123457, RequestCount: 452, AffCode: "eb"},
		{Id: 104, Username: "canonical-b", OidcId: "primary-b", Email: "b@example.test", Quota: 10000000, AffCode: "cb"},
	}
	for i := range users {
		users[i].DisplayName = fmt.Sprintf("profile-%d", users[i].Id)
		users[i].Group = "default"
		users[i].AuthVersion = 1
		require.NoError(t, db.Create(&users[i]).Error)
		require.NoError(t, db.Create(&model.UserSession{
			SID: fmt.Sprintf("session-%d", users[i].Id), UserID: users[i].Id, UserAuthVersion: 1,
			Status: model.UserSessionStatusActive, RefreshHash: strings.Repeat("a", 64),
			LoginMethod: "oauth:oidc", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}).Error)
		require.NoError(t, db.Create(&model.AuthFlow{
			UserId: users[i].Id, Purpose: "login", TokenHash: fmt.Sprintf("%064d", users[i].Id),
			ExpiresAt: time.Now().Add(time.Minute),
		}).Error)
	}
	ip := "192.0.2.1"
	tokens := []model.Token{
		{UserId: 101, Key: "fixture-active-a", Name: "active-a", UsedQuota: 2345679, RemainQuota: 123, ExpiredTime: -1},
		{UserId: 102, Key: "fixture-unused-a", Name: "unused-a", RemainQuota: 456, ExpiredTime: 2000000000, AllowIps: &ip, Group: "default", ModelLimitsEnabled: true, ModelLimits: "fixture-model", CrossGroupRetry: true, AutoGroups: `["default"]`},
		{UserId: 103, Key: "fixture-active-b", Name: "active-b", UsedQuota: 123457, RemainQuota: 789, ExpiredTime: -1},
	}
	for i := range tokens {
		require.NoError(t, db.Create(&tokens[i]).Error)
	}
	require.NoError(t, model.LOG_DB.Create(&model.Log{UserId: 103, Type: model.LogTypeConsume, Quota: 123457}).Error)
	return db, []model.OIDCConsolidation{
		{SurvivorID: 101, RetiredID: 102, SurvivorSubject: "primary-a", RetiredSubject: "roster-a", CanonicalSubject: "primary-a"},
		{SurvivorID: 103, RetiredID: 104, SurvivorSubject: "roster-b", RetiredSubject: "primary-b", CanonicalSubject: "primary-b"},
	}, tokens
}

func TestOIDCConsolidationDatabaseMatrix(t *testing.T) {
	t.Run("preserves_keys_accounting_and_canonical_login", func(t *testing.T) {
		db, pairs, beforeTokens := setupOIDCConsolidation(t)
		previousRedis := common.RDB
		cache := miniredis.RunT(t)
		common.RDB = redis.NewClient(&redis.Options{Addr: cache.Addr()})
		common.RedisEnabled = true
		t.Cleanup(func() {
			require.NoError(t, common.RDB.Close())
			common.RDB = previousRedis
			common.RedisEnabled = false
		})
		for _, userID := range []int{101, 102, 103, 104} {
			_, err := model.GetUserCache(userID)
			require.NoError(t, err)
			_, err = model.GetUserSessionCached(fmt.Sprintf("session-%d", userID))
			require.NoError(t, err)
		}
		var beforeUsers []model.User
		require.NoError(t, db.Order("id").Find(&beforeUsers).Error)
		committed, err := model.ConsolidateIdleOIDCAccounts(context.Background(), pairs)
		require.NoError(t, err)
		require.True(t, committed)
		for i, before := range beforeUsers {
			var after model.User
			require.NoError(t, db.First(&after, before.Id).Error)
			assert.Equal(t, before.Quota, after.Quota)
			assert.Equal(t, before.UsedQuota, after.UsedQuota)
			assert.Equal(t, before.RequestCount, after.RequestCount)
			assert.Equal(t, int64(2), after.AuthVersion)
			if i == 1 || i == 3 {
				assert.Equal(t, common.UserStatusDisabled, after.Status)
				assert.Empty(t, after.OidcId)
			}
		}
		for _, pair := range pairs {
			var resolved model.User
			resolved.OidcId = pair.CanonicalSubject
			require.NoError(t, resolved.FillUserByOidcId())
			assert.Equal(t, pair.SurvivorID, resolved.Id)
			assert.Equal(t, common.UserStatusEnabled, resolved.Status)
			var subjectCount int64
			require.NoError(t, db.Model(&model.User{}).Where("oidc_id = ?", pair.CanonicalSubject).Count(&subjectCount).Error)
			assert.Equal(t, int64(1), subjectCount)
		}
		var survivor model.User
		require.NoError(t, db.First(&survivor, 103).Error)
		assert.Equal(t, "b@example.test", survivor.Email)
		assert.Equal(t, "profile-104", survivor.DisplayName)
		var afterTokens []model.Token
		require.NoError(t, db.Order("id").Find(&afterTokens).Error)
		beforeTokens[1].UserId = 101
		assert.Equal(t, beforeTokens, afterTokens)
		var activeSessions, flows int64
		require.NoError(t, db.Model(&model.UserSession{}).Where("status = ?", model.UserSessionStatusActive).Count(&activeSessions).Error)
		require.NoError(t, db.Model(&model.AuthFlow{}).Count(&flows).Error)
		assert.Zero(t, activeSessions)
		assert.Zero(t, flows)
		movedToken, err := model.ValidateUserToken(beforeTokens[1].Key)
		require.NoError(t, err)
		assert.Equal(t, 101, movedToken.UserId)
		retiredCache, err := model.GetUserCache(102)
		require.NoError(t, err)
		assert.Equal(t, common.UserStatusDisabled, retiredCache.Status)
		require.Error(t, model.ValidateAuthSessionWithTx(db, model.AuthSessionIdentity{
			UserID: 103, SessionID: "session-103", UserAuthVersion: 1, SessionVersion: 1,
		}))
		require.NoError(t, model.CreateUserSession(&model.UserSession{
			SID: "fresh-canonical-session", UserID: 103, UserAuthVersion: 2,
			RefreshHash: strings.Repeat("b", 64), LoginMethod: "oauth:oidc",
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}))
		require.NoError(t, model.ValidateAuthSessionWithTx(db, model.AuthSessionIdentity{
			UserID: 103, SessionID: "fresh-canonical-session", UserAuthVersion: 2, SessionVersion: 1,
		}))
		var historicalLog model.Log
		require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).First(&historicalLog).Error)
		assert.Equal(t, 103, historicalLog.UserId)
		committed, err = model.ConsolidateIdleOIDCAccounts(context.Background(), pairs)
		assert.Error(t, err)
		assert.False(t, committed, "replay must not grant balances or move ownership again")
	})
	t.Run("late_failure_rolls_back_every_pair", func(t *testing.T) {
		db, pairs, beforeTokens := setupOIDCConsolidation(t)
		pairs[1].RetiredSubject = "unexpected-subject"
		pairs[1].CanonicalSubject = "unexpected-subject"
		committed, err := model.ConsolidateIdleOIDCAccounts(context.Background(), pairs)
		require.Error(t, err)
		assert.False(t, committed)
		var tokens []model.Token
		require.NoError(t, db.Order("id").Find(&tokens).Error)
		assert.Equal(t, beforeTokens, tokens)
		var retired model.User
		require.NoError(t, db.First(&retired, 102).Error)
		assert.Equal(t, common.UserStatusEnabled, retired.Status)
		assert.Equal(t, int64(1), retired.AuthVersion)
		var active int64
		require.NoError(t, db.Model(&model.UserSession{}).Where("status = ?", model.UserSessionStatusActive).Count(&active).Error)
		assert.Equal(t, int64(4), active)
	})
	t.Run("shared_log_database", func(t *testing.T) {
		t.Setenv("TEST_MANAGE_USER_SEPARATE_LOG_DB", "0")
		_, pairs, _ := setupOIDCConsolidation(t)
		committed, err := model.ConsolidateIdleOIDCAccounts(context.Background(), pairs)
		require.NoError(t, err)
		assert.True(t, committed)
	})
	for _, guard := range []string{"resource", "usage", "subject_collision", "group"} {
		t.Run("rejects_"+guard, func(t *testing.T) {
			db, pairs, _ := setupOIDCConsolidation(t)
			switch guard {
			case "resource":
				require.NoError(t, db.Table("top_ups").Create(map[string]any{"user_id": 102}).Error)
			case "usage":
				require.NoError(t, db.Model(&model.Token{}).Where("user_id = ?", 102).Update("used_quota", 1).Error)
			case "subject_collision":
				require.NoError(t, db.Create(&model.User{Username: "collision", OidcId: "primary-a", AffCode: "cc"}).Error)
			case "group":
				require.NoError(t, db.Model(&model.User{}).Where("id = ?", 102).Update("group", "vip").Error)
			}
			committed, err := model.ConsolidateIdleOIDCAccounts(context.Background(), pairs)
			assert.Error(t, err)
			assert.False(t, committed)
		})
	}
}
