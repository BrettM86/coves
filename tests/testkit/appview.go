package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The XRPC client, and the AppView it talks to.
//
// XRPCClient is the one HTTP/JSON client in the kit: appview.go uses it against
// the AppView, pds.go uses it against the PDS, and neither hand-rolls a request.
// It is deliberately small — headers, a JSON body, a typed error — because the
// interesting behaviour belongs to the callers.
//
// WHY THE ERRORS ARE TYPED
//
// wait.go's Probe contract splits every failure in two: "not yet" is
// (false, nil) and gets retried, anything else is terminal and fails the test
// immediately with the reason attached. Making that split correctly needs the
// HTTP status, so a client that returns fmt.Errorf("unexpected status 401")
// forces every probe to either string-match or give up and retry everything —
// which is how "the session was rejected" becomes "timed out after 30s waiting
// for the post to appear". StatusError carries the status, the XRPC error name
// and the body; PendingIfNotFound turns the common case into one line.

// maxErrorBody bounds how much of a failing response is captured into an error
// message. A stack trace or an HTML error page is useful; a paginated feed that
// answered 500 halfway through is not worth a megabyte in the test log.
const maxErrorBody = 64 << 10

// defaultXRPCTimeout bounds a single request. Long enough for a blob upload on a
// loaded CI machine, short enough that a hung service fails the test instead of
// the go test timeout killing the whole binary with no attribution.
const defaultXRPCTimeout = 30 * time.Second

// StatusError is a non-2xx answer from an XRPC endpoint.
//
// Both the transport status and the lexicon-level error name are kept: a PDS
// answers 400 with error "InvalidRequest" for a malformed record and 400 with
// "RecordNotFound" for a missing one, so the status alone cannot tell a test
// what happened.
type StatusError struct {
	// Method is the NSID that was called, e.g. "com.atproto.repo.createRecord".
	Method     string
	StatusCode int
	// XRPCError and XRPCMessage come from the {"error", "message"} body every
	// atProto service returns on failure. Both are empty when the body was not
	// XRPC-shaped — an HTML 502 from a proxy, for instance.
	XRPCError   string
	XRPCMessage string
	// XRPCShaped records whether the body was an XRPC error envelope at all.
	//
	// It is the difference between a service answering "no such record" and a
	// router answering "no such route". Both are 404s, and only the first is
	// worth waiting on: see IsNotFound.
	XRPCShaped bool
	// Body is the raw response, truncated to maxErrorBody.
	Body string
}

func (e *StatusError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: HTTP %d", e.Method, e.StatusCode)
	if e.XRPCError != "" {
		fmt.Fprintf(&b, " %s", e.XRPCError)
	}
	switch {
	case e.XRPCMessage != "":
		fmt.Fprintf(&b, ": %s", e.XRPCMessage)
	case e.Body != "":
		fmt.Fprintf(&b, ": %s", e.Body)
	}
	return b.String()
}

// StatusOf returns the HTTP status err carries, or 0 if it is not a
// StatusError — a connection refused, a DNS failure, a cancelled context.
func StatusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.StatusCode
	}
	return 0
}

// IsStatus reports whether err is a StatusError with the given status.
func IsStatus(err error, code int) bool { return StatusOf(err) == code }

// IsNotFound reports whether err is a service saying the thing is not there.
//
// A 404 ALONE IS NOT ENOUGH, and the distinction is load-bearing. A mistyped
// NSID, a route that was never registered, or a reverse proxy in front of the
// wrong upstream all answer 404 with a plain-text body — chi's is literally
// "404 page not found". Treating those as "not indexed yet" makes every such
// typo cost the full WaitFor timeout and then report the wrong problem: the
// wait says the record never appeared, when the truth is that nothing ever
// asked for it. So a 404 counts only when the body is an XRPC error envelope,
// which is what a service that understood the request produces.
//
// atProto also expresses "no such record" as a 400 with the XRPC error name
// RecordNotFound, so that spelling counts wherever it appears — otherwise a
// probe waiting for a record to be indexed would treat the PDS' own "not there
// yet" as terminal.
func IsNotFound(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch {
	case strings.EqualFold(se.XRPCError, "RecordNotFound"), strings.EqualFold(se.XRPCError, "NotFound"):
		return true
	case se.StatusCode == http.StatusNotFound:
		return se.XRPCShaped
	default:
		return false
	}
}

// IsTransient reports whether err is a service that is momentarily unable to
// answer rather than one that has answered.
//
// These are the statuses a restarting or rate-limited service returns: 429, and
// the gateway family 502/503/504. They say nothing about the request, so a
// wait that is going to keep asking anyway should keep asking.
func IsTransient(err error) bool {
	switch StatusOf(err) {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// PendingIfNotFound converts a lookup error into a Probe result: a service
// saying "not there" is "not yet", and everything else is terminal.
//
// It is the body of nearly every WaitFor probe against a serving endpoint:
//
//	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
//	        err := appview.Query(ctx, "social.coves.actor.getProfile", params, &got)
//	        return testkit.PendingIfNotFound(err)
//	}, testkit.WithDescription("profile for %s indexed", did))
//
// A nil error reports done, so returning it directly is also correct when err
// may be nil.
//
// It is STRICT on purpose: a 401, a 400, a 500 and a bare router 404 all fail
// the wait immediately with the response attached. Use it whenever the service
// under observation is expected to stay up for the duration of the wait, which
// is every steady-state contract. When the wait deliberately spans a restart —
// the reliability suite's cursor-resume and replay cases — use
// PendingIfUnavailable instead.
func PendingIfNotFound(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// PendingIfUnavailable is PendingIfNotFound plus tolerance for a service that is
// momentarily down: 429 and the 502/503/504 gateway family are "not yet" as
// well.
//
// Use it ONLY where a restart is part of what the test is doing. Everywhere else
// the strict version is what you want, because a 503 that nobody expected is a
// finding, and converting it into thirty seconds of patient retrying is how it
// stops being one.
//
// Retry-After is not honoured; the WaitFor poll interval governs. Nothing in
// this stack sends it, and guessing at a server's pacing hint would make the
// wait's timing depend on a header no test controls.
func PendingIfUnavailable(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case IsNotFound(err), IsTransient(err):
		return false, nil
	default:
		return false, err
	}
}

// ---------------------------------------------------------------------------
// XRPC client
// ---------------------------------------------------------------------------

// XRPCClient calls XRPC endpoints on one service, optionally authenticated.
//
// Zero HTTP is exposed to callers: give it an NSID, get a decoded response or a
// StatusError. It uses its own http.Client rather than http.DefaultClient,
// which the suite has historically mutated in places.
type XRPCClient struct {
	// BaseURL is the service root, without a trailing slash or the /xrpc path.
	BaseURL string
	// Bearer, when set, is sent as the Authorization header.
	Bearer string
	// Headers are sent on every request, before Accept and Authorization (so
	// those two always win). This is how a test controls what the service sees
	// about the caller rather than about the call — X-Real-IP above all, which
	// is what the AppView's rate limiter buckets by.
	Headers http.Header
	// HTTP is the transport. Never nil for a client built by NewXRPCClient.
	HTTP *http.Client
}

// NewXRPCClient builds a client for a service root.
func NewXRPCClient(baseURL string) *XRPCClient {
	return &XRPCClient{
		BaseURL: trimURL(baseURL),
		HTTP:    &http.Client{Timeout: defaultXRPCTimeout},
	}
}

// clone returns an independent copy, deep enough that a caller cannot reach
// through it into the original's headers.
//
// A plain struct copy would share the Headers map, so setting a header on one
// identity's client would silently set it on every other client derived from
// the same root — the exact aliasing the copy exists to prevent.
func (c *XRPCClient) clone() *XRPCClient {
	copied := *c
	if c.Headers != nil {
		copied.Headers = c.Headers.Clone()
	}
	return &copied
}

// WithBearer returns a copy of the client authenticated as the holder of token.
//
// A copy, not a mutation: a test that holds one client per identity must not be
// able to change what another identity's client sends.
func (c *XRPCClient) WithBearer(token string) *XRPCClient {
	copied := c.clone()
	copied.Bearer = token
	return copied
}

// WithHeader returns a copy of the client that sends one extra header on every
// request.
func (c *XRPCClient) WithHeader(name, value string) *XRPCClient {
	copied := c.clone()
	if copied.Headers == nil {
		copied.Headers = make(http.Header)
	}
	copied.Headers.Set(name, value)
	return copied
}

// URL renders the absolute URL of an XRPC method on this service.
func (c *XRPCClient) URL(nsid string) string {
	return c.BaseURL + "/xrpc/" + nsid
}

// Query performs a GET against an XRPC query method, decoding a JSON response
// into out. A nil out discards the body.
func (c *XRPCClient) Query(ctx context.Context, nsid string, params url.Values, out any) error {
	target := c.URL(nsid)
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("%s: building request: %w", nsid, err)
	}
	return c.do(req, nsid, out)
}

// Procedure performs a POST against an XRPC procedure method, sending in as
// JSON and decoding the response into out. Either may be nil.
func (c *XRPCClient) Procedure(ctx context.Context, nsid string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s: encoding request: %w", nsid, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL(nsid), body)
	if err != nil {
		return fmt.Errorf("%s: building request: %w", nsid, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, nsid, out)
}

// Upload performs a POST whose body is raw bytes under the given content type,
// for the binary procedures (com.atproto.repo.uploadBlob).
//
// contentType must be the payload's concrete MIME type. A PDS enforcing the
// granular blob:*/* scope matches the granted accept patterns against it, and
// "*/*" matches nothing — an upload that sends a wildcard fails with a scope
// error that says nothing about content types.
func (c *XRPCClient) Upload(ctx context.Context, nsid, contentType string, data []byte, out any) error {
	if contentType == "" || strings.Contains(contentType, "*") {
		return fmt.Errorf("%s: contentType must be a concrete MIME type such as \"image/png\", got %q", nsid, contentType)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL(nsid), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%s: building request: %w", nsid, err)
	}
	req.Header.Set("Content-Type", contentType)
	return c.do(req, nsid, out)
}

// Get performs a GET against a plain path on the service, outside the /xrpc
// namespace: /health, /health/consumers, the OAuth metadata documents.
//
// path is joined to the base URL as given, so it must start with "/".
func (c *XRPCClient) Get(ctx context.Context, path string, out any) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("testkit: path %q must start with \"/\"", path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s: building request: %w", path, err)
	}
	return c.do(req, path, out)
}

// BinaryResponse is a non-JSON response: what was served, and enough about it
// to assert the service really served content rather than merely not failing.
type BinaryResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

// GetBinary fetches a plain path and returns the raw response.
//
// # WHY THIS EXISTS ALONGSIDE Get
//
// Get answers only "did this 2xx", and discards the body. That is the right
// shape for a health probe and the WRONG shape for asserting that an image URL
// serves an image: a 204 with no body satisfies "did not fail" while serving
// nothing at all, and so does a 200 whose body is an empty byte slice or an
// HTML error page the upstream returned with the wrong status. An image path is
// exactly where those distinctions matter, because the failure being guarded
// against — a proxy that cannot reach the blob store — is upstream of the
// status code the proxy chooses to report.
//
// So this returns the three facts a caller needs to make the real claim
// (status, content type, bytes) rather than folding them into a bool. The body
// is bounded: a test asserting an image is non-empty does not need to buffer an
// arbitrarily large one, and an unbounded read here would make a runaway
// response a hang instead of a failure.
//
// Unlike Get, a non-2xx is returned as a StatusError, so callers keep the
// familiar testkit.IsStatus handling.
func (c *XRPCClient) GetBinary(ctx context.Context, path string) (BinaryResponse, error) {
	if !strings.HasPrefix(path, "/") {
		return BinaryResponse{}, fmt.Errorf("testkit: path %q must start with \"/\"", path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return BinaryResponse{}, fmt.Errorf("%s: building request: %w", path, err)
	}
	for name, values := range c.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	// Deliberately NOT "application/json": this path serves bytes, and a server
	// content-negotiating on the header would be handed the wrong answer.
	req.Header.Set("Accept", "*/*")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return BinaryResponse{}, fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return BinaryResponse{}, newStatusError(path, resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBinaryBody))
	if err != nil {
		return BinaryResponse{}, fmt.Errorf("%s: reading %d response: %w", path, resp.StatusCode, err)
	}
	return BinaryResponse{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}, nil
}

// maxBinaryBody bounds how much of a binary response GetBinary buffers. Test
// fixtures are a few hundred bytes; this is generous enough that a truncation
// means something is wrong, and small enough that a runaway response fails
// rather than exhausts memory.
const maxBinaryBody = 8 << 20

// Health calls the service's _health endpoint.
//
// It asserts only that the service answered 2xx. Both the AppView and the PDS
// answer this endpoint with a version document and no status field, so there is
// nothing further to check here — in particular, an AppView whose consumers have
// all stalled still answers /xrpc/_health with a 200. Consumer liveness is a
// separate question with a separate endpoint: see AppView.ConsumerHealth.
func (c *XRPCClient) Health(ctx context.Context) error {
	return c.Query(ctx, "_health", nil, nil)
}

func (c *XRPCClient) do(req *http.Request, nsid string, out any) error {
	for name, values := range c.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", nsid, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newStatusError(nsid, resp)
	}
	if out == nil {
		// Drained rather than abandoned, so the connection returns to the pool
		// instead of being torn down after every discarded response.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decoding %d response: %w", nsid, resp.StatusCode, err)
	}
	return nil
}

// newStatusError reads a failing response into a StatusError, parsing the XRPC
// error envelope when there is one.
func newStatusError(nsid string, resp *http.Response) *StatusError {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	se := &StatusError{
		Method:     nsid,
		StatusCode: resp.StatusCode,
		Body:       strings.TrimSpace(string(body)),
	}
	if readErr != nil {
		// Said rather than swallowed. An empty Body reads as "the service
		// answered with nothing", which would be a lie about a response that was
		// cut off mid-transfer — and the difference matters when the next
		// question is whether the service is healthy.
		se.Body = strings.TrimSpace(se.Body) +
			fmt.Sprintf(" [body truncated: %v]", readErr)
		return se
	}

	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != "" {
		se.XRPCError = envelope.Error
		se.XRPCMessage = envelope.Message
		se.XRPCShaped = true
	}
	return se
}

// ---------------------------------------------------------------------------
// The AppView
// ---------------------------------------------------------------------------

// AppView is an XRPC client pointed at the running AppView container.
//
// It is thin on purpose. The pipeline tier's semantics — which endpoint proves
// which contract, what a consumer-health snapshot should say when a wait times
// out — land in phase 4; this is the transport those tests will be written on.
type AppView struct {
	*XRPCClient
}

type appViewConfig struct {
	baseURL  string
	bearer   string
	clientIP string
}

// AppViewOption customises NewAppView.
//
// Options configure a private struct rather than mutating the client, so an
// option cannot be applied to a live client that another test is already using.
type AppViewOption func(*appViewConfig)

// WithAppViewBearer authenticates every call as the holder of token.
func WithAppViewBearer(token string) AppViewOption {
	return func(c *appViewConfig) { c.bearer = token }
}

// WithAppViewURL overrides the AppView address from Endpoints(), for the rare
// test that runs its own server (an httptest instance standing in for the real
// one).
func WithAppViewURL(baseURL string) AppViewOption {
	return func(c *appViewConfig) { c.baseURL = trimURL(baseURL) }
}

// WithAppViewClientIP makes every request claim to come from ip, via X-Real-IP.
//
// THIS IS A RATE-LIMIT CONCERN, NOT A SPOOFING TRICK. The AppView rate limits
// globally by client IP (cmd/server/routes.go: 100 requests per minute), and it
// reads that IP from X-Real-IP first — the header its reverse proxy sets. Every
// test in the hermetic stack shares one network namespace, so without this
// header every request from every test arrives from 127.0.0.1 and lands in ONE
// bucket. A pipeline test that polls a serving endpoint is a request generator,
// so tests would start starving each other of quota and failing with 429s that
// look like an application defect.
//
// Pair it with SyntheticClientIP, which mints an address unique to this run and
// to a caller-chosen label.
func WithAppViewClientIP(ip string) AppViewOption {
	return func(c *appViewConfig) { c.clientIP = ip }
}

// NewAppView returns a client for the AppView the test stack is running.
func NewAppView(t TestingT, opts ...AppViewOption) *AppView {
	t.Helper()
	cfg := appViewConfig{baseURL: Endpoints().AppView.BaseURL}
	for _, opt := range opts {
		opt(&cfg)
	}
	client := NewXRPCClient(cfg.baseURL)
	client.Bearer = cfg.bearer
	if cfg.clientIP != "" {
		client = client.WithHeader("X-Real-IP", cfg.clientIP)
	}
	return &AppView{XRPCClient: client}
}

// As returns a copy of the client authenticated as the holder of token, so one
// test can hold a client per identity.
//
// The client IP carries over: the two identities are the same test, and giving
// them separate rate-limit buckets would hide exactly the quota a contract is
// spending.
func (a *AppView) As(token string) *AppView {
	return &AppView{XRPCClient: a.WithBearer(token)}
}

// WaitHealthy blocks until the AppView answers its health endpoint, failing the
// test if it has not within timeout.
func (a *AppView) WaitHealthy(t TestingT, timeout time.Duration) {
	t.Helper()
	waitHealthy(t, timeout, "the AppView", a.BaseURL, a.Health)
}

// waitHealthy is the shared body of AppView.WaitHealthy and PDS.WaitHealthy.
//
// It does NOT swallow every failure into "not yet", which is the trap a health
// wait falls into: a service reachable at the wrong path answers 404 instantly
// and forever, and a wait that treats that as "still starting" spends its whole
// timeout and then reports that the service never answered — when in truth it
// answered every single time, with the news that the URL is wrong.
//
// So the three cases are kept apart. A transport failure is genuinely "not yet":
// nothing is listening, which is what a starting service looks like. A 5xx or
// 429 is also "not yet": it is listening and not ready. Any other status is an
// ANSWER, and answering 404 or 401 to a health check is a configuration finding
// that should surface in the first second rather than the thirtieth.
func waitHealthy(t TestingT, timeout time.Duration, subject, baseURL string, probe func(context.Context) error) {
	t.Helper()
	var last error
	WaitFor(t, timeout, func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		last = probe(ctx)
		switch {
		case last == nil:
			return true, nil
		case StatusOf(last) == 0, IsTransient(last):
			return false, nil
		default:
			return false, fmt.Errorf(
				"%s at %s answered its health check with HTTP %d rather than becoming healthy — "+
					"that is a reachable service saying no, so check the address before the service: %w",
				subject, baseURL, StatusOf(last), last)
		}
	},
		WithDescription("%s at %s to answer its health endpoint", subject, baseURL),
		WithDiagnostics(func() string {
			if last == nil {
				return ""
			}
			return "last health probe: " + last.Error()
		}))
}

// ConsumerHealth reads the AppView's /health/consumers document: per-consumer
// connection state, cursor positions and dead-letter backlog.
//
// This is the endpoint the pipeline tier attaches to its timeouts. "The record
// never appeared" and "the consumer that indexes that record has been
// disconnected for four minutes with 12 dead letters" are the same failure, and
// only the second one can be acted on.
func (a *AppView) ConsumerHealth(ctx context.Context) (ConsumerHealthReport, error) {
	var report ConsumerHealthReport
	err := a.Get(ctx, "/health/consumers", &report)
	return report, err
}

// ConsumerHealthReport mirrors cmd/server's /health/consumers response.
//
// Only the fields a failing test would want to read are modelled; the endpoint
// is a diagnostic surface, not a contract, and an unmodelled field costs a line
// of a failure message rather than a wrong result.
type ConsumerHealthReport struct {
	Status                   string          `json:"status"` // "ok", "degraded", "stalled"
	DeadLetterBacklogUnknown bool            `json:"deadLetterBacklogUnknown"`
	Consumers                []ConsumerState `json:"consumers"`
}

// ConsumerState is one consumer's entry in a ConsumerHealthReport.
type ConsumerState struct {
	Name                string `json:"name"`
	Connected           bool   `json:"connected"`
	CursorTimeUS        int64  `json:"cursorTimeUs"`
	EventsProcessed     uint64 `json:"eventsProcessed"`
	EventsDeadLettered  uint64 `json:"eventsDeadLettered"`
	DeadLetterBacklog   int64  `json:"deadLetterBacklog"`
	LastEventAgeSeconds *int64 `json:"lastEventAgeSeconds,omitempty"`
}

// String renders a report compactly, for failure messages.
func (r ConsumerHealthReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "consumers: %s", r.Status)
	if r.DeadLetterBacklogUnknown {
		b.WriteString(" (dead-letter backlog uncountable)")
	}
	for _, c := range r.Consumers {
		fmt.Fprintf(&b, "\n  %s: connected=%t cursor=%d processed=%d deadLettered=%d backlog=%d",
			c.Name, c.Connected, c.CursorTimeUS, c.EventsProcessed, c.EventsDeadLettered, c.DeadLetterBacklog)
	}
	return b.String()
}

// WithConsumerHealth snapshots the AppView's consumer health into a wait's
// failure message, and only into the failure message.
//
// This is docs/TEST_ARCHITECTURE.md §3.3's promise that a T2 timeout arrives
// with cursor positions and dead-letter counts attached:
//
//	testkit.WaitFor(t, 30*time.Second, probe,
//	        testkit.WithDescription("the post to be indexed"),
//	        testkit.WithConsumerHealth(appview))
func WithConsumerHealth(a *AppView) WaitOption {
	return WithDiagnostics(func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		report, err := a.ConsumerHealth(ctx)
		if err != nil {
			// Best effort: this runs on a path that is already failing, and
			// must not fail differently. Saying why it could not be read is
			// still worth a line.
			return "consumer health unavailable: " + err.Error()
		}
		return report.String()
	})
}
