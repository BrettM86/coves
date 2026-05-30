package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewTemplates(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}
	if templates == nil {
		t.Fatal("NewTemplates() returned nil")
	}
}

func TestTemplatesRender_LandingPage(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}

	data := LandingPageData{
		Title:        "Test Title",
		Description:  "Test Description",
		AppStoreURL:  "https://example.com/appstore",
		PlayStoreURL: "https://example.com/playstore",
	}

	w := httptest.NewRecorder()
	err = templates.Render(w, "landing.html", data)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := w.Body.String()

	// Check that key elements are present
	if !bytes.Contains([]byte(body), []byte("Test Title")) {
		t.Error("Rendered output does not contain title")
	}
	if !bytes.Contains([]byte(body), []byte("Test Description")) {
		t.Error("Rendered output does not contain description")
	}
	if !bytes.Contains([]byte(body), []byte("App Store")) {
		t.Error("Rendered output does not contain App Store text")
	}
	if !bytes.Contains([]byte(body), []byte("Google Play")) {
		t.Error("Rendered output does not contain Google Play text")
	}
	if !bytes.Contains([]byte(body), []byte(data.AppStoreURL)) {
		t.Error("Rendered output does not link to App Store URL")
	}
	if !bytes.Contains([]byte(body), []byte(data.PlayStoreURL)) {
		t.Error("Rendered output does not link to Play Store URL")
	}
	if !bytes.Contains([]byte(body), []byte("/static/images/lil_dude.png")) {
		t.Error("Rendered output does not contain mascot image path")
	}
}

func TestTemplatesRender_DeleteAccount(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}

	// Test logged out state
	data := DeleteAccountPageData{
		LoggedIn: false,
	}

	w := httptest.NewRecorder()
	err = templates.Render(w, "delete_account.html", data)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := w.Body.String()
	if !bytes.Contains([]byte(body), []byte("Sign In")) {
		t.Error("Logged out state does not show sign in button")
	}

	// Test logged in state
	dataLoggedIn := DeleteAccountPageData{
		LoggedIn: true,
		Handle:   "testuser.bsky.social",
		DID:      "did:plc:test123",
	}

	w2 := httptest.NewRecorder()
	err = templates.Render(w2, "delete_account.html", dataLoggedIn)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body2 := w2.Body.String()
	if !bytes.Contains([]byte(body2), []byte("@testuser.bsky.social")) {
		t.Error("Logged in state does not show user handle")
	}
	if !bytes.Contains([]byte(body2), []byte("Delete My Account")) {
		t.Error("Logged in state does not show delete button")
	}
}

func TestTemplatesRender_DeleteSuccess(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}

	w := httptest.NewRecorder()
	err = templates.Render(w, "delete_success.html", nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := w.Body.String()
	if !bytes.Contains([]byte(body), []byte("Account Deleted")) {
		t.Error("Success page does not contain confirmation message")
	}
	if !bytes.Contains([]byte(body), []byte("Return to Homepage")) {
		t.Error("Success page does not contain return link")
	}
}

func TestTemplatesRender_NotFound(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}

	w := httptest.NewRecorder()
	err = templates.Render(w, "nonexistent.html", nil)
	if err == nil {
		t.Fatal("Render() should return error for nonexistent template")
	}
}

func TestTemplatesRender_Turnstile(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}

	data := TurnstilePageData{SiteKey: "0x4AAAAAAA_TEST_SITE_KEY"}

	w := httptest.NewRecorder()
	if err := templates.Render(w, "turnstile.html", data); err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := w.Body.String()
	if !strings.Contains(body, `data-sitekey="0x4AAAAAAA_TEST_SITE_KEY"`) {
		t.Error("rendered output does not embed site key into data-sitekey attribute")
	}
	if !strings.Contains(body, "https://challenges.cloudflare.com/turnstile/v0/api.js") {
		t.Error("rendered output does not include Cloudflare Turnstile JS")
	}
	if !strings.Contains(body, "window.Turnstile.postMessage") {
		t.Error("rendered output does not call window.Turnstile.postMessage")
	}
	if !strings.Contains(body, `data-callback="onTurnstileSuccess"`) {
		t.Error("rendered output does not bind the success callback")
	}
}

func TestTemplatesRender_TurnstileEscapesSiteKey(t *testing.T) {
	// Site keys are operator-controlled, but treat them as untrusted defense-in-depth.
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}

	data := TurnstilePageData{SiteKey: `"><script>alert(1)</script>`}

	w := httptest.NewRecorder()
	if err := templates.Render(w, "turnstile.html", data); err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := w.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("template did not escape site key — XSS via TURNSTILE_SITE_KEY would be possible")
	}
}

func TestTurnstileHandler_MissingSiteKeyReturns503(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}
	h := NewHandlers(templates, nil, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/m/turnstile.html", nil)
	w := httptest.NewRecorder()
	h.TurnstileHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when TURNSTILE_SITE_KEY is empty, got %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control: no-store on 503, got %q", cc)
	}
}

func TestTurnstileHandler_RendersWithCSP(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}
	h := NewHandlers(templates, nil, nil, "0x4AAAAAAA_REAL_LOOKING_KEY")

	req := httptest.NewRequest(http.MethodGet, "/m/turnstile.html", nil)
	w := httptest.NewRecorder()
	h.TurnstileHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("expected Content-Security-Policy header")
	}
	if !strings.Contains(csp, "https://challenges.cloudflare.com") {
		t.Errorf("CSP does not allow Cloudflare challenges origin: %s", csp)
	}
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP does not deny by default: %s", csp)
	}

	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("expected X-Content-Type-Options: nosniff, got %q", got)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("expected Referrer-Policy: no-referrer, got %q", got)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=300") {
		t.Errorf("expected short cache lifetime, got %q", cc)
	}

	if !strings.Contains(w.Body.String(), "0x4AAAAAAA_REAL_LOOKING_KEY") {
		t.Error("rendered body does not embed the configured site key")
	}
}

func TestTemplatesRender_Privacy(t *testing.T) {
	templates, err := NewTemplates()
	if err != nil {
		t.Fatalf("NewTemplates() error = %v", err)
	}

	w := httptest.NewRecorder()
	err = templates.Render(w, "privacy.html", nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := w.Body.String()
	if !bytes.Contains([]byte(body), []byte("Privacy Policy")) {
		t.Error("Privacy page does not contain title")
	}
	if !bytes.Contains([]byte(body), []byte("Coves Team")) {
		t.Error("Privacy page does not contain company name")
	}
	if !bytes.Contains([]byte(body), []byte("support@coves.social")) {
		t.Error("Privacy page does not contain contact email")
	}
	if !bytes.Contains([]byte(body), []byte("atProto")) {
		t.Error("Privacy page does not mention atProto")
	}
	if !bytes.Contains([]byte(body), []byte("18 years of age or older")) {
		t.Error("Privacy page does not contain age requirement")
	}
}
