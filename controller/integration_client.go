package controller

import (
	"errors"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func ListIntegrationClients(c *gin.Context) {
	if _, ok := requireBrowserSession(c); !ok {
		return
	}
	items, err := model.ListIntegrationClients(c.Request.Context())
	if err != nil {
		writeSecurityOperationError(c, err)
		return
	}
	config, configErr := system_setting.GetOIDCConfiguration()
	common.ApiSuccess(c, gin.H{
		"items": items, "oidc_configured": configErr == nil,
		"oidc_issuer": config.Issuer, "oidc_client_id": config.Settings.ClientId,
	})
}

func CreateIntegrationClient(c *gin.Context) {
	identity, ok := requireBrowserSession(c)
	if !ok {
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &request) != nil {
		writeAccessTokenError(c, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request body.")
		return
	}
	name, ok := normalizeAccessTokenName(request.Name)
	if !ok {
		writeAccessTokenError(c, http.StatusBadRequest, "INVALID_REQUEST", "Name must be 1 to 64 characters.")
		return
	}
	config, err := system_setting.GetOIDCConfiguration()
	if err != nil {
		writeAccessTokenError(c, http.StatusConflict, "OIDC_REQUIRED", "Configure OIDC before creating an integration client.")
		return
	}
	context := service.IntegrationClientContext{Name: name, Issuer: config.Issuer, OIDCClientID: config.Settings.ClientId}
	if requireAdminUserProof(c, service.VerificationScopeIntegrationClientCreate, context) == nil {
		return
	}
	id, err := common.GenerateRandomCharsKey(24)
	if err != nil {
		writeSecurityOperationError(c, err)
		return
	}
	raw, hash, err := service.NewIntegrationSecret()
	if err != nil {
		writeSecurityOperationError(c, err)
		return
	}
	now := time.Now()
	client := model.IntegrationClient{
		ClientID: "nac_" + id, Name: name, SecretHash: hash, SecretHint: model.AccessTokenHint(raw),
		Version: uuid.NewString(), OIDCIssuer: config.Issuer, OIDCClientID: config.Settings.ClientId,
		Enabled: true, CreatedBy: identity.UserID, CreatedAt: now.Unix(), ExpiresAt: now.Add(service.IntegrationSecretTTL).Unix(),
	}
	if err := model.DB.WithContext(c.Request.Context()).Create(&client).Error; err != nil {
		writeSecurityOperationError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"item": client, "client_secret": raw})
}

func RotateIntegrationClient(c *gin.Context) {
	if _, ok := requireBrowserSession(c); !ok {
		return
	}
	client, err := model.FindIntegrationClient(c.Request.Context(), c.Param("client_id"))
	if err != nil {
		writeSecurityOperationError(c, err)
		return
	}
	if client == nil {
		writeAccessTokenError(c, http.StatusNotFound, "CLIENT_NOT_FOUND", "Integration client not found.")
		return
	}
	if requireAdminUserProof(c, service.VerificationScopeIntegrationClientRotate, service.IntegrationClientContext{ClientID: client.ClientID}) == nil {
		return
	}
	raw, hash, err := service.NewIntegrationSecret()
	if err != nil {
		writeSecurityOperationError(c, err)
		return
	}
	err = model.UpdateIntegrationClient(c.Request.Context(), client, map[string]any{
		"secret_hash": hash, "secret_hint": model.AccessTokenHint(raw), "version": uuid.NewString(),
		"expires_at": time.Now().Add(service.IntegrationSecretTTL).Unix(),
	})
	if err != nil {
		writeIntegrationUpdateError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"client_id": client.ClientID, "client_secret": raw})
}

func SetIntegrationClientStatus(c *gin.Context) {
	if _, ok := requireBrowserSession(c); !ok {
		return
	}
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &request) != nil || request.Enabled == nil {
		writeAccessTokenError(c, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request body.")
		return
	}
	client, err := model.FindIntegrationClient(c.Request.Context(), c.Param("client_id"))
	if err != nil {
		writeSecurityOperationError(c, err)
		return
	}
	if client == nil {
		writeAccessTokenError(c, http.StatusNotFound, "CLIENT_NOT_FOUND", "Integration client not found.")
		return
	}
	if requireAdminUserProof(c, service.VerificationScopeIntegrationClientStatus, service.IntegrationClientContext{ClientID: client.ClientID, Enabled: request.Enabled}) == nil {
		return
	}
	if err := model.UpdateIntegrationClient(c.Request.Context(), client, map[string]any{"enabled": *request.Enabled, "version": uuid.NewString()}); err != nil {
		writeIntegrationUpdateError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"client_id": client.ClientID, "enabled": *request.Enabled})
}

func writeIntegrationUpdateError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrIntegrationClientChanged) {
		writeAccessTokenError(c, http.StatusConflict, "CLIENT_CHANGED", "Integration client changed. Reload and try again.")
		return
	}
	writeSecurityOperationError(c, err)
}

// IntegrationToken implements only client_credentials and client_secret_basic.
// Browser sessions, model keys and user OIDC tokens are deliberately unrelated.
func IntegrationToken(c *gin.Context) {
	clientID, secret, basic := c.Request.BasicAuth()
	if basic {
		var idErr, secretErr error
		clientID, idErr = url.QueryUnescape(clientID)
		secret, secretErr = url.QueryUnescape(secret)
		basic = idErr == nil && secretErr == nil
	}
	if !basic || clientID == "" || secret == "" || c.Request.URL.RawQuery != "" {
		c.Header("WWW-Authenticate", `Basic realm="new-api-integration"`)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_client"})
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.ContentType())
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if c.Request.ParseForm() != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	form := c.Request.PostForm
	if len(form["grant_type"]) != 1 || form.Get("grant_type") != "client_credentials" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported_grant_type"})
		return
	}
	if len(form["scope"]) > 1 || (form.Get("scope") != "" && form.Get("scope") != service.IntegrationBalanceScope) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_scope"})
		return
	}
	for key := range form {
		if key != "grant_type" && key != "scope" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return
		}
	}
	client, err := service.AuthenticateIntegrationClient(c.Request.Context(), clientID, secret)
	if err != nil {
		if errors.Is(err, service.ErrIntegrationCredential) {
			logger.LogWarn(c.Request.Context(), common.LogText("integration client authentication rejected (request_id=%s)", c.GetString(common.RequestIdKey)))
			c.Header("WWW-Authenticate", `Basic realm="new-api-integration"`)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_client"})
			return
		}
		integrationUnavailable(c)
		return
	}
	token, expires, err := service.IssueIntegrationToken(client)
	if err != nil {
		integrationUnavailable(c)
		return
	}
	if err := model.TouchIntegrationClient(c.Request.Context(), client.Id, client.Version); err != nil {
		integrationUnavailable(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"access_token": token, "token_type": "Bearer", "expires_in": expires, "scope": service.IntegrationBalanceScope})
}

func GetIntegrationBalance(c *gin.Context) {
	value, ok := c.Get("integration_client")
	client, valid := value.(*model.IntegrationClient)
	if !ok || !valid || !service.IntegrationClientActive(client) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		return
	}
	subject := c.Query("subject")
	if subject == "" || len(subject) > 255 || strings.TrimSpace(subject) != subject ||
		len(c.Request.URL.Query()) != 1 || len(c.Request.URL.Query()["subject"]) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_subject"})
		return
	}
	user, err := model.FindIntegrationUser(c.Request.Context(), subject)
	if err != nil {
		integrationUnavailable(c)
		return
	}
	if user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account_not_available"})
		return
	}
	quota, err := model.GetUserQuota(user.Id, false)
	if err != nil {
		integrationUnavailable(c)
		return
	}
	unit := operation_setting.GetQuotaDisplayType()
	symbol := operation_setting.GetCurrencySymbol()
	remaining := decimal.NewFromInt(int64(quota))
	if unit != operation_setting.QuotaDisplayTypeTokens {
		rate := operation_setting.GetUsdToCurrencyRate(operation_setting.USDExchangeRate)
		if common.QuotaPerUnit <= 0 || math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) ||
			rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			integrationUnavailable(c)
			return
		}
		remaining = remaining.Div(decimal.NewFromFloat(common.QuotaPerUnit)).Mul(decimal.NewFromFloat(rate))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"remaining": remaining.String(), "unit": unit, "symbol": symbol,
		"as_of": time.Now().UTC().Format(time.RFC3339),
	}})
}

func integrationUnavailable(c *gin.Context) {
	logger.LogError(c.Request.Context(), common.LogText("integration service unavailable (request_id=%s)", c.GetString(common.RequestIdKey)))
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "temporarily_unavailable"})
}
