package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestWebCompliancePagesSupportHEAD(t *testing.T) {
	t.Parallel()
	router := chi.NewRouter()
	RegisterWebRoutes(router, nil, nil, "")
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	for _, path := range []string{"/privacy", "/delete-account", "/delete-account/success", "/safety/child-safety"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			var contentType string
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				request, err := http.NewRequest(method, server.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if response.StatusCode != http.StatusOK {
					t.Fatalf("%s %s: got %d, want 200; body: %s", method, path, response.StatusCode, body)
				}
				if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
					t.Errorf("%s: expected HTML, got %q", method, response.Header.Get("Content-Type"))
				}
				if method == http.MethodGet {
					contentType = response.Header.Get("Content-Type")
					if !strings.Contains(strings.ToLower(string(body)), "<!doctype html>") {
						t.Error("GET must serve the real public HTML page")
					}
				} else {
					if len(body) != 0 {
						t.Errorf("HEAD returned %d body bytes", len(body))
					}
					if response.Header.Get("Content-Type") != contentType {
						t.Error("HEAD Content-Type differs from GET")
					}
				}
			}
		})
	}
}

func TestWebCompliancePOSTMethodsRemainRestricted(t *testing.T) {
	t.Parallel()
	router := chi.NewRouter()
	RegisterWebRoutes(router, nil, nil, "")
	for _, path := range []string{"/privacy", "/safety/child-safety", "/delete-account/success"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: got %d, want 405", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/delete-account", nil))
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/delete-account" {
		t.Errorf("unauthenticated deletion must retain its login guard; got %d, Location %q", response.Code, response.Header().Get("Location"))
	}
}
