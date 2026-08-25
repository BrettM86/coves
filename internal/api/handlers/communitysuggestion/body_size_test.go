package communitysuggestion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
)

// The four suggestion endpoints share the small tier and the shared decode
// path. These tests pin the rejection contract for the whole package — until
// now nothing here was tested at any tier. All four handlers reject at the
// decode step, before the service is touched: the handlers below are built
// with a nil service, so any regression in that ordering panics.

// suggestionBodyCase drives one handler with one raw body and asserts status.
type suggestionBodyCase struct {
	handler http.HandlerFunc
	name    string
	path    string
}

func suggestionHandlers() []suggestionBodyCase {
	adminDID := "did:plc:admin"
	create := NewCreateHandler(nil)
	vote := NewVoteHandler(nil)
	status := NewUpdateStatusHandler(nil, []string{adminDID})
	return []suggestionBodyCase{
		{name: "create", path: "/xrpc/social.coves.community.suggestion.create", handler: create.HandleCreate},
		{name: "vote", path: "/xrpc/social.coves.community.suggestion.vote", handler: vote.HandleVote},
		{name: "removeVote", path: "/xrpc/social.coves.community.suggestion.removeVote", handler: vote.HandleRemoveVote},
		{name: "updateStatus", path: "/xrpc/social.coves.community.suggestion.updateStatus", handler: status.HandleUpdateStatus},
	}
}

func postSuggestion(t *testing.T, tc suggestionBodyCase, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// The admin DID passes updateStatus's authorization gate, which runs
	// before the decode; the other handlers just need any authenticated DID.
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, "did:plc:admin"))
	rec := httptest.NewRecorder()
	tc.handler(rec, req)
	return rec
}

func TestSuggestionHandlers_OversizedBodyReturns413(t *testing.T) {
	t.Parallel()
	oversized := `{"note":"` + strings.Repeat("a", int(reqbody.LimitSmall)) + `"}`

	for _, tc := range suggestionHandlers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := postSuggestion(t, tc, oversized)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413 (body: %.200s)", rec.Code, rec.Body.String())
			}
			var resp struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error != "PayloadTooLarge" {
				t.Fatalf("error code = %q (unmarshal err %v), want PayloadTooLarge", resp.Error, err)
			}
		})
	}
}

func TestSuggestionHandlers_MalformedBodyReturns400(t *testing.T) {
	t.Parallel()
	for _, tc := range suggestionHandlers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := postSuggestion(t, tc, `{not json`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %.200s)", rec.Code, rec.Body.String())
			}
		})
	}
}
