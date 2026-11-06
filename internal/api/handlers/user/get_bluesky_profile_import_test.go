package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/users"
	"Coves/internal/validation"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDismissalReader answers the dismissal read with fixed values.
type fakeDismissalReader struct {
	dismissedAt *time.Time
	err         error
}

func (f fakeDismissalReader) GetBlueskyProfileImportDismissedAt(_ context.Context, _ string) (*time.Time, error) {
	return f.dismissedAt, f.err
}

func getImportRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getBlueskyProfileImport", nil)
	return setTestOAuthSession(req, importTestDID, createTestOAuthSession(importTestDID))
}

func serveGetImport(t *testing.T, factory PDSClientFactory, reader BlueskyImportDismissalReader) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewGetBlueskyProfileImportHandlerWithFactory(factory, reader)
	require.NotNil(t, handler, "constructor returned no handler")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, getImportRequest())
	return w
}

// decodeImportOffer decodes a successful response into its wire form.
func decodeImportOffer(t *testing.T, w *httptest.ResponseRecorder) (string, []map[string]any) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "response body: %q", w.Body.String())
	status, _ := resp["status"].(string)
	rawFields, ok := resp["fields"].([]any)
	require.True(t, ok, "fields must be a JSON array, got %#v", resp["fields"])
	fields := make([]map[string]any, 0, len(rawFields))
	for _, raw := range rawFields {
		entry, ok := raw.(map[string]any)
		require.True(t, ok, "field entry must be an object, got %#v", raw)
		fields = append(fields, entry)
	}
	return status, fields
}

func fullBlueskyProfile() *pds.RecordResponse {
	return blueskyProfile(map[string]any{
		"$type":       blueskyProfileCollection,
		"displayName": "Bluesky Name",
		"description": "Bluesky bio",
		"avatar":      blueskyBlob(blueskyAvatarCID, "image/png", 1000),
		"banner":      blueskyBlob(blueskyBannerCID, "image/jpeg", 2000),
	})
}

func textOfferField(field, bluesky string, covesIsEmpty bool) map[string]any {
	return map[string]any{"field": field, "bluesky": bluesky, "covesIsEmpty": covesIsEmpty}
}

// imageOfferField is an image entry without its preview URL, which
// TestGetBlueskyProfileImportHandler_ImageFieldsCarryPreviewURLs owns.
func imageOfferField(field string, covesIsEmpty bool) map[string]any {
	return map[string]any{"field": field, "covesIsEmpty": covesIsEmpty}
}

func TestGetBlueskyProfileImportHandler_StatusAndFields(t *testing.T) {
	dismissedAt := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	covesFullyPopulated := func() *pds.RecordResponse {
		return existingCovesProfile(map[string]any{
			"avatar": blueskyBlob(covesBannerCID, "image/png", 500),
			"banner": blueskyBlob(covesBannerCID, "image/png", 500),
		})
	}
	onlyInvalidBluesky := func() *pds.RecordResponse {
		return blueskyProfile(map[string]any{
			"$type":       blueskyProfileCollection,
			"displayName": "   ",
			"avatar":      blueskyBlob(blueskyAvatarCID, "image/gif", 1000),
		})
	}
	allFields := func(covesIsEmpty bool) []map[string]any {
		return []map[string]any{
			textOfferField("displayName", "Bluesky Name", covesIsEmpty),
			textOfferField("description", "Bluesky bio", covesIsEmpty),
			imageOfferField("avatar", covesIsEmpty),
			imageOfferField("banner", covesIsEmpty),
		}
	}

	tests := []struct {
		name        string
		bluesky     *pds.RecordResponse
		coves       *pds.RecordResponse
		dismissedAt *time.Time
		wantStatus  string
		wantFields  []map[string]any
	}{
		{
			name:       "no Bluesky record",
			bluesky:    nil,
			coves:      existingCovesProfile(nil),
			wantStatus: "noBlueskyProfile",
			wantFields: []map[string]any{},
		},
		{
			name:        "no Bluesky record beats dismissed",
			bluesky:     nil,
			coves:       nil,
			dismissedAt: &dismissedAt,
			wantStatus:  "noBlueskyProfile",
			wantFields:  []map[string]any{},
		},
		{
			name:        "only invalid Bluesky fields beats dismissed",
			bluesky:     onlyInvalidBluesky(),
			coves:       nil,
			dismissedAt: &dismissedAt,
			wantStatus:  "nothingToImport",
			wantFields:  []map[string]any{},
		},
		{
			name:       "zero valid Bluesky fields is nothingToImport, not complete",
			bluesky:    onlyInvalidBluesky(),
			coves:      covesFullyPopulated(),
			wantStatus: "nothingToImport",
			wantFields: []map[string]any{},
		},
		{
			name:        "dismissed lists every valid field",
			bluesky:     fullBlueskyProfile(),
			coves:       nil,
			dismissedAt: &dismissedAt,
			wantStatus:  "dismissed",
			wantFields:  allFields(true),
		},
		{
			name:        "dismissed beats complete",
			bluesky:     fullBlueskyProfile(),
			coves:       covesFullyPopulated(),
			dismissedAt: &dismissedAt,
			wantStatus:  "dismissed",
			wantFields:  allFields(false),
		},
		{
			name:       "every valid field already set on Coves is complete",
			bluesky:    fullBlueskyProfile(),
			coves:      covesFullyPopulated(),
			wantStatus: "complete",
			wantFields: allFields(false),
		},
		{
			name: "complete depends only on the Bluesky-valid fields",
			bluesky: blueskyProfile(map[string]any{
				"$type":       blueskyProfileCollection,
				"displayName": "Bluesky Name",
			}),
			coves:      existingCovesProfile(nil),
			wantStatus: "complete",
			wantFields: []map[string]any{textOfferField("displayName", "Bluesky Name", false)},
		},
		{
			name: "partially populated Coves profile is an offer",
			bluesky: blueskyProfile(map[string]any{
				"$type":       blueskyProfileCollection,
				"displayName": "Bluesky Name",
				"avatar":      blueskyBlob(blueskyAvatarCID, "image/png", 1000),
			}),
			coves:      existingCovesProfile(nil),
			wantStatus: "offer",
			wantFields: []map[string]any{
				textOfferField("displayName", "Bluesky Name", false),
				imageOfferField("avatar", true),
			},
		},
		{
			name:       "no Coves record at all is an offer with every field empty",
			bluesky:    fullBlueskyProfile(),
			coves:      nil,
			wantStatus: "offer",
			wantFields: allFields(true),
		},
		{
			name:    "whitespace, non-string and CID-less Coves values count as empty",
			bluesky: fullBlueskyProfile(),
			coves: existingCovesProfile(map[string]any{
				"displayName": "   ",
				"description": float64(42),
				"avatar":      map[string]any{"$type": "blob", "mimeType": "image/png", "size": float64(500)},
			}),
			wantStatus: "offer",
			wantFields: allFields(true),
		},
		{
			name:    "a Coves avatar without a banner offers only the banner",
			bluesky: fullBlueskyProfile(),
			coves: existingCovesProfile(map[string]any{
				"avatar": blueskyBlob(covesBannerCID, "image/png", 500),
			}),
			wantStatus: "offer",
			wantFields: []map[string]any{
				textOfferField("displayName", "Bluesky Name", false),
				textOfferField("description", "Bluesky bio", false),
				imageOfferField("avatar", false),
				imageOfferField("banner", true),
			},
		},
		{
			name: "a Bluesky image whose link is not a canonical CID is not offered",
			bluesky: blueskyProfile(map[string]any{
				"$type":       blueskyProfileCollection,
				"displayName": "Bluesky Name",
				"avatar":      blueskyBlob("bafynotacanonicalcid", "image/png", 1000),
			}),
			coves:      existingCovesProfile(nil),
			wantStatus: "complete",
			wantFields: []map[string]any{textOfferField("displayName", "Bluesky Name", false)},
		},
		{
			name: "only a non-canonical Bluesky image is nothingToImport",
			bluesky: blueskyProfile(map[string]any{
				"$type":  blueskyProfileCollection,
				"avatar": blueskyBlob("bafynotacanonicalcid", "image/png", 1000),
			}),
			coves:      nil,
			wantStatus: "nothingToImport",
			wantFields: []map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newImportClient(tt.coves, tt.bluesky)
			w := serveGetImport(t, createMockFactory(client, nil), fakeDismissalReader{dismissedAt: tt.dismissedAt})

			status, fields := decodeImportOffer(t, w)
			assert.Equal(t, tt.wantStatus, status)
			for _, entry := range fields {
				delete(entry, "blueskyImageUrl")
			}
			assert.Equal(t, tt.wantFields, fields)
		})
	}
}

func TestGetBlueskyProfileImportHandler_ImageFieldsCarryPreviewURLs(t *testing.T) {
	tests := []struct {
		name          string
		configure     bool
		source        blobs.BlueskyProfileImagePreviewSource
		wantAvatarURL string
		wantBannerURL string
	}{
		{
			name:          "unconfigured handler uses the Bluesky CDN",
			wantAvatarURL: "https://cdn.bsky.app/img/avatar/plain/did:plc:testuser123/" + blueskyAvatarCID + "@jpeg", // coves:allow-public-host: expected output of the production Bluesky CDN URL builder, matched as a string and never dialled
			wantBannerURL: "https://cdn.bsky.app/img/banner/plain/did:plc:testuser123/" + blueskyBannerCID + "@jpeg", // coves:allow-public-host: expected output of the production Bluesky CDN URL builder, matched as a string and never dialled
		},
		{
			name:          "Bluesky CDN form",
			configure:     true,
			source:        blobs.BlueskyProfileImagePreviewSourceBlueskyCDN,
			wantAvatarURL: "https://cdn.bsky.app/img/avatar/plain/did:plc:testuser123/" + blueskyAvatarCID + "@jpeg", // coves:allow-public-host: expected output of the production Bluesky CDN URL builder, matched as a string and never dialled
			wantBannerURL: "https://cdn.bsky.app/img/banner/plain/did:plc:testuser123/" + blueskyBannerCID + "@jpeg", // coves:allow-public-host: expected output of the production Bluesky CDN URL builder, matched as a string and never dialled
		},
		{
			name:      "PDS getBlob form",
			configure: true,
			source:    blobs.BlueskyProfileImagePreviewSourcePDS,
			wantAvatarURL: "https://test.pds.example/xrpc/com.atproto.sync.getBlob?cid=" + blueskyAvatarCID +
				"&did=did%3Aplc%3Atestuser123",
			wantBannerURL: "https://test.pds.example/xrpc/com.atproto.sync.getBlob?cid=" + blueskyBannerCID +
				"&did=did%3Aplc%3Atestuser123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newImportClient(nil, fullBlueskyProfile())
			handler := NewGetBlueskyProfileImportHandlerWithFactory(createMockFactory(client, nil), fakeDismissalReader{})
			if tt.configure {
				handler.WithImagePreviewSource(tt.source)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, getImportRequest())

			_, fields := decodeImportOffer(t, w)
			images := map[string]map[string]any{}
			for _, entry := range fields {
				if name, _ := entry["field"].(string); name == "avatar" || name == "banner" {
					images[name] = entry
				}
			}
			require.Contains(t, images, "avatar")
			require.Contains(t, images, "banner")
			assert.Equal(t, tt.wantAvatarURL, images["avatar"]["blueskyImageUrl"])
			assert.Equal(t, tt.wantBannerURL, images["banner"]["blueskyImageUrl"])
			assert.NotContains(t, images["avatar"], "bluesky", "image fields carry a URL, not a value")
			assert.NotContains(t, images["banner"], "bluesky", "image fields carry a URL, not a value")

			body := w.Body.String()
			assert.NotContains(t, body, "$link", "no raw blob ref is returned")
			assert.NotContains(t, body, `"ref"`, "no raw blob ref is returned")
			assert.NotContains(t, body, "mimeType", "no raw blob ref is returned")
		})
	}
}

// unreachablePDSErrors returns the errors the real PDS client reports when its
// host refuses connections, for the bounded and the plain getRecord read.
func unreachablePDSErrors(t *testing.T) (bounded, plain error) {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	host := server.URL
	server.Close()

	client, err := pds.NewFromAccessToken(host, importTestDID, "test-access-token", pds.WithPrivateHostsAllowed())
	require.NoError(t, err)
	reader, ok := client.(pds.BoundedRecordGetter)
	require.True(t, ok, "the real PDS client must support bounded reads")

	_, bounded = reader.GetRecordBounded(context.Background(), CovesProfileCollection, "self", 1<<20)
	require.Error(t, bounded)
	_, plain = client.GetRecord(context.Background(), CovesProfileCollection, "self")
	require.Error(t, plain)
	return bounded, plain
}

// The query shares updateProfile's error contract for session restore and an
// unreachable PDS.
func TestGetBlueskyProfileImportHandler_ErrorParityWithUpdateProfile(t *testing.T) {
	tests := []struct {
		name                  string
		factoryErr            error
		unreachableCollection string
		wantStatus            int
		wantCode              string
	}{
		{
			name:       "session cannot be restored",
			factoryErr: errors.New("session restoration failed"),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "SessionError",
		},
		{
			name:       "session expired",
			factoryErr: pds.ErrSessionExpired,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "SessionError",
		},
		{
			name:                  "PDS unreachable on the Bluesky read",
			unreachableCollection: blueskyProfileCollection,
			wantStatus:            http.StatusInternalServerError,
			wantCode:              "PDSError",
		},
		{
			name:                  "PDS unreachable on the Coves read",
			unreachableCollection: CovesProfileCollection,
			wantStatus:            http.StatusInternalServerError,
			wantCode:              "PDSError",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var importFactory, updateFactory PDSClientFactory
			if tt.factoryErr != nil {
				importFactory = createMockFactory(nil, tt.factoryErr)
				updateFactory = createMockFactory(nil, tt.factoryErr)
			} else {
				boundedErr, plainErr := unreachablePDSErrors(t)
				client := newImportClient(existingCovesProfile(nil), fullBlueskyProfile())
				client.readErrors = map[string]error{tt.unreachableCollection: boundedErr}
				importFactory = createMockFactory(client, nil)
				updateFactory = createMockFactory(&mockPDSClient{getRecordError: plainErr}, nil)
			}

			updateHandler := NewUpdateProfileHandlerWithFactory(updateFactory, testMediaPipeline(t))
			updateReq := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile",
				bytes.NewReader([]byte(`{"displayName":"Test User"}`)))
			updateReq.Header.Set("Content-Type", "application/json")
			updateReq = setTestOAuthSession(updateReq, importTestDID, createTestOAuthSession(importTestDID))
			updateW := httptest.NewRecorder()
			updateHandler.ServeHTTP(updateW, updateReq)
			require.Equal(t, tt.wantStatus, updateW.Code, "updateProfile baseline: %s", updateW.Body.String())
			require.Equal(t, tt.wantCode, decodeXRPCBody(t, updateW)["error"], "updateProfile baseline")

			w := serveGetImport(t, importFactory, fakeDismissalReader{})

			require.Equal(t, updateW.Code, w.Code, w.Body.String())
			assert.Equal(t, tt.wantCode, decodeXRPCBody(t, w)["error"])
		})
	}
}

// paddedRecordBody builds a valid getRecord JSON body of exactly size bytes,
// padded with a top-level key the reader ignores.
func paddedRecordBody(t *testing.T, uri, cid, value string, size int) []byte {
	t.Helper()
	prefix := `{"uri":"` + uri + `","cid":"` + cid + `","value":` + value + `,"padding":"`
	suffix := `"}`
	padding := size - len(prefix) - len(suffix)
	require.Positive(t, padding)
	body := []byte(prefix + strings.Repeat("a", padding) + suffix)
	require.Len(t, body, size)
	require.True(t, json.Valid(body))
	return body
}

// A getRecord body over the 1 MiB cap for either record fails the query with
// updateProfile's getRecord-failure mapping, never a computed status.
func TestGetBlueskyProfileImportHandler_OversizedRecordBodyFails(t *testing.T) {
	const (
		blueskyURI   = "at://did:plc:testuser123/app.bsky.actor.profile/self"
		blueskyValue = `{"$type":"app.bsky.actor.profile","displayName":"Bluesky Name"}`
		covesURI     = "at://did:plc:testuser123/social.coves.actor.profile/self"
		covesValue   = `{"$type":"social.coves.actor.profile","description":"Old bio"}`
		overCap      = 1<<20 + 1
		belowCap     = 1<<20 - 4096
	)

	tests := []struct {
		name             string
		paddedCollection string
		bodyBytes        int
		wantHTTPStatus   int
		wantError        string
		wantStatus       string
	}{
		{
			name:             "Bluesky record over the cap",
			paddedCollection: blueskyProfileCollection,
			bodyBytes:        overCap,
			wantHTTPStatus:   http.StatusBadGateway,
			wantError:        "UpstreamFailure",
		},
		{
			name:             "Coves record over the cap",
			paddedCollection: CovesProfileCollection,
			bodyBytes:        overCap,
			wantHTTPStatus:   http.StatusBadGateway,
			wantError:        "UpstreamFailure",
		},
		{
			name:             "Bluesky record below the cap",
			paddedCollection: blueskyProfileCollection,
			bodyBytes:        belowCap,
			wantHTTPStatus:   http.StatusOK,
			wantStatus:       "offer",
		},
		{
			name:             "Coves record below the cap",
			paddedCollection: CovesProfileCollection,
			bodyBytes:        belowCap,
			wantHTTPStatus:   http.StatusOK,
			wantStatus:       "offer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodies := map[string][]byte{
				blueskyProfileCollection: []byte(`{"uri":"` + blueskyURI + `","cid":"bafybluesky","value":` + blueskyValue + `}`),
				CovesProfileCollection:   []byte(`{"uri":"` + covesURI + `","cid":"bafyexisting","value":` + covesValue + `}`),
			}
			if tt.paddedCollection == blueskyProfileCollection {
				bodies[blueskyProfileCollection] = paddedRecordBody(t, blueskyURI, "bafybluesky", blueskyValue, tt.bodyBytes)
			} else {
				bodies[CovesProfileCollection] = paddedRecordBody(t, covesURI, "bafyexisting", covesValue, tt.bodyBytes)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/xrpc/com.atproto.repo.getRecord" {
					w.WriteHeader(http.StatusNotImplemented)
					return
				}
				body, ok := bodies[r.URL.Query().Get("collection")]
				w.Header().Set("Content-Type", "application/json")
				if !ok {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"RecordNotFound","message":"Could not locate record"}`))
					return
				}
				_, _ = w.Write(body)
			}))
			t.Cleanup(server.Close)

			realClient, err := pds.NewFromAccessToken(server.URL, importTestDID, "test-access-token", pds.WithPrivateHostsAllowed())
			require.NoError(t, err)
			commitClient, ok := realClient.(pds.CommitClient)
			require.True(t, ok, "the real PDS client must be commit-aware")

			w := serveGetImport(t, createMockFactory(commitClient, nil), fakeDismissalReader{})

			if tt.wantError != "" {
				require.Equal(t, tt.wantHTTPStatus, w.Code, w.Body.String())
				assert.Equal(t, tt.wantError, decodeXRPCBody(t, w)["error"])
				return
			}
			status, fields := decodeImportOffer(t, w)
			assert.Equal(t, tt.wantStatus, status)
			assert.Equal(t, []map[string]any{textOfferField("displayName", "Bluesky Name", true)}, fields)
		})
	}
}

func TestGetBlueskyProfileImportHandler_ResponseConformsToLexicon(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../../atproto/lexicon"))

	tests := []struct {
		name       string
		bluesky    *pds.RecordResponse
		wantStatus string
	}{
		{name: "offer with text and image fields", bluesky: fullBlueskyProfile(), wantStatus: "offer"},
		{name: "no Bluesky profile", bluesky: nil, wantStatus: "noBlueskyProfile"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newImportClient(existingCovesProfile(nil), tt.bluesky)
			w := serveGetImport(t, createMockFactory(client, nil), fakeDismissalReader{})

			status, _ := decodeImportOffer(t, w)
			require.Equal(t, tt.wantStatus, status)

			decoded, err := atdata.UnmarshalJSON(w.Body.Bytes())
			require.NoError(t, err)
			assert.NoError(t, validation.ValidateData(catalog, decoded, "social.coves.actor.getBlueskyProfileImport#output", 0))
		})
	}
}

// A failed dismissal read must not be guessed around: with a valid Bluesky
// record and an empty Coves profile the status hinges on the dismissal, so the
// handler answers an error instead of an offer, mapped as the dismiss
// endpoint maps the same service errors.
func TestGetBlueskyProfileImportHandler_DismissalReadFailureIsAnError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "user not indexed",
			err:        users.ErrUserNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "AccountNotFound",
		},
		{
			name:       "invalid DID",
			err:        &users.InvalidDIDError{DID: "bad", Reason: "must start with 'did:'"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidDID",
		},
		{
			name:       "database unavailable",
			err:        errors.New("database unavailable"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "InternalServerError",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newImportClient(nil, fullBlueskyProfile())
			w := serveGetImport(t, createMockFactory(client, nil), fakeDismissalReader{err: tt.err})

			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Equal(t, tt.wantCode, decodeXRPCBody(t, w)["error"])
		})
	}
}
