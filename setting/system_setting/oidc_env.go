package system_setting

import (
	"errors"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Environment-managed OIDC is deliberately separate from database-backed
// settings, so option refreshes cannot overwrite it or persist its secrets.
var oidcEnvironment *OIDCConfiguration

type OIDCConfiguration struct {
	Settings          OIDCSettings
	Issuer            string
	RedirectURI       string
	Scopes            string
	TokenAuthMethod   string
	AllowRegistration bool
}

func OIDCManagedByEnvironment() bool {
	return oidcEnvironment != nil
}

func OIDCRegistrationEnabled() bool {
	return oidcEnvironment != nil && oidcEnvironment.Settings.Enabled && oidcEnvironment.AllowRegistration
}

func LoadOIDCEnvironment() error {
	raw, managed := os.LookupEnv("OIDC_ENABLED")
	if !managed {
		oidcEnvironment = nil
		return nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return errors.New("OIDC_ENABLED must be true or false")
	}
	allowRegistration := false
	if raw := os.Getenv("OIDC_ALLOW_REGISTRATION"); raw != "" {
		allowRegistration, err = strconv.ParseBool(raw)
		if err != nil {
			return errors.New("OIDC_ALLOW_REGISTRATION must be true or false")
		}
	}
	configuration := &OIDCConfiguration{
		Settings: OIDCSettings{
			Enabled:      enabled,
			DisplayName:  strings.TrimSpace(os.Getenv("OIDC_DISPLAY_NAME")),
			ClientId:     strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID")),
			ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		},
		Issuer:            strings.TrimRight(strings.TrimSpace(os.Getenv("OIDC_ISSUER_URL")), "/"),
		RedirectURI:       strings.TrimSpace(os.Getenv("OIDC_REDIRECT_URI")),
		Scopes:            strings.Join(strings.Fields(os.Getenv("OIDC_SCOPES")), " "),
		TokenAuthMethod:   strings.TrimSpace(os.Getenv("OIDC_TOKEN_AUTH_METHOD")),
		AllowRegistration: allowRegistration,
	}
	if configuration.Scopes == "" {
		configuration.Scopes = "openid profile email"
	}
	if configuration.TokenAuthMethod == "" {
		configuration.TokenAuthMethod = "client_secret_post"
	}
	if enabled {
		if !validOIDCURL(configuration.Issuer) || !validOIDCURL(configuration.RedirectURI) ||
			configuration.Settings.ClientId == "" || strings.TrimSpace(configuration.Settings.ClientSecret) == "" ||
			!slices.Contains(strings.Fields(configuration.Scopes), "openid") ||
			(configuration.TokenAuthMethod != "client_secret_post" && configuration.TokenAuthMethod != "client_secret_basic") {
			return errors.New("OIDC environment configuration is incomplete or invalid; check issuer, redirect URI, client credentials, scopes and token authentication method")
		}
	}
	configuration.Settings.WellKnown = configuration.Issuer + "/.well-known/openid-configuration"
	oidcEnvironment = configuration
	return nil
}

func validOIDCURL(raw string) bool {
	value, err := url.Parse(raw)
	if err != nil || value.Host == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
		return false
	}
	if value.Scheme == "https" {
		return true
	}
	return value.Scheme == "http" && slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, value.Hostname())
}

func GetOIDCConfiguration() (OIDCConfiguration, error) {
	if oidcEnvironment != nil {
		if !oidcEnvironment.Settings.Enabled {
			return OIDCConfiguration{}, errors.New("OIDC is disabled")
		}
		return *oidcEnvironment, nil
	}
	settings := *GetOIDCSettings()
	issuer, ok := strings.CutSuffix(settings.WellKnown, "/.well-known/openid-configuration")
	redirectURI := strings.TrimRight(ServerAddress, "/") + "/oauth/oidc"
	if !settings.Enabled || !ok || !validOIDCURL(issuer) || !validOIDCURL(redirectURI) ||
		settings.ClientId == "" || settings.ClientSecret == "" {
		return OIDCConfiguration{}, errors.New("OIDC configuration is incomplete or disabled")
	}
	return OIDCConfiguration{
		Settings: settings, Issuer: issuer, RedirectURI: redirectURI,
		Scopes: "openid profile email", TokenAuthMethod: "client_secret_post",
	}, nil
}
