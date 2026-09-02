package imageproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/imageproxy"
)

// Valid test constants that pass validation
const (
	// validTestDID is a valid did:plc identifier (24 lowercase base32 chars after did:plc:)
	validTestDID = "did:plc:z72i7hdynmk6r22z27h6tvur"
	// validTestCID is a valid CIDv1 base32 identifier
	validTestCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
)

// mockService implements imageproxy.Service for testing
type mockService struct {
	getImageFunc func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error)
}

func (m *mockService) GetImage(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
	if m.getImageFunc != nil {
		return m.getImageFunc(ctx, preset, did, cid, pdsURL)
	}
	return nil, errors.New("not implemented")
}

// mockIdentityResolver implements identity.Resolver for testing
type mockIdentityResolver struct {
	resolveFunc    func(ctx context.Context, identifier string) (*identity.Identity, error)
	resolveDIDFunc func(ctx context.Context, did string) (*identity.DIDDocument, error)
}

func (m *mockIdentityResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	if m.resolveFunc != nil {
		return m.resolveFunc(ctx, identifier)
	}
	return nil, errors.New("not implemented")
}

func (m *mockIdentityResolver) ResolveHandle(ctx context.Context, handle string) (did, pdsURL string, err error) {
	return "", "", errors.New("not implemented")
}

func (m *mockIdentityResolver) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	if m.resolveDIDFunc != nil {
		return m.resolveDIDFunc(ctx, did)
	}
	return nil, errors.New("not implemented")
}

func (m *mockIdentityResolver) Purge(ctx context.Context, identifier string) error {
	return nil
}

// createTestRequest creates an HTTP request with chi URL params
func createTestRequest(method, path string, params map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestHandler_HandleImage_Success(t *testing.T) {
	expectedImage := []byte{0xFF, 0xD8, 0xFF, 0xE0} // JPEG magic bytes
	testPDSURL := "https://pds.example.com"
	testPreset := "avatar"

	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			if preset != testPreset {
				t.Errorf("Expected preset %q, got %q", testPreset, preset)
			}
			if did != validTestDID {
				t.Errorf("Expected DID %q, got %q", validTestDID, did)
			}
			if cid != validTestCID {
				t.Errorf("Expected CID %q, got %q", validTestCID, cid)
			}
			if pdsURL != testPDSURL {
				t.Errorf("Expected PDS URL %q, got %q", testPDSURL, pdsURL)
			}
			return expectedImage, nil
		},
	}

	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{
					{
						ID:              "#atproto_pds",
						Type:            "AtprotoPersonalDataServer",
						ServiceEndpoint: testPDSURL,
					},
				},
			}, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": testPreset,
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Verify Content-Type
	contentType := w.Header().Get("Content-Type")
	if contentType != "image/jpeg" {
		t.Errorf("Expected Content-Type image/jpeg, got %s", contentType)
	}

	// Verify Cache-Control
	cacheControl := w.Header().Get("Cache-Control")
	expectedCacheControl := "public, max-age=31536000, immutable"
	if cacheControl != expectedCacheControl {
		t.Errorf("Expected Cache-Control %q, got %q", expectedCacheControl, cacheControl)
	}

	// Verify ETag format
	etag := w.Header().Get("ETag")
	expectedETag := `"avatar-` + validTestCID + `"`
	if etag != expectedETag {
		t.Errorf("Expected ETag %q, got %q", expectedETag, etag)
	}

	// Verify body
	if w.Body.Len() != len(expectedImage) {
		t.Errorf("Expected body length %d, got %d", len(expectedImage), w.Body.Len())
	}
}

func TestHandler_HandleImage_ETagMatch_Returns304(t *testing.T) {
	testPreset := "avatar"

	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			t.Error("Service should not be called when ETag matches")
			return nil, nil
		},
	}

	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			t.Error("Resolver should not be called when ETag matches")
			return nil, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": testPreset,
		"did":    validTestDID,
		"cid":    validTestCID,
	})
	// Set If-None-Match header with matching ETag
	req.Header.Set("If-None-Match", `"avatar-`+validTestCID+`"`)

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusNotModified {
		t.Errorf("Expected status 304, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Verify no body in 304 response
	if w.Body.Len() != 0 {
		t.Errorf("Expected empty body for 304 response, got %d bytes", w.Body.Len())
	}
}

func TestHandler_HandleImage_ETagMismatch_ReturnsImage(t *testing.T) {
	expectedImage := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	testPreset := "avatar"
	testPDSURL := "https://pds.example.com"

	serviceCalled := false
	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			serviceCalled = true
			return expectedImage, nil
		},
	}

	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{
					{
						ID:              "#atproto_pds",
						Type:            "AtprotoPersonalDataServer",
						ServiceEndpoint: testPDSURL,
					},
				},
			}, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": testPreset,
		"did":    validTestDID,
		"cid":    validTestCID,
	})
	// Set If-None-Match header with different ETag
	req.Header.Set("If-None-Match", `"other-preset-somecid"`)

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	if !serviceCalled {
		t.Error("Service should have been called when ETag doesn't match")
	}
}

func TestHandler_HandleImage_InvalidPreset_Returns400(t *testing.T) {
	mockSvc := &mockService{}
	mockResolver := &mockIdentityResolver{}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/invalid_preset/plain/did:plc:test/somecid", map[string]string{
		"preset": "invalid_preset",
		"did":    "did:plc:test",
		"cid":    "somecid",
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Verify error response contains error info
	body := w.Body.String()
	if body == "" {
		t.Error("Expected error message in response body")
	}
}

func TestHandler_HandleImage_DIDResolutionFailed_Returns502(t *testing.T) {
	mockSvc := &mockService{}
	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return nil, errors.New("failed to resolve DID")
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected status 502, got %d. Body: %s", w.Code, w.Body.String())
	}
}

func TestHandler_HandleImage_BlobNotFound_Returns404(t *testing.T) {
	testPDSURL := "https://pds.example.com"

	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			return nil, imageproxy.ErrPDSNotFound
		},
	}

	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{
					{
						ID:              "#atproto_pds",
						Type:            "AtprotoPersonalDataServer",
						ServiceEndpoint: testPDSURL,
					},
				},
			}, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d. Body: %s", w.Code, w.Body.String())
	}
}

func TestHandler_HandleImage_Timeout_Returns504(t *testing.T) {
	testPDSURL := "https://pds.example.com"

	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			return nil, imageproxy.ErrPDSTimeout
		},
	}

	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{
					{
						ID:              "#atproto_pds",
						Type:            "AtprotoPersonalDataServer",
						ServiceEndpoint: testPDSURL,
					},
				},
			}, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("Expected status 504, got %d. Body: %s", w.Code, w.Body.String())
	}
}

func TestHandler_HandleImage_InternalError_Returns500(t *testing.T) {
	testPDSURL := "https://pds.example.com"

	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			return nil, errors.New("unexpected internal error")
		},
	}

	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{
					{
						ID:              "#atproto_pds",
						Type:            "AtprotoPersonalDataServer",
						ServiceEndpoint: testPDSURL,
					},
				},
			}, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500, got %d. Body: %s", w.Code, w.Body.String())
	}
}

func TestHandler_HandleImage_PDSFetchFailed_Returns502(t *testing.T) {
	testPDSURL := "https://pds.example.com"

	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			return nil, imageproxy.ErrPDSFetchFailed
		},
	}

	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{
					{
						ID:              "#atproto_pds",
						Type:            "AtprotoPersonalDataServer",
						ServiceEndpoint: testPDSURL,
					},
				},
			}, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected status 502, got %d. Body: %s", w.Code, w.Body.String())
	}
}

func TestHandler_HandleImage_MissingParams(t *testing.T) {
	mockSvc := &mockService{}
	mockResolver := &mockIdentityResolver{}

	handler := NewHandler(mockSvc, mockResolver)

	tests := []struct {
		name   string
		params map[string]string
	}{
		{
			name:   "missing preset",
			params: map[string]string{"did": "did:plc:test", "cid": "somecid"},
		},
		{
			name:   "missing did",
			params: map[string]string{"preset": "avatar", "cid": "somecid"},
		},
		{
			name:   "missing cid",
			params: map[string]string{"preset": "avatar", "did": "did:plc:test"},
		},
		{
			name:   "empty preset",
			params: map[string]string{"preset": "", "did": "did:plc:test", "cid": "somecid"},
		},
		{
			name:   "empty did",
			params: map[string]string{"preset": "avatar", "did": "", "cid": "somecid"},
		},
		{
			name:   "empty cid",
			params: map[string]string{"preset": "avatar", "did": "did:plc:test", "cid": ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := createTestRequest(http.MethodGet, "/img/test/plain/did:plc:test/cid", tc.params)

			w := httptest.NewRecorder()
			handler.HandleImage(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandler_HandleImage_AllPresets(t *testing.T) {
	expectedImage := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	testPDSURL := "https://pds.example.com"

	// Test all valid presets
	validPresets := []string{"avatar", "avatar_small", "banner", "content_preview", "content_full", "embed_thumbnail"}

	for _, preset := range validPresets {
		t.Run(preset, func(t *testing.T) {
			mockSvc := &mockService{
				getImageFunc: func(ctx context.Context, p, did, cid, pdsURL string) ([]byte, error) {
					if p != preset {
						t.Errorf("Expected preset %q, got %q", preset, p)
					}
					return expectedImage, nil
				},
			}

			mockResolver := &mockIdentityResolver{
				resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
					return &identity.DIDDocument{
						DID: did,
						Service: []identity.Service{
							{
								ID:              "#atproto_pds",
								Type:            "AtprotoPersonalDataServer",
								ServiceEndpoint: testPDSURL,
							},
						},
					}, nil
				},
			}

			handler := NewHandler(mockSvc, mockResolver)

			req := createTestRequest(http.MethodGet, "/img/"+preset+"/plain/"+validTestDID+"/"+validTestCID, map[string]string{
				"preset": preset,
				"did":    validTestDID,
				"cid":    validTestCID,
			})

			w := httptest.NewRecorder()
			handler.HandleImage(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200 for preset %q, got %d. Body: %s", preset, w.Code, w.Body.String())
			}

			// Verify ETag matches preset
			etag := w.Header().Get("ETag")
			expectedETag := `"` + preset + `-` + validTestCID + `"`
			if etag != expectedETag {
				t.Errorf("Expected ETag %q, got %q", expectedETag, etag)
			}
		})
	}
}

func TestHandler_HandleImage_NoPDSEndpoint_Returns502(t *testing.T) {
	mockSvc := &mockService{}
	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			// Return document without PDS service
			return &identity.DIDDocument{
				DID:     did,
				Service: []identity.Service{},
			}, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected status 502, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TestHandler_HandleImage_InvalidDID tests that invalid DIDs are rejected
// Note: We use Indigo's syntax.ParseDID for validation consistency with the codebase.
// Some DIDs that look "wrong" (like did:plc:abc) are actually valid per Indigo's parser.
func TestHandler_HandleImage_InvalidDID(t *testing.T) {
	mockSvc := &mockService{}
	mockResolver := &mockIdentityResolver{}

	handler := NewHandler(mockSvc, mockResolver)

	// These DIDs are invalid per Indigo's syntax.ParseDID (or fail our security checks)
	// Note: null bytes can't be tested at HTTP layer - Go's HTTP library rejects them first
	invalidDIDs := []struct {
		name string
		did  string
	}{
		{"missing method", "did:abc123"},
		{"path traversal", "did:plc:../../../etc/passwd"},
		{"not a DID", "notadid"},
		{"forward slash", "did:plc:abc/def"},
		{"backslash", "did:plc:abc\\def"},
		{"empty string", ""},
	}

	for _, tc := range invalidDIDs {
		t.Run(tc.name, func(t *testing.T) {
			req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+tc.did+"/"+validTestCID, map[string]string{
				"preset": "avatar",
				"did":    tc.did,
				"cid":    validTestCID,
			})

			w := httptest.NewRecorder()
			handler.HandleImage(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("Expected status 400 for invalid DID %q, got %d. Body: %s", tc.did, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandler_HandleImage_InvalidCID tests that invalid CIDs are rejected
func TestHandler_HandleImage_InvalidCID(t *testing.T) {
	mockSvc := &mockService{}
	mockResolver := &mockIdentityResolver{}

	handler := NewHandler(mockSvc, mockResolver)

	invalidCIDs := []struct {
		name string
		cid  string
	}{
		{"too short", "bafyabc"},
		{"path traversal", "../../../etc/passwd"},
		{"contains slash", "bafy/path/to/file"},
		{"random string", "this_is_not_a_cid"},
	}

	for _, tc := range invalidCIDs {
		t.Run(tc.name, func(t *testing.T) {
			req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+tc.cid, map[string]string{
				"preset": "avatar",
				"did":    validTestDID,
				"cid":    tc.cid,
			})

			w := httptest.NewRecorder()
			handler.HandleImage(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("Expected status 400 for invalid CID %q, got %d. Body: %s", tc.cid, w.Code, w.Body.String())
			}
		})
	}
}

// This route sits behind a CDN and advertises a one-year immutable lifetime on
// success, which is correct for content-addressed blobs. Inheriting anything
// cacheable on an error would pin a transient failure — a PDS timeout, a DID
// that had not propagated yet — at the edge long after the image became
// fetchable. Every error path must therefore say no-store.
func TestHandler_HandleImage_ErrorsAreNeverCacheable(t *testing.T) {
	tests := []struct {
		name       string
		params     map[string]string
		service    *mockService
		resolver   *mockIdentityResolver
		wantStatus int
	}{
		{
			name:       "invalid preset",
			params:     map[string]string{"preset": "nope", "did": validTestDID, "cid": validTestCID},
			service:    &mockService{},
			resolver:   &mockIdentityResolver{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid DID",
			params:     map[string]string{"preset": "avatar", "did": "not-a-did", "cid": validTestCID},
			service:    &mockService{},
			resolver:   &mockIdentityResolver{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:    "DID resolution failure",
			params:  map[string]string{"preset": "avatar", "did": validTestDID, "cid": validTestCID},
			service: &mockService{},
			resolver: &mockIdentityResolver{
				resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
					return nil, errors.New("transient PLC failure")
				},
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name:   "blob not found",
			params: map[string]string{"preset": "avatar", "did": validTestDID, "cid": validTestCID},
			service: &mockService{
				getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
					return nil, imageproxy.ErrPDSNotFound
				},
			},
			resolver:   resolverForPDS("https://pds.example.com"),
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "PDS timeout",
			params: map[string]string{"preset": "avatar", "did": validTestDID, "cid": validTestCID},
			service: &mockService{
				getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
					return nil, imageproxy.ErrPDSTimeout
				},
			},
			resolver:   resolverForPDS("https://pds.example.com"),
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:   "processing failure",
			params: map[string]string{"preset": "avatar", "did": validTestDID, "cid": validTestCID},
			service: &mockService{
				getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
					return nil, imageproxy.ErrProcessingFailed
				},
			},
			resolver:   resolverForPDS("https://pds.example.com"),
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:   "processor busy",
			params: map[string]string{"preset": "avatar", "did": validTestDID, "cid": validTestCID},
			service: &mockService{
				getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
					return nil, imageproxy.ErrProcessorBusy
				},
			},
			resolver:   resolverForPDS("https://pds.example.com"),
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:   "too many pixels",
			params: map[string]string{"preset": "avatar", "did": validTestDID, "cid": validTestCID},
			service: &mockService{
				getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
					return nil, imageproxy.ErrImageTooManyPixels
				},
			},
			resolver:   resolverForPDS("https://pds.example.com"),
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(tt.service, tt.resolver)

			path := "/img/" + tt.params["preset"] + "/plain/" + tt.params["did"] + "/" + tt.params["cid"]
			w := httptest.NewRecorder()
			handler.HandleImage(w, createTestRequest(http.MethodGet, path, tt.params))

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d. Body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want %q so the CDN cannot cache this failure",
					got, "no-store")
			}
		})
	}
}

// resolverForPDS returns a resolver whose DID document advertises pdsURL as the
// repo's PDS, so a test can reach the service call rather than stopping at
// resolution.
func resolverForPDS(pdsURL string) *mockIdentityResolver {
	return &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{
					{
						ID:              "#atproto_pds",
						Type:            "AtprotoPersonalDataServer",
						ServiceEndpoint: pdsURL,
					},
				},
			}, nil
		},
	}
}

// TestHandler_HandleImage_ProcessorBusy_Returns503 pins the wire shape of load
// shedding. ErrProcessorBusy means every decode slot was taken for the whole
// queue wait; that is a capacity condition, not a fault in the request or the
// server, so it is a 503 with a Retry-After the CDN and clients can honour
// rather than a 500 that reads as a bug and invites an immediate retry storm.
// Retry-After is fixed at 5 seconds: long enough for in-flight decodes to
// drain, short enough that a real user's avatar still appears.
func TestHandler_HandleImage_ProcessorBusy_Returns503(t *testing.T) {
	testPDSURL := "https://pds.example.com"

	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			return nil, fmt.Errorf("%w: waited 5s", imageproxy.ErrProcessorBusy)
		},
	}

	mockResolver := &mockIdentityResolver{
		resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{
					{
						ID:              "#atproto_pds",
						Type:            "AtprotoPersonalDataServer",
						ServiceEndpoint: testPDSURL,
					},
				},
			}, nil
		},
	}

	handler := NewHandler(mockSvc, mockResolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})

	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d. Body: %s", w.Code, w.Body.String())
	}
	// Retry-After must track the queue wait the service actually enforced,
	// not a literal that can drift from it.
	wantRetryAfter := strconv.Itoa(int(imageproxy.DefaultProcessQueueWait / time.Second))
	if got := w.Header().Get("Retry-After"); got != wantRetryAfter {
		t.Errorf("Expected Retry-After %q (DefaultProcessQueueWait in seconds), got %q", wantRetryAfter, got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Expected Cache-Control %q, got %q", "no-store", got)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Expected Content-Type %q, got %q", "text/plain; charset=utf-8", got)
	}
	body := w.Body.String()
	if body == "" {
		t.Error("Expected a plain-text body explaining the refusal, got empty body")
	}
	if strings.Contains(body, "internal server error") {
		t.Errorf("Body must not present load shedding as an internal failure, got %q", body)
	}
}

// TestHandler_HandleImage_TooManyPixels_Returns400: a header that declares
// more pixels than the budget is the client's bytes, so it is a 400. The body
// is deliberately the same as the byte cap's: the two limits guard different
// resources internally, but a client is told only that the image is too large,
// and the distinction lives in the log line, not the response.
func TestHandler_HandleImage_TooManyPixels_Returns400(t *testing.T) {
	mockSvc := &mockService{
		getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
			return nil, fmt.Errorf("%w: 12000x12000 exceeds the 50-megapixel budget", imageproxy.ErrImageTooManyPixels)
		},
	}
	handler := NewHandler(mockSvc, resolverForPDS("https://pds.example.com"))

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})
	w := httptest.NewRecorder()
	handler.HandleImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "image too large" {
		t.Errorf("Expected body %q (same wording as the byte cap, on purpose), got %q", "image too large", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Expected Cache-Control %q, got %q", "no-store", got)
	}
}

// captureHandlerLogs routes the process-wide slog default into a buffer for
// the duration of the test. It restores the previous logger in Cleanup, and
// because the default logger is global the calling test must not be parallel.
func captureHandlerLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// logLinesAtLevel returns the captured lines carrying the given slog level.
func logLinesAtLevel(logs string, level string) []string {
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level="+level) {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestHandler_HandleImage_SecurityLogging pins what the handler writes to the
// log for the three outcomes an operator has to tell apart after the fact.
// A pixel-budget refusal is a security event: it is the signature of a
// decompression bomb, so it is logged at WARN with enough to find the repo and
// the blob (preset, DID, CID) and the declared size. A processing failure is
// our fault and is logged at ERROR with the underlying error. Load shedding is
// logged by the service that counted it, and a second line here would double
// every busy refusal in the log during exactly the burst that makes logs
// expensive.
func TestHandler_HandleImage_SecurityLogging(t *testing.T) {
	newHandlerReturning := func(err error) *Handler {
		return NewHandler(&mockService{
			getImageFunc: func(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error) {
				return nil, err
			},
		}, resolverForPDS("https://pds.example.com"))
	}
	newRequest := func() *http.Request {
		return createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
			"preset": "avatar",
			"did":    validTestDID,
			"cid":    validTestCID,
		})
	}

	t.Run("too many pixels is a WARN naming the blob and its declared size", func(t *testing.T) {
		logs := captureHandlerLogs(t)
		handler := newHandlerReturning(fmt.Errorf("%w: 12000x12000 exceeds the 50-megapixel budget", imageproxy.ErrImageTooManyPixels))

		handler.HandleImage(httptest.NewRecorder(), newRequest())

		warnings := logLinesAtLevel(logs.String(), "WARN")
		if len(warnings) == 0 {
			t.Fatalf("expected a WARN line for a pixel-budget refusal, got none. All output:\n%s", logs.String())
		}
		found := false
		for _, line := range warnings {
			if strings.Contains(line, "avatar") && strings.Contains(line, validTestDID) &&
				strings.Contains(line, validTestCID) && strings.Contains(line, "12000x12000") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a WARN line containing the preset, DID, CID and \"12000x12000\", got:\n%s", strings.Join(warnings, "\n"))
		}
	})

	t.Run("processing failure is an ERROR carrying the underlying error", func(t *testing.T) {
		logs := captureHandlerLogs(t)
		underlying := "failed to decode image: png: invalid format: bad filter"
		handler := newHandlerReturning(fmt.Errorf("%w: %s", imageproxy.ErrProcessingFailed, underlying))

		handler.HandleImage(httptest.NewRecorder(), newRequest())

		errorLines := logLinesAtLevel(logs.String(), "ERROR")
		if len(errorLines) == 0 {
			t.Fatalf("expected an ERROR line for a processing failure, got none. All output:\n%s", logs.String())
		}
		found := false
		for _, line := range errorLines {
			if strings.Contains(line, underlying) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected an ERROR line containing %q, got:\n%s", underlying, strings.Join(errorLines, "\n"))
		}
	})

	t.Run("processor busy produces no handler log line", func(t *testing.T) {
		logs := captureHandlerLogs(t)
		handler := newHandlerReturning(fmt.Errorf("%w: waited 5s", imageproxy.ErrProcessorBusy))

		handler.HandleImage(httptest.NewRecorder(), newRequest())

		if got := strings.TrimSpace(logs.String()); got != "" {
			t.Errorf("expected no handler log output for load shedding (the service logs and counts it), got:\n%s", got)
		}
	})
}
