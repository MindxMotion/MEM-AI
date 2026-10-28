package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOIDCRegistrationPolicy(t *testing.T) {
	db := setupManageUserTestDB(t)
	previousRegister, previousQuota := common.RegisterEnabled, common.QuotaForNewUser
	t.Cleanup(func() {
		common.RegisterEnabled, common.QuotaForNewUser = previousRegister, previousQuota
		require.NoError(t, os.Unsetenv("OIDC_ENABLED"))
		require.NoError(t, system_setting.LoadOIDCEnvironment())
	})
	common.QuotaForNewUser = 0
	t.Setenv("OIDC_ENABLED", "true")
	t.Setenv("OIDC_ISSUER_URL", "https://identity.example.com/realms/test")
	t.Setenv("OIDC_REDIRECT_URI", "https://gateway.example.com/new-api/oauth/oidc")
	t.Setenv("OIDC_CLIENT_ID", "fixture-client")
	t.Setenv("OIDC_CLIENT_SECRET", "fixture-secret")
	t.Setenv("OIDC_SCOPES", "openid profile email")
	t.Setenv("OIDC_TOKEN_AUTH_METHOD", "client_secret_post")
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodGet, "/api/oauth/oidc", nil)

	for _, test := range []struct {
		name, policy string
		provider     oauth.Provider
		publicSignup bool
		allowed      bool
	}{
		{"closed by default", "", &oauth.OIDCProvider{}, false, false},
		{"explicitly closed", "false", &oauth.OIDCProvider{}, false, false},
		{"OIDC only", "true", &oauth.OIDCProvider{}, false, true},
		{"other OAuth stays closed", "true", &authFlowTestOAuthProvider{}, false, false},
		{"public signup unchanged", "false", &oauth.OIDCProvider{}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("OIDC_ALLOW_REGISTRATION", test.policy)
			require.NoError(t, system_setting.LoadOIDCEnvironment())
			common.RegisterEnabled = test.publicSignup
			identity := &oauth.OAuthUser{
				ProviderUserID: test.name,
				Username:       strings.ReplaceAll(test.name, " ", "_"),
			}
			var before int64
			require.NoError(t, db.Model(&model.User{}).Count(&before).Error)
			user, migration, err := findOrCreateOAuthUser(context, test.provider, identity, &oauth.OAuthToken{}, "")
			if !test.allowed {
				var registrationDisabled *OAuthRegistrationDisabledError
				require.ErrorAs(t, err, &registrationDisabled)
				assert.Nil(t, user)
				var after int64
				require.NoError(t, db.Model(&model.User{}).Count(&after).Error)
				assert.Equal(t, before, after)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, user)
			assert.Nil(t, migration)
			assert.Equal(t, common.RoleCommonUser, user.Role)
			assert.Equal(t, common.UserStatusEnabled, user.Status)
			assert.Empty(t, user.Password)
			assert.Equal(t, identity.ProviderUserID, user.OidcId)
			assert.Equal(t, test.publicSignup, common.RegisterEnabled)

			require.NoError(t, db.Model(user).Update("quota", 12345).Error)
			common.RegisterEnabled = false
			t.Setenv("OIDC_ALLOW_REGISTRATION", "false")
			require.NoError(t, system_setting.LoadOIDCEnvironment())
			existing, _, err := findOrCreateOAuthUser(context, test.provider, identity, &oauth.OAuthToken{}, "")
			require.NoError(t, err)
			assert.Equal(t, user.Id, existing.Id)
			assert.Equal(t, 12345, existing.Quota)
		})
	}

	t.Setenv("OIDC_ALLOW_REGISTRATION", "true")
	require.NoError(t, system_setting.LoadOIDCEnvironment())
	common.RegisterEnabled = false
	owner := model.User{
		Username: "existing-owner", Email: "owner@example.com",
		Role: common.RoleRootUser, Status: common.UserStatusEnabled, Quota: 54321,
	}
	require.NoError(t, db.Create(&owner).Error)
	duplicateName, _, err := findOrCreateOAuthUser(context, &oauth.OIDCProvider{}, &oauth.OAuthUser{
		ProviderUserID: "new-subject", Username: owner.Username,
	}, &oauth.OAuthToken{}, "")
	require.NoError(t, err)
	assert.NotEqual(t, owner.Id, duplicateName.Id)
	assert.NotEqual(t, owner.Username, duplicateName.Username)
	assert.Equal(t, common.RoleCommonUser, duplicateName.Role)

	_, _, err = findOrCreateOAuthUser(context, &oauth.OIDCProvider{}, &oauth.OAuthUser{
		ProviderUserID: "email-conflict", Email: owner.Email,
	}, &oauth.OAuthToken{}, "")
	var emailConflict *OAuthEmailAlreadyTakenError
	require.ErrorAs(t, err, &emailConflict)
	require.NoError(t, db.First(&owner, owner.Id).Error)
	assert.Empty(t, owner.OidcId)
	assert.Equal(t, common.RoleRootUser, owner.Role)
	assert.Equal(t, 54321, owner.Quota)

	signup := httptest.NewRecorder()
	signupContext, _ := gin.CreateTestContext(signup)
	signupContext.Request = httptest.NewRequest(http.MethodPost, "/api/user/register", strings.NewReader(`{}`))
	Register(signupContext)
	assert.Contains(t, signup.Body.String(), `"success":false`)
	assert.Contains(t, signup.Body.String(), "New user registration has been disabled by administrator")

	t.Setenv("OIDC_ENABLED", "false")
	require.NoError(t, system_setting.LoadOIDCEnvironment())
	assert.False(t, system_setting.OIDCRegistrationEnabled())
	t.Setenv("OIDC_ENABLED", "true")
	t.Setenv("OIDC_ALLOW_REGISTRATION", "invalid")
	require.ErrorContains(t, system_setting.LoadOIDCEnvironment(), "OIDC_ALLOW_REGISTRATION")
}

func TestGetStatusReturnsEffectiveOIDCDisplayName(t *testing.T) {
	settings := system_setting.GetOIDCSettings()
	originalDisplayName := settings.DisplayName
	originalOptionMap := common.OptionMap
	t.Cleanup(func() {
		settings.DisplayName = originalDisplayName
		common.OptionMap = originalOptionMap
	})
	common.OptionMap = map[string]string{}

	tests := []struct {
		name        string
		displayName string
		want        string
	}{
		{
			name:        "custom name is trimmed",
			displayName: "  Acme SSO  ",
			want:        "Acme SSO",
		},
		{
			name:        "whitespace-only name falls back",
			displayName: "   ",
			want:        "OIDC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings.DisplayName = tt.displayName
			response := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(response)
			context.Request = httptest.NewRequest(http.MethodGet, "/api/status", nil)

			GetStatus(context)

			var payload struct {
				Success bool           `json:"success"`
				Data    map[string]any `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
			require.True(t, payload.Success)
			assert.Equal(t, tt.want, payload.Data["oidc_display_name"])
		})
	}
}

func TestOIDCStateRequiresOriginalBrowserAndRejectsReplay(t *testing.T) {
	setupAuthFlowControllerTest(t)
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, err := common.Marshal(map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
			"userinfo_endpoint": issuer + "/userinfo", "jwks_uri": issuer + "/keys",
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
		})
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(body)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	issuer = server.URL
	settings := system_setting.GetOIDCSettings()
	previousSettings, previousAddress := *settings, system_setting.ServerAddress
	previousProvider := oauth.GetProvider("oidc")
	previousCookiePath := common.SessionHintCookiePath
	t.Cleanup(func() {
		*settings, system_setting.ServerAddress = previousSettings, previousAddress
		common.SessionHintCookiePath = previousCookiePath
		oauth.Register("oidc", previousProvider)
	})
	*settings = system_setting.OIDCSettings{
		Enabled: true, ClientId: "fixture-client", ClientSecret: "fixture-secret",
		WellKnown: issuer + "/.well-known/openid-configuration",
	}
	system_setting.ServerAddress = "https://gateway.example.com/new-api"
	common.SessionHintCookiePath = "/new-api/"
	oauth.Register("oidc", oauth.NewOIDCProvider(server.Client()))
	started := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(started)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/oauth/state", strings.NewReader(`{"provider":"oidc","intent":"login"}`))
	GenerateOAuthCode(context)
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Token string `json:"flow_token"`
			URL   string `json:"authorization_url"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(started.Body.Bytes(), &result))
	require.True(t, result.Success)
	require.Contains(t, result.Data.URL, "code_challenge_method=S256")
	require.NotContains(t, result.Data.URL, "fixture-secret")
	cookies := started.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.True(t, cookies[0].HttpOnly)
	assert.True(t, cookies[0].Secure)
	assert.Equal(t, http.SameSiteLaxMode, cookies[0].SameSite)
	assert.Equal(t, "/new-api/", cookies[0].Path)

	router := gin.New()
	router.GET("/api/oauth/:provider", HandleOAuth)
	for _, test := range []struct {
		name   string
		cookie *http.Cookie
		status int
	}{
		{"missing browser", nil, http.StatusForbidden},
		{"wrong browser", &http.Cookie{Name: cookies[0].Name, Value: "other-browser"}, http.StatusForbidden},
		{"original browser", cookies[0], http.StatusOK},
		{"replay", cookies[0], http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/oauth/oidc?state="+result.Data.Token+"&error=access_denied&error_description=private-provider-details", nil)
			if test.cookie != nil {
				request.AddCookie(test.cookie)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			assert.Equal(t, test.status, recorder.Code)
			assert.NotContains(t, recorder.Body.String(), "private-provider-details")
		})
	}
	_, err := model.GetAuthFlow(result.Data.Token, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeOAuth, Provider: "oidc"})
	assert.ErrorIs(t, err, model.ErrAuthFlowConsumed)
}
