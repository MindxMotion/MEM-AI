package controller

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/logger"
)

func setupIntegrationTest(t *testing.T) (*gin.Engine, service.AuthIdentity, string) {
	t.Helper()
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.TwoFABackupCode{}, &model.UserOAuthBinding{}))
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	t.Cleanup(func() { require.NoError(t, system_setting.LoadOIDCEnvironment()) })
	for key, value := range map[string]string{
		"OIDC_ENABLED": "true", "OIDC_ISSUER_URL": "https://identity.example.test",
		"OIDC_CLIENT_ID": "gateway", "OIDC_CLIENT_SECRET": "not-a-production-secret",
		"OIDC_REDIRECT_URI":      "https://gateway.example.test/oauth/oidc",
		"OIDC_TOKEN_AUTH_METHOD": "client_secret_basic",
	} {
		t.Setenv(key, value)
	}
	require.NoError(t, system_setting.LoadOIDCEnvironment())
	previousPassword := common.PasswordLoginEnabled
	common.PasswordLoginEnabled = true
	t.Cleanup(func() { common.PasswordLoginEnabled = previousPassword })
	previousGeneral := *operation_setting.GetGeneralSetting()
	previousQuota, previousRate := common.QuotaPerUnit, operation_setting.USDExchangeRate
	common.QuotaPerUnit, operation_setting.USDExchangeRate = 500000, 7
	operation_setting.GetGeneralSetting().QuotaDisplayType = "CNY"
	t.Cleanup(func() {
		*operation_setting.GetGeneralSetting() = previousGeneral
		common.QuotaPerUnit, operation_setting.USDExchangeRate = previousQuota, previousRate
	})
	require.NoError(t, model.MigrateIntegrationClients(db))
	root := createQuotaTestOperator(t, db, common.RoleRootUser)
	require.NoError(t, model.PublishUserAuthCache(root.Id))
	identity, credential := createAccessTokenTestSession(t, root.Id, "integration-root-session")
	router := gin.New()
	router.Use(middleware.RequestId(), middleware.DisableCache())
	clients := router.Group("/api/integration-clients", middleware.RootAuth())
	clients.GET("", ListIntegrationClients)
	clients.POST("", CreateIntegrationClient)
	clients.POST("/:client_id/rotate", RotateIntegrationClient)
	clients.PATCH("/:client_id", SetIntegrationClientStatus)
	router.POST("/api/integration/v1/token", IntegrationToken)
	router.GET("/api/integration/v1/balance", middleware.IntegrationBalanceAuth(), GetIntegrationBalance)
	router.GET("/v1/integration-test", middleware.TokenAuth(), func(c *gin.Context) { c.Status(204) })
	router.GET("/api/integration-relay-key-test", middleware.TokenAuthReadOnly(), func(c *gin.Context) { c.Status(204) })
	return router, identity, credential
}

func integrationProof(t *testing.T, identity service.AuthIdentity, scope string, context service.IntegrationClientContext) string {
	t.Helper()
	payload, err := common.Marshal(context)
	require.NoError(t, err)
	return issueSecurityEnrollmentProof(t, identity, service.VerificationOperation{Scope: scope, Context: payload}, "password")
}

func createIntegrationFixture(t *testing.T, router http.Handler, identity service.AuthIdentity, credential string) (model.IntegrationClient, string) {
	t.Helper()
	proof := integrationProof(t, identity, service.VerificationScopeIntegrationClientCreate, service.IntegrationClientContext{
		Name: "Profile cards", Issuer: "https://identity.example.test", OIDCClientID: "gateway",
	})
	response := accessTokenRequest(router, "POST", "/api/integration-clients", credential, proof, `{"name":"Profile cards"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Item   model.IntegrationClient `json:"item"`
			Secret string                  `json:"client_secret"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
	require.True(t, body.Success, response.Body.String())
	require.NotEmpty(t, body.Data.Secret)
	return body.Data.Item, body.Data.Secret
}

func integrationTokenRequest(router http.Handler, clientID, secret, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", "/api/integration/v1/token", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(secret))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func integrationToken(t *testing.T, router http.Handler, clientID, secret string) string {
	t.Helper()
	response := integrationTokenRequest(router, clientID, secret, "grant_type=client_credentials&scope=balance%3Aread")
	require.Equal(t, 200, response.Code, response.Body.String())
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, int64(300), body.ExpiresIn)
	assert.Equal(t, "balance:read", body.Scope)
	require.NotEmpty(t, body.AccessToken)
	assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
	return body.AccessToken
}

func TestIntegrationClientLifecycle(t *testing.T) {
	router, identity, credential := setupIntegrationTest(t)
	client, secret := createIntegrationFixture(t, router, identity, credential)
	var stored model.IntegrationClient
	require.NoError(t, model.DB.First(&stored, client.Id).Error)
	assert.NotEqual(t, secret, stored.SecretHash)
	assert.Equal(t, model.AccessTokenFingerprint(secret), stored.SecretHash)
	list := accessTokenRequest(router, "GET", "/api/integration-clients", credential, "", "")
	require.Equal(t, 200, list.Code)
	assert.NotContains(t, list.Body.String(), secret)
	assert.NotContains(t, list.Body.String(), stored.SecretHash)
	assert.NotContains(t, list.Body.String(), stored.Version)
	token := integrationToken(t, router, client.ClientID, secret)
	user := model.User{Username: "oidc-user", OidcId: "Card/User-A", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Quota: 1234500, AffCode: "card-user"}
	require.NoError(t, model.DB.Create(&user).Error)
	path := "/api/integration/v1/balance?subject=" + url.QueryEscape(user.OidcId)
	balance := accessTokenRequest(router, "GET", path, token, "", "")
	require.Equal(t, 200, balance.Code, balance.Body.String())
	var result struct {
		Data struct{ Remaining, Unit, Symbol, AsOf string } `json:"data"`
	}
	require.NoError(t, common.Unmarshal(balance.Body.Bytes(), &result))
	assert.Equal(t, "17.283", result.Data.Remaining)
	assert.Equal(t, "CNY", result.Data.Unit)
	assert.Equal(t, "¥", result.Data.Symbol)
	assert.NotContains(t, balance.Body.String(), "quota")
	assert.NotContains(t, balance.Body.String(), "email")
	for _, invalidSubject := range []string{"unknown", "card/user-a", user.OidcId + " "} {
		response := accessTokenRequest(router, "GET", "/api/integration/v1/balance?subject="+url.QueryEscape(invalidSubject), token, "", "")
		assert.NotEqual(t, 200, response.Code)
	}
	require.NoError(t, model.DB.Model(&user).Update("quota", 0).Error)
	zero := accessTokenRequest(router, "GET", path, token, "", "")
	assert.Contains(t, zero.Body.String(), `"remaining":"0"`)
	require.NoError(t, model.DB.Model(&user).Update("status", common.UserStatusDisabled).Error)
	assert.Equal(t, 404, accessTokenRequest(router, "GET", path, token, "", "").Code)
	require.NoError(t, model.DB.Model(&user).Update("status", common.UserStatusEnabled).Error)
	// A repeated binding must not disclose whichever record the DB returns first.
	duplicate := model.User{Username: "duplicate", OidcId: user.OidcId, Status: common.UserStatusEnabled, AffCode: "duplicate-card"}
	require.NoError(t, model.DB.Create(&duplicate).Error)
	assert.Equal(t, 404, accessTokenRequest(router, "GET", path, token, "", "").Code)
	require.NoError(t, model.DB.Delete(&duplicate).Error)

	proof := integrationProof(t, identity, service.VerificationScopeIntegrationClientRotate, service.IntegrationClientContext{ClientID: client.ClientID})
	rotationPath := "/api/integration-clients/" + client.ClientID + "/rotate"
	rotation := accessTokenRequest(router, "POST", rotationPath, credential, proof, "")
	require.Equal(t, 200, rotation.Code, rotation.Body.String())
	var rotated struct {
		Data struct {
			Secret string `json:"client_secret"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rotation.Body.Bytes(), &rotated))
	require.NotEmpty(t, rotated.Data.Secret)
	assert.Equal(t, 403, accessTokenRequest(router, "POST", rotationPath, credential, proof, "").Code, "proof is one use")
	assert.Equal(t, 401, integrationTokenRequest(router, client.ClientID, secret, "grant_type=client_credentials").Code)
	assert.Equal(t, 401, accessTokenRequest(router, "GET", path, token, "", "").Code)
	newToken := integrationToken(t, router, client.ClientID, rotated.Data.Secret)
	enabled := false
	statusPath := "/api/integration-clients/" + client.ClientID
	proof = integrationProof(t, identity, service.VerificationScopeIntegrationClientStatus, service.IntegrationClientContext{ClientID: client.ClientID, Enabled: &enabled})
	require.Equal(t, 200, accessTokenRequest(router, "PATCH", statusPath, credential, proof, `{"enabled":false}`).Code)
	assert.Equal(t, 401, accessTokenRequest(router, "GET", path, newToken, "", "").Code)
	assert.Equal(t, 401, integrationTokenRequest(router, client.ClientID, rotated.Data.Secret, "grant_type=client_credentials").Code)
	enabled = true
	proof = integrationProof(t, identity, service.VerificationScopeIntegrationClientStatus, service.IntegrationClientContext{ClientID: client.ClientID, Enabled: &enabled})
	// A proof for enabling cannot disable, nor can it target another client.
	assert.Equal(t, 403, accessTokenRequest(router, "PATCH", statusPath, credential, proof, `{"enabled":false}`).Code)
	require.Equal(t, 200, accessTokenRequest(router, "PATCH", statusPath, credential, proof, `{"enabled":true}`).Code)
	assert.Equal(t, 401, accessTokenRequest(router, "GET", path, newToken, "", "").Code, "reenabling does not revive revoked tokens")
}

func TestIntegrationClientAuthorization(t *testing.T) {
	router, identity, credential := setupIntegrationTest(t)
	client, secret := createIntegrationFixture(t, router, identity, credential)
	token := integrationToken(t, router, client.ClientID, secret)
	for _, tc := range []struct {
		name, id, secret, body string
		status                 int
	}{
		{"wrong secret", client.ClientID, "incorrect", "grant_type=client_credentials", 401},
		{"secret requires exact match", client.ClientID, secret + " ", "grant_type=client_credentials", 401},
		{"unknown client", "unknown", secret, "grant_type=client_credentials", 401},
		{"case changed client", strings.ToUpper(client.ClientID), secret, "grant_type=client_credentials", 401},
		{"scope escalation", client.ClientID, secret, "grant_type=client_credentials&scope=admin", 400},
		{"duplicate scope", client.ClientID, secret, "grant_type=client_credentials&scope=balance%3Aread&scope=admin", 400},
		{"no password grant", client.ClientID, secret, "grant_type=password", 400},
		{"no second credential source", client.ClientID, secret, "grant_type=client_credentials&client_secret=other", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.status, integrationTokenRequest(router, tc.id, tc.secret, tc.body).Code)
		})
	}
	var lookupLog bytes.Buffer
	previousLogger := model.DB.Logger
	model.DB.Logger = logger.New(log.New(&lookupLog, "", 0), logger.Config{LogLevel: logger.Info})
	for _, credential := range []string{"", token, secret} {
		assert.NotEqual(t, 200, accessTokenRequest(router, "GET", "/api/integration-clients", credential, "", "").Code)
		assert.NotEqual(t, 204, accessTokenRequest(router, "GET", "/v1/integration-test", credential, "", "").Code)
		assert.NotEqual(t, 204, accessTokenRequest(router, "GET", "/api/integration-relay-key-test", credential, "", "").Code)
	}
	model.DB.Logger = previousLogger
	assert.NotContains(t, lookupLog.String(), secret, "service secrets never enter legacy SQL lookups or diagnostics")
	assert.NotContains(t, lookupLog.String(), token, "service tokens never enter relay SQL lookups or diagnostics")
	assert.Equal(t, 401, accessTokenRequest(router, "GET", "/api/integration/v1/balance?subject=user", credential, "", "").Code, "dashboard JWT is not a service credential")
	pat, _ := createScopedAccessToken(t, identity.UserID, 0, "profile:read")
	assert.Equal(t, 403, accessTokenRequest(router, "GET", "/api/integration-clients", pat, "", "").Code)
	admin := model.User{Username: "not-root", Password: "hash", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "not-root"}
	require.NoError(t, model.DB.Create(&admin).Error)
	require.NoError(t, model.PublishUserAuthCache(admin.Id))
	adminIdentity, adminCredential := createAccessTokenTestSession(t, admin.Id, "integration-admin-session")
	assert.Equal(t, 403, accessTokenRequest(router, "GET", "/api/integration-clients", adminCredential, "", "").Code)
	_, err := service.GetVerificationRequirements(adminIdentity, service.VerificationScopeIntegrationClientCreate)
	require.ErrorIs(t, err, service.ErrVerificationForbidden)
	assert.Equal(t, 403, accessTokenRequest(router, "POST", "/api/integration-clients", credential, "", `{"name":"Missing proof"}`).Code)
	// Modified claims cannot be used with an attacker's signing key.
	claims := jwt.MapClaims{}
	_, _, err = jwt.NewParser().ParseUnverified(token, claims)
	require.NoError(t, err)
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("not-the-signing-key"))
	require.NoError(t, err)
	assert.Equal(t, 401, accessTokenRequest(router, "GET", "/api/integration/v1/balance?subject=user", forged, "", "").Code)
	require.NoError(t, model.DB.Model(&model.IntegrationClient{}).Where("id = ?", client.Id).Update("expires_at", time.Now().Add(-time.Second).Unix()).Error)
	assert.Equal(t, 401, accessTokenRequest(router, "GET", "/api/integration/v1/balance?subject=user", token, "", "").Code)
	assert.Equal(t, 401, integrationTokenRequest(router, client.ClientID, secret, "grant_type=client_credentials").Code)
	require.NoError(t, model.DB.Model(&model.IntegrationClient{}).Where("id = ?", client.Id).Update("expires_at", time.Now().Add(time.Hour).Unix()).Error)
	t.Setenv("OIDC_CLIENT_ID", "different-client-with-pairwise-subjects")
	require.NoError(t, system_setting.LoadOIDCEnvironment())
	assert.Equal(t, 401, accessTokenRequest(router, "GET", "/api/integration/v1/balance?subject=user", token, "", "").Code)
	assert.Equal(t, 401, integrationTokenRequest(router, client.ClientID, secret, "grant_type=client_credentials").Code)
}

func TestIntegrationClientDatabaseMatrix(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("released-schema-upgrade=%v", upgrade), func(t *testing.T) {
			db := setupManageUserTestDB(t)
			before := model.User{Username: "preserved", Status: common.UserStatusEnabled, Quota: 7654321, OidcId: "preserved-subject", AffCode: "preserved"}
			if upgrade {
				require.NoError(t, db.Create(&before).Error)
			}
			require.False(t, db.Migrator().HasTable(&model.IntegrationClient{}), "released schema has no integration table")
			require.NoError(t, model.MigrateIntegrationClients(db))
			client := model.IntegrationClient{ClientID: "matrix-client", Name: "service", SecretHash: "verifier", Version: "original-version", Enabled: true, OIDCIssuer: "https://identity.example.test"}
			require.NoError(t, db.Create(&client).Error)
			require.NoError(t, model.MigrateIntegrationClients(db))
			require.NoError(t, model.MigrateIntegrationClients(db))
			var reloaded model.IntegrationClient
			require.NoError(t, db.First(&reloaded, client.Id).Error)
			assert.Equal(t, client, reloaded)
			duplicate := model.IntegrationClient{ClientID: client.ClientID}
			assert.Error(t, db.Create(&duplicate).Error, "client ID remains unique after repeated migration")
			require.NoError(t, model.UpdateIntegrationClient(t.Context(), &client, map[string]any{"enabled": false, "version": "disabled-version"}))
			require.NoError(t, db.First(&reloaded, client.Id).Error)
			assert.False(t, reloaded.Enabled, "explicit false must survive all dialects")
			assert.ErrorIs(t, model.UpdateIntegrationClient(t.Context(), &client, map[string]any{"enabled": true}), model.ErrIntegrationClientChanged)
			if upgrade {
				var after model.User
				require.NoError(t, db.First(&after, before.Id).Error)
				assert.Equal(t, before.Quota, after.Quota)
				assert.Equal(t, before.OidcId, after.OidcId)
				assert.True(t, db.Migrator().HasIndex(&model.User{}, "OidcId"))
			}
		})
	}
}
