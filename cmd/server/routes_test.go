package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/reqbody"
)

// TestNewRouter_GlobalBodyCapIsWired proves the RequestSize backstop is in
// the middleware chain and ordered ahead of handlers: a body over
// reqbody.LimitGlobal must fail the handler's read with *http.MaxBytesError
// even though the probe handler applies no cap of its own. Without the
// middleware, the probe would read the whole body and answer 200 — chi's
// RequestSize never writes a response itself, so observing the read error is
// the only way to see it from a handler.
func TestNewRouter_GlobalBodyCapIsWired(t *testing.T) {
	r := newRouter(nil)

	r.Post("/probe", func(w http.ResponseWriter, req *http.Request) {
		_, err := io.Copy(io.Discard, req.Body)
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	t.Run("over the global cap is refused", func(t *testing.T) {
		body := strings.Repeat("a", int(reqbody.LimitGlobal)+1)
		req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 — the RequestSize backstop is not in the chain", rec.Code)
		}
	})

	t.Run("under the global cap passes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader("small body"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}
