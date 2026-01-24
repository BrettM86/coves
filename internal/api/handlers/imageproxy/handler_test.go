package imageproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
