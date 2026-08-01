package users

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTurnstileSiteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	// turnstileHTTPTimeout bounds the round-trip to Cloudflare. Long enough to
	// tolerate a slow region, short enough that a hung siteverify can't pin a
	// signup-token request slot for the global request timeout.
	turnstileHTTPTimeout = 10 * time.Second
)

// TurnstileVerifier verifies Cloudflare Turnstile tokens against the siteverify endpoint.
// Implementations must fail-closed: any error, non-2xx response, or success:false result
// must return an error. Never log the raw token.
//
// Two distinct error types are returned:
//   - InvalidCaptchaError → user-side rejection (token bad/expired/replayed); 403
//   - CaptchaUnavailableError → our-side outage (network, 5xx, decode); 503
//
// This split lets ops alert on Cloudflare outages separately from user-rejection
// noise.
type TurnstileVerifier interface {
	Verify(ctx context.Context, token, remoteIP string) error
}

type cloudflareTurnstile struct {
	secret        string
	siteverifyURL string
	httpClient    *http.Client
}

// TurnstileOption customises a verifier built by NewCloudflareTurnstile.
type TurnstileOption func(*cloudflareTurnstile)

// WithSiteverifyURL points the verifier at a siteverify endpoint other than
// Cloudflare's. It exists for the hermetic CI stack, whose Docker network is
// `internal: true` and therefore cannot reach challenges.cloudflare.com: the
// stack runs a stub that answers success, so the signup handshake stays under
// test without the merge gate depending on a third party being up. Production
// must never set it — internal/config only reads the env var that reaches here
// when IS_DEV_ENV is true. An empty url is ignored.
func WithSiteverifyURL(url string) TurnstileOption {
	return func(c *cloudflareTurnstile) {
		if url != "" {
			c.siteverifyURL = url
		}
	}
}

// NewCloudflareTurnstile returns a verifier bound to the given secret.
// secret == "" produces a verifier whose Verify always returns ErrSignupTokenDisabled
// (the empty-secret check lives in Verify so misuse surfaces as the right ops
// signal rather than a silent dead object). Callers should normally guard at
// construction (see userService); this is the defense-in-depth path.
func NewCloudflareTurnstile(secret string, opts ...TurnstileOption) TurnstileVerifier {
	c := &cloudflareTurnstile{
		secret:        secret,
		siteverifyURL: defaultTurnstileSiteverifyURL,
		httpClient:    &http.Client{Timeout: turnstileHTTPTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

type turnstileResponse struct {
	Success     bool     `json:"success"`
	ErrorCodes  []string `json:"error-codes"`
	ChallengeTS string   `json:"challenge_ts,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	Action      string   `json:"action,omitempty"`
}

// Verify posts the token to Cloudflare's siteverify endpoint. Fails closed on any
// transport, decode, or verification error. The raw token is never logged.
//
// Hostname/action are intentionally NOT validated: Coves accepts any origin
// because mobile clients (iOS/Android WebView) report inconsistent hostnames
// and a future federated web client may run on third-party domains. The
// front-line abuse signal is the secret-key binding to the Cloudflare site,
// not server-side hostname matching.
func (c *cloudflareTurnstile) Verify(ctx context.Context, token, remoteIP string) error {
	if c.secret == "" {
		return ErrSignupTokenDisabled
	}
	if strings.TrimSpace(token) == "" {
		return &InvalidCaptchaError{Reason: "missing token"}
	}

	form := url.Values{}
	form.Set("secret", c.secret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.siteverifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		slog.Warn("turnstile verify: failed to build request", slog.String("error", err.Error()))
		return &CaptchaUnavailableError{Reason: "build request"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Warn("turnstile verify: request failed",
			slog.String("error", err.Error()),
			slog.String("remote_ip", remoteIP),
		)
		return &CaptchaUnavailableError{Reason: "transport"}
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("turnstile: failed to close response body", slog.String("error", closeErr.Error()))
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("turnstile verify: non-2xx status",
			slog.Int("status", resp.StatusCode),
			slog.String("remote_ip", remoteIP),
		)
		return &CaptchaUnavailableError{Reason: fmt.Sprintf("siteverify status %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("turnstile verify: read body failed", slog.String("error", err.Error()))
		return &CaptchaUnavailableError{Reason: "read body"}
	}

	var parsed turnstileResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		slog.Warn("turnstile verify: decode failed", slog.String("error", err.Error()))
		return &CaptchaUnavailableError{Reason: "decode body"}
	}

	if !parsed.Success {
		errorCodes := parsed.ErrorCodes
		if len(errorCodes) == 0 {
			errorCodes = []string{"unknown"}
		}
		slog.Warn("turnstile verify: rejected",
			slog.Any("error_codes", errorCodes),
			slog.String("remote_ip", remoteIP),
		)
		return &InvalidCaptchaError{Reason: strings.Join(errorCodes, ",")}
	}

	return nil
}
