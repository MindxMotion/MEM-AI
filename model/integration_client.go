package model

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrIntegrationClientChanged = errors.New("integration client changed or no longer exists")

// IntegrationClient is a service identity, never a dashboard user. Only a
// verifier of the high-entropy secret is stored; plaintext is shown once.
type IntegrationClient struct {
	Id           int    `json:"id"`
	ClientID     string `json:"client_id" gorm:"type:varchar(64);uniqueIndex"`
	Name         string `json:"name" gorm:"type:varchar(64)"`
	SecretHash   string `json:"-" gorm:"type:varchar(64)"`
	SecretHint   string `json:"secret_hint" gorm:"type:varchar(8)"`
	Version      string `json:"-" gorm:"type:varchar(64)"`
	OIDCIssuer   string `json:"oidc_issuer" gorm:"column:oidc_issuer;type:varchar(512)"`
	OIDCClientID string `json:"oidc_client_id" gorm:"column:oidc_client_id;type:varchar(256)"`
	Enabled      bool   `json:"enabled"`
	CreatedBy    int    `json:"created_by"`
	CreatedAt    int64  `json:"created_at" gorm:"type:bigint"`
	ExpiresAt    int64  `json:"expires_at" gorm:"type:bigint"`
	LastUsedAt   int64  `json:"last_used_at" gorm:"type:bigint"`
}

func MigrateIntegrationClients(db *gorm.DB) error {
	return db.AutoMigrate(&IntegrationClient{})
}

func ListIntegrationClients(ctx context.Context) ([]IntegrationClient, error) {
	items := make([]IntegrationClient, 0)
	err := DB.WithContext(ctx).Order("id DESC").Find(&items).Error
	return items, err
}

func FindIntegrationClient(ctx context.Context, clientID string) (*IntegrationClient, error) {
	var client IntegrationClient
	err := DB.WithContext(ctx).Where("client_id = ?", clientID).First(&client).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// MySQL's default collation is case insensitive; OAuth client IDs are not.
	if client.ClientID != clientID {
		return nil, nil
	}
	return &client, nil
}

func UpdateIntegrationClient(ctx context.Context, client *IntegrationClient, updates map[string]any) error {
	result := DB.WithContext(ctx).Model(&IntegrationClient{}).
		Where("id = ? AND version = ?", client.Id, client.Version).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrIntegrationClientChanged
	}
	return nil
}

func TouchIntegrationClient(ctx context.Context, id int, version string) error {
	now := common.GetTimestamp()
	return DB.WithContext(ctx).Model(&IntegrationClient{}).
		Where("id = ? AND version = ? AND last_used_at < ?", id, version, now-60).
		Update("last_used_at", now).Error
}

// FindIntegrationUser requires one exact subject match. Legacy duplicate
// bindings and collation-equivalent subjects must never select an arbitrary user.
func FindIntegrationUser(ctx context.Context, subject string) (*User, error) {
	var users []User
	if err := DB.WithContext(ctx).Select("id", "oidc_id", "status").
		Where("oidc_id = ?", subject).Limit(2).Find(&users).Error; err != nil {
		return nil, err
	}
	if len(users) != 1 || users[0].OidcId != subject || users[0].Status != common.UserStatusEnabled {
		return nil, nil
	}
	return &users[0], nil
}
