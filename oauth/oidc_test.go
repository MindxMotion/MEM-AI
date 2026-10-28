package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestOIDCProvider_GetName(t *testing.T) {
	settings := system_setting.GetOIDCSettings()
	originalDisplayName := settings.DisplayName
	defer func() { settings.DisplayName = originalDisplayName }()

	p := &OIDCProvider{}

	settings.DisplayName = ""
	assert.Equal(t, "OIDC", p.GetName())

	settings.DisplayName = "  Acme SSO  "
	assert.Equal(t, "Acme SSO", p.GetName())
}

func configureOIDCTest(t *testing.T, issuer string) {
	t.Helper()
	t.Setenv("OIDC_ENABLED", "true")
	t.Setenv("OIDC_ISSUER_URL", issuer)
	t.Setenv("OIDC_REDIRECT_URI", "https://gateway.example.com/new-api/oauth/oidc")
	t.Setenv("OIDC_CLIENT_ID", "fixture-client")
	t.Setenv("OIDC_CLIENT_SECRET", "fixture-secret")
	t.Setenv("OIDC_SCOPES", "openid profile email")
	t.Setenv("OIDC_TOKEN_AUTH_METHOD", "client_secret_post")
	require.NoError(t, system_setting.LoadOIDCEnvironment())
	t.Cleanup(func() {
		require.NoError(t, os.Unsetenv("OIDC_ENABLED"))
		require.NoError(t, system_setting.LoadOIDCEnvironment())
	})
}

func TestOIDCEnvironmentConfiguration(t *testing.T) {
	configureOIDCTest(t, "https://identity.example.com/realms/test")
	settings := system_setting.GetOIDCSettings()
	require.True(t, system_setting.OIDCManagedByEnvironment())
	assert.Equal(t, "fixture-client", settings.ClientId)
	assert.True(t, settings.Enabled)

	for _, test := range []struct {
		name, key, value string
	}{
		{"invalid toggle", "OIDC_ENABLED", "yes"},
		{"missing issuer", "OIDC_ISSUER_URL", ""},
		{"insecure issuer", "OIDC_ISSUER_URL", "http://identity.example.com/realms/test"},
		{"issuer query", "OIDC_ISSUER_URL", "https://identity.example.com/realms/test?secret=fixture"},
		{"missing redirect", "OIDC_REDIRECT_URI", ""},
		{"redirect credentials", "OIDC_REDIRECT_URI", "https://user:fixture@gateway.example.com/oauth/oidc"},
		{"missing client", "OIDC_CLIENT_ID", ""},
		{"missing secret", "OIDC_CLIENT_SECRET", ""},
		{"missing openid", "OIDC_SCOPES", "profile email"},
		{"unsupported client authentication", "OIDC_TOKEN_AUTH_METHOD", "none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			err := system_setting.LoadOIDCEnvironment()
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "fixture-secret")
			assert.NotContains(t, err.Error(), "identity.example.com")
			assert.NotContains(t, err.Error(), "gateway.example.com")
		})
	}
	t.Setenv("OIDC_ENABLED", "false")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	require.NoError(t, system_setting.LoadOIDCEnvironment())
	assert.False(t, system_setting.GetOIDCSettings().Enabled)
	_, err := system_setting.GetOIDCConfiguration()
	assert.Error(t, err)
}

func TestOIDCVerifiedAuthorizationCodeFlow(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	var issuer, expectedVerifier, nonce, rawToken string
	var form url.Values
	metadataIssuer, tokenStatus, userSubject := "", http.StatusOK, "subject-1"
	emailVerified := true
	redirectEndpoint := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			discoveryIssuer := issuer
			if metadataIssuer != "" {
				discoveryIssuer = metadataIssuer
			}
			body = map[string]any{
				"issuer": discoveryIssuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint": issuer + "/token", "userinfo_endpoint": issuer + "/userinfo", "jwks_uri": issuer + "/keys",
				"code_challenge_methods_supported":      []string{"S256"},
				"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
			}
		case "/keys":
			body = map[string]any{"keys": []any{map[string]any{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "fixture",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}}
		case "/token":
			require.NoError(t, r.ParseForm())
			form = r.PostForm
			if tokenStatus != http.StatusOK {
				w.WriteHeader(tokenStatus)
				return
			}
			if redirectEndpoint {
				http.Redirect(w, r, issuer+"/leak", http.StatusFound)
				return
			}
			if r.PostForm.Get("code_verifier") != expectedVerifier {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			body = map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "id_token": rawToken}
		case "/userinfo":
			assert.Equal(t, "Bearer fixture-access", r.Header.Get("Authorization"))
			body = map[string]any{"sub": userSubject, "email": "user@example.com", "email_verified": emailVerified, "preferred_username": "fixture-user"}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, err := common.Marshal(body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(data)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	issuer = server.URL
	configureOIDCTest(t, issuer)
	provider := NewOIDCProvider(&http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	})

	for _, test := range []struct {
		name   string
		change func(jwt.MapClaims)
	}{
		{name: "valid"},
		{name: "unverified email"},
		{name: "wrong issuer", change: func(c jwt.MapClaims) { c["iss"] = "https://other.example.com" }},
		{name: "wrong audience", change: func(c jwt.MapClaims) { c["aud"] = "other-client" }},
		{name: "expired", change: func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{name: "wrong nonce", change: func(c jwt.MapClaims) { c["nonce"] = "different-transaction" }},
		{name: "missing nonce", change: func(c jwt.MapClaims) { delete(c, "nonce") }},
		{name: "missing subject", change: func(c jwt.MapClaims) { c["sub"] = "" }},
		{name: "wrong authorized party", change: func(c jwt.MapClaims) { c["azp"] = "other-client" }},
		{name: "multiple audiences without authorized party", change: func(c jwt.MapClaims) { c["aud"] = []string{"fixture-client", "other-client"} }},
		{name: "future issue time", change: func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Hour).Unix() }},
		{name: "invalid signature"},
		{name: "token rejection"},
		{name: "token redirect"},
		{name: "configuration changed"},
		{name: "unverified token"},
		{name: "userinfo subject mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokenStatus, redirectEndpoint, userSubject = http.StatusOK, false, "subject-1"
			emailVerified = test.name != "unverified email"
			flow, err := provider.NewFlow(context.Background(), "fixture-browser")
			require.NoError(t, err)
			assert.True(t, flow.MatchesBrowser("fixture-browser"))
			assert.False(t, flow.MatchesBrowser("other-browser"))
			assert.False(t, flow.MatchesBrowser(""))
			authorization, err := provider.AuthorizationURL(context.Background(), flow, "fixture-state")
			require.NoError(t, err)
			parsed, err := url.Parse(authorization)
			require.NoError(t, err)
			query := parsed.Query()
			assert.Equal(t, "S256", query.Get("code_challenge_method"))
			assert.Equal(t, oauth2.S256ChallengeFromVerifier(flow.CodeVerifier), query.Get("code_challenge"))
			assert.Equal(t, "fixture-state", query.Get("state"))
			assert.Equal(t, flow.Nonce, query.Get("nonce"))
			assert.Equal(t, "https://gateway.example.com/new-api/oauth/oidc", query.Get("redirect_uri"))
			assert.NotContains(t, authorization, "fixture-secret")
			expectedVerifier, nonce = flow.CodeVerifier, flow.Nonce
			claims := jwt.MapClaims{
				"iss": issuer, "aud": "fixture-client", "sub": "subject-1", "nonce": nonce,
				"exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(),
			}
			if test.change != nil {
				test.change(claims)
			}
			signing := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			signing.Header["kid"] = "fixture"
			rawToken, err = signing.SignedString(key)
			require.NoError(t, err)
			switch test.name {
			case "invalid signature":
				parts := strings.Split(rawToken, ".")
				signature, err := base64.RawURLEncoding.DecodeString(parts[2])
				require.NoError(t, err)
				signature[0] ^= 1
				parts[2] = base64.RawURLEncoding.EncodeToString(signature)
				rawToken = strings.Join(parts, ".")
			case "token rejection":
				tokenStatus = http.StatusUnauthorized
			case "token redirect":
				redirectEndpoint = true
			case "configuration changed":
				flow.Configuration = "previous-configuration"
			case "userinfo subject mismatch":
				userSubject = "different-subject"
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set(OIDCFlowContextKey, flow)
			if test.name == "unverified token" {
				_, err := provider.GetUserInfo(context.Background(), &OAuthToken{AccessToken: "fixture-access"})
				require.Error(t, err)
				return
			}
			token, err := provider.ExchangeToken(context.Background(), "fixture-code", c)
			if test.name != "valid" && test.name != "unverified email" && test.name != "userinfo subject mismatch" {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "fixture-secret")
				assert.NotContains(t, err.Error(), issuer)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, flow.CodeVerifier, form.Get("code_verifier"))
			assert.Equal(t, "fixture-secret", form.Get("client_secret"))
			user, err := provider.GetUserInfo(context.Background(), token)
			if test.name == "userinfo subject mismatch" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "subject-1", user.ProviderUserID)
			if emailVerified {
				assert.Equal(t, "user@example.com", user.Email)
			} else {
				assert.Empty(t, user.Email)
			}
		})
	}
	metadataIssuer = "https://different.example.com"
	_, err = NewOIDCProvider(server.Client()).NewFlow(context.Background(), "fixture-browser")
	require.Error(t, err)
}
