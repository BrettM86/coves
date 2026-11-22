// Package cloudflaretest is test support for the Workers AI classifier: a
// recording http.RoundTripper that answers in-process (never dials) or
// forwards to another RoundTripper, and builders for canned Workers AI
// responses.
//
// coves:allow-bare-client-file: test-support recording transport; it answers in-process or forwards only to a caller-supplied RoundTripper (the live test passes the real client transport)
package cloudflaretest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// Request is one recorded outbound request.
type Request struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// Responder answers a recorded request in-process.
type Responder func(req *http.Request) (*http.Response, error)

// Transport records every request it receives. It either answers through its
// Responder or forwards the request unchanged to Next.
type Transport struct {
	mu       sync.Mutex
	requests []Request
	respond  Responder
	next     http.RoundTripper
	before   func(req *http.Request)
}

// NewTransport returns a recording transport that answers with respond and
// never opens a connection.
func NewTransport(respond Responder) *Transport {
	return &Transport{respond: respond}
}

// NewForwardingTransport returns a recording transport that forwards each
// request unchanged to next after recording it.
func NewForwardingTransport(next http.RoundTripper) *Transport {
	return &Transport{next: next}
}

// SetBeforeAnswer installs a hook run after a request is recorded and before
// it is answered or forwarded. The hook runs outside the transport's lock, so
// it may block or call Requests.
func (t *Transport) SetBeforeAnswer(hook func(req *http.Request)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.before = hook
}

// Client returns an http.Client that uses this transport.
func (t *Transport) Client() *http.Client {
	return &http.Client{Transport: t}
}

// Requests returns a copy of every request recorded so far, in arrival order.
func (t *Transport) Requests() []Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Request, len(t.requests))
	copy(out, t.requests)
	return out
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		read, err := io.ReadAll(req.Body) // coves:allow-raw-body-decode: test RoundTripper recording an outbound client request the code under test built, not an inbound handler body
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = read
	}
	recorded := Request{
		Method: req.Method,
		URL:    req.URL.String(),
		Header: req.Header.Clone(),
		Body:   append([]byte(nil), body...),
	}

	t.mu.Lock()
	t.requests = append(t.requests, recorded)
	hook, respond, next := t.before, t.respond, t.next
	t.mu.Unlock()

	if hook != nil {
		hook(req)
	}

	if next != nil {
		forwarded := req.Clone(req.Context())
		forwarded.Body = io.NopCloser(bytes.NewReader(body))
		forwarded.ContentLength = int64(len(body))
		forwarded.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		return next.RoundTrip(forwarded)
	}
	return respond(req)
}

// Response builds an in-process HTTP response for req with the given status
// and JSON body.
func Response(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// Status returns a Responder that always answers with status and body.
func Status(status int, body []byte) Responder {
	return func(req *http.Request) (*http.Response, error) {
		return Response(req, status, body), nil
	}
}

// Canned returns a Responder that always answers 200 with the envelope built
// by Envelope.
func Canned(probabilities map[string]float64, inputTokens int) Responder {
	return Status(http.StatusOK, Envelope(probabilities, inputTokens))
}

// Envelope builds a successful Cloudflare v4 envelope whose result answers
// each key with a noul probability:
//
//	{"result":{"model":"clef-flash","answers":{"<key>":{"type":"noul","noul":p}},
//	 "usage":{"input_tokens":n,"output_tokens":6}},"success":true,"errors":[],"messages":[]}
func Envelope(probabilities map[string]float64, inputTokens int) []byte {
	answers := make(map[string]any, len(probabilities))
	for key, probability := range probabilities {
		answers[key] = map[string]any{"type": "noul", "noul": probability}
	}
	return marshalEnvelope(map[string]any{
		"model":   "clef-flash",
		"answers": answers,
		"usage":   map[string]any{"input_tokens": inputTokens, "output_tokens": 6},
	}, true)
}

// EnvelopeWithAnswers builds a Cloudflare v4 envelope with raw answers, for
// malformed-answer cases (a missing noul, a null noul).
func EnvelopeWithAnswers(answers map[string]any, inputTokens int, success bool) []byte {
	return marshalEnvelope(map[string]any{
		"model":   "clef-flash",
		"answers": answers,
		"usage":   map[string]any{"input_tokens": inputTokens, "output_tokens": 6},
	}, success)
}

func marshalEnvelope(result map[string]any, success bool) []byte {
	encoded, err := json.Marshal(map[string]any{
		"result":   result,
		"success":  success,
		"errors":   []any{},
		"messages": []any{},
	})
	if err != nil {
		panic(err)
	}
	return encoded
}
