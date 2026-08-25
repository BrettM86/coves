package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/reqbody"
)

// /oauth/refresh is unauthenticated by design — the sealed token in the body
// IS the credential — so the body size cap is the tightest bound on what a
// stranger can make this handler buffer. Both rejections must happen at the
// decode step. The oversized case in particular proves the guard runs first:
// its body carries a non-empty sealed_token, so the zero-value OAuthHandler
// below would nil-panic at the first h.client dereference if the payload ever
// got past the cap. (The malformed case leaves the struct zero, so it would
// exit on the empty-token 401 instead — the assertion still discriminates,
// but by status, not by panic.)

func TestHandleRefresh_OversizedBodyIsRefusedBeforeParsing(t *testing.T) {
	h := &OAuthHandler{}

	body := `{"sealed_token":"` + strings.Repeat("a", int(reqbody.LimitSmall)) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/oauth/refresh", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.HandleRefresh(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleRefresh_MalformedBodyIsRefused(t *testing.T) {
	h := &OAuthHandler{}

	r := httptest.NewRequest(http.MethodPost, "/oauth/refresh", strings.NewReader(`{"did":`))
	w := httptest.NewRecorder()

	h.HandleRefresh(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}
