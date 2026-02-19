package oauth

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
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
	PDSURL          string // For dev mode: resolve handles via local PDS
	Scopes          []string
	SessionTTL      time.Duration
	SealedTokenTTL  time.Duration
	DevMode         bool
	AllowPrivateIPs bool

	// Confidential client (optional - if set, upgrades to confidential)
	ClientPrivateKeyMultibase string
	ClientKeyID               string
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
	// - Public clients: 2-week (14 day) maximum session lifetime (enforced by auth server)
	// - Confidential clients: up to 2 years maximum session lifetime
	// Note: The auth server ultimately enforces these limits. Public clients with longer TTLs
	// configured here will still be limited to 14 days by the auth server.
	if config.SessionTTL == 0 {
		config.SessionTTL = 365 * 24 * time.Hour // 1 year (confidential clients only)
	}
	if config.SealedTokenTTL == 0 {
		config.SealedTokenTTL = 548 * 24 * time.Hour // 18 months (confidential clients only)
	}

	// Create indigo client config
	var clientConfig oauth.ClientConfig
	if config.DevMode {
		// Dev mode: loopback OAuth client
		// Per ATProto OAuth spec: client_id base MUST be "http://localhost" (not 127.0.0.1)
		// The redirect_uri in the query params CAN use 127.0.0.1 with port
		// Format: http://localhost?redirect_uri=http%3A%2F%2F127.0.0.1%3A8081%2Foauth%2Fcallback&scope=atproto
		callbackURL := config.PublicURL + "/oauth/callback"
		clientConfig = oauth.NewLocalhostConfig(callbackURL, config.Scopes)
		slog.Info("dev mode: OAuth client configured",
			"callback_url", callbackURL,
			"client_id", clientConfig.ClientID)
	} else {
		// Production mode: public OAuth client with HTTPS
		// client_id must be the URL of the client metadata document per atproto OAuth spec
		clientID := config.PublicURL + "/oauth-client-metadata.json"
		callbackURL := config.PublicURL + "/oauth/callback"
		clientConfig = oauth.NewPublicConfig(clientID, callbackURL, config.Scopes)
	}

	// Upgrade to confidential client if private key is configured
	// Confidential clients get longer session lifetimes (up to 90/180 days vs 14 days for public)
	if config.ClientPrivateKeyMultibase != "" && config.ClientKeyID != "" {
		priv, err := atcrypto.ParsePrivateMultibase(config.ClientPrivateKeyMultibase)
		if err != nil {
			return nil, fmt.Errorf("parsing OAuth client private key: %w", err)
		}
		if err := clientConfig.SetClientSecret(priv, config.ClientKeyID); err != nil {
			return nil, fmt.Errorf("setting OAuth client secret: %w", err)
		}
		slog.Info("OAuth client configured as confidential", "key_id", config.ClientKeyID)
	} else if config.ClientPrivateKeyMultibase != "" || config.ClientKeyID != "" {
		// Partial configuration - warn operator that both fields are required
		// Without both, we fall back to public client with 14-day session limit
		slog.Warn("OAuth confidential client partially configured - both OAUTH_CLIENT_PRIVATE_KEY and OAUTH_CLIENT_KEY_ID are required",
			"has_private_key", config.ClientPrivateKeyMultibase != "",
			"has_key_id", config.ClientKeyID != "")
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
		// Log the PLC URL being used for OAuth directory resolution
		fmt.Printf("🔐 OAuth client directory configured with PLC URL: %s (AllowPrivateIPs: %v)\n", config.PLCURL, config.AllowPrivateIPs)
	} else {
		fmt.Println("⚠️  OAuth client using DEFAULT PLC directory (production plc.directory)")
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
	metadata.LogoURI = strPtr(c.Config.PublicURL + "/static/images/lil_dude.png")
	metadata.PolicyURI = strPtr(c.Config.PublicURL + "/privacy")

	if !c.Config.DevMode {
		metadata.ClientURI = strPtr(c.Config.PublicURL)
		// For confidential clients, include the JWKS URI for the public key
		if c.ClientApp.Config.IsConfidential() {
			jwksURI := c.Config.PublicURL + "/oauth-client-keys.json"
			metadata.JWKSURI = &jwksURI
		}
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
