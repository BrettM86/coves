package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
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

// OAuthHandler handles OAuth-related HTTP endpoints
type OAuthHandler struct {
	client          *OAuthClient
	store           oauth.ClientAuthStore
	mobileStore     MobileOAuthStore    // For server-side CSRF validation
	devResolver     *DevHandleResolver  // For dev mode: resolve handles via local PDS
	devAuthResolver *DevAuthResolver    // For dev mode: bypass HTTPS validation for localhost OAuth
}

// NewOAuthHandler creates a new OAuth handler
func NewOAuthHandler(client *OAuthClient, store oauth.ClientAuthStore) *OAuthHandler {
	handler := &OAuthHandler{
		client: client,
		store:  store,
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

// HandleClientMetadata serves the OAuth client metadata document
// GET /oauth/client-metadata.json
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
	if !isAllowedMobileRedirectURI(mobileRedirectURI) {
		slog.Warn("rejected unauthorized mobile redirect URI", "scheme", extractScheme(mobileRedirectURI))
		http.Error(w, "invalid redirect_uri: scheme not allowed", http.StatusBadRequest)
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
	// We store it in a local variable for validation after ProcessCallback completes.
	var serverMobileData *MobileOAuthData
	var mobileDataLookupErr error
	oauthState := r.URL.Query().Get("state")

	// Check if this might be a mobile callback (mobile_redirect_uri cookie present)
	// We do a preliminary check here to decide if we need to fetch mobile data
	mobileRedirectCookie, _ := r.Cookie("mobile_redirect_uri")
	isMobileFlow := mobileRedirectCookie != nil && mobileRedirectCookie.Value != ""

	if isMobileFlow && h.mobileStore != nil && oauthState != "" {
		// Fetch mobile data BEFORE ProcessCallback deletes the row
		serverMobileData, mobileDataLookupErr = h.mobileStore.GetMobileOAuthData(ctx, oauthState)
		// We'll handle errors after ProcessCallback - for now just capture the result
	}

	// Process the callback (this deletes the oauth_requests row)
	sessData, err := h.client.ClientApp.ProcessCallback(ctx, r.URL.Query())
	if err != nil {
		slog.Error("failed to process OAuth callback", "error", err)
		http.Error(w, fmt.Sprintf("OAuth callback failed: %v", err), http.StatusBadRequest)
		return
	}

	// Ensure sessData is not nil before using it
	if sessData == nil {
		slog.Error("OAuth callback returned nil session data")
		http.Error(w, "OAuth callback failed: no session data", http.StatusInternalServerError)
		return
	}

	// Bidirectional handle verification: ensure the DID actually controls a valid handle
	// This prevents impersonation via compromised PDS that issues tokens with invalid handle mappings
	// Per AT Protocol spec: "Bidirectional verification required; confirm DID document claims handle"
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
	} else {
		// No directory client available - log warning but proceed
		// This should only happen in testing scenarios
		slog.Warn("OAuth callback: directory client not available, skipping handle verification",
			"did", sessData.AccountDID)
		slog.Info("OAuth callback successful (no handle verification)", "did", sessData.AccountDID)
	}
handleVerificationPassed:

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
		h.handleMobileCallback(w, r, sessData, mobileRedirect.Value, csrfCookie.Value)
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

	// Redirect to home or app
	redirectURL := "/"
	if !h.client.Config.DevMode {
		redirectURL = h.client.Config.PublicURL + "/"
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// handleMobileCallback handles the mobile OAuth callback flow
func (h *OAuthHandler) handleMobileCallback(w http.ResponseWriter, r *http.Request, sessData *oauth.ClientSessionData, mobileRedirectURIEncoded, csrfToken string) {
	// Decode the mobile redirect URI
	mobileRedirectURI, err := url.QueryUnescape(mobileRedirectURIEncoded)
	if err != nil {
		slog.Error("failed to decode mobile redirect URI", "error", err)
		http.Error(w, "invalid mobile redirect URI", http.StatusBadRequest)
		return
	}

	// SECURITY FIX 1: Re-validate redirect URI against allowlist
	if !isAllowedMobileRedirectURI(mobileRedirectURI) {
		slog.Error("mobile callback attempted with unauthorized redirect URI", "scheme", extractScheme(mobileRedirectURI))
		http.Error(w, "invalid redirect URI", http.StatusBadRequest)
		return
	}

	// Seal the session data for mobile
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
	handle := ""
	if ident, err := h.client.ClientApp.Dir.LookupDID(r.Context(), sessData.AccountDID); err == nil {
		handle = ident.Handle.String()
	}

	// Clear all mobile cookies to prevent reuse (defense in depth)
	clearMobileCookies(w)

	// Build deep link with sealed token
	deepLink := fmt.Sprintf("%s?token=%s&did=%s&session_id=%s",
		mobileRedirectURI,
		url.QueryEscape(sealedToken),
		url.QueryEscape(sessData.AccountDID.String()),
		url.QueryEscape(sessData.SessionID),
	)
	if handle != "" {
		deepLink += "&handle=" + url.QueryEscape(handle)
	}

	// Log mobile redirect (sanitized - no token or session ID to avoid leaking credentials)
	slog.Info("redirecting to mobile app", "did", sessData.AccountDID, "handle", handle)

	// Serve intermediate page that redirects to the app
	// This prevents the browser from showing a stale PDS page after the custom scheme redirect
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")

	data := struct {
		DeepLink string
		Handle   string
	}{
		DeepLink: deepLink,
		Handle:   handle,
	}

	if err := mobileCallbackTemplate.Execute(w, data); err != nil {
		slog.Error("failed to render mobile callback template", "error", err)
		// Fallback to direct redirect if template fails
		http.Redirect(w, r, deepLink, http.StatusFound)
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
