package oauth

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
)

// OAuthClient wraps indigo's OAuth ClientApp with Coves-specific configuration
type OAuthClient struct {
	ClientApp  *oauth.ClientApp
	Config     *OAuthConfig
	SealSecret []byte // For sealing mobile tokens
}

// OAuthConfig holds Coves OAuth client configuration
type OAuthConfig struct {
	PublicURL       string
	SealSecret      string
	PLCURL          string
	Scopes          []string
	SessionTTL      time.Duration
	SealedTokenTTL  time.Duration
	DevMode         bool
	AllowPrivateIPs bool
}

// NewOAuthClient creates a new OAuth client for Coves
func NewOAuthClient(config *OAuthConfig, store oauth.ClientAuthStore) (*OAuthClient, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}

	// Validate seal secret
	var sealSecret []byte
	if config.SealSecret != "" {
		decoded, err := base64.StdEncoding.DecodeString(config.SealSecret)
		if err != nil {
			return nil, fmt.Errorf("failed to decode seal secret: %w", err)
		}
		if len(decoded) != 32 {
			return nil, fmt.Errorf("seal secret must be 32 bytes, got %d", len(decoded))
		}
		sealSecret = decoded
	}

	// Validate scopes
	if len(config.Scopes) == 0 {
		return nil, fmt.Errorf("scopes are required")
	}
	hasAtproto := false
	for _, scope := range config.Scopes {
		if scope == "atproto" {
			hasAtproto = true
			break
		}
	}
	if !hasAtproto {
		return nil, fmt.Errorf("scopes must include 'atproto'")
	}

	// Set default TTL values if not specified
	// Per atproto OAuth spec:
	// - Public clients: 2-week (14 day) maximum session lifetime
	// - Confidential clients: 180-day maximum session lifetime
	if config.SessionTTL == 0 {
		config.SessionTTL = 7 * 24 * time.Hour // 7 days default
	}
	if config.SealedTokenTTL == 0 {
		config.SealedTokenTTL = 14 * 24 * time.Hour // 14 days (public client limit)
	}

	// Create indigo client config
	var clientConfig oauth.ClientConfig
	if config.DevMode {
		// Dev mode: localhost with HTTP
		callbackURL := "http://localhost:3000/oauth/callback"
		clientConfig = oauth.NewLocalhostConfig(callbackURL, config.Scopes)
	} else {
		// Production mode: public OAuth client with HTTPS
		callbackURL := config.PublicURL + "/oauth/callback"
		clientConfig = oauth.NewPublicConfig(config.PublicURL, callbackURL, config.Scopes)
	}

	// Set user agent
	clientConfig.UserAgent = "Coves/1.0"

	// Create the indigo OAuth ClientApp
	clientApp := oauth.NewClientApp(&clientConfig, store)

	// Override the default HTTP client with our SSRF-safe client
	// This protects against SSRF attacks via malicious PDS URLs, DID documents, and JWKS URIs
	clientApp.Client = NewSSRFSafeHTTPClient(config.AllowPrivateIPs)

	// Override the directory if a custom PLC URL is configured
	// This is necessary for local development with a local PLC directory
	if config.PLCURL != "" {
		// Use SSRF-safe HTTP client for PLC directory requests
		httpClient := NewSSRFSafeHTTPClient(config.AllowPrivateIPs)
		baseDir := &identity.BaseDirectory{
			PLCURL:     config.PLCURL,
			HTTPClient: *httpClient,
			UserAgent:  "Coves/1.0",
		}
		// Wrap in cache directory for better performance
		// Use pointer since CacheDirectory methods have pointer receivers
		cacheDir := identity.NewCacheDirectory(baseDir, 100_000, time.Hour*24, time.Minute*2, time.Minute*5)
		clientApp.Dir = &cacheDir
	}

	return &OAuthClient{
		ClientApp:  clientApp,
		Config:     config,
		SealSecret: sealSecret,
	}, nil
}

// ClientMetadata returns the OAuth client metadata document
func (c *OAuthClient) ClientMetadata() oauth.ClientMetadata {
	metadata := c.ClientApp.Config.ClientMetadata()

	// Add additional metadata for Coves
	metadata.ClientName = strPtr("Coves")
	if !c.Config.DevMode {
		metadata.ClientURI = strPtr(c.Config.PublicURL)
	}

	return metadata
}

// strPtr is a helper to get a pointer to a string
func strPtr(s string) *string {
	return &s
}

// ValidateCallbackURL validates that a callback URL matches the expected callback URL
func (c *OAuthClient) ValidateCallbackURL(callbackURL string) error {
	expectedCallback := c.ClientApp.Config.CallbackURL

	// Parse both URLs
	expected, err := url.Parse(expectedCallback)
	if err != nil {
		return fmt.Errorf("invalid expected callback URL: %w", err)
	}

	actual, err := url.Parse(callbackURL)
	if err != nil {
		return fmt.Errorf("invalid callback URL: %w", err)
	}

	// Compare scheme, host, and path (ignore query params)
	if expected.Scheme != actual.Scheme {
		return fmt.Errorf("callback URL scheme mismatch: expected %s, got %s", expected.Scheme, actual.Scheme)
	}
	if expected.Host != actual.Host {
		return fmt.Errorf("callback URL host mismatch: expected %s, got %s", expected.Host, actual.Host)
	}
	if expected.Path != actual.Path {
		return fmt.Errorf("callback URL path mismatch: expected %s, got %s", expected.Path, actual.Path)
	}

	return nil
}
