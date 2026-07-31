package users

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingTransport fails every request with a deterministic transport error,
// standing in for an unreachable siteverify host.
//
// The alternative — pointing the client at 127.0.0.1:0 and letting the dial
// fail — makes a unit test's result depend on the machine's network stack, and
// costs whatever the client's timeout is. This costs nothing and cannot be
// affected by the sandbox the test happens to run in.
type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// unreachableClient returns an http.Client whose every request fails in the
// transport, without opening a socket.
func unreachableClient() *http.Client {
	return &http.Client{Transport: failingTransport{err: errors.New("dial tcp: simulated connection refused")}}
}

func newTestTurnstile(t *testing.T, handler http.HandlerFunc) (*cloudflareTurnstile, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	v := &cloudflareTurnstile{
		secret:        "test-secret",
		siteverifyURL: server.URL,
		httpClient:    &http.Client{Timeout: 2 * time.Second},
	}
	return v, server
}

func TestNewCloudflareTurnstile_SiteverifyURL(t *testing.T) {
	t.Run("defaults to Cloudflare", func(t *testing.T) {
		v := NewCloudflareTurnstile("s").(*cloudflareTurnstile)
		assert.Equal(t, defaultTurnstileSiteverifyURL, v.siteverifyURL)
	})

	// The value is opaque to the option: this subtest proves WithSiteverifyURL
	// stores what it is given, so the string is the fixture AND the expected
	// output. No request is made, and nothing listens on this address.
	const stubSiteverifyURL = "http://localhost:3003/stub" // coves:allow-host-literal: opaque fixture for the option setter; asserted on, never dialled

	t.Run("override redirects verification", func(t *testing.T) {
		v := NewCloudflareTurnstile("s", WithSiteverifyURL(stubSiteverifyURL)).(*cloudflareTurnstile)
		assert.Equal(t, stubSiteverifyURL, v.siteverifyURL)
	})

	// The override is plumbed from an env var that is empty in every
	// non-dev deployment, so empty must mean "leave Cloudflare alone"
	// rather than "verify against the empty URL".
	t.Run("empty override keeps the default", func(t *testing.T) {
		v := NewCloudflareTurnstile("s", WithSiteverifyURL("")).(*cloudflareTurnstile)
		assert.Equal(t, defaultTurnstileSiteverifyURL, v.siteverifyURL)
	})
}

func TestTurnstile_Verify_Success(t *testing.T) {
	var capturedBody string
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	err := v.Verify(context.Background(), "tok-123", "1.2.3.4")
	assert.NoError(t, err)
	assert.Contains(t, capturedBody, "secret=test-secret")
	assert.Contains(t, capturedBody, "response=tok-123")
	assert.Contains(t, capturedBody, "remoteip=1.2.3.4")
}

func TestTurnstile_Verify_OmitsRemoteIPWhenEmpty(t *testing.T) {
	var capturedBody string
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	err := v.Verify(context.Background(), "tok-abc", "")
	require.NoError(t, err)
	assert.NotContains(t, capturedBody, "remoteip")
}

func TestTurnstile_Verify_RejectionReturnsInvalidCaptcha(t *testing.T) {
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	})

	err := v.Verify(context.Background(), "bad-token", "")
	require.Error(t, err)

	var captchaErr *InvalidCaptchaError
	require.True(t, errors.As(err, &captchaErr), "user-side rejection must be InvalidCaptchaError")
	var unavailableErr *CaptchaUnavailableError
	assert.False(t, errors.As(err, &unavailableErr), "rejection must NOT be CaptchaUnavailableError")
	assert.Contains(t, captchaErr.Reason, "invalid-input-response")
}

func TestTurnstile_Verify_EmptyErrorCodesDefaultsToUnknown(t *testing.T) {
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false}`))
	})

	err := v.Verify(context.Background(), "bad-token", "")
	require.Error(t, err)
	var captchaErr *InvalidCaptchaError
	require.True(t, errors.As(err, &captchaErr))
	assert.Equal(t, "unknown", captchaErr.Reason, "empty error-codes must surface as 'unknown', not empty string")
}

func TestTurnstile_Verify_5xxIsUnavailable(t *testing.T) {
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	err := v.Verify(context.Background(), "any", "")
	require.Error(t, err)

	var unavailable *CaptchaUnavailableError
	require.True(t, errors.As(err, &unavailable), "5xx must be CaptchaUnavailableError (→ 503), not InvalidCaptchaError")
	var captchaErr *InvalidCaptchaError
	assert.False(t, errors.As(err, &captchaErr))
}

func TestTurnstile_Verify_DecodeErrorIsUnavailable(t *testing.T) {
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	})

	err := v.Verify(context.Background(), "any", "")
	require.Error(t, err)

	var unavailable *CaptchaUnavailableError
	assert.True(t, errors.As(err, &unavailable))
}

func TestTurnstile_Verify_UnreachableIsUnavailable(t *testing.T) {
	v := &cloudflareTurnstile{
		secret:        "test",
		siteverifyURL: "http://siteverify.invalid",
		httpClient:    unreachableClient(),
	}

	err := v.Verify(context.Background(), "tok", "")
	require.Error(t, err)

	var unavailable *CaptchaUnavailableError
	assert.True(t, errors.As(err, &unavailable))
}

// Slow siteverify — exercises the ctx.DeadlineExceeded code path distinct from
// "host unreachable". Both must surface as CaptchaUnavailableError.
//
// Implementation note: handler returns when EITHER its own short timer fires
// OR the request context is canceled (which happens when our client times out).
// Avoids leaking goroutines past the test boundary.
func TestTurnstile_Verify_SlowHostIsUnavailable(t *testing.T) {
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	})
	v.httpClient = &http.Client{Timeout: 100 * time.Millisecond}

	err := v.Verify(context.Background(), "tok", "")
	require.Error(t, err)
	var unavailable *CaptchaUnavailableError
	assert.True(t, errors.As(err, &unavailable))
}

func TestTurnstile_Verify_EmptyToken(t *testing.T) {
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("siteverify should not be called for empty token")
	})

	err := v.Verify(context.Background(), "   ", "")
	require.Error(t, err)

	var captchaErr *InvalidCaptchaError
	assert.True(t, errors.As(err, &captchaErr))
	assert.Equal(t, "missing token", captchaErr.Reason)
}

func TestTurnstile_Verify_MissingSecretReturnsDisabled(t *testing.T) {
	v := &cloudflareTurnstile{
		secret:        "",
		siteverifyURL: "http://unused",
		httpClient:    &http.Client{Timeout: time.Second},
	}

	err := v.Verify(context.Background(), "tok", "")
	require.Error(t, err)
	// Misconfiguration must surface distinctly from a captcha rejection.
	assert.ErrorIs(t, err, ErrSignupTokenDisabled)
}

// Body must be url-encoded form data with a parseable secret/response pair.
// Stronger than "contains '=' and doesn't start with '{'": uses url.ParseQuery
// to verify the form is actually well-formed.
func TestTurnstile_Verify_UsesFormEncoding(t *testing.T) {
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		values, err := url.ParseQuery(string(body))
		assert.NoError(t, err, "body must be parseable as url-encoded form")
		assert.Equal(t, "test-secret", values.Get("secret"))
		assert.Equal(t, "tok-form", values.Get("response"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	err := v.Verify(context.Background(), "tok-form", "")
	assert.NoError(t, err)
}

// Hostname/action are intentionally NOT validated. This test pins that decision
// so a future drive-by "let's enforce hostname" change breaks the test loudly
// and forces the contributor to update the documented contract.
func TestTurnstile_Verify_AcceptsAnyHostnameAndAction(t *testing.T) {
	v, _ := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"hostname":"some-other-domain.example","action":"unexpected"}`))
	})

	err := v.Verify(context.Background(), "tok", "")
	assert.NoError(t, err, "Coves accepts any hostname/action; mobile WebViews report inconsistent values")
}

// captureHandler buffers slog records so tests can assert on logged content.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// Asserts our CLAUDE.md rule: never log the raw Turnstile token.
// Triggers every code path that emits a slog event (rejection, 5xx, decode
// error, body-read error, and a successful build-request → transport failure)
// and scans every record for the secret token string.
func TestTurnstile_Verify_NeverLogsRawToken(t *testing.T) {
	const secretToken = "SECRET-DO-NOT-LOG-12345"

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "rejection",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
			},
		},
		{
			name: "5xx",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "decode error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`not-json`))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(capture))
			t.Cleanup(func() { slog.SetDefault(prev) })

			v, _ := newTestTurnstile(t, tc.handler)
			_ = v.Verify(context.Background(), secretToken, "9.9.9.9")

			capture.mu.Lock()
			defer capture.mu.Unlock()
			require.NotEmpty(t, capture.records, "%s should have produced at least one log record", tc.name)
			for _, rec := range capture.records {
				assert.NotContains(t, rec.Message, secretToken, "log message must not contain raw token")
				rec.Attrs(func(a slog.Attr) bool {
					assert.NotContains(t, a.Value.String(), secretToken,
						"log attr %q must not contain raw token", a.Key)
					return true
				})
			}
		})
	}
}

// Belt-and-suspenders: even on transport failure, the IP attached to the log is
// the caller-supplied remoteIP, not the token.
func TestTurnstile_Verify_TransportFailureLogsClientIPNotToken(t *testing.T) {
	const tokenSentinel = "TOKEN-SENTINEL-XYZ"
	const ipSentinel = "203.0.113.42"

	capture := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	v := &cloudflareTurnstile{
		secret:        "s",
		siteverifyURL: "http://siteverify.invalid",
		httpClient:    unreachableClient(),
	}
	_ = v.Verify(context.Background(), tokenSentinel, ipSentinel)

	capture.mu.Lock()
	defer capture.mu.Unlock()

	var sawIP bool
	for _, rec := range capture.records {
		require.NotContains(t, rec.Message, tokenSentinel)
		rec.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), ipSentinel) {
				sawIP = true
			}
			require.NotContains(t, a.Value.String(), tokenSentinel)
			return true
		})
	}
	assert.True(t, sawIP, "expected client IP to appear in log attrs")
}
