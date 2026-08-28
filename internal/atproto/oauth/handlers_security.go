package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
)

const (
	// postLoginRedirectCookieName is the one-shot cookie carrying the
	// post-login redirect target from HandleLogin to handleWebCallback.
	postLoginRedirectCookieName = "oauth_redirect"

	// maxPostLoginRedirectLen bounds the post-login redirect target. Nothing we
	// link to is anywhere near this long; the cap exists so an attacker cannot
	// use the cookie as unbounded storage or push a giant Location header
	// through the callback.
	maxPostLoginRedirectLen = 2048
)

// baseMobileRedirectURIs contains the EXACT allowed redirect URIs for mobile apps.
// These are always allowed regardless of configuration.
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
var baseMobileRedirectURIs = map[string]bool{
	// Custom scheme per atproto spec (reverse-domain of coves.social)
	"social.coves:/callback":        true,
	"social.coves://callback":       true, // Some platforms add double slash
	"social.coves:/oauth/callback":  true, // Alternative path
	"social.coves://oauth/callback": true,
	// Universal Links - cryptographically bound to app (preferred for security)
	"https://coves.social/app/oauth/callback": true,
}

// BuildAllowedRedirectURIs returns a copy of the allowed mobile redirect URIs.
// The allowlist uses exact URI matching to prevent token theft.
//
// For web frontends like Kelp, use a reverse proxy (Vite proxy in dev, nginx in prod)
// to serve from the same origin as Coves, allowing HTTP-only cookies to work.
func BuildAllowedRedirectURIs() map[string]bool {
	// Return a copy of base mobile URIs
	allowed := make(map[string]bool, len(baseMobileRedirectURIs))
	for uri := range baseMobileRedirectURIs {
		allowed[uri] = true
	}
	return allowed
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

// isSafeLocalPath reports whether s may be used as a post-login redirect
// target: a path on this origin, and nothing else.
//
// A leading '/' alone is not that test. "//host" is a scheme-relative URL, so
// a browser reads everything after the "//" as an authority and navigates
// off-origin. A backslash is the same attack in another spelling: at position
// 2 it opens an authority, and elsewhere browsers simply rewrite it to '/'.
// Rather than reason about where it lands after that rewriting, we refuse it
// everywhere; the second-byte check therefore overlaps the loop below on
// purpose, stating the authority case outright rather than leaving it implied.
//
// Percent-encoded separators are the mirror image and are ACCEPTED: %2f and
// %5c are ordinary path bytes decoded long after the origin has been fixed, so
// refusing them would break legitimate paths without buying any safety.
//
// Control bytes are refused outright: CR and LF are header injection into the
// Location header, and NUL and DEL are parser-confusion bytes. Browsers strip
// tab, CR and LF from ANYWHERE in a URL before parsing it, not merely from the
// front - "/\t//evil" becomes "///evil" - so position tells us nothing and
// every control byte is refused wherever it appears.
//
// url.Parse is a second, structural gate behind the byte checks: whatever the
// bytes say, the result must parse as a URL with no scheme and no authority.
// It also catches the one input the byte checks let through, a malformed
// percent-escape such as "/%zz".
func isSafeLocalPath(s string) bool {
	if s == "" || len(s) > maxPostLoginRedirectLen {
		return false
	}
	if s[0] != '/' {
		return false
	}
	// Reject scheme-relative authorities in either spelling.
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c < 0x20 || c == 0x7F {
			return false
		}
	}

	parsed, err := url.Parse(s)
	if err != nil {
		return false
	}
	return parsed.Scheme == "" && parsed.Host == ""
}

// postLoginRedirectCookie builds the one-shot post-login redirect cookie
// carrying a redirect target, or nil if raw is not a safe local path. Callers
// pass secure=!DevMode, like the other session cookies.
//
// Returning nil rather than an empty cookie is deliberate: an unsafe target
// must leave nothing behind for the callback to read back, so a rejection here
// cannot be laundered into a redirect later.
//
// The value is stored percent-encoded and handleWebCallback decodes it before
// validating, so the string that was checked is the string that comes back.
// Without that, net/http silently DROPS bytes it will not serialize in a
// cookie value ('"', ';', anything non-ASCII) rather than rejecting them,
// which would let a validated path like `/";/attacker.example` go out on the
// wire as `//attacker.example` - validation having run on a string the browser
// never sees.
func postLoginRedirectCookie(raw string, secure bool) *http.Cookie {
	if !isSafeLocalPath(raw) {
		return nil
	}
	return &http.Cookie{
		Name:     postLoginRedirectCookieName,
		Value:    url.QueryEscape(raw),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300, // 5 minutes - enough for the OAuth round trip and no more
	}
}

// expirePostLoginRedirectCookie builds the cookie that clears any existing
// post-login redirect target. HandleLogin emits it whenever it does not store
// a fresh validated target, and handleWebCallback emits it after consuming
// one: the cookie lives for five minutes and survives across login attempts,
// so a target planted by an earlier visit would otherwise steer an unrelated
// flow.
func expirePostLoginRedirectCookie() *http.Cookie {
	return &http.Cookie{
		Name:   postLoginRedirectCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	}
}
