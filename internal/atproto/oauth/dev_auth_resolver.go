//go:build dev

package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// DevAuthResolver is a custom OAuth resolver that allows HTTP localhost URLs for development.
// The standard indigo OAuth resolver requires HTTPS and no port numbers, which breaks local testing.
type DevAuthResolver struct {
	Client         *http.Client
	UserAgent      string
	PDSURL         string // For resolving handles via local PDS
	handleResolver *DevHandleResolver
}

// ProtectedResourceMetadata matches the OAuth protected resource metadata document format
type ProtectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// NewDevAuthResolver creates a resolver that accepts localhost HTTP URLs
func NewDevAuthResolver(pdsURL string, allowPrivateIPs bool) *DevAuthResolver {
	resolver := &DevAuthResolver{
		Client:    NewSSRFSafeHTTPClient(allowPrivateIPs),
		UserAgent: "Coves/1.0",
		PDSURL:    pdsURL,
	}
	// Create handle resolver for resolving handles via local PDS
	if pdsURL != "" {
		resolver.handleResolver = NewDevHandleResolver(pdsURL, allowPrivateIPs)
	}
	return resolver
}

// ResolveAuthServerURL resolves a PDS URL to an auth server URL.
// Unlike indigo's standard resolver, this allows HTTP and ports for localhost.
func (r *DevAuthResolver) ResolveAuthServerURL(ctx context.Context, hostURL string) (string, error) {
	u, err := url.Parse(hostURL)
	if err != nil {
		return "", err
	}

	// For localhost, allow HTTP and port numbers
	isLocalhost := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	if !isLocalhost {
		// For non-localhost, enforce HTTPS and no port (standard rules)
		if u.Scheme != "https" || u.Port() != "" {
			return "", fmt.Errorf("not a valid public host URL: %s", hostURL)
		}
	}

	// Build the protected resource document URL
	var docURL string
	if isLocalhost {
		// For localhost, preserve the port and use HTTP
		port := u.Port()
		if port == "" {
			port = "3001" // Default PDS port
		}
		docURL = fmt.Sprintf("http://%s:%s/.well-known/oauth-protected-resource", u.Hostname(), port)
	} else {
		docURL = fmt.Sprintf("https://%s/.well-known/oauth-protected-resource", u.Hostname())
	}

	// Fetch the protected resource document
	req, err := http.NewRequestWithContext(ctx, "GET", docURL, nil)
	if err != nil {
		return "", err
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching protected resource document: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP error fetching protected resource document: %d", resp.StatusCode)
	}

	var body ProtectedResourceMetadata
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("invalid protected resource document: %w", err)
	}

	if len(body.AuthorizationServers) < 1 {
		return "", fmt.Errorf("no auth server URL in protected resource document")
	}

	authURL := body.AuthorizationServers[0]

	// Validate the auth server URL (with localhost exception)
	au, err := url.Parse(authURL)
	if err != nil {
		return "", fmt.Errorf("invalid auth server URL: %w", err)
	}

	authIsLocalhost := au.Hostname() == "localhost" || au.Hostname() == "127.0.0.1"
	if !authIsLocalhost {
		if au.Scheme != "https" || au.Port() != "" {
			return "", fmt.Errorf("invalid auth server URL: %s", authURL)
		}
	}

	return authURL, nil
}

// ResolveAuthServerMetadataDev fetches OAuth server metadata from a given auth server URL.
// Unlike indigo's resolver, this allows HTTP and ports for localhost.
func (r *DevAuthResolver) ResolveAuthServerMetadataDev(ctx context.Context, serverURL string) (*oauthlib.AuthServerMetadata, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}

	// Build metadata URL - preserve port for localhost
	var metaURL string
	isLocalhost := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	if isLocalhost && u.Port() != "" {
		metaURL = fmt.Sprintf("%s://%s:%s/.well-known/oauth-authorization-server", u.Scheme, u.Hostname(), u.Port())
	} else if isLocalhost {
		metaURL = fmt.Sprintf("%s://%s/.well-known/oauth-authorization-server", u.Scheme, u.Hostname())
	} else {
		metaURL = fmt.Sprintf("https://%s/.well-known/oauth-authorization-server", u.Hostname())
	}

	slog.Debug("dev mode: fetching auth server metadata", "url", metaURL)

	req, err := http.NewRequestWithContext(ctx, "GET", metaURL, nil)
	if err != nil {
		return nil, err
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching auth server metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error fetching auth server metadata: %d", resp.StatusCode)
	}

	var metadata oauthlib.AuthServerMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("invalid auth server metadata: %w", err)
	}

	// Skip validation for localhost (indigo's Validate checks HTTPS)
	if !isLocalhost {
		if err := metadata.Validate(serverURL); err != nil {
			return nil, fmt.Errorf("invalid auth server metadata: %w", err)
		}
	}

	return &metadata, nil
}

// StartDevAuthFlow performs OAuth flow for localhost development.
// This bypasses indigo's HTTPS validation for the auth server URL.
// It resolves the identity, gets the PDS endpoint, fetches auth server metadata,
// and returns a redirect URL for the user to approve.
func (r *DevAuthResolver) StartDevAuthFlow(ctx context.Context, client *OAuthClient, identifier string, dir identity.Directory) (string, error) {
	var accountDID syntax.DID
	var pdsEndpoint string

	// Check if identifier is a handle or DID
	if strings.HasPrefix(identifier, "did:") {
		// It's a DID - look up via directory (PLC)
		atid, err := syntax.ParseAtIdentifier(identifier)
		if err != nil {
			return "", fmt.Errorf("not a valid DID (%s): %w", identifier, err)
		}
		ident, err := dir.Lookup(ctx, *atid)
		if err != nil {
			return "", fmt.Errorf("failed to resolve DID (%s): %w", identifier, err)
		}
		accountDID = ident.DID
		pdsEndpoint = ident.PDSEndpoint()
	} else {
		// It's a handle - resolve via local PDS first
		if r.handleResolver == nil {
			return "", fmt.Errorf("handle resolution not configured (PDS URL not set)")
		}

		// Resolve handle to DID via local PDS
		did, err := r.handleResolver.ResolveHandle(ctx, identifier)
		if err != nil {
			return "", fmt.Errorf("failed to resolve handle via PDS (%s): %w", identifier, err)
		}
		if did == "" {
			return "", fmt.Errorf("handle not found: %s", identifier)
		}

		slog.Info("dev mode: resolved handle via local PDS", "handle", identifier, "did", did)

		// Parse the DID
		parsedDID, err := syntax.ParseDID(did)
		if err != nil {
			return "", fmt.Errorf("invalid DID from PDS (%s): %w", did, err)
		}
		accountDID = parsedDID

		// Now look up the DID document via PLC to get PDS endpoint
		atid, err := syntax.ParseAtIdentifier(did)
		if err != nil {
			return "", fmt.Errorf("not a valid DID (%s): %w", did, err)
		}
		ident, err := dir.Lookup(ctx, *atid)
		if err != nil {
			return "", fmt.Errorf("failed to resolve DID document (%s): %w", did, err)
		}
		pdsEndpoint = ident.PDSEndpoint()
	}

	if pdsEndpoint == "" {
		return "", fmt.Errorf("identity does not link to an atproto host (PDS)")
	}

	slog.Debug("dev mode: resolving auth server",
		"did", accountDID,
		"pds", pdsEndpoint)

	// Resolve auth server URL (allowing HTTP for localhost)
	authServerURL, err := r.ResolveAuthServerURL(ctx, pdsEndpoint)
	if err != nil {
		return "", fmt.Errorf("resolving auth server: %w", err)
	}

	slog.Info("dev mode: resolved auth server", "url", authServerURL)

	// Fetch auth server metadata using our dev-friendly resolver
	authMeta, err := r.ResolveAuthServerMetadataDev(ctx, authServerURL)
	if err != nil {
		return "", fmt.Errorf("fetching auth server metadata: %w", err)
	}

	slog.Debug("dev mode: got auth server metadata",
		"issuer", authMeta.Issuer,
		"authorization_endpoint", authMeta.AuthorizationEndpoint,
		"token_endpoint", authMeta.TokenEndpoint)

	// Send auth request (PAR) using indigo's method
	info, err := client.ClientApp.SendAuthRequest(ctx, authMeta, client.Config.Scopes, identifier)
	if err != nil {
		return "", fmt.Errorf("auth request failed: %w", err)
	}

	// Set the account DID
	info.AccountDID = &accountDID

	// Persist auth request info
	client.ClientApp.Store.SaveAuthRequestInfo(ctx, *info)

	// Build redirect URL
	params := url.Values{}
	params.Set("client_id", client.ClientApp.Config.ClientID)
	params.Set("request_uri", info.RequestURI)

	authEndpoint := authMeta.AuthorizationEndpoint
	redirectURL := fmt.Sprintf("%s?%s", authEndpoint, params.Encode())

	slog.Info("dev mode: OAuth redirect URL built", "url_prefix", authEndpoint)

	return redirectURL, nil
}
