package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// mobileCallbackTemplate is the intermediate page shown after OAuth completes
// before redirecting to the mobile app via custom scheme.
// This prevents the browser from showing a stale PDS page after the redirect.
var mobileCallbackTemplate = template.Must(template.New("mobile_callback").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Login Complete - Coves</title>
  <meta http-equiv="refresh" content="1;url={{.DeepLink}}">
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
      background: #0B0F14;
      color: #e4e6e7;
      min-height: 100vh;
      display: flex;
      justify-content: center;
      align-items: center;
      padding: 24px;
    }
    .card {
      text-align: center;
      max-width: 320px;
    }
    .logo {
      width: 80px;
      height: 80px;
      margin: 0 auto 16px;
    }
    .checkmark {
      width: 64px;
      height: 64px;
      margin: 0 auto 24px;
      background: #FF6B35;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      animation: scale-in 0.3s ease-out;
    }
    .checkmark svg {
      width: 32px;
      height: 32px;
      stroke: white;
      stroke-width: 3;
      fill: none;
    }
    @keyframes scale-in {
      0% { transform: scale(0); }
      50% { transform: scale(1.1); }
      100% { transform: scale(1); }
    }
    h1 {
      font-size: 24px;
      font-weight: 600;
      margin-bottom: 8px;
      color: #e4e6e7;
    }
    .subtitle {
      font-size: 16px;
      color: #B6C2D2;
      margin-bottom: 24px;
    }
    .handle {
      font-size: 14px;
      color: #7CB9E8;
      background: #1A1F26;
      padding: 8px 16px;
      border-radius: 8px;
      margin-bottom: 24px;
      display: inline-block;
    }
    .hint {
      font-size: 13px;
      color: #6B7280;
      line-height: 1.5;
    }
    .spinner {
      width: 20px;
      height: 20px;
      border: 2px solid #2A2F36;
      border-top-color: #FF6B35;
      border-radius: 50%;
      animation: spin 1s linear infinite;
      display: inline-block;
      vertical-align: middle;
      margin-right: 8px;
    }
    @keyframes spin {
      to { transform: rotate(360deg); }
    }
  </style>
</head>
<body>
  <div class="card">
    <div class="checkmark">
      <svg viewBox="0 0 24 24">
        <polyline points="20 6 9 17 4 12"></polyline>
      </svg>
    </div>
    <h1>Login Complete</h1>
    <p class="subtitle">
      <span class="spinner"></span>
      Returning to Coves...
    </p>
    {{if .Handle}}
    <div class="handle">@{{.Handle}}</div>
    {{end}}
    <p class="hint">If the app doesn't open automatically,<br>you can close this window.</p>
  </div>
  <script>
    // Redirect to app immediately
    window.location.href = {{.DeepLink}};
    // Try to close window after a delay
    setTimeout(function() {
      window.close();
    }, 1500);
  </script>
</body>
</html>
`))

// MobileOAuthStore interface for mobile-specific OAuth operations
// This extends the base OAuth store with mobile CSRF tracking
type MobileOAuthStore interface {
	SaveMobileOAuthData(ctx context.Context, state string, data MobileOAuthData) error
	GetMobileOAuthData(ctx context.Context, state string) (*MobileOAuthData, error)
}

// UserIndexer is the minimal interface for indexing users after OAuth login.
// This decouples the OAuth handler from the full UserService.
type UserIndexer interface {
	// IndexUser creates or updates a user in the local database.
	// This is idempotent - calling it multiple times with the same DID is safe.
	IndexUser(ctx context.Context, did, handle, pdsURL string) error
}

// OAuthHandler handles OAuth-related HTTP endpoints
type OAuthHandler struct {
	client              *OAuthClient
	store               oauth.ClientAuthStore
	mobileStore         MobileOAuthStore   // For server-side CSRF validation
	userIndexer         UserIndexer        // For indexing users after OAuth login
	devResolver         *DevHandleResolver // For dev mode: resolve handles via local PDS
	devAuthResolver     *DevAuthResolver   // For dev mode: bypass HTTPS validation for localhost OAuth
	allowedRedirectURIs map[string]bool    // Combined allowlist for mobile + external OAuth clients
}

// OAuthHandlerOption is a functional option for configuring OAuthHandler
type OAuthHandlerOption func(*OAuthHandler)

// WithUserIndexer sets the user indexer for indexing users after OAuth login.
// When set, users are automatically indexed into the local database after successful authentication.
func WithUserIndexer(indexer UserIndexer) OAuthHandlerOption {
	return func(h *OAuthHandler) {
		h.userIndexer = indexer
	}
}

// NewOAuthHandler creates a new OAuth handler
func NewOAuthHandler(client *OAuthClient, store oauth.ClientAuthStore, opts ...OAuthHandlerOption) *OAuthHandler {
	handler := &OAuthHandler{
		client:              client,
		store:               store,
		allowedRedirectURIs: BuildAllowedRedirectURIs(),
	}

	// Apply functional options
	for _, opt := range opts {
		opt(handler)
	}

	// Check if the store implements MobileOAuthStore for server-side CSRF
	if mobileStore, ok := store.(MobileOAuthStore); ok {
		handler.mobileStore = mobileStore
	}

	// In dev mode, create resolvers for local PDS/PLC
	// This is needed because:
	// 1. Local handles (e.g., user.local.coves.dev) can't be resolved via DNS/HTTP
	// 2. Indigo's OAuth library requires HTTPS, which localhost doesn't have
	if client.Config.DevMode {
		if client.Config.PDSURL != "" {
			handler.devResolver = NewDevHandleResolver(client.Config.PDSURL, client.Config.AllowPrivateIPs)
			slog.Info("dev mode: handle resolution via local PDS enabled", "pds_url", client.Config.PDSURL)
		}
		// Create dev auth resolver to bypass HTTPS validation (pass PDS URL for handle resolution)
		handler.devAuthResolver = NewDevAuthResolver(client.Config.PDSURL, client.Config.AllowPrivateIPs)
		slog.Info("dev mode: localhost OAuth auth resolver enabled", "pds_url", client.Config.PDSURL)
	}

	return handler
}

// isAllowedRedirectURI checks if a redirect URI is in the configured allowlist.
// This includes both the base mobile redirect URIs and any configured external client URIs.
//
// SECURITY: Uses exact string matching - no wildcards or pattern matching.
// The URI must match exactly as configured in the allowlist.
func (h *OAuthHandler) isAllowedRedirectURI(redirectURI string) bool {
	return h.allowedRedirectURIs[redirectURI]
}

// HandleClientMetadata serves the OAuth client metadata document
// GET /oauth-client-metadata.json
func (h *OAuthHandler) HandleClientMetadata(w http.ResponseWriter, r *http.Request) {
	metadata := h.client.ClientMetadata()

	// Validate metadata before returning (skip in dev mode - localhost doesn't need https validation)
	if !h.client.Config.DevMode {
		if err := metadata.Validate(h.client.ClientApp.Config.ClientID); err != nil {
			slog.Error("client metadata validation failed", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metadata); err != nil {
		slog.Error("failed to encode client metadata", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
}

// HandleClientJWKS serves the OAuth client public keys (JWKS)
// GET /oauth-client-keys.json
// This endpoint is only relevant for confidential clients; public clients don't have keys.
func (h *OAuthHandler) HandleClientJWKS(w http.ResponseWriter, r *http.Request) {
	jwks := h.client.ClientApp.Config.PublicJWKS()

	// Encode to buffer first to avoid setting headers on a response that may fail mid-write.
	// If encoding fails after headers are set, clients receive Content-Type: application/json
	// but an HTML error body, causing parsing failures.
	data, err := json.Marshal(jwks)
	if err != nil {
		slog.Error("failed to encode client JWKS", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if _, err := w.Write(data); err != nil {
		slog.Error("failed to write client JWKS response", "error", err)
		// Headers already sent, can't change response at this point
	}
}

// HandleLogin starts the OAuth flow (web version)
// GET /oauth/login?handle=user.bsky.social
func (h *OAuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get handle or DID from query params
	identifier := r.URL.Query().Get("handle")
	if identifier == "" {
		identifier = r.URL.Query().Get("did")
	}
	if identifier == "" {
		http.Error(w, "missing handle or did parameter", http.StatusBadRequest)
		return
	}

	var redirectURL string
	var err error

	// DEV MODE: Use custom OAuth flow that bypasses HTTPS validation
	// This is needed because:
	// 1. Local handles can't be resolved via DNS/HTTP well-known
	// 2. Indigo's OAuth library requires HTTPS for auth servers
	if h.devAuthResolver != nil {
		slog.Info("dev mode: using localhost OAuth flow", "identifier", identifier)
		redirectURL, err = h.devAuthResolver.StartDevAuthFlow(ctx, h.client, identifier, h.client.ClientApp.Dir)
		if err != nil {
			slog.Error("dev mode: failed to start OAuth flow", "error", err, "identifier", identifier)
			http.Error(w, fmt.Sprintf("failed to start OAuth flow: %v", err), http.StatusBadRequest)
			return
		}
	} else {
		// Production mode: use standard indigo OAuth flow
		redirectURL, err = h.client.ClientApp.StartAuthFlow(ctx, identifier)
		if err != nil {
			slog.Error("failed to start OAuth flow", "error", err, "identifier", identifier)
			http.Error(w, fmt.Sprintf("failed to start OAuth flow: %v", err), http.StatusBadRequest)
			return
		}
	}

	// Log OAuth flow initiation (sanitized - no full URL to avoid leaking state)
	slog.Info("redirecting to PDS for OAuth", "identifier", identifier)

	// Store post-login redirect URL in cookie if provided
	// This allows redirecting to a specific page after OAuth completes (e.g., /delete-account)
	if postLoginRedirect := r.URL.Query().Get("redirect"); postLoginRedirect != "" {
		// Only allow relative paths to prevent open redirect vulnerabilities
		if len(postLoginRedirect) > 0 && postLoginRedirect[0] == '/' {
			http.SetCookie(w, &http.Cookie{
				Name:     "oauth_redirect",
				Value:    postLoginRedirect,
				Path:     "/",
				HttpOnly: true,
				Secure:   !h.client.Config.DevMode,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   300, // 5 minutes - enough for OAuth flow
			})
		}
	}

	// Redirect to PDS
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// HandleMobileLogin starts the OAuth flow for mobile apps
// GET /oauth/mobile/login?handle=user.bsky.social&redirect_uri=coves-app://callback
func (h *OAuthHandler) HandleMobileLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// DEV MODE: Redirect localhost to 127.0.0.1 for cookie consistency
	// The OAuth callback URL uses 127.0.0.1 (per RFC 8252), so cookies must be set
	// on 127.0.0.1. If user calls localhost, redirect to 127.0.0.1 first.
	if h.client.Config.DevMode && strings.Contains(r.Host, "localhost") {
		// Use the configured PublicURL host for consistency
		redirectURL := h.client.Config.PublicURL + r.URL.RequestURI()
		slog.Info("dev mode: redirecting localhost to PublicURL host for cookie consistency",
			"from", r.Host, "to", h.client.Config.PublicURL)
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	// Get handle or DID from query params
	identifier := r.URL.Query().Get("handle")
	if identifier == "" {
		identifier = r.URL.Query().Get("did")
	}
	if identifier == "" {
		http.Error(w, "missing handle or did parameter", http.StatusBadRequest)
		return
	}

	// Get mobile redirect URI (deep link)
	mobileRedirectURI := r.URL.Query().Get("redirect_uri")
	if mobileRedirectURI == "" {
		http.Error(w, "missing redirect_uri parameter", http.StatusBadRequest)
		return
	}

	// SECURITY FIX 1: Validate redirect_uri against allowlist
	// Uses configurable allowlist that includes both mobile deep links and external client URIs
	if !h.isAllowedRedirectURI(mobileRedirectURI) {
		slog.Warn("rejected unauthorized redirect URI", "scheme", extractScheme(mobileRedirectURI))
		http.Error(w, "invalid redirect_uri: not in allowlist", http.StatusBadRequest)
		return
	}

	// SECURITY: Verify store is properly configured for mobile OAuth
	// A plain PostgresOAuthStore implements MobileOAuthStore (has Save/GetMobileOAuthData),
	// but without the MobileAwareStoreWrapper, SaveMobileOAuthData is never called during
	// StartAuthFlow, so server-side CSRF data is never stored. This causes mobile callbacks
	// to silently fall back to web flow. Fail fast here instead of silent breakage.
	if _, ok := h.store.(MobileAwareClientStore); !ok {
		slog.Error("mobile OAuth not supported: store is not wrapped with MobileAwareStoreWrapper",
			"store_type", fmt.Sprintf("%T", h.store))
		http.Error(w, "mobile OAuth not configured on this server", http.StatusInternalServerError)
		return
	}

	// SECURITY FIX 2: Generate CSRF token
	csrfToken, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// SECURITY FIX 4: Store CSRF server-side tied to OAuth state
	// Add mobile data to context so the store wrapper can capture it when
	// SaveAuthRequestInfo is called by indigo's StartAuthFlow.
	// This is necessary because PAR redirects don't include the state in the URL,
	// so we can't extract it after StartAuthFlow returns.
	mobileCtx := ContextWithMobileFlowData(ctx, MobileOAuthData{
		CSRFToken:   csrfToken,
		RedirectURI: mobileRedirectURI,
	})

	var redirectURL string

	// DEV MODE: Use custom OAuth flow that bypasses HTTPS validation
	// This is needed because:
	// 1. Local handles can't be resolved via DNS/HTTP well-known
	// 2. Indigo's OAuth library requires HTTPS for auth servers
	if h.devAuthResolver != nil {
		slog.Info("dev mode: using localhost OAuth flow for mobile", "identifier", identifier)
		redirectURL, err = h.devAuthResolver.StartDevAuthFlow(mobileCtx, h.client, identifier, h.client.ClientApp.Dir)
		if err != nil {
			slog.Error("dev mode: failed to start OAuth flow", "error", err, "identifier", identifier)
			http.Error(w, fmt.Sprintf("failed to start OAuth flow: %v", err), http.StatusBadRequest)
			return
		}
	} else {
		// Production mode: use standard indigo OAuth flow
		redirectURL, err = h.client.ClientApp.StartAuthFlow(mobileCtx, identifier)
		if err != nil {
			slog.Error("failed to start OAuth flow", "error", err, "identifier", identifier)
			http.Error(w, fmt.Sprintf("failed to start OAuth flow: %v", err), http.StatusBadRequest)
			return
		}
	}

	// Log mobile OAuth flow initiation (sanitized - no full URLs or sensitive params)
	slog.Info("redirecting to PDS for mobile OAuth", "identifier", identifier)

	// SECURITY FIX 2: Store CSRF token in cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_csrf",
		Value:    csrfToken,
		Path:     "/oauth",
		MaxAge:   600, // 10 minutes
		HttpOnly: true,
		Secure:   !h.client.Config.DevMode,
		SameSite: http.SameSiteLaxMode,
	})

	// SECURITY FIX 3: Generate binding token to tie CSRF token + mobile redirect to this OAuth flow
	// This prevents session fixation attacks where an attacker plants a mobile_redirect_uri
	// cookie, then the user does a web login, and credentials get sent to attacker's deep link.
	// The binding includes the CSRF token so we validate its VALUE (not just presence) on callback.
	mobileBinding := generateMobileRedirectBinding(csrfToken, mobileRedirectURI)

	// Set cookie with mobile redirect URI for use in callback
	http.SetCookie(w, &http.Cookie{
		Name:     "mobile_redirect_uri",
		Value:    url.QueryEscape(mobileRedirectURI),
		Path:     "/oauth",
		HttpOnly: true,
		Secure:   !h.client.Config.DevMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600, // 10 minutes
	})

	// Set binding cookie to validate mobile redirect in callback
	http.SetCookie(w, &http.Cookie{
		Name:     "mobile_redirect_binding",
		Value:    mobileBinding,
		Path:     "/oauth",
		HttpOnly: true,
		Secure:   !h.client.Config.DevMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600, // 10 minutes
	})

	// Redirect to PDS
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// HandleCallback handles the OAuth callback from the PDS
// GET /oauth/callback?code=...&state=...&iss=...
func (h *OAuthHandler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// IMPORTANT: Look up mobile CSRF data BEFORE ProcessCallback
	// ProcessCallback deletes the oauth_requests row, so we must retrieve mobile data first.
	// Both the success path and the error paths below consume this result (all of
	// them tolerate a nil result). The lookup is unconditional whenever we have a
	// state and a mobile store - NOT gated on the mobile cookies - so mobile flows
	// are recognized from server-side data even when the browser dropped cookies,
	// and so we can distinguish real callbacks from forged ones.
	var serverMobileData *MobileOAuthData
	var mobileDataLookupErr error
	stateRowExists := false
	oauthState := r.URL.Query().Get("state")

	// Cookie presence is tracked for observability only - the server-side data,
	// keyed by the OAuth state, is what qualifies a mobile flow.
	mobileRedirectCookie, _ := r.Cookie("mobile_redirect_uri")
	hadMobileCookie := mobileRedirectCookie != nil && mobileRedirectCookie.Value != ""

	if h.mobileStore != nil && oauthState != "" {
		serverMobileData, mobileDataLookupErr = h.mobileStore.GetMobileOAuthData(ctx, oauthState)
		switch {
		case mobileDataLookupErr == nil:
			// A pending oauth_requests row exists for this state.
			// serverMobileData is nil for web flows, non-nil for mobile flows.
			stateRowExists = true
		case errors.Is(mobileDataLookupErr, ErrAuthRequestNotFound):
			// No pending auth request for this state - expected for forged or
			// expired callbacks. Not a lookup failure.
			mobileDataLookupErr = nil
		}
	}

	// Authorization server errors (user cancelled/denied, expired request, ...)
	// arrive as ?error=...&error_description=... instead of a code. Send the
	// user back to the client instead of stranding them on a raw error page.
	//
	// SECURITY: this endpoint is reachable via cross-site GET riding the
	// victim's SameSite=Lax cookies, with attacker-chosen error params. So the
	// error is only honored when the state param correlates with a pending
	// oauth_requests row (mirroring indigo's ProcessCallback, which validates
	// state before looking at error), and the outgoing code is clamped to a
	// known OAuth error code.
	if asError := r.URL.Query().Get("error"); asError != "" {
		rawDesc := r.URL.Query().Get("error_description")
		errCode, errDesc := clampOAuthError(asError, rawDesc)
		slog.Info("OAuth callback returned authorization server error",
			"error", asError, "clamped_error", errCode, "description", rawDesc)

		// (a) Forged or expired: no state, or no pending auth request for it.
		// Do NOT clear mobile cookies (a forged request must not kill an
		// in-flight login) and do NOT redirect into the app.
		if oauthState == "" || (h.mobileStore != nil && mobileDataLookupErr == nil && !stateRowExists) {
			slog.Warn("OAuth callback error without matching pending auth request - possible forged callback",
				"error", asError, "state_present", oauthState != "", "had_mobile_cookie", hadMobileCookie)
			h.webErrorRedirect(w, r, "invalid_request")
			return
		}

		// Lookup failed: we cannot tell a real callback from a forged one, so
		// keep cookies intact and degrade to a generic web error.
		if mobileDataLookupErr != nil {
			slog.Warn("failed to look up OAuth request state while handling authorization server error",
				"error", mobileDataLookupErr, "had_mobile_cookie", hadMobileCookie)
			h.webErrorRedirect(w, r, "server_error")
			return
		}

		// (b) Pending row with server-stored mobile redirect data: deliver the
		// error into the app (allowlist-checked, clears mobile cookies).
		if h.redirectMobileError(w, r, serverMobileData, errCode, errDesc) {
			return
		}

		// (c) Pending row without mobile data (web flow), or no mobile store to
		// correlate against: end the flow on the web side, not a raw text page.
		slog.Info("returning OAuth error to web client",
			"error", errCode, "had_mobile_cookie", hadMobileCookie)
		clearMobileCookies(w)
		h.webErrorRedirect(w, r, errCode)
		return
	}

	// Process the callback (this deletes the oauth_requests row)
	sessData, err := h.client.ClientApp.ProcessCallback(ctx, r.URL.Query())
	if err != nil {
		slog.Error("failed to process OAuth callback", "error", err)
		if mobileDataLookupErr != nil {
			slog.Warn("mobile OAuth data lookup had failed before callback processing",
				"error", mobileDataLookupErr, "had_mobile_cookie", hadMobileCookie)
		}
		// Give mobile flows closure in the app rather than a dead-end page.
		// Details stay in the server log; the client only gets a generic code.
		if h.redirectMobileError(w, r, serverMobileData, "server_error", "OAuth callback failed") {
			return
		}
		slog.Info("returning OAuth error to web client",
			"error", "server_error", "had_mobile_cookie", hadMobileCookie)
		clearMobileCookies(w)
		h.webErrorRedirect(w, r, "server_error")
		return
	}

	// Ensure sessData is not nil before using it
	if sessData == nil {
		slog.Error("OAuth callback returned nil session data")
		http.Error(w, "OAuth callback failed: no session data", http.StatusInternalServerError)
		return
	}

	// Validate that critical scopes were granted by the authorization server.
	// Log warnings for missing scopes but don't fail auth - users can still use limited functionality.
	criticalScopes := []string{"atproto", "blob:*/*"}
	for _, required := range criticalScopes {
		if !scopesContain(sessData.Scopes, required) {
			slog.Warn("OAuth callback: critical scope not granted",
				"did", sessData.AccountDID,
				"missing_scope", required,
				"granted_scopes", sessData.Scopes)
		}
	}

	// Bidirectional handle verification: ensure the DID actually controls a valid handle
	// This prevents impersonation via compromised PDS that issues tokens with invalid handle mappings
	// Per AT Protocol spec: "Bidirectional verification required; confirm DID document claims handle"
	// verifiedHandle stores the successfully verified handle for use in mobile callback
	// verifiedIdent stores the identity for reuse (PDS URL extraction, etc.)
	verifiedHandle := ""
	var verifiedIdent *identity.Identity
	if h.client.ClientApp.Dir != nil {
		ident, err := h.client.ClientApp.Dir.LookupDID(ctx, sessData.AccountDID)
		if err != nil {
			// Directory lookup failed - this is a hard error for security
			slog.Error("OAuth callback: DID lookup failed during handle verification",
				"did", sessData.AccountDID, "error", err)
			http.Error(w, "Handle verification failed", http.StatusUnauthorized)
			return
		}

		// Check if the handle is the special "handle.invalid" value
		// This indicates that bidirectional verification failed (DID->handle->DID roundtrip failed)
		if ident.Handle.String() == "handle.invalid" {
			// DEV MODE: For local handles, verify via PDS instead of DNS/HTTP
			// Local handles like "user.local.coves.dev" can't be resolved via DNS
			if h.devResolver != nil {
				// Get the handle from DID document (alsoKnownAs)
				declaredHandle := ""
				if len(ident.AlsoKnownAs) > 0 {
					// Extract handle from at:// URI
					for _, aka := range ident.AlsoKnownAs {
						if len(aka) > 5 && aka[:5] == "at://" {
							declaredHandle = aka[5:]
							break
						}
					}
				}

				if declaredHandle != "" {
					// Verify handle via PDS
					resolvedDID, err := h.devResolver.ResolveHandle(ctx, declaredHandle)
					if err == nil && resolvedDID == sessData.AccountDID.String() {
						slog.Info("OAuth callback successful (dev mode: handle verified via PDS)",
							"did", sessData.AccountDID, "handle", declaredHandle)
						verifiedHandle = declaredHandle
						verifiedIdent = ident // Reuse the identity for PDS URL extraction
						goto handleVerificationPassed
					}
					slog.Warn("dev mode: PDS handle verification failed",
						"did", sessData.AccountDID, "handle", declaredHandle,
						"resolved_did", resolvedDID, "error", err)
				}
			}

			slog.Warn("OAuth callback: bidirectional handle verification failed",
				"did", sessData.AccountDID,
				"handle", "handle.invalid",
				"reason", "DID document claims a handle that doesn't resolve back to this DID")
			http.Error(w, "Handle verification failed: DID/handle mismatch", http.StatusUnauthorized)
			return
		}

		// Success: handle is valid and bidirectionally verified
		slog.Info("OAuth callback successful", "did", sessData.AccountDID, "handle", ident.Handle)
		verifiedHandle = ident.Handle.String()
		verifiedIdent = ident
	} else {
		// No directory client available - log warning but proceed
		// This should only happen in testing scenarios
		slog.Warn("OAuth callback: directory client not available, skipping handle verification",
			"did", sessData.AccountDID)
		slog.Info("OAuth callback successful (no handle verification)", "did", sessData.AccountDID)
	}
handleVerificationPassed:

	// Index user in local database after successful OAuth login
	// This ensures users are available for profile lookups immediately after authentication
	if h.userIndexer != nil && verifiedHandle != "" && verifiedIdent != nil {
		pdsURL := verifiedIdent.PDSEndpoint()
		if pdsURL == "" {
			// No PDS URL available - skip indexing, user will be indexed on next login
			// We don't fallback to bsky.social since not all users are on Bluesky
			slog.Warn("skipping user indexing: no PDS URL in identity",
				"did", sessData.AccountDID, "handle", verifiedHandle)
		} else if indexErr := h.userIndexer.IndexUser(ctx, sessData.AccountDID.String(), verifiedHandle, pdsURL); indexErr != nil {
			// Log but don't fail - user can still proceed with their session
			// They'll be indexed on next login or via Jetstream identity event
			slog.Warn("failed to index user after OAuth login",
				"did", sessData.AccountDID, "handle", verifiedHandle, "error", indexErr)
		} else {
			slog.Info("indexed user after OAuth login", "did", sessData.AccountDID, "handle", verifiedHandle)
		}
	}

	// Check if this is a mobile callback (check for mobile_redirect_uri cookie)
	mobileRedirect, err := r.Cookie("mobile_redirect_uri")
	if err == nil && mobileRedirect.Value != "" {
		// SECURITY FIX 2: Validate CSRF token for mobile callback
		csrfCookie, err := r.Cookie("oauth_csrf")
		if err != nil {
			slog.Warn("mobile callback missing CSRF token")
			clearMobileCookies(w)
			http.Error(w, "invalid request: missing CSRF token", http.StatusForbidden)
			return
		}

		// SECURITY FIX 3: Validate mobile redirect binding
		// This prevents session fixation attacks where an attacker plants a mobile_redirect_uri
		// cookie, then the user does a web login, and credentials get sent to attacker's deep link
		bindingCookie, err := r.Cookie("mobile_redirect_binding")
		if err != nil {
			slog.Warn("mobile callback missing redirect binding - possible attack attempt")
			clearMobileCookies(w)
			http.Error(w, "invalid request: missing redirect binding", http.StatusForbidden)
			return
		}

		// Decode the mobile redirect URI to validate binding
		mobileRedirectURI, err := url.QueryUnescape(mobileRedirect.Value)
		if err != nil {
			slog.Error("failed to decode mobile redirect URI", "error", err)
			clearMobileCookies(w)
			http.Error(w, "invalid mobile redirect URI", http.StatusBadRequest)
			return
		}

		// Validate that the binding matches both the CSRF token AND redirect URI
		// This is the actual CSRF validation - we verify the token VALUE by checking
		// that hash(csrfToken + redirectURI) == binding. This prevents:
		// 1. CSRF attacks: attacker can't forge binding without knowing CSRF token
		// 2. Session fixation: cookies must all originate from the same /oauth/mobile/login request
		if !validateMobileRedirectBinding(csrfCookie.Value, mobileRedirectURI, bindingCookie.Value) {
			slog.Warn("mobile redirect binding/CSRF validation failed - possible attack attempt",
				"expected_scheme", extractScheme(mobileRedirectURI))
			clearMobileCookies(w)
			// Fail closed: treat as web flow instead of mobile
			h.handleWebCallback(w, r, sessData)
			return
		}

		// SECURITY FIX 4: Validate CSRF cookie against server-side state
		// This compares the cookie CSRF against a value tied to the OAuth state parameter
		// (which comes back through the OAuth response), satisfying the requirement to
		// validate against server-side state rather than only against other cookies.
		//
		// CRITICAL: If mobile cookies are present but server-side mobile data is MISSING,
		// this indicates a potential attack where:
		// 1. Attacker did a WEB OAuth flow (no mobile data stored)
		// 2. Attacker planted mobile cookies via cross-site /oauth/mobile/login
		// 3. Attacker sends victim to callback with attacker's web-flow state/code
		// We MUST fail closed and use web flow when server-side mobile data is missing.
		//
		// NOTE: serverMobileData was fetched BEFORE ProcessCallback (which deletes the row)
		// at the top of this function. We use the pre-fetched result here.
		if h.mobileStore != nil && oauthState != "" {
			if mobileDataLookupErr != nil {
				// Database error - fail closed, use web flow
				slog.Warn("failed to retrieve server-side mobile OAuth data - using web flow",
					"error", mobileDataLookupErr, "state", oauthState)
				clearMobileCookies(w)
				h.handleWebCallback(w, r, sessData)
				return
			}
			if serverMobileData == nil {
				// No server-side mobile data for this state - this OAuth flow was NOT started
				// via /oauth/mobile/login. Mobile cookies are likely attacker-planted.
				// Fail closed: clear cookies and use web flow.
				slog.Warn("mobile cookies present but no server-side mobile data for OAuth state - "+
					"possible cross-flow attack, using web flow", "state", oauthState)
				clearMobileCookies(w)
				h.handleWebCallback(w, r, sessData)
				return
			}
			// Server-side mobile data exists - validate it matches cookies
			if !constantTimeCompare(csrfCookie.Value, serverMobileData.CSRFToken) {
				slog.Warn("mobile callback CSRF mismatch: cookie differs from server-side state",
					"state", oauthState)
				clearMobileCookies(w)
				h.handleWebCallback(w, r, sessData)
				return
			}
			if serverMobileData.RedirectURI != mobileRedirectURI {
				slog.Warn("mobile callback redirect URI mismatch: cookie differs from server-side state",
					"cookie_uri", extractScheme(mobileRedirectURI),
					"server_uri", extractScheme(serverMobileData.RedirectURI))
				clearMobileCookies(w)
				h.handleWebCallback(w, r, sessData)
				return
			}
			slog.Debug("server-side CSRF validation passed", "state", oauthState)
		} else if h.mobileStore != nil {
			// mobileStore exists but no state in query - shouldn't happen with valid OAuth
			slog.Warn("mobile cookies present but no OAuth state in callback - using web flow")
			clearMobileCookies(w)
			h.handleWebCallback(w, r, sessData)
			return
		}
		// Note: if h.mobileStore is nil (e.g., in tests), we fall back to cookie-only validation

		// All security checks passed - proceed with mobile flow
		// Mobile flow: seal the session and redirect to deep link
		h.handleMobileCallback(w, r, sessData, mobileRedirect.Value, csrfCookie.Value, verifiedHandle)
		return
	}

	// Web flow: set session cookie
	h.handleWebCallback(w, r, sessData)
}

// handleWebCallback handles the web OAuth callback flow
func (h *OAuthHandler) handleWebCallback(w http.ResponseWriter, r *http.Request, sessData *oauth.ClientSessionData) {
	// Use sealed tokens for web flow (same as mobile) per atProto OAuth spec:
	// "Access and refresh tokens should never be copied or shared across end devices.
	// They should not be stored in session cookies."

	// Seal the session data using AES-GCM encryption
	sealedToken, err := h.client.SealSession(
		sessData.AccountDID.String(),
		sessData.SessionID,
		h.client.Config.SealedTokenTTL,
	)
	if err != nil {
		slog.Error("failed to seal session for web", "error", err)
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "coves_session",
		Value:    sealedToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   !h.client.Config.DevMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.client.Config.SealedTokenTTL.Seconds()),
	})

	// Clear all mobile cookies if they exist (defense in depth)
	clearMobileCookies(w)

	// Check for post-login redirect cookie
	redirectURL := "/"
	if redirectCookie, err := r.Cookie("oauth_redirect"); err == nil && redirectCookie.Value != "" {
		// Validate it's a relative path (security check)
		if len(redirectCookie.Value) > 0 && redirectCookie.Value[0] == '/' {
			redirectURL = redirectCookie.Value
		}
		// Clear the redirect cookie
		http.SetCookie(w, &http.Cookie{
			Name:   "oauth_redirect",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
	}

	// Add base URL for production
	if !h.client.Config.DevMode && redirectURL == "/" {
		redirectURL = h.client.Config.PublicURL + "/"
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// oauthErrorCodes are the RFC 6749 / OIDC authorization error codes we allow
// to flow through to clients. Anything else is collapsed to server_error so
// attacker-chosen strings never reach the app or the web frontend.
var oauthErrorCodes = map[string]bool{
	"access_denied":             true,
	"invalid_request":           true,
	"unauthorized_client":       true,
	"server_error":              true,
	"temporarily_unavailable":   true,
	"invalid_scope":             true,
	"unsupported_response_type": true,
	"login_required":            true,
	"interaction_required":      true,
}

// clampOAuthError restricts an authorization server error to a known OAuth
// error code. Unknown codes become server_error, and the (equally untrusted)
// description is dropped with them.
func clampOAuthError(code, description string) (string, string) {
	if oauthErrorCodes[code] {
		return code, description
	}
	return "server_error", ""
}

// webErrorRedirect ends a failed OAuth flow on the web side: it clamps the
// error code to a known OAuth code and redirects to the web app root with
// ?oauth_error=<code>, mirroring handleWebCallback's PublicURL/DevMode
// handling (absolute PublicURL in production, relative path in dev).
// It does NOT touch cookies; callers decide whether to clear mobile cookies.
func (h *OAuthHandler) webErrorRedirect(w http.ResponseWriter, r *http.Request, errCode string) {
	code, _ := clampOAuthError(errCode, "")
	target := "/?oauth_error=" + url.QueryEscape(code)
	if !h.client.Config.DevMode {
		target = h.client.Config.PublicURL + target
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// redirectMobileError sends an OAuth error outcome (user denied consent,
// cancelled sign-in, failed code exchange, ...) back to the mobile app's
// redirect URI as ?error=...&error_description=..., so the user lands in the
// app instead of on a raw error page at the AppView callback URL.
//
// The flow is qualified by SERVER-SIDE data: serverMobileData is the mobile
// redirect information stored (keyed by the OAuth state) when
// /oauth/mobile/login started the flow, and its redirect URI is the only
// redirect target. Cookies are a secondary binding check, not a prerequisite:
// when both the CSRF and binding cookies are present the binding must
// validate; a partial pair fails closed; when both are absent the
// server-stored data alone is sufficient. Whatever URI is used must pass the
// redirect URI allowlist.
//
// Returns false when this is not a qualified mobile flow - the caller must
// then handle the error itself (cookies are left untouched). On a successful
// redirect the mobile cookies are cleared. No tokens are involved on this
// path - the redirect carries only an OAuth error code — either the one the
// authorization server returned or a generic server_error — never internal
// details or tokens.
func (h *OAuthHandler) redirectMobileError(w http.ResponseWriter, r *http.Request, serverMobileData *MobileOAuthData, errCode, errDesc string) bool {
	// Clamp defensively so no caller can leak an unknown code into the app.
	errCode, errDesc = clampOAuthError(errCode, errDesc)

	// Server-stored redirect data is the primary (and only) qualification and
	// target; it cannot have been planted via cross-site cookie writes.
	if serverMobileData == nil || serverMobileData.RedirectURI == "" {
		return false
	}
	redirectURI := serverMobileData.RedirectURI

	// Observability: a redirect cookie that fails decoding is suspicious even
	// though the server-stored URI is what we redirect to.
	if cookie, cookieErr := r.Cookie("mobile_redirect_uri"); cookieErr == nil && cookie.Value != "" {
		if _, unescapeErr := url.QueryUnescape(cookie.Value); unescapeErr != nil {
			slog.Warn("mobile error redirect: mobile_redirect_uri cookie failed to decode",
				"error", unescapeErr)
		}
	}

	// Cookie-based CSRF binding is a secondary check when present.
	csrfCookie, csrfErr := r.Cookie("oauth_csrf")
	bindingCookie, bindingErr := r.Cookie("mobile_redirect_binding")
	hasCSRF := csrfErr == nil && csrfCookie.Value != ""
	hasBinding := bindingErr == nil && bindingCookie.Value != ""
	switch {
	case hasCSRF != hasBinding:
		// Exactly one of the pair present: fail closed - a legitimate
		// /oauth/mobile/login sets both together.
		slog.Warn("mobile error redirect: partial binding cookie pair, not redirecting",
			"has_csrf", hasCSRF, "has_binding", hasBinding)
		return false
	case hasCSRF && hasBinding:
		if !validateMobileRedirectBinding(csrfCookie.Value, redirectURI, bindingCookie.Value) {
			slog.Warn("mobile error redirect: binding validation failed, not redirecting",
				"scheme", extractScheme(redirectURI))
			return false
		}
	}
	// Both absent: proceed on server-stored data alone (e.g. the browser
	// dropped the cookies mid-flow).

	if !h.isAllowedRedirectURI(redirectURI) {
		slog.Warn("mobile error redirect: redirect URI not in allowlist",
			"scheme", extractScheme(redirectURI))
		return false
	}

	clearMobileCookies(w)

	errorURL := fmt.Sprintf("%s?error=%s", redirectURI, url.QueryEscape(errCode))
	if errDesc != "" {
		errorURL += "&error_description=" + url.QueryEscape(errDesc)
	}

	slog.Info("redirecting OAuth error to mobile app",
		"error", errCode, "scheme", extractScheme(redirectURI))
	http.Redirect(w, r, errorURL, http.StatusFound)
	return true
}

// handleMobileCallback handles the mobile OAuth callback flow.
// This handles both mobile deep links (custom schemes like social.coves://) and
// Universal Links (https:// URLs verified via .well-known).
func (h *OAuthHandler) handleMobileCallback(w http.ResponseWriter, r *http.Request, sessData *oauth.ClientSessionData, mobileRedirectURIEncoded, csrfToken, verifiedHandle string) {
	// Decode the redirect URI
	redirectURI, err := url.QueryUnescape(mobileRedirectURIEncoded)
	if err != nil {
		slog.Error("failed to decode redirect URI", "error", err)
		http.Error(w, "invalid redirect URI", http.StatusBadRequest)
		return
	}

	// SECURITY FIX 1: Re-validate redirect URI against allowlist
	// Uses configurable allowlist that includes both mobile deep links and external client URIs
	if !h.isAllowedRedirectURI(redirectURI) {
		slog.Error("callback attempted with unauthorized redirect URI", "scheme", extractScheme(redirectURI))
		http.Error(w, "invalid redirect URI", http.StatusBadRequest)
		return
	}

	// Seal the session data
	sealedToken, err := h.client.SealSession(
		sessData.AccountDID.String(),
		sessData.SessionID,
		h.client.Config.SealedTokenTTL,
	)
	if err != nil {
		slog.Error("failed to seal session data", "error", err)
		http.Error(w, "failed to create session token", http.StatusInternalServerError)
		return
	}

	// Get account handle for convenience
	// Use the already-verified handle if available (important for dev mode where LookupDID returns "handle.invalid")
	handle := verifiedHandle
	if handle == "" {
		if ident, err := h.client.ClientApp.Dir.LookupDID(r.Context(), sessData.AccountDID); err == nil {
			handle = ident.Handle.String()
		}
	}

	// Clear all mobile/external cookies to prevent reuse (defense in depth)
	clearMobileCookies(w)

	// Build redirect URL with sealed token
	callbackURL := fmt.Sprintf("%s?token=%s&did=%s&session_id=%s",
		redirectURI,
		url.QueryEscape(sealedToken),
		url.QueryEscape(sessData.AccountDID.String()),
		url.QueryEscape(sessData.SessionID),
	)
	if handle != "" {
		callbackURL += "&handle=" + url.QueryEscape(handle)
	}

	// Determine redirect type based on scheme
	parsedURI, parseErr := url.Parse(redirectURI)
	isWebClient := parseErr == nil && (parsedURI.Scheme == "http" || parsedURI.Scheme == "https")

	if isWebClient {
		// HTTPS Universal Links or web clients get a direct HTTP redirect.
		// The OS intercepts Universal Links and opens the app; no intermediate page needed.
		slog.Info("redirecting via HTTP", "did", sessData.AccountDID, "handle", handle, "host", parsedURI.Host)
		http.Redirect(w, r, callbackURL, http.StatusFound)
		return
	}

	// Mobile app with custom scheme (e.g., social.coves://)
	// Serve intermediate page that redirects to the app
	// This prevents the browser from showing a stale PDS page after the custom scheme redirect
	slog.Info("redirecting to mobile app", "did", sessData.AccountDID, "handle", handle)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")

	data := struct {
		DeepLink string
		Handle   string
	}{
		DeepLink: callbackURL,
		Handle:   handle,
	}

	if err := mobileCallbackTemplate.Execute(w, data); err != nil {
		slog.Error("failed to render mobile callback template", "error", err)
		// Fallback to direct redirect if template fails
		http.Redirect(w, r, callbackURL, http.StatusFound)
	}
}

// HandleLogout revokes the session and clears cookies
// POST /oauth/logout
func (h *OAuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get session from cookie (now sealed)
	cookie, err := r.Cookie("coves_session")
	if err != nil {
		// No session, just return success
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "logged_out"})
		return
	}

	// Unseal the session token
	sealed, err := h.client.UnsealSession(cookie.Value)
	if err != nil {
		// Invalid session, clear cookie and return
		h.clearSessionCookie(w)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "logged_out"})
		return
	}

	// Parse DID
	did, err := syntax.ParseDID(sealed.DID)
	if err != nil {
		// Invalid DID, clear cookie and return
		h.clearSessionCookie(w)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "logged_out"})
		return
	}

	// Revoke session on auth server
	if err := h.client.ClientApp.Logout(ctx, did, sealed.SessionID); err != nil {
		slog.Error("failed to revoke session on auth server", "error", err, "did", did)
		// Continue anyway to clear local session
	}

	// Clear session cookie
	h.clearSessionCookie(w)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "logged_out"})
}

// HandleRefresh refreshes the session token (for mobile)
// POST /oauth/refresh
// Body: {"did": "did:plc:...", "session_id": "...", "sealed_token": "..."}
func (h *OAuthHandler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		DID         string `json:"did"`
		SessionID   string `json:"session_id"`
		SealedToken string `json:"sealed_token,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// SECURITY: Require sealed_token for proof of possession
	// Without this, anyone who knows a DID + session_id can steal credentials
	if req.SealedToken == "" {
		slog.Warn("refresh: missing sealed_token", "did", req.DID)
		http.Error(w, "sealed_token required for refresh", http.StatusUnauthorized)
		return
	}

	// SECURITY: Unseal and validate the token
	unsealed, err := h.client.UnsealSession(req.SealedToken)
	if err != nil {
		slog.Warn("refresh: invalid sealed token", "error", err)
		http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
		return
	}

	// SECURITY: Verify the unsealed token matches the claimed DID
	if unsealed.DID != req.DID {
		slog.Warn("refresh: DID mismatch", "token_did", unsealed.DID, "claimed_did", req.DID)
		http.Error(w, "Token DID mismatch", http.StatusUnauthorized)
		return
	}

	// SECURITY: Verify the unsealed token matches the claimed session_id
	if unsealed.SessionID != req.SessionID {
		slog.Warn("refresh: session_id mismatch", "token_session", unsealed.SessionID, "claimed_session", req.SessionID)
		http.Error(w, "Token session mismatch", http.StatusUnauthorized)
		return
	}

	// Parse DID after validation
	did, err := syntax.ParseDID(req.DID)
	if err != nil {
		http.Error(w, "invalid DID", http.StatusBadRequest)
		return
	}

	// Resume session (now authenticated via sealed token)
	sess, err := h.client.ClientApp.ResumeSession(ctx, did, req.SessionID)
	if err != nil {
		slog.Error("failed to resume session", "error", err, "did", did, "session_id", req.SessionID)
		http.Error(w, "session not found", http.StatusUnauthorized)
		return
	}

	// Refresh tokens
	newAccessToken, err := sess.RefreshTokens(ctx)
	if err != nil {
		slog.Error("failed to refresh tokens", "error", err, "did", did)
		http.Error(w, "failed to refresh tokens", http.StatusUnauthorized)
		return
	}

	// Create new sealed token for mobile
	sealedToken, err := h.client.SealSession(
		sess.Data.AccountDID.String(),
		sess.Data.SessionID,
		h.client.Config.SealedTokenTTL,
	)
	if err != nil {
		slog.Error("failed to seal new session data", "error", err)
		http.Error(w, "failed to create session token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": newAccessToken,
		"sealed_token": sealedToken,
	})
}

// clearSessionCookie clears the session cookie
func (h *OAuthHandler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   "coves_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}

// GetSessionFromRequest extracts session data from an HTTP request
func (h *OAuthHandler) GetSessionFromRequest(r *http.Request) (*oauth.ClientSessionData, error) {
	// Try to get session from cookie (web) - now using sealed tokens
	cookie, err := r.Cookie("coves_session")
	if err == nil && cookie.Value != "" {
		// Unseal the token to get DID and session ID
		sealed, err := h.client.UnsealSession(cookie.Value)
		if err == nil {
			did, err := syntax.ParseDID(sealed.DID)
			if err == nil {
				return h.store.GetSession(r.Context(), did, sealed.SessionID)
			}
		}
	}

	// Try to get session from Authorization header (mobile)
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		// Expected format: "Bearer <sealed_token>"
		const prefix = "Bearer "
		if len(authHeader) > len(prefix) && authHeader[:len(prefix)] == prefix {
			sealedToken := authHeader[len(prefix):]
			sealed, err := h.client.UnsealSession(sealedToken)
			if err != nil {
				return nil, fmt.Errorf("invalid sealed token: %w", err)
			}
			did, err := syntax.ParseDID(sealed.DID)
			if err != nil {
				return nil, fmt.Errorf("invalid DID in sealed token: %w", err)
			}
			return h.store.GetSession(r.Context(), did, sealed.SessionID)
		}
	}

	return nil, fmt.Errorf("no session found")
}

// HandleProtectedResourceMetadata returns OAuth protected resource metadata
// per RFC 9449 and atproto OAuth spec. This endpoint allows third-party OAuth
// clients to discover which authorization server to use for this resource.
// Spec: https://datatracker.ietf.org/doc/html/rfc9449#section-5
func (h *OAuthHandler) HandleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	metadata := map[string]interface{}{
		"resource":              h.client.Config.PublicURL,
		"authorization_servers": []string{"https://bsky.social"},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if err := json.NewEncoder(w).Encode(metadata); err != nil {
		slog.Error("failed to encode protected resource metadata", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
}

// HandleMobileDeepLinkFallback handles requests to /app/oauth/callback when
// Universal Links fail to intercept the redirect.
//
// If this handler is reached, it means the mobile app did NOT intercept the
// Universal Link redirect. The OAuth flow succeeded server-side, but the
// credentials couldn't be delivered to the app.
func (h *OAuthHandler) HandleMobileDeepLinkFallback(w http.ResponseWriter, r *http.Request) {
	// Log the failure for debugging
	slog.Warn("Universal Link not intercepted - mobile app did not handle redirect",
		"path", r.URL.Path,
		"has_token", r.URL.Query().Get("token") != "",
		"has_did", r.URL.Query().Get("did") != "",
	)

	http.Error(w, "Universal Link not intercepted: The mobile app should have opened this URL. "+
		"Check that Universal Links (iOS) or App Links (Android) are properly configured.", http.StatusBadRequest)
}

// scopesContain checks if a required scope is present in the granted scopes list.
// It performs an exact match comparison.
func scopesContain(granted []string, required string) bool {
	for _, scope := range granted {
		if scope == required {
			return true
		}
	}
	return false
}
