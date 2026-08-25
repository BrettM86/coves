package xrpc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/reqbody"
)

func TestDecodeJSON_ValidBodyReturnsTrue(t *testing.T) {
	r := httptest.NewRequest("POST", "/xrpc/test", strings.NewReader(`{"name":"a"}`))
	w := httptest.NewRecorder()

	var dst struct {
		Name string `json:"name"`
	}
	if !DecodeJSON(w, r, reqbody.LimitTiny, &dst) {
		t.Fatalf("DecodeJSON returned false for valid body; response: %s", w.Body.String())
	}
	if dst.Name != "a" {
		t.Fatalf("decoded Name = %q, want %q", dst.Name, "a")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("no response must be written on success, got status %d", w.Code)
	}
}

func TestDecodeJSON_OverLimitWrites413(t *testing.T) {
	body := `{"name":"` + strings.Repeat("a", int(reqbody.LimitTiny)) + `"}`
	r := httptest.NewRequest("POST", "/xrpc/test", strings.NewReader(body))
	w := httptest.NewRecorder()

	var dst struct{}
	if DecodeJSON(w, r, reqbody.LimitTiny, &dst) {
		t.Fatal("DecodeJSON returned true for oversized body")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	var resp Error
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("413 body is not JSON: %v", err)
	}
	if resp.Error != "PayloadTooLarge" {
		t.Fatalf("error code = %q, want PayloadTooLarge", resp.Error)
	}
}

func TestDecodeJSON_MalformedWrites400(t *testing.T) {
	r := httptest.NewRequest("POST", "/xrpc/test", strings.NewReader(`{"name":`))
	w := httptest.NewRecorder()

	var dst struct{}
	if DecodeJSON(w, r, reqbody.LimitTiny, &dst) {
		t.Fatal("DecodeJSON returned true for malformed body")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var resp Error
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("400 body is not JSON: %v", err)
	}
	if resp.Error != "InvalidRequest" {
		t.Fatalf("error code = %q, want InvalidRequest", resp.Error)
	}
}
