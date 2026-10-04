package imageproxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/atproto/identity"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingOwnerDIDService struct {
	*mockService
	getCalls int
}

func (service *countingOwnerDIDService) GetImageResolvingPDS(ctx context.Context, preset, did, cid string, resolvePDS func(context.Context) (string, error)) ([]byte, error) {
	service.getCalls++
	return service.mockService.GetImageResolvingPDS(ctx, preset, did, cid, resolvePDS)
}

func TestHandlerRefusesNoncanonicalOwnerDIDBeforeService(t *testing.T) {
	const preset = "content_preview"
	imageBytes := []byte{0xff, 0xd8, 0xff, 0xe0}
	for _, test := range []struct {
		name, did string
	}{
		{name: "uppercase plc identifier", did: "did:plc:Testauthor1"},
		{name: "uppercase web host", did: "did:web:Example.test"},
		{name: "percent-encoded web host letter", did: "did:web:%65xample.test"},
		{name: "path-based web DID", did: "did:web:example.test:a:b"},
	} {
		for _, conditional := range []bool{false, true} {
			name := "ordinary"
			if conditional {
				name = "matching If-None-Match"
			}
			t.Run(test.name+"/"+name, func(t *testing.T) {
				blockChecks, resolutions := 0, 0
				service := &countingOwnerDIDService{mockService: &mockService{
					isBlobBlockedFunc: func(context.Context, string, string) (bool, error) {
						blockChecks++
						return false, nil
					},
					getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
						return imageBytes, nil
					},
				}}
				resolver := &mockIdentityResolver{resolveDIDFunc: func(_ context.Context, did string) (*identity.DIDDocument, error) {
					resolutions++
					return &identity.DIDDocument{DID: did, Service: []identity.Service{{
						Type: "AtprotoPersonalDataServer", ServiceEndpoint: "https://pds.example.test",
					}}}, nil
				}}
				handler := NewHandler(service, resolver)
				router := chi.NewRouter()
				router.Get("/img/{preset}/plain/{did}/{cid}", func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, test.did, chi.URLParam(r, "did"), "chi must pass the exact raw owner spelling to the handler")
					handler.HandleImage(w, r)
				})
				// Construct the path directly so %65 stays in RawPath rather than
				// being escaped a second time before chi routes it.
				request := httptest.NewRequest(http.MethodGet, "/img/"+preset+"/plain/"+test.did+"/"+validTestCID, nil)
				if strings.Contains(test.did, "%") {
					require.Contains(t, request.URL.RawPath, "%65")
				}
				if conditional {
					request.Header.Set("If-None-Match", `"`+preset+`-`+validTestCID+`"`)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				assert.Equal(t, http.StatusBadRequest, response.Code)
				assert.Equal(t, "invalid DID format", response.Body.String())
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
				assert.NotContains(t, response.Header().Get("Content-Type"), "image/")
				assert.False(t, bytes.Equal(imageBytes, response.Body.Bytes()), "no image bytes can escape")
				assert.Zero(t, blockChecks, "no moderation block check before DID refusal")
				assert.Zero(t, service.getCalls, "no cache lookup or image fetch before DID refusal")
				assert.Zero(t, resolutions, "no PDS resolution before DID refusal")
			})
		}
	}
}

func TestHandlerServesCanonicalWebOwnerDIDThroughRouter(t *testing.T) {
	const preset = "content_preview"
	const owner = "did:web:example.test"
	const pdsURL = "https://pds.example.test"
	imageBytes := []byte{0xff, 0xd8, 0xff, 0xe0}
	var resolvedDIDs, fetchedDIDs, fetchedPDSURLs []string
	service := &countingOwnerDIDService{mockService: &mockService{
		getImageFunc: func(_ context.Context, _, did, _, resolvedPDSURL string) ([]byte, error) {
			fetchedDIDs = append(fetchedDIDs, did)
			fetchedPDSURLs = append(fetchedPDSURLs, resolvedPDSURL)
			return imageBytes, nil
		},
	}}
	resolver := &mockIdentityResolver{resolveDIDFunc: func(_ context.Context, did string) (*identity.DIDDocument, error) {
		resolvedDIDs = append(resolvedDIDs, did)
		return &identity.DIDDocument{DID: did, Service: []identity.Service{{
			Type: "AtprotoPersonalDataServer", ServiceEndpoint: pdsURL,
		}}}, nil
	}}
	router := chi.NewRouter()
	router.Get("/img/{preset}/plain/{did}/{cid}", NewHandler(service, resolver).HandleImage)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/img/"+preset+"/plain/"+owner+"/"+validTestCID, nil))

	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	assert.Equal(t, imageBytes, response.Body.Bytes())
	assert.Equal(t, "image/jpeg", response.Header().Get("Content-Type"))
	assert.Equal(t, successCacheControl, response.Header().Get("Cache-Control"))
	assert.Equal(t, 1, service.getCalls, "the canonical did:web owner must reach the service")
	assert.Equal(t, []string{owner}, resolvedDIDs, "the canonical did:web owner must be resolved as routed")
	assert.Equal(t, []string{owner}, fetchedDIDs)
	assert.Equal(t, []string{pdsURL}, fetchedPDSURLs)
}
