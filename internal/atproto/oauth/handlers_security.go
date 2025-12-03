package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
)

// allowedMobileRedirectURIs contains the EXACT allowed redirect URIs for mobile apps.
//
// Per atproto OAuth spec (https://atproto.com/specs/oauth#mobile-clients):
// - Custom URL schemes are allowed for native mobile apps
// - The scheme must match the client_id hostname in REVERSE-DOMAIN order
// - For client_id https://coves.social/..., the scheme is "social.coves"
//
// We support two redirect URI types:
// 1. Custom scheme: social.coves:/callback (per atproto spec, simpler for mobile)
// 2. Universal Links: https://coves.social/app/oauth/callback (cryptographically bound)
//
// Universal Links provide stronger security guarantees but require:
// - iOS: Verified via /.well-known/apple-app-site-association
// - Android: Verified via /.well-known/assetlinks.json
var allowedMobileRedirectURIs = map[string]bool{
	// Custom scheme per atproto spec (reverse-domain of coves.social)
	"social.coves:/callback":        true,
	"social.coves://callback":       true, // Some platforms add double slash
	"social.coves:/oauth/callback":  true, // Alternative path
	"social.coves://oauth/callback": true,
	// Universal Links - cryptographically bound to app (preferred for security)
	"https://coves.social/app/oauth/callback": true,
}

// isAllowedMobileRedirectURI validates that the redirect URI is in the exact allowlist.
// SECURITY: Exact URI matching prevents token theft by rogue apps.
//
// Per atproto OAuth spec, custom schemes must match the client_id hostname
// in reverse-domain order (social.coves for coves.social), which provides
// some protection as malicious apps would need to know the specific scheme.
//
// Universal Links (https://) provide stronger security as they're cryptographically
// bound to the app via .well-known verification files.
func isAllowedMobileRedirectURI(redirectURI string) bool {
	// Normalize and check exact match
	return allowedMobileRedirectURIs[redirectURI]
}

// extractScheme extracts the scheme from a URI for logging purposes
func extractScheme(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Scheme != "" {
		return u.Scheme
	}
	return "invalid"
}

// generateCSRFToken generates a cryptographically secure CSRF token
func generateCSRFToken() (string, error) {
	csrfToken := make([]byte, 32)
	if _, err := rand.Read(csrfToken); err != nil {
		slog.Error("failed to generate CSRF token", "error", err)
		return "", err
	}
	return base64.URLEncoding.EncodeToString(csrfToken), nil
}

// generateMobileRedirectBinding generates a cryptographically secure binding token
// that ties the CSRF token and mobile redirect URI to this specific OAuth flow.
// SECURITY: This prevents multiple attack vectors:
// 1. Session fixation: attacker plants mobile_redirect_uri cookie, user does web login
// 2. CSRF bypass: attacker manipulates cookies without knowing the CSRF token
// 3. Cookie replay: binding validates both CSRF and redirect URI together
//
// The binding is hash(csrfToken + "|" + mobileRedirectURI) which ensures:
// - CSRF token value is verified (not just presence)
// - Redirect URI is tied to the specific CSRF token that started the flow
// - Cannot forge binding without knowing both values
func generateMobileRedirectBinding(csrfToken, mobileRedirectURI string) string {
	// Combine CSRF token and redirect URI with separator to prevent length extension
	combined := csrfToken + "|" + mobileRedirectURI
	hash := sha256.Sum256([]byte(combined))
	// Use first 16 bytes (128 bits) for the binding - sufficient for this purpose
	return base64.URLEncoding.EncodeToString(hash[:16])
}

// validateMobileRedirectBinding validates that the CSRF token and mobile redirect URI
// together match the binding token, preventing CSRF attacks and cross-flow token theft.
// This implements a proper double-submit cookie pattern where the CSRF token value
// (not just presence) is cryptographically verified.
func validateMobileRedirectBinding(csrfToken, mobileRedirectURI, binding string) bool {
	expectedBinding := generateMobileRedirectBinding(csrfToken, mobileRedirectURI)
	// Constant-time comparison to prevent timing attacks
	return constantTimeCompare(expectedBinding, binding)
}

// constantTimeCompare performs a constant-time string comparison to prevent timing attacks
func constantTimeCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := 0; i < len(a); i++ {
		result |= a[i] ^ b[i]
	}
	return result == 0
}

// clearMobileCookies clears all mobile-related cookies to prevent reuse
func clearMobileCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   "mobile_redirect_uri",
		Value:  "",
		Path:   "/oauth",
		MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:   "mobile_redirect_binding",
		Value:  "",
		Path:   "/oauth",
		MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:   "oauth_csrf",
		Value:  "",
		Path:   "/oauth",
		MaxAge: -1,
	})
}
