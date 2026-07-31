package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The AppView client is pure client logic — headers in, status and JSON out —
// so it is tested against an httptest server rather than the running AppView.
// That is not a mock standing in for infrastructure: the behaviour under test IS
// the client, and a real AppView cannot be asked to answer 401 on demand. The
// contracts that prove the AppView itself are phase 4, and they run against the
// container.

// stubService answers XRPC calls from a table of canned responses.
type stubService struct {
	*httptest.Server
	// A clone, not the *http.Request: the request is not valid once the
	// handler returns, so keeping it and reading headers from the test
	// goroutine afterwards is a use-after-free with extra steps.
	lastHeaders atomic.Pointer[http.Header]
	lastBody    atomic.Pointer[[]byte]
}

func newStubService(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *stubService {
	t.Helper()
	stub := &stubService{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			// Put it back. Recording the body must not consume it, or a handler
			// that decodes the request sees an empty one — which reads as the
			// client having sent nothing.
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		headers := r.Header.Clone()
		stub.lastHeaders.Store(&headers)
		stub.lastBody.Store(&body)
		handler(w, r)
	}))
	t.Cleanup(stub.Close)
	return stub
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func TestXRPCClient_QuerySendsParamsAndDecodes(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/xrpc/social.coves.actor.getProfile", r.URL.Path)
		assert.Equal(t, "did:plc:alice", r.URL.Query().Get("actor"))
		writeJSON(w, http.StatusOK, map[string]any{"did": "did:plc:alice", "handle": "alice.test"})
	})

	var profile struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	}
	err := NewXRPCClient(stub.URL).Query(context.Background(),
		"social.coves.actor.getProfile", url.Values{"actor": {"did:plc:alice"}}, &profile)

	require.NoError(t, err)
	assert.Equal(t, "did:plc:alice", profile.DID)
	assert.Equal(t, "alice.test", profile.Handle)
}

func TestXRPCClient_ProcedureSendsJSON(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		writeJSON(w, http.StatusOK, map[string]any{"uri": "at://did:plc:alice/c/1"})
	})

	var out struct {
		URI string `json:"uri"`
	}
	err := NewXRPCClient(stub.URL).Procedure(context.Background(),
		"social.coves.community.create", map[string]any{"name": "testcove"}, &out)

	require.NoError(t, err)
	assert.Equal(t, "at://did:plc:alice/c/1", out.URI)
	assert.Contains(t, string(*stub.lastBody.Load()), `"name":"testcove"`)
}

func TestXRPCClient_SendsTheBearerToken(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})

	anonymous := NewXRPCClient(stub.URL)
	authenticated := anonymous.WithBearer("token-abc")

	require.NoError(t, authenticated.Query(context.Background(), "some.method", nil, nil))
	assert.Equal(t, "Bearer token-abc", stub.lastHeaders.Load().Get("Authorization"))

	// WithBearer copies rather than mutating: one test holding a client per
	// identity must not be able to change what another one sends.
	require.NoError(t, anonymous.Query(context.Background(), "some.method", nil, nil))
	assert.Empty(t, stub.lastHeaders.Load().Get("Authorization"))
}

func TestXRPCClient_UploadSendsRawBytes(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "image/png", r.Header.Get("Content-Type"))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	err := NewXRPCClient(stub.URL).Upload(context.Background(),
		"com.atproto.repo.uploadBlob", "image/png", TestPNG(4, 4), nil)

	require.NoError(t, err)
}

func TestXRPCClient_UploadRejectsAnUnusableContentType(t *testing.T) {
	client := NewXRPCClient("http://unused.test")
	for _, mime := range []string{"", "*/*", "image/*"} {
		err := client.Upload(context.Background(), "com.atproto.repo.uploadBlob", mime, []byte("x"), nil)
		require.Error(t, err, "content type %q should be rejected before the request", mime)
		assert.Contains(t, err.Error(), "concrete MIME type")
	}
}

// ---------------------------------------------------------------------------
// Typed errors: the split WaitFor probes depend on
// ---------------------------------------------------------------------------

func TestStatusError_CarriesTheXRPCEnvelope(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "AuthenticationRequired",
			"message": "the session has expired",
		})
	})

	err := NewXRPCClient(stub.URL).Query(context.Background(), "social.coves.feed.getTimeline", nil, nil)

	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, StatusOf(err))
	assert.True(t, IsStatus(err, http.StatusUnauthorized))
	assert.False(t, IsNotFound(err))

	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, "AuthenticationRequired", statusErr.XRPCError)
	assert.Equal(t, "the session has expired", statusErr.XRPCMessage)

	message := err.Error()
	assert.Contains(t, message, "social.coves.feed.getTimeline")
	assert.Contains(t, message, "401")
	assert.Contains(t, message, "the session has expired")
}

func TestStatusError_HandlesANonXRPCBody(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>upstream is down</html>"))
	})

	err := NewXRPCClient(stub.URL).Query(context.Background(), "some.method", nil, nil)

	require.Error(t, err)
	assert.Equal(t, http.StatusBadGateway, StatusOf(err))
	// A proxy's HTML page is not an XRPC envelope, and swallowing it would leave
	// "HTTP 502" with no indication of what answered.
	assert.Contains(t, err.Error(), "upstream is down")
}

func TestStatusOf_IsZeroForTransportFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()

	err := NewXRPCClient(address).Query(context.Background(), "some.method", nil, nil)

	require.Error(t, err)
	assert.Zero(t, StatusOf(err), "a refused connection is not a status")
	assert.False(t, IsNotFound(err))
}

func TestPendingIfNotFound_SplitsRetryableFromTerminal(t *testing.T) {
	serviceNotFound := &StatusError{Method: "m", StatusCode: http.StatusNotFound,
		XRPCError: "NotFound", XRPCShaped: true}
	recordNotFound := &StatusError{Method: "m", StatusCode: http.StatusBadRequest,
		XRPCError: "RecordNotFound", XRPCShaped: true}
	routerNotFound := &StatusError{Method: "m", StatusCode: http.StatusNotFound,
		Body: "404 page not found"}
	unauthorized := &StatusError{Method: "m", StatusCode: http.StatusUnauthorized}
	serverError := &StatusError{Method: "m", StatusCode: http.StatusInternalServerError}
	unavailable := &StatusError{Method: "m", StatusCode: http.StatusServiceUnavailable}

	done, err := PendingIfNotFound(nil)
	assert.True(t, done)
	assert.NoError(t, err)

	// Not yet: a service that understood the request and has nothing to return.
	for _, pending := range []error{serviceNotFound, recordNotFound} {
		done, err = PendingIfNotFound(pending)
		assert.False(t, done)
		assert.NoError(t, err, "%v should be waited out", pending)
	}

	// Terminal. The router 404 is the interesting one: a mistyped NSID answers
	// 404 forever, and waiting it out spends the whole timeout before reporting
	// that the record never appeared — when the truth is that nothing ever
	// asked for it.
	for _, terminal := range []error{routerNotFound, unauthorized, serverError, unavailable} {
		done, err = PendingIfNotFound(terminal)
		assert.False(t, done)
		assert.ErrorIs(t, err, terminal, "%v should fail the wait immediately", terminal)
	}
}

func TestPendingIfUnavailable_ToleratesARestartingService(t *testing.T) {
	notFound := &StatusError{Method: "m", StatusCode: http.StatusNotFound,
		XRPCError: "NotFound", XRPCShaped: true}
	unauthorized := &StatusError{Method: "m", StatusCode: http.StatusUnauthorized}
	badRequest := &StatusError{Method: "m", StatusCode: http.StatusBadRequest}

	done, err := PendingIfUnavailable(nil)
	assert.True(t, done)
	assert.NoError(t, err)

	// The statuses a service answers while it is coming back up. They say
	// nothing about the request, so a wait that is going to keep asking anyway
	// should keep asking.
	for _, status := range []int{
		http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	} {
		transient := &StatusError{Method: "m", StatusCode: status}
		assert.True(t, IsTransient(transient))
		done, err = PendingIfUnavailable(transient)
		assert.False(t, done)
		assert.NoError(t, err, "HTTP %d should be waited out by the restart-tolerant probe", status)

		// ...and NOT by the strict one, which is the default for a reason.
		_, strictErr := PendingIfNotFound(transient)
		assert.Error(t, strictErr, "HTTP %d must stay terminal for the strict probe", status)
	}

	// Everything the strict version fails on that is not transient still fails.
	for _, terminal := range []error{unauthorized, badRequest} {
		done, err = PendingIfUnavailable(terminal)
		assert.False(t, done)
		assert.ErrorIs(t, err, terminal)
	}

	// Not-found is still not-found.
	done, err = PendingIfUnavailable(notFound)
	assert.False(t, done)
	assert.NoError(t, err)
}

func TestIsNotFound_RequiresAnXRPCAnswerNotJustA404(t *testing.T) {
	// chi's own 404 body, which is what a mistyped NSID produces.
	router := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	err := NewXRPCClient(router.URL).Query(context.Background(), "social.coves.actor.getProfyle", nil, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusNotFound, StatusOf(err))
	assert.False(t, IsNotFound(err), "a router 404 is a wrong address, not a missing record")
	assert.Contains(t, err.Error(), "404 page not found")

	// The same status from a service that understood the request.
	service := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "NotFound", "message": "profile not indexed",
		})
	})
	err = NewXRPCClient(service.URL).Query(context.Background(), "social.coves.actor.getProfile", nil, nil)
	require.Error(t, err)
	assert.True(t, IsNotFound(err))
}

// TestPendingIfNotFound_DrivesWaitFor is the contract in situ: a 404 is waited
// out, a 401 fails immediately with the reason attached.
func TestPendingIfNotFound_DrivesWaitFor(t *testing.T) {
	t.Run("waits out a 404", func(t *testing.T) {
		var calls atomic.Int32
		stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "NotFound"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"did": "did:plc:alice"})
		})
		client := NewXRPCClient(stub.URL)

		WaitFor(t, 5*time.Second, func() (bool, error) {
			return PendingIfNotFound(client.Query(context.Background(), "social.coves.actor.getProfile", nil, nil))
		}, WithPollInterval(10*time.Millisecond), WithDescription("the profile to be indexed"))

		assert.EqualValues(t, 3, calls.Load())
	})

	t.Run("fails immediately on a 401", func(t *testing.T) {
		stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "AuthenticationRequired", "message": "no session",
			})
		})
		client := NewXRPCClient(stub.URL)

		ft := &fakeT{}
		start := time.Now()
		runIsolated(func() {
			WaitFor(ft, 30*time.Second, func() (bool, error) {
				return PendingIfNotFound(client.Query(context.Background(), "social.coves.actor.getProfile", nil, nil))
			}, WithDescription("the profile to be indexed"))
		})

		require.True(t, ft.failed())
		assert.Less(t, time.Since(start), 5*time.Second, "a terminal error must not be retried to the deadline")
		assert.Contains(t, ft.message(), "no session")
	})
}

// ---------------------------------------------------------------------------
// The AppView wrapper
// ---------------------------------------------------------------------------

func TestNewAppView_UsesTheConfiguredEndpoint(t *testing.T) {
	appview := NewAppView(t)
	assert.Equal(t, Endpoints().AppView.BaseURL, appview.BaseURL)
	assert.Empty(t, appview.Bearer)

	authenticated := NewAppView(t, WithAppViewBearer("token-abc"), WithAppViewURL("http://appview.test:9999/"))
	assert.Equal(t, "http://appview.test:9999", authenticated.BaseURL, "a trailing slash is trimmed")
	assert.Equal(t, "token-abc", authenticated.Bearer)
}

func TestAppView_AsIsIndependentPerIdentity(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	appview := NewAppView(t, WithAppViewURL(stub.URL))

	alice := appview.As("alice-token")
	bob := appview.As("bob-token")

	require.NoError(t, alice.Query(context.Background(), "m", nil, nil))
	assert.Equal(t, "Bearer alice-token", stub.lastHeaders.Load().Get("Authorization"))
	require.NoError(t, bob.Query(context.Background(), "m", nil, nil))
	assert.Equal(t, "Bearer bob-token", stub.lastHeaders.Load().Get("Authorization"))
	assert.Empty(t, appview.Bearer, "the original client stays anonymous")
}

// A service becomes healthy after answering 503 a few times, and the wait has
// to keep asking. The delay is counted in PROBES, not slept: flipping a flag
// from a timer goroutine would make the claim "it polled" depend on the
// scheduler beating the poll interval, which is the guess this package exists
// to delete from the rest of the suite.
func TestAppView_WaitHealthy(t *testing.T) {
	var probes atomic.Int32
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/xrpc/_health", r.URL.Path)
		if probes.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"version": "test"})
	})
	appview := NewAppView(t, WithAppViewURL(stub.URL))

	appview.WaitHealthy(t, 5*time.Second)
	assert.Equal(t, int32(3), probes.Load(),
		"WaitHealthy must poll past a 503 rather than accepting the first answer")
}

func TestAppView_WaitHealthyReportsTheLastFailure(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		// 503: listening, not ready. Waited out, then reported.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "NotReady", "message": "the database is unreachable",
		})
	})
	appview := NewAppView(t, WithAppViewURL(stub.URL))

	ft := &fakeT{}
	runIsolated(func() { appview.WaitHealthy(ft, 300*time.Millisecond) })

	require.True(t, ft.failed())
	// A health probe that times out with no explanation is the failure this
	// whole tier exists to stop producing.
	assert.Contains(t, ft.message(), "timed out")
	assert.Contains(t, ft.message(), "the database is unreachable")
	assert.Contains(t, ft.message(), stub.URL)
}

// TestAppView_WaitHealthyDoesNotWaitOutAnAnswer is the fix for a health check
// that lied by omission.
//
// Swallowing every error into "not yet" means a service reachable at the WRONG
// PATH — a proxy in front of the wrong upstream, a stale APPVIEW_URL — answers
// 404 instantly and forever, and the wait spends its whole timeout before
// reporting that the service never answered. It answered every time. What it
// said was that the address is wrong.
func TestAppView_WaitHealthyDoesNotWaitOutAnAnswer(t *testing.T) {
	var probes atomic.Int32
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		http.NotFound(w, r)
	})
	appview := NewAppView(t, WithAppViewURL(stub.URL))

	ft := &fakeT{}
	start := time.Now()
	runIsolated(func() { appview.WaitHealthy(ft, 30*time.Second) })

	require.True(t, ft.failed())
	assert.Less(t, time.Since(start), 5*time.Second, "an answered 404 is not something to wait out")
	assert.EqualValues(t, 1, probes.Load(), "the first answer was already conclusive")
	message := ft.message()
	assert.Contains(t, message, "HTTP 404")
	assert.Contains(t, message, "check the address before the service")
	assert.NotContains(t, message, "timed out")
}

func TestPDS_WaitHealthyDoesNotWaitOutAnAnswer(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	pds := NewPDS(t, WithPDSURL(stub.URL))

	ft := &fakeT{}
	runIsolated(func() { pds.WaitHealthy(ft, 30*time.Second) })

	require.True(t, ft.failed())
	assert.Contains(t, ft.message(), "the PDS")
	assert.Contains(t, ft.message(), "HTTP 404")
}

// ---------------------------------------------------------------------------
// Consumer health: the diagnostic surface the pipeline tier attaches to timeouts
// ---------------------------------------------------------------------------

func TestXRPCClient_GetReachesPlainPaths(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/health/consumers", r.URL.Path, "not everything lives under /xrpc")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	var out struct {
		Status string `json:"status"`
	}
	require.NoError(t, NewXRPCClient(stub.URL).Get(context.Background(), "/health/consumers", &out))
	assert.Equal(t, "ok", out.Status)

	err := NewXRPCClient(stub.URL).Get(context.Background(), "health/consumers", nil)
	require.Error(t, err, "a path without a leading slash would silently join wrong")
}

func TestAppView_ConsumerHealth(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "stalled",
			"consumers": []map[string]any{{
				"name": "posts", "connected": false, "cursorTimeUs": 1751000000000000,
				"eventsProcessed": 12, "eventsDeadLettered": 3, "deadLetterBacklog": 3,
			}},
		})
	})
	appview := NewAppView(t, WithAppViewURL(stub.URL))

	report, err := appview.ConsumerHealth(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "stalled", report.Status)
	require.Len(t, report.Consumers, 1)
	assert.Equal(t, "posts", report.Consumers[0].Name)
	assert.False(t, report.Consumers[0].Connected)
	assert.EqualValues(t, 3, report.Consumers[0].DeadLetterBacklog)
	assert.Contains(t, report.String(), "posts: connected=false")
}

// TestWithConsumerHealth_AttachesToATimeout is docs/TEST_ARCHITECTURE.md §3.3's
// promise made good: a pipeline wait that times out says what the consumers were
// doing, so "the record never appeared" arrives with the reason attached.
func TestWithConsumerHealth_AttachesToATimeout(t *testing.T) {
	var healthProbes atomic.Int32
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/consumers" {
			healthProbes.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "stalled",
				"consumers": []map[string]any{
					{"name": "posts", "connected": false, "deadLetterBacklog": 7},
				},
			})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "NotFound"})
	})
	appview := NewAppView(t, WithAppViewURL(stub.URL))

	ft := &fakeT{}
	runIsolated(func() {
		WaitFor(ft, 200*time.Millisecond, func() (bool, error) {
			return PendingIfNotFound(appview.Query(context.Background(), "social.coves.feed.getPost", nil, nil))
		},
			WithPollInterval(50*time.Millisecond),
			WithDescription("the post to be indexed"),
			WithConsumerHealth(appview))
	})

	require.True(t, ft.failed())
	message := ft.message()
	assert.Contains(t, message, "the post to be indexed")
	assert.Contains(t, message, "consumers: stalled")
	assert.Contains(t, message, "posts: connected=false")
	assert.Contains(t, message, "backlog=7")
	// Diagnostics run on the failure path only; a passing wait must not be
	// polling the health endpoint every interval.
	assert.EqualValues(t, 1, healthProbes.Load())
}

func TestWithConsumerHealth_SurvivesAnUnreachableAppView(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()
	appview := NewAppView(t, WithAppViewURL(address))

	ft := &fakeT{}
	runIsolated(func() {
		WaitFor(ft, 50*time.Millisecond, func() (bool, error) { return false, nil },
			WithDescription("something"), WithConsumerHealth(appview))
	})

	require.True(t, ft.failed())
	// Best effort: a diagnostic hook runs on a path that is already failing and
	// must not fail differently.
	assert.Contains(t, ft.message(), "consumer health unavailable")
}

func TestAppView_ClientIPIsSentOnEveryRequest(t *testing.T) {
	// The AppView rate limits by client IP and reads X-Real-IP first. Every
	// service in the hermetic stack shares one network namespace, so without
	// this header every test in the pipeline tier spends from one bucket.
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	ip := SyntheticClientIP("contract/example")
	appview := NewAppView(t, WithAppViewURL(stub.URL), WithAppViewClientIP(ip))

	require.NoError(t, appview.Query(context.Background(), "m", nil, nil))
	assert.Equal(t, ip, stub.lastHeaders.Load().Get("X-Real-IP"))

	require.NoError(t, appview.Procedure(context.Background(), "m", map[string]string{"a": "b"}, nil))
	assert.Equal(t, ip, stub.lastHeaders.Load().Get("X-Real-IP"),
		"a procedure is a request too; a half-labelled client splits its own bucket")

	require.NoError(t, appview.Get(context.Background(), "/health/consumers", nil))
	assert.Equal(t, ip, stub.lastHeaders.Load().Get("X-Real-IP"),
		"the plain-path helper must carry it as well — consumer-health polling is requests")
}

func TestAppView_ClientIPSurvivesAuthentication(t *testing.T) {
	// As() clones the client. A shallow copy would keep the SAME header map, so
	// the two identities would alias — and a later WithHeader on one would
	// silently rewrite the other's. Both must carry the IP, independently.
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	ip := SyntheticClientIP("contract/authenticated")
	appview := NewAppView(t, WithAppViewURL(stub.URL), WithAppViewClientIP(ip))

	alice := appview.As("alice-token")
	require.NoError(t, alice.Query(context.Background(), "m", nil, nil))
	assert.Equal(t, ip, stub.lastHeaders.Load().Get("X-Real-IP"))
	assert.Equal(t, "Bearer alice-token", stub.lastHeaders.Load().Get("Authorization"))
}

func TestXRPCClient_WithHeaderDoesNotMutateTheOriginal(t *testing.T) {
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	base := NewXRPCClient(stub.URL).WithHeader("X-Real-IP", "2001:db8::1")
	other := base.WithHeader("X-Real-IP", "2001:db8::2")

	require.NoError(t, base.Query(context.Background(), "m", nil, nil))
	assert.Equal(t, "2001:db8::1", stub.lastHeaders.Load().Get("X-Real-IP"),
		"deriving a second client must not reach back into the first one's headers")
	require.NoError(t, other.Query(context.Background(), "m", nil, nil))
	assert.Equal(t, "2001:db8::2", stub.lastHeaders.Load().Get("X-Real-IP"))
}
