package imageproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/imageproxy"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func moderationImageRequest(etag bool) *http.Request {
	request := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar", "did": validTestDID, "cid": validTestCID,
	})
	if etag {
		request.Header.Set("If-None-Match", `"avatar-`+validTestCID+`"`)
	}
	return request
}

func TestHandler_ModerationErrorsDoNotDiscloseBlockedBlob(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"blocked blob", fmt.Errorf("%w: private decision", imageproxy.ErrBlobBlocked), http.StatusNotFound, "blob not found"},
		{"block lookup unavailable", fmt.Errorf("%w: database unavailable", imageproxy.ErrBlockCheckFailed), http.StatusServiceUnavailable, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &mockService{getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
				return nil, test.err
			}}
			response := httptest.NewRecorder()
			NewHandler(service, resolverForPDS("https://pds.example.com")).HandleImage(response, moderationImageRequest(false))
			assert.Equal(t, test.status, response.Code)
			if test.body != "" {
				assert.Equal(t, test.body, response.Body.String())
			} else {
				assert.NotEmpty(t, response.Body.String())
				assert.NotContains(t, response.Body.String(), "database unavailable")
			}
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Empty(t, response.Header().Get("ETag"))
		})
	}
}

func TestHandler_ConditionalRequestChecksBlockBeforeReturning304(t *testing.T) {
	for _, test := range []struct {
		name       string
		blocked    bool
		lookupErr  error
		wantStatus int
		wantBody   string
	}{
		{"blocked", true, nil, http.StatusNotFound, "blob not found"},
		{"unblocked", false, nil, http.StatusNotModified, ""},
		{"lookup failed", false, fmt.Errorf("lookup unavailable"), http.StatusServiceUnavailable, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			service := &mockService{
				isBlobBlockedFunc: func(_ context.Context, did, cid string) (bool, error) {
					calls++
					assert.Equal(t, validTestDID, did)
					assert.Equal(t, validTestCID, cid)
					return test.blocked, test.lookupErr
				},
				getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
					t.Error("conditional request must not fetch image data")
					return nil, nil
				},
			}
			resolver := &mockIdentityResolver{resolveDIDFunc: func(context.Context, string) (*identity.DIDDocument, error) {
				t.Error("conditional request must not resolve a PDS")
				return nil, nil
			}}
			response := httptest.NewRecorder()
			NewHandler(service, resolver).HandleImage(response, moderationImageRequest(true))
			assert.Equal(t, 1, calls, "If-None-Match must consult the block checker")
			assert.Equal(t, test.wantStatus, response.Code)
			if test.wantBody != "" || test.wantStatus == http.StatusNotModified {
				assert.Equal(t, test.wantBody, response.Body.String())
			} else {
				assert.NotEmpty(t, response.Body.String())
				assert.NotContains(t, response.Body.String(), "lookup unavailable")
			}
			if test.wantStatus != http.StatusNotModified {
				require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			}
		})
	}
}

// Every multibase encoding of a CID names the same blob on the PDS, so the
// handler must hand the block check, the ETag and the fetch one canonical form.
func TestHandler_CanonicalizesCIDBeforeBlockCheckAndFetch(t *testing.T) {
	parsed, err := cid.Decode(validTestCID)
	require.NoError(t, err)
	require.Equal(t, validTestCID, parsed.String())
	alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)

	t.Run("fetch and ETag use the canonical CID", func(t *testing.T) {
		var fetched string
		service := &mockService{getImageFunc: func(_ context.Context, _, _, requested, _ string) ([]byte, error) {
			fetched = requested
			return []byte("image"), nil
		}}
		response := httptest.NewRecorder()
		NewHandler(service, resolverForPDS("https://pds.example.com")).HandleImage(response, createTestRequest(http.MethodGet,
			"/img/avatar/plain/"+validTestDID+"/"+alias, map[string]string{"preset": "avatar", "did": validTestDID, "cid": alias}))
		require.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, validTestCID, fetched)
		assert.Equal(t, `"avatar-`+validTestCID+`"`, response.Header().Get("ETag"))
	})

	t.Run("conditional request checks the canonical CID", func(t *testing.T) {
		var checked string
		service := &mockService{
			isBlobBlockedFunc: func(_ context.Context, _, requested string) (bool, error) {
				checked = requested
				return true, nil
			},
			getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
				t.Error("a blocked conditional request must not fetch")
				return nil, nil
			},
		}
		request := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+alias,
			map[string]string{"preset": "avatar", "did": validTestDID, "cid": alias})
		request.Header.Set("If-None-Match", `"avatar-`+validTestCID+`"`)
		response := httptest.NewRecorder()
		NewHandler(service, resolverForPDS("https://pds.example.com")).HandleImage(response, request)
		assert.Equal(t, http.StatusNotFound, response.Code)
		assert.Equal(t, validTestCID, checked)
	})

	t.Run("a syntax-valid string that is not a CID is a 400 without a fetch", func(t *testing.T) {
		service := &mockService{getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
			t.Error("an undecodable CID must not be fetched")
			return nil, nil
		}}
		response := httptest.NewRecorder()
		NewHandler(service, resolverForPDS("https://pds.example.com")).HandleImage(response, createTestRequest(http.MethodGet,
			"/img/avatar/plain/"+validTestDID+"/bafynotacid", map[string]string{"preset": "avatar", "did": validTestDID, "cid": "bafynotacid"}))
		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.Equal(t, "invalid CID format", response.Body.String())
	})
}
