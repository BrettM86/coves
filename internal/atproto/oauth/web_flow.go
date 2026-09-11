package oauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const webOAuthCookieName = "oauth_web_binding"
const webOAuthLifetime = 10 * time.Minute

// ErrWebBindingNotSaved reports that no unbound pending web request matched
// the state, so the browser binding could not be attached to it.
var ErrWebBindingNotSaved = errors.New("web OAuth binding not saved")

// WebOAuthData binds one pending OAuth request to its initiating browser.
type WebOAuthData struct {
	BrowserNonce string
	ReturnURL    string
	ExpiresAt    time.Time
}

// WebOAuthStore persists browser bindings alongside pending OAuth requests.
// ClaimWebOAuthData must atomically consume only an unexpired, matching binding,
// at most once, while retaining the underlying OAuth request for token exchange.
// Missing, expired, mismatched, or consumed bindings return ErrAuthRequestNotFound.
type WebOAuthStore interface {
	SaveWebOAuthData(context.Context, string, WebOAuthData) error
	ClaimWebOAuthData(context.Context, string, string) (*WebOAuthData, error)
}

type webFlowContextKey struct{}
type claimedWebFlowContextKey struct{}
type webPersistenceContextKey struct{}

// Indigo ignores SaveAuthRequestInfo errors. This request-scoped receipt lets
// the handler require confirmed persistence before releasing a browser cookie.
type webPersistenceReceipt struct{ err error }

func safeWebReturnURL(value string) string {
	if isSafeLocalPath(value) {
		return value
	}
	if len(value) > maxPostLoginRedirectLen {
		slog.Warn("web OAuth return destination too long", "operation", "return_url", "category", "too_long", "length", len(value))
	}
	return "/"
}

// The binding cookie is scoped to Go's /oauth routes, like the mobile cookies,
// so browsers never send it to the frontend server.
func newWebBindingCookie(nonce string, secure bool) *http.Cookie {
	return &http.Cookie{Name: webOAuthCookieName, Value: nonce, Path: "/oauth", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(webOAuthLifetime.Seconds())}
}

// expiredWebBindingCookie clears the binding cookie; its attributes must match
// newWebBindingCookie or the browser keeps the original.
func expiredWebBindingCookie(secure bool) *http.Cookie {
	return &http.Cookie{Name: webOAuthCookieName, Value: "", Path: "/oauth", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1}
}

// SaveWebOAuthData binds a previously saved web request to its browser.
func (s *PostgresOAuthStore) SaveWebOAuthData(ctx context.Context, state string, data WebOAuthData) error {
	result, err := s.db.ExecContext(ctx, `UPDATE oauth_requests
 SET web_browser_nonce = $2, return_url = $3, web_expires_at = $4
 WHERE state = $1 AND web_browser_nonce IS NULL AND mobile_csrf_token IS NULL`,
		state, data.BrowserNonce, data.ReturnURL, data.ExpiresAt)
	if err != nil {
		return fmt.Errorf("save web OAuth binding: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count web OAuth bindings: %w", err)
	}
	if count != 1 {
		return ErrWebBindingNotSaved
	}
	return nil
}

// ClaimWebOAuthData uses a conditional UPDATE so concurrent callbacks have one winner.
// Keep the OAuth request and PKCE data intact for Indigo's ProcessCallback.
func (s *PostgresOAuthStore) ClaimWebOAuthData(ctx context.Context, state, nonce string) (*WebOAuthData, error) {
	if nonce == "" {
		return nil, ErrAuthRequestNotFound
	}
	var data WebOAuthData
	err := s.db.QueryRowContext(ctx, `UPDATE oauth_requests SET web_claimed_at = NOW()
 WHERE state = $1 AND web_browser_nonce = $2 AND web_expires_at > NOW()
 AND web_claimed_at IS NULL AND mobile_csrf_token IS NULL
 RETURNING web_browser_nonce, return_url, web_expires_at`, state, nonce).
		Scan(&data.BrowserNonce, &data.ReturnURL, &data.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAuthRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("claim web OAuth binding: %w", err)
	}
	return &data, nil
}

// SaveWebOAuthData delegates browser binding persistence to the underlying store.
func (w *MobileAwareStoreWrapper) SaveWebOAuthData(ctx context.Context, state string, data WebOAuthData) error {
	store, ok := w.ClientAuthStore.(WebOAuthStore)
	if !ok {
		return errors.New("web OAuth binding store unavailable")
	}
	return store.SaveWebOAuthData(ctx, state, data)
}

// ClaimWebOAuthData delegates the atomic, expiring claim to the underlying store.
func (w *MobileAwareStoreWrapper) ClaimWebOAuthData(ctx context.Context, state, nonce string) (*WebOAuthData, error) {
	store, ok := w.ClientAuthStore.(WebOAuthStore)
	if !ok {
		return nil, errors.New("web OAuth binding store unavailable")
	}
	return store.ClaimWebOAuthData(ctx, state, nonce)
}
