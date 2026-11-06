package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/atproto/pds"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	blueskyProfileCollection = "app.bsky.actor.profile"
	importTestDID            = "did:plc:testuser123"
	blueskyAvatarCID         = "bafkreidhqb6afzkiias6nwij3ckl6cv2zdbyes7hhljww56uuusre4jaiq"
	blueskyBannerCID         = "bafkreigrgaoxwbprispsenknucpnisupzbbtyzb6vmhi7lqbvw73k6v2ki"
	covesBannerCID           = "bafkreibpa6xft7sc4d6gb3qoe2vbmxzwpf6cfnjsbjfkbipbti5urib6gm"
)

// boundedPDSClient extends mockPDSClient with the bounded read the import
// handler must use for both records. A collection with no entry in records or
// readErrors reads as pds.ErrNotFound.
type boundedPDSClient struct {
	*mockPDSClient
	records      map[string]*pds.RecordResponse
	readErrors   map[string]error
	boundedReads []string
	getRecords   int
	commitWrites int
}

var (
	_ pds.CommitClient        = (*boundedPDSClient)(nil)
	_ pds.BoundedRecordGetter = (*boundedPDSClient)(nil)
)

func (b *boundedPDSClient) GetRecordBounded(_ context.Context, collection, _ string, _ int64) (*pds.RecordResponse, error) {
	b.boundedReads = append(b.boundedReads, collection)
	if err := b.readErrors[collection]; err != nil {
		return nil, err
	}
	if record, ok := b.records[collection]; ok {
		return record, nil
	}
	return nil, pds.ErrNotFound
}

func (b *boundedPDSClient) GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	b.getRecords++
	return b.mockPDSClient.GetRecord(ctx, collection, rkey)
}

func (b *boundedPDSClient) PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error) {
	b.commitWrites++
	return b.mockPDSClient.PutRecordWithCommit(ctx, collection, rkey, record, swapRecord)
}

type recordingDismisser struct {
	err       error
	dids      []string
	ctxErrs   []error
	deadlines []time.Time
}

func (d *recordingDismisser) DismissBlueskyProfileImport(ctx context.Context, did string) error {
	d.dids = append(d.dids, did)
	d.ctxErrs = append(d.ctxErrs, ctx.Err())
	deadline, _ := ctx.Deadline()
	d.deadlines = append(d.deadlines, deadline)
	return d.err
}

// assertBoundedDeadline fails unless deadline is set and at most five seconds
// away, the bound on work that outlives the request.
func assertBoundedDeadline(t *testing.T, deadline time.Time) {
	t.Helper()
	remaining := time.Until(deadline)
	assert.Positive(t, remaining, "the detached context must carry a deadline")
	assert.LessOrEqual(t, remaining, 5*time.Second, "the detached context must be bounded to five seconds")
}

// cancelAfterCommitClient cancels the request context once the profile write
// has committed, as a client disconnecting mid-request would.
type cancelAfterCommitClient struct {
	*boundedPDSClient
	cancel context.CancelFunc
}

func (c *cancelAfterCommitClient) PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error) {
	commit, err := c.boundedPDSClient.PutRecordWithCommit(ctx, collection, rkey, record, swapRecord)
	c.cancel()
	return commit, err
}

func importRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.importBlueskyProfile", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return setTestOAuthSession(req, importTestDID, createTestOAuthSession(importTestDID))
}

func blueskyBlob(cid, mimeType string, size float64) map[string]any {
	return map[string]any{
		"$type":    "blob",
		"ref":      map[string]any{"$link": cid},
		"mimeType": mimeType,
		"size":     size,
	}
}

// existingCovesProfile is the standing Coves record every import test merges into.
func existingCovesProfile(extra map[string]any) *pds.RecordResponse {
	value := map[string]any{
		"$type":       CovesProfileCollection,
		"displayName": "Old",
		"description": "Old bio",
		"descriptionFacets": []any{
			map[string]any{"index": map[string]any{"byteStart": float64(0), "byteEnd": float64(3)}},
		},
		"pronouns": "they/them",
	}
	for key, v := range extra {
		value[key] = v
	}
	return &pds.RecordResponse{
		URI:   "at://did:plc:testuser123/social.coves.actor.profile/self",
		CID:   "bafyexisting",
		Value: value,
	}
}

func blueskyProfile(value map[string]any) *pds.RecordResponse {
	return &pds.RecordResponse{
		URI:   "at://did:plc:testuser123/app.bsky.actor.profile/self",
		CID:   "bafybluesky",
		Value: value,
	}
}

func newImportClient(coves, bluesky *pds.RecordResponse) *boundedPDSClient {
	records := map[string]*pds.RecordResponse{}
	if coves != nil {
		records[CovesProfileCollection] = coves
	}
	if bluesky != nil {
		records[blueskyProfileCollection] = bluesky
	}
	return &boundedPDSClient{
		mockPDSClient: &mockPDSClient{
			putRecordURI: "at://did:plc:testuser123/social.coves.actor.profile/self",
			putRecordCID: "bafyimported",
		},
		records: records,
	}
}

// asJSONValue normalises a written record to its wire form, so the assertion
// is about what reaches the PDS, not the Go types used to build it.
func asJSONValue(t *testing.T, value any) map[string]any {
	t.Helper()
	require.NotNil(t, value, "no record was written")
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	return decoded
}

func TestImportBlueskyProfileHandler_WritesRequestedFields(t *testing.T) {
	fullBluesky := func() *pds.RecordResponse {
		return blueskyProfile(map[string]any{
			"$type":       blueskyProfileCollection,
			"displayName": "Bluesky Name",
			"description": "Bluesky bio",
			"avatar":      blueskyBlob(blueskyAvatarCID, "image/png", 1000),
			"banner":      blueskyBlob(blueskyBannerCID, "image/jpeg", 2000),
		})
	}

	tests := []struct {
		name             string
		coves            *pds.RecordResponse
		bluesky          *pds.RecordResponse
		body             string
		wantValue        map[string]any
		wantImported     []string
		wantIndexedField func(t *testing.T, indexer *recordingProfileIndexer)
	}{
		{
			name:    "description and avatar merge into the standing record",
			coves:   existingCovesProfile(nil),
			bluesky: fullBluesky(),
			body:    `{"fields":["description","avatar"]}`,
			wantValue: map[string]any{
				"$type":       "social.coves.actor.profile",
				"displayName": "Old",
				"description": "Bluesky bio",
				"pronouns":    "they/them",
				"avatar": map[string]any{
					"$type":    "blob",
					"ref":      map[string]any{"$link": blueskyAvatarCID},
					"mimeType": "image/png",
					"size":     float64(1000),
				},
			},
			wantImported: []string{"description", "avatar"},
			wantIndexedField: func(t *testing.T, indexer *recordingProfileIndexer) {
				require.NotNil(t, indexer.input.AvatarCID)
				assert.Equal(t, blueskyAvatarCID, *indexer.input.AvatarCID)
				require.NotNil(t, indexer.input.Description)
				assert.Equal(t, "Bluesky bio", *indexer.input.Description)
			},
		},
		{
			name:  "a requested field Bluesky lacks is skipped",
			coves: existingCovesProfile(nil),
			bluesky: blueskyProfile(map[string]any{
				"$type":       blueskyProfileCollection,
				"displayName": "Bluesky Name",
				"description": "Bluesky bio",
			}),
			body: `{"fields":["displayName","banner"]}`,
			wantValue: map[string]any{
				"$type":       "social.coves.actor.profile",
				"displayName": "Bluesky Name",
				"description": "Old bio",
				"descriptionFacets": []any{
					map[string]any{"index": map[string]any{"byteStart": float64(0), "byteEnd": float64(3)}},
				},
				"pronouns": "they/them",
			},
			wantImported: []string{"displayName"},
			wantIndexedField: func(t *testing.T, indexer *recordingProfileIndexer) {
				require.NotNil(t, indexer.input.DisplayName)
				assert.Equal(t, "Bluesky Name", *indexer.input.DisplayName)
			},
		},
		{
			name: "banner overwrites an existing Coves banner",
			coves: existingCovesProfile(map[string]any{
				"banner": blueskyBlob(covesBannerCID, "image/png", 500),
			}),
			bluesky: fullBluesky(),
			body:    `{"fields":["banner"]}`,
			wantValue: map[string]any{
				"$type":       "social.coves.actor.profile",
				"displayName": "Old",
				"description": "Old bio",
				"descriptionFacets": []any{
					map[string]any{"index": map[string]any{"byteStart": float64(0), "byteEnd": float64(3)}},
				},
				"pronouns": "they/them",
				"banner": map[string]any{
					"$type":    "blob",
					"ref":      map[string]any{"$link": blueskyBannerCID},
					"mimeType": "image/jpeg",
					"size":     float64(2000),
				},
			},
			wantImported: []string{"banner"},
			wantIndexedField: func(t *testing.T, indexer *recordingProfileIndexer) {
				require.NotNil(t, indexer.input.BannerCID)
				assert.Equal(t, blueskyBannerCID, *indexer.input.BannerCID)
			},
		},
		{
			name:  "text is imported and an image whose link is not a CID is skipped",
			coves: existingCovesProfile(nil),
			bluesky: blueskyProfile(map[string]any{
				"$type":       blueskyProfileCollection,
				"displayName": "Bluesky Name",
				"avatar":      blueskyBlob("bafynotacanonicalcid", "image/png", 1000),
			}),
			body: `{"fields":["displayName","avatar"]}`,
			wantValue: map[string]any{
				"$type":       "social.coves.actor.profile",
				"displayName": "Bluesky Name",
				"description": "Old bio",
				"descriptionFacets": []any{
					map[string]any{"index": map[string]any{"byteStart": float64(0), "byteEnd": float64(3)}},
				},
				"pronouns": "they/them",
			},
			wantImported: []string{"displayName"},
			wantIndexedField: func(t *testing.T, indexer *recordingProfileIndexer) {
				require.NotNil(t, indexer.input.DisplayName)
				assert.Equal(t, "Bluesky Name", *indexer.input.DisplayName)
				require.NotNil(t, indexer.input.AvatarCID)
				assert.Empty(t, *indexer.input.AvatarCID, "the write-through mirrors a record with no avatar")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newImportClient(tt.coves, tt.bluesky)
			dismisser := &recordingDismisser{}
			indexer := &recordingProfileIndexer{}
			handler := NewImportBlueskyProfileHandlerWithFactory(createMockFactory(client, nil), dismisser).
				WithProfileIndexer(indexer)
			require.NotNil(t, handler, "constructor returned no handler")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, importRequest(t, tt.body))

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, 1, client.commitWrites, "exactly one guarded profile write")
			assert.False(t, client.putRecordCalled, "the unguarded PutRecord must not be used")
			assert.Equal(t, CovesProfileCollection, client.putRecordCollection)
			assert.Equal(t, "self", client.putRecordRKey)
			assert.Equal(t, "bafyexisting", client.putRecordSwap)
			assert.Equal(t, tt.wantValue, asJSONValue(t, client.putRecordValue))
			assert.Empty(t, client.uploads, "imported blobs are referenced, never re-uploaded")
			assert.Zero(t, client.getRecords, "records are read only through GetRecordBounded")

			var resp struct {
				URI      string   `json:"uri"`
				CID      string   `json:"cid"`
				Imported []string `json:"imported"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
			assert.Equal(t, "at://did:plc:testuser123/social.coves.actor.profile/self", resp.URI)
			assert.Equal(t, "bafyimported", resp.CID)
			assert.ElementsMatch(t, tt.wantImported, resp.Imported)

			assert.Equal(t, []string{importTestDID}, dismisser.dids)

			require.Equal(t, 1, indexer.calls)
			assert.Equal(t, importTestDID, indexer.did)
			tt.wantIndexedField(t, indexer)
		})
	}
}

func TestImportBlueskyProfileHandler_InvalidFieldsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty fields", body: `{"fields":[]}`},
		{name: "missing fields", body: `{}`},
		{name: "duplicate field", body: `{"fields":["displayName","displayName"]}`},
		{name: "unknown field", body: `{"fields":["handle"]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factoryCalls := 0
			factory := func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.CommitClient, error) {
				factoryCalls++
				return newImportClient(existingCovesProfile(nil), nil), nil
			}
			dismisser := &recordingDismisser{}
			handler := NewImportBlueskyProfileHandlerWithFactory(factory, dismisser)
			require.NotNil(t, handler, "constructor returned no handler")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, importRequest(t, tt.body))

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, "InvalidRequest", decodeXRPCBody(t, w)["error"])
			assert.Zero(t, factoryCalls, "no PDS client is built for an invalid request")
			assert.Empty(t, dismisser.dids)
		})
	}
}

func TestImportBlueskyProfileHandler_NothingToImport(t *testing.T) {
	tests := []struct {
		name    string
		bluesky *pds.RecordResponse
		body    string
	}{
		{
			name:    "no Bluesky profile record",
			bluesky: nil,
			body:    `{"fields":["displayName","avatar"]}`,
		},
		{
			name: "every requested field fails validation",
			bluesky: blueskyProfile(map[string]any{
				"$type":       blueskyProfileCollection,
				"displayName": "   ",
				"avatar":      blueskyBlob(blueskyAvatarCID, "image/gif", 1000),
				"description": "A valid bio that was not requested",
			}),
			body: `{"fields":["displayName","avatar"]}`,
		},
		{
			name: "the only requested field is an image whose link is not a CID",
			bluesky: blueskyProfile(map[string]any{
				"$type":  blueskyProfileCollection,
				"avatar": blueskyBlob("bafynotacanonicalcid", "image/png", 1000),
			}),
			body: `{"fields":["avatar"]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newImportClient(existingCovesProfile(nil), tt.bluesky)
			dismisser := &recordingDismisser{}
			handler := NewImportBlueskyProfileHandlerWithFactory(createMockFactory(client, nil), dismisser)
			require.NotNil(t, handler, "constructor returned no handler")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, importRequest(t, tt.body))

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, "NothingToImport", decodeXRPCBody(t, w)["error"])
			assert.Zero(t, client.commitWrites, "nothing is written")
			assert.False(t, client.putRecordCalled, "nothing is written")
			assert.Empty(t, dismisser.dids, "a failed import does not dismiss the offer")
		})
	}
}

// The PDS is untrusted: a getRecord body that never ends must fail the import
// rather than be read into memory. TestGetBlueskyProfileImportHandler_OversizedRecordBodyFails
// owns the byte boundary; this is the wiring proof that both reads go through
// the bounded getter.
func TestImportBlueskyProfileHandler_EndlessRecordBodyFails(t *testing.T) {
	tests := []struct {
		name              string
		endlessCollection string
	}{
		{name: "endless Bluesky record", endlessCollection: blueskyProfileCollection},
		{name: "endless Coves record", endlessCollection: CovesProfileCollection},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var putRecords atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/xrpc/com.atproto.repo.getRecord":
					collection := r.URL.Query().Get("collection")
					w.Header().Set("Content-Type", "application/json")
					if collection == tt.endlessCollection {
						streamEndlessRecord(w, r)
						return
					}
					switch collection {
					case blueskyProfileCollection:
						_, _ = w.Write([]byte(`{"uri":"at://did:plc:testuser123/app.bsky.actor.profile/self","cid":"bafybluesky","value":{"$type":"app.bsky.actor.profile","displayName":"Bluesky Name"}}`))
					case CovesProfileCollection:
						_, _ = w.Write([]byte(`{"uri":"at://did:plc:testuser123/social.coves.actor.profile/self","cid":"bafyexisting","value":{"$type":"social.coves.actor.profile","displayName":"Old"}}`))
					default:
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"error":"RecordNotFound","message":"Could not locate record"}`))
					}
				case "/xrpc/com.atproto.repo.putRecord":
					putRecords.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"uri":"at://did:plc:testuser123/social.coves.actor.profile/self","cid":"bafyimported"}`))
				default:
					w.WriteHeader(http.StatusNotImplemented)
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(server.CloseClientConnections)

			realClient, err := pds.NewFromAccessToken(server.URL, importTestDID, "test-access-token", pds.WithPrivateHostsAllowed())
			require.NoError(t, err)
			commitClient, ok := realClient.(pds.CommitClient)
			require.True(t, ok, "the real PDS client must be commit-aware")

			dismisser := &recordingDismisser{}
			handler := NewImportBlueskyProfileHandlerWithFactory(createMockFactory(commitClient, nil), dismisser)
			require.NotNil(t, handler, "constructor returned no handler")

			// Watchdog only: the handler must give up on its own long before this.
			watchdog, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			req := importRequest(t, `{"fields":["displayName"]}`)

			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				handler.ServeHTTP(w, req)
			}()
			select {
			case <-done:
			case <-watchdog.Done():
				t.Fatal("watchdog expired: the handler kept reading an endless record body")
			}

			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			assert.Equal(t, "UpstreamFailure", decodeXRPCBody(t, w)["error"])
			assert.Zero(t, putRecords.Load(), "no profile write after a failed read")
			assert.Empty(t, dismisser.dids)
		})
	}
}

// streamEndlessRecord writes the start of a valid getRecord body and then pads
// it forever, until the client goes away.
func streamEndlessRecord(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(`{"uri":"at://did:plc:testuser123/x/self","cid":"bafyendless","value":{"pad":"`))
	chunk := []byte(strings.Repeat("a", 32*1024))
	flusher, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		if _, err := w.Write(chunk); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// The import shares updateProfile's error contract for session, read and write
// failures.
func TestImportBlueskyProfileHandler_ErrorParityWithUpdateProfile(t *testing.T) {
	tests := []struct {
		name       string
		factoryErr error
		readErr    error
		writeErr   error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "session cannot be restored",
			factoryErr: pds.ErrSessionExpired,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "SessionError",
		},
		{
			name:       "swap conflict on write",
			writeErr:   pds.ErrSwapConflict,
			wantStatus: http.StatusConflict,
			wantCode:   "Conflict",
		},
		{
			name:       "forbidden read",
			readErr:    pds.ErrForbidden,
			wantStatus: http.StatusForbidden,
			wantCode:   "PermissionDenied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newImportClient(existingCovesProfile(nil), blueskyProfile(map[string]any{
				"$type":       blueskyProfileCollection,
				"displayName": "Bluesky Name",
			}))
			client.putRecordWithCommitError = tt.writeErr
			if tt.readErr != nil {
				client.readErrors = map[string]error{
					CovesProfileCollection:   tt.readErr,
					blueskyProfileCollection: tt.readErr,
				}
			}
			var factory PDSClientFactory
			if tt.factoryErr != nil {
				factory = createMockFactory(nil, tt.factoryErr)
			} else {
				factory = createMockFactory(client, nil)
			}
			dismisser := &recordingDismisser{}
			handler := NewImportBlueskyProfileHandlerWithFactory(factory, dismisser)
			require.NotNil(t, handler, "constructor returned no handler")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, importRequest(t, `{"fields":["displayName"]}`))

			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Equal(t, tt.wantCode, decodeXRPCBody(t, w)["error"])
			assert.Empty(t, dismisser.dids, "a failed import leaves the offer in place")
		})
	}
}

// The import is done once the record is written: a dismissal that fails
// afterwards is logged, and the client still gets the committed result.
func TestImportBlueskyProfileHandler_DismissalFailureAfterWriteStillSucceeds(t *testing.T) {
	client := newImportClient(existingCovesProfile(nil), blueskyProfile(map[string]any{
		"$type":       blueskyProfileCollection,
		"displayName": "Bluesky Name",
	}))
	dismisser := &recordingDismisser{err: errors.New("database unavailable")}
	handler := NewImportBlueskyProfileHandlerWithFactory(createMockFactory(client, nil), dismisser)
	require.NotNil(t, handler, "constructor returned no handler")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, importRequest(t, `{"fields":["displayName"]}`))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, client.commitWrites, "exactly one profile write")
	assert.Equal(t, []string{importTestDID}, dismisser.dids)

	var resp struct {
		URI      string   `json:"uri"`
		CID      string   `json:"cid"`
		Imported []string `json:"imported"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	assert.Equal(t, "at://did:plc:testuser123/social.coves.actor.profile/self", resp.URI)
	assert.Equal(t, "bafyimported", resp.CID)
	assert.Equal(t, []string{"displayName"}, resp.Imported)
}

// A client that disconnects after the write has committed must not cancel the
// dismissal: the record is imported, so the offer must not come back. The
// dismissal still has a deadline, so a stalled database cannot hold it forever.
func TestImportBlueskyProfileHandler_DismissalSurvivesRequestCancelAfterWrite(t *testing.T) {
	inner := newImportClient(existingCovesProfile(nil), blueskyProfile(map[string]any{
		"$type":       blueskyProfileCollection,
		"displayName": "Bluesky Name",
	}))
	req := importRequest(t, `{"fields":["displayName"]}`)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	client := &cancelAfterCommitClient{boundedPDSClient: inner, cancel: cancel}

	dismisser := &recordingDismisser{}
	handler := NewImportBlueskyProfileHandlerWithFactory(createMockFactory(client, nil), dismisser)
	require.NotNil(t, handler, "constructor returned no handler")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, 1, inner.commitWrites, "exactly one profile write")
	require.Equal(t, []string{importTestDID}, dismisser.dids)
	assert.Equal(t, []error{nil}, dismisser.ctxErrs, "the dismissal runs on a context the disconnect does not cancel")
	assertBoundedDeadline(t, dismisser.deadlines[0])
}

// A client that disconnects after the write has committed must not cancel the
// write-through: the record is on the PDS, so the user's next read must see it.
func TestImportBlueskyProfileHandler_WriteThroughSurvivesRequestCancelAfterWrite(t *testing.T) {
	inner := newImportClient(existingCovesProfile(nil), blueskyProfile(map[string]any{
		"$type":       blueskyProfileCollection,
		"displayName": "Bluesky Name",
	}))
	req := importRequest(t, `{"fields":["displayName"]}`)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	client := &cancelAfterCommitClient{boundedPDSClient: inner, cancel: cancel}

	indexer := &recordingProfileIndexer{}
	handler := NewImportBlueskyProfileHandlerWithFactory(createMockFactory(client, nil), &recordingDismisser{}).
		WithProfileIndexer(indexer)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.Equal(t, 1, indexer.calls)
	assert.NoError(t, indexer.ctxErr, "the write-through runs on a context the disconnect does not cancel")
	assertBoundedDeadline(t, indexer.deadline)
}

func TestImportBlueskyHandlerConstructors_PanicOnMissingDependency(t *testing.T) {
	factory := createMockFactory(newImportClient(nil, nil), nil)
	oauthClient := &oauthlib.ClientApp{}

	tests := []struct {
		name      string
		construct func()
	}{
		{name: "import with factory, no dismisser", construct: func() { NewImportBlueskyProfileHandlerWithFactory(factory, nil) }},
		{name: "import with OAuth, no dismisser", construct: func() { NewImportBlueskyProfileHandler(oauthClient, nil) }},
		{name: "query with factory, no reader", construct: func() { NewGetBlueskyProfileImportHandlerWithFactory(factory, nil) }},
		{name: "query with OAuth, no reader", construct: func() { NewGetBlueskyProfileImportHandler(oauthClient, nil) }},
		{name: "dismiss, no dismisser", construct: func() { NewDismissBlueskyProfileImportHandler(nil) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Panics(t, tt.construct)
		})
	}
}
