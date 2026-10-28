package oauth

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
)

const OIDCFlowContextKey = "oidc_oauth_flow"

type OIDCFlow struct {
	CodeVerifier  string `json:"code_verifier"`
	Nonce         string `json:"nonce"`
	Configuration string `json:"configuration"`
	BrowserHash   string `json:"browser_hash"`
}

type oidcMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserInfoEndpoint      string   `json:"userinfo_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	PKCEMethods           []string `json:"code_challenge_methods_supported"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
}

type oidcUser struct {
	OpenID            string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	AuthorizedParty   string `json:"azp"`
}

type OIDCProvider struct {
	client   *http.Client
	mutex    sync.Mutex
	metadata oidcMetadata
	keys     oidc.KeySet
	expires  time.Time
}

func init() {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	Register("oidc", NewOIDCProvider(&http.Client{
		Timeout: 15 * time.Second, Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("OIDC endpoint redirects are not allowed")
		},
	}))
}

func NewOIDCProvider(client *http.Client) *OIDCProvider {
	return &OIDCProvider{client: client}
}

func (p *OIDCProvider) GetName() string {
	return system_setting.GetOIDCSettings().GetEffectiveDisplayName()
}

func (p *OIDCProvider) IsEnabled() bool {
	_, err := system_setting.GetOIDCConfiguration()
	return err == nil
}

func oidcConfigurationHash(configuration system_setting.OIDCConfiguration) (string, error) {
	encoded, err := common.Marshal(configuration)
	if err != nil {
		return "", err
	}
	return common.GenerateHMACWithKey([]byte("oidc-configuration:"+common.SessionSecret), string(encoded)), nil
}

func oidcFailure(stage string) error {
	common.SysError("OIDC authentication failed: " + stage)
	return NewOAuthError(common.NewMessage("Failed to initialize OAuth"))
}

func (p *OIDCProvider) discover(ctx context.Context, configuration system_setting.OIDCConfiguration) (oidcMetadata, oidc.KeySet, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.metadata.Issuer == configuration.Issuer && time.Now().Before(p.expires) {
		return p.metadata, p.keys, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, configuration.Issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return oidcMetadata{}, nil, oidcFailure("discovery request")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return oidcMetadata{}, nil, oidcFailure("discovery connection")
	}
	defer response.Body.Close()
	var metadata oidcMetadata
	if response.StatusCode != http.StatusOK {
		return oidcMetadata{}, nil, oidcFailure("discovery status")
	}
	if err := common.DecodeJson(io.LimitReader(response.Body, 1<<20), &metadata); err != nil {
		return oidcMetadata{}, nil, oidcFailure("discovery response")
	}
	issuer, err := url.Parse(configuration.Issuer)
	if err != nil || metadata.Issuer != configuration.Issuer || !slices.Contains(metadata.PKCEMethods, "S256") {
		return oidcMetadata{}, nil, oidcFailure("discovery issuer or PKCE")
	}
	for _, endpoint := range []string{metadata.AuthorizationEndpoint, metadata.TokenEndpoint, metadata.UserInfoEndpoint, metadata.JWKSURI} {
		value, err := url.Parse(endpoint)
		if err != nil || value.Scheme != issuer.Scheme || value.Host != issuer.Host ||
			value.User != nil || value.Fragment != "" {
			return oidcMetadata{}, nil, oidcFailure("discovery endpoint origin")
		}
	}
	if len(metadata.AuthMethods) == 0 {
		metadata.AuthMethods = []string{"client_secret_basic"}
	}
	p.metadata = metadata
	p.keys = oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), p.client), metadata.JWKSURI)
	p.expires = time.Now().Add(5 * time.Minute)
	return metadata, p.keys, nil
}

func (p *OIDCProvider) NewFlow(ctx context.Context, browserToken string) (*OIDCFlow, error) {
	configuration, err := system_setting.GetOIDCConfiguration()
	if err != nil {
		return nil, oidcFailure("configuration")
	}
	if _, _, err := p.discover(ctx, configuration); err != nil {
		return nil, err
	}
	fingerprint, err := oidcConfigurationHash(configuration)
	if err != nil {
		return nil, err
	}
	return &OIDCFlow{
		CodeVerifier: oauth2.GenerateVerifier(), Nonce: oauth2.GenerateVerifier(),
		Configuration: fingerprint,
		BrowserHash:   common.GenerateHMACWithKey([]byte("oidc-browser:"+common.SessionSecret), browserToken),
	}, nil
}

func (p *OIDCProvider) AuthorizationURL(ctx context.Context, flow *OIDCFlow, state string) (string, error) {
	configuration, err := system_setting.GetOIDCConfiguration()
	if err != nil {
		return "", oidcFailure("configuration")
	}
	metadata, _, err := p.discover(ctx, configuration)
	if err != nil {
		return "", err
	}
	value, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return "", oidcFailure("authorization endpoint")
	}
	query := value.Query()
	for key, entry := range map[string]string{
		"client_id": configuration.Settings.ClientId, "redirect_uri": configuration.RedirectURI,
		"response_type": "code", "scope": configuration.Scopes, "state": state, "nonce": flow.Nonce,
		"code_challenge": oauth2.S256ChallengeFromVerifier(flow.CodeVerifier), "code_challenge_method": "S256",
	} {
		query.Set(key, entry)
	}
	value.RawQuery = query.Encode()
	return value.String(), nil
}

func (flow *OIDCFlow) MatchesBrowser(browserToken string) bool {
	if browserToken == "" || flow.BrowserHash == "" {
		return false
	}
	hash := common.GenerateHMACWithKey([]byte("oidc-browser:"+common.SessionSecret), browserToken)
	return subtle.ConstantTimeCompare([]byte(hash), []byte(flow.BrowserHash)) == 1
}

func (p *OIDCProvider) ExchangeToken(ctx context.Context, code string, c *gin.Context) (*OAuthToken, error) {
	configuration, err := system_setting.GetOIDCConfiguration()
	if err != nil {
		return nil, oidcFailure("configuration")
	}
	value, _ := c.Get(OIDCFlowContextKey)
	flow, ok := value.(*OIDCFlow)
	fingerprint, err := oidcConfigurationHash(configuration)
	if err != nil || !ok || flow == nil || flow.CodeVerifier == "" || flow.Nonce == "" ||
		code == "" || subtle.ConstantTimeCompare([]byte(fingerprint), []byte(flow.Configuration)) != 1 {
		return nil, oidcFailure("flow or configuration mismatch")
	}
	metadata, keys, err := p.discover(ctx, configuration)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(metadata.AuthMethods, configuration.TokenAuthMethod) {
		return nil, oidcFailure("unsupported client authentication")
	}
	values := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {configuration.Settings.ClientId}, "redirect_uri": {configuration.RedirectURI},
		"code_verifier": {flow.CodeVerifier},
	}
	if configuration.TokenAuthMethod == "client_secret_post" {
		values.Set("client_secret", configuration.Settings.ClientSecret)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.TokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, oidcFailure("token request")
	}
	if configuration.TokenAuthMethod == "client_secret_basic" {
		request.SetBasicAuth(url.QueryEscape(configuration.Settings.ClientId), url.QueryEscape(configuration.Settings.ClientSecret))
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return nil, oidcFailure("token connection")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, oidcFailure("token status")
	}
	var token OAuthToken
	if err := common.DecodeJson(io.LimitReader(response.Body, 1<<20), &token); err != nil ||
		token.IDToken == "" || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return nil, oidcFailure("token response")
	}
	verifier := oidc.NewVerifier(configuration.Issuer, keys, &oidc.Config{
		ClientID: configuration.Settings.ClientId, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.PS256},
	})
	verified, err := verifier.Verify(ctx, token.IDToken)
	if err != nil || verified.Subject == "" ||
		subtle.ConstantTimeCompare([]byte(verified.Nonce), []byte(flow.Nonce)) != 1 ||
		verified.IssuedAt.After(time.Now().Add(time.Minute)) {
		return nil, oidcFailure("ID token verification")
	}
	var rawClaims json.RawMessage
	if err := verified.Claims(&rawClaims); err != nil {
		return nil, oidcFailure("ID token claims")
	}
	var claims oidcUser
	if err := common.Unmarshal(rawClaims, &claims); err != nil ||
		(len(verified.Audience) > 1 && claims.AuthorizedParty == "") ||
		(claims.AuthorizedParty != "" && claims.AuthorizedParty != configuration.Settings.ClientId) {
		return nil, oidcFailure("ID token authorized party")
	}
	if verified.AccessTokenHash != "" {
		if err := verified.VerifyAccessToken(token.AccessToken); err != nil {
			return nil, oidcFailure("access token hash")
		}
	}
	token.ClientID = configuration.Settings.ClientId
	token.oidcSubject = verified.Subject
	token.oidcConfiguration = fingerprint
	return &token, nil
}

func (p *OIDCProvider) GetUserInfo(ctx context.Context, token *OAuthToken) (*OAuthUser, error) {
	configuration, err := system_setting.GetOIDCConfiguration()
	if err != nil {
		return nil, oidcFailure("configuration")
	}
	fingerprint, err := oidcConfigurationHash(configuration)
	if err != nil || token == nil || token.oidcSubject == "" ||
		token.ClientID != configuration.Settings.ClientId || token.oidcConfiguration != fingerprint {
		return nil, oidcFailure("unverified token")
	}
	metadata, _, err := p.discover(ctx, configuration)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadata.UserInfoEndpoint, nil)
	if err != nil {
		return nil, oidcFailure("userinfo request")
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response, err := p.client.Do(request)
	if err != nil {
		return nil, oidcFailure("userinfo connection")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, oidcFailure("userinfo status")
	}
	var user oidcUser
	if err := common.DecodeJson(io.LimitReader(response.Body, 1<<20), &user); err != nil ||
		user.OpenID != token.oidcSubject || user.Email == "" {
		return nil, oidcFailure("userinfo subject or claims")
	}
	if !user.EmailVerified {
		user.Email = ""
	}
	return &OAuthUser{
		ProviderUserID: user.OpenID, Username: user.PreferredUsername, DisplayName: user.Name, Email: user.Email,
	}, nil
}

func (p *OIDCProvider) IsUserIDTaken(id string) bool { return model.IsOidcIdAlreadyTaken(id) }

func (p *OIDCProvider) FillUserByProviderID(user *model.User, id string) error {
	user.OidcId = id
	return user.FillUserByOidcId()
}

func (p *OIDCProvider) SetProviderUserID(user *model.User, id string) { user.OidcId = id }

func (p *OIDCProvider) GetProviderPrefix() string { return "oidc_" }

func (p *OIDCProvider) ProviderUserIDColumn() string { return "oidc_id" }
