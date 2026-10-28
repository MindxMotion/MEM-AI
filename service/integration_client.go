package service

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	IntegrationBalanceScope = "balance:read"
	IntegrationTokenTTL     = 5 * time.Minute
	IntegrationSecretTTL    = 90 * 24 * time.Hour
	integrationIssuer       = "new-api-client-credentials"
	integrationAudience     = "new-api-integration"
)

var ErrIntegrationCredential = errors.New("invalid integration credential")

type integrationClaims struct {
	Scope   string `json:"scope"`
	Version string `json:"cv"`
	jwt.RegisteredClaims
}

// IsIntegrationCredential prevents service credentials from falling through to
// legacy dashboard key lookups, including their SQL error diagnostics.
func IsIntegrationCredential(raw string) bool {
	if strings.HasPrefix(raw, "nacs_") {
		return true
	}
	claims := &integrationClaims{}
	_, _, err := jwt.NewParser().ParseUnverified(raw, claims)
	return err == nil && claims.Issuer == integrationIssuer
}

func IntegrationClientActive(client *model.IntegrationClient) bool {
	if client == nil || !client.Enabled || client.ExpiresAt <= time.Now().Unix() {
		return false
	}
	config, err := system_setting.GetOIDCConfiguration()
	return err == nil && config.Issuer == client.OIDCIssuer && config.Settings.ClientId == client.OIDCClientID
}

func AuthenticateIntegrationClient(ctx context.Context, clientID, secret string) (*model.IntegrationClient, error) {
	if len(clientID) > 64 || len(secret) > 128 {
		return nil, ErrIntegrationCredential
	}
	client, err := model.FindIntegrationClient(ctx, clientID)
	if err != nil {
		return nil, err
	}
	expected := "0000000000000000000000000000000000000000000000000000000000000000"
	if client != nil {
		expected = client.SecretHash
	}
	actual := hex.EncodeToString(common.Sha256Raw([]byte(secret)))
	matches := hmac.Equal([]byte(expected), []byte(actual))
	if !matches || !IntegrationClientActive(client) {
		return nil, ErrIntegrationCredential
	}
	return client, nil
}

func IssueIntegrationToken(client *model.IntegrationClient) (string, int64, error) {
	if !IntegrationClientActive(client) {
		return "", 0, ErrIntegrationCredential
	}
	now := time.Now().Truncate(time.Second)
	expiry := min(now.Add(IntegrationTokenTTL).Unix(), client.ExpiresAt)
	claims := integrationClaims{
		Scope: IntegrationBalanceScope, Version: client.Version,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: integrationIssuer, Subject: client.ClientID,
			Audience:  jwt.ClaimStrings{integrationAudience},
			ExpiresAt: jwt.NewNumericDate(time.Unix(expiry, 0)),
			NotBefore: jwt.NewNumericDate(now), IssuedAt: jwt.NewNumericDate(now),
			ID: uuid.NewString(),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(authSigningKey("integration-client"))
	return token, expiry - now.Unix(), err
}

func ValidateIntegrationToken(ctx context.Context, raw string) (*model.IntegrationClient, error) {
	if raw == "" || len(raw) > 4096 {
		return nil, ErrIntegrationCredential
	}
	claims := &integrationClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(_ *jwt.Token) (any, error) {
		return authSigningKey("integration-client"), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(integrationIssuer), jwt.WithAudience(integrationAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid || claims.Subject == "" || claims.Version == "" ||
		claims.Scope != IntegrationBalanceScope || claims.ID == "" ||
		claims.IssuedAt == nil || claims.NotBefore == nil ||
		claims.ExpiresAt.Unix()-claims.IssuedAt.Unix() > int64(IntegrationTokenTTL/time.Second) {
		return nil, ErrIntegrationCredential
	}
	client, err := model.FindIntegrationClient(ctx, claims.Subject)
	if err != nil {
		return nil, err
	}
	// Recheck on every request: rotation, disabling, expiry and issuer/client
	// reconfiguration revoke already-issued tokens without a stale auth cache.
	if !IntegrationClientActive(client) || client.Version != claims.Version {
		return nil, ErrIntegrationCredential
	}
	return client, nil
}

func NewIntegrationSecret() (string, string, error) {
	suffix, err := common.GenerateRandomCharsKey(43)
	if err != nil {
		return "", "", err
	}
	raw := "nacs_" + suffix
	return raw, hex.EncodeToString(common.Sha256Raw([]byte(raw))), nil
}
