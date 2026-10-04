package testkit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type CloudflarePurgeRequest struct {
	Method, Path, Authorization string
	Files                       []string
	DecodeError                 error
}

type CloudflarePurgeEndpoint struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []CloudflarePurgeRequest
	status   int
	body     string
	hook     func([]string)
	scripted []cloudflarePurgeResponse
	holds    []cloudflarePurgeHold
}

type cloudflarePurgeResponse struct {
	status int
	body   string
}

type cloudflarePurgeHold struct {
	entered chan struct{}
	release chan struct{}
}

func NewCloudflarePurgeEndpoint(t *testing.T) *CloudflarePurgeEndpoint {
	t.Helper()
	endpoint := &CloudflarePurgeEndpoint{status: http.StatusOK, body: `{"success":true}`}
	endpoint.server = httptest.NewServer(http.HandlerFunc(endpoint.serve))
	t.Cleanup(endpoint.server.Close)
	return endpoint
}

func (endpoint *CloudflarePurgeEndpoint) URL() string { return endpoint.server.URL }

func (endpoint *CloudflarePurgeEndpoint) SetResponse(status int, body string) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	endpoint.status, endpoint.body = status, body
}

// QueueResponse scripts the next request, then falls back to SetResponse.
func (endpoint *CloudflarePurgeEndpoint) QueueResponse(status int, body string) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	endpoint.scripted = append(endpoint.scripted, cloudflarePurgeResponse{status, body})
}

// HoldNextRequest holds one request until release is called. Cleanup also
// releases it, before the endpoint's server is closed.
func (endpoint *CloudflarePurgeEndpoint) HoldNextRequest(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	hold := cloudflarePurgeHold{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(hold.release) }) }
	t.Cleanup(release)
	endpoint.mu.Lock()
	endpoint.holds = append(endpoint.holds, hold)
	endpoint.mu.Unlock()
	return hold.entered, release
}

func (endpoint *CloudflarePurgeEndpoint) SetHook(hook func([]string)) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	endpoint.hook = hook
}

func (endpoint *CloudflarePurgeEndpoint) Requests() []CloudflarePurgeRequest {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	requests := make([]CloudflarePurgeRequest, len(endpoint.requests))
	for i, request := range endpoint.requests {
		requests[i] = request
		requests[i].Files = append([]string(nil), request.Files...)
	}
	return requests
}

func (endpoint *CloudflarePurgeEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Files []string `json:"files"`
	}
	decodeError := json.NewDecoder(r.Body).Decode(&payload)
	files := append([]string(nil), payload.Files...)
	endpoint.mu.Lock()
	endpoint.requests = append(endpoint.requests, CloudflarePurgeRequest{
		Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"),
		Files: files, DecodeError: decodeError,
	})
	status, body, hook := endpoint.status, endpoint.body, endpoint.hook
	if len(endpoint.scripted) > 0 {
		status, body = endpoint.scripted[0].status, endpoint.scripted[0].body
		endpoint.scripted = endpoint.scripted[1:]
	}
	var hold cloudflarePurgeHold
	if len(endpoint.holds) > 0 {
		hold = endpoint.holds[0]
		endpoint.holds = endpoint.holds[1:]
	}
	endpoint.mu.Unlock()
	if hook != nil {
		hook(append([]string(nil), files...))
	}
	if hold.entered != nil {
		close(hold.entered)
		<-hold.release
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
