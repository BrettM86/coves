package web

import (
	"bytes"
	"net/http/httptest"
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
	if !bytes.Contains([]byte(body), []byte("https://example.com/appstore")) {
		t.Error("Rendered output does not contain App Store URL")
	}
	if !bytes.Contains([]byte(body), []byte("https://example.com/playstore")) {
		t.Error("Rendered output does not contain Play Store URL")
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
	if !bytes.Contains([]byte(body), []byte("Sign in with Bluesky")) {
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
