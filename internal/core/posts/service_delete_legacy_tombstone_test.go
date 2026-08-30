package posts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type legacyDeleteCommunities struct {
	communities.Service
	community *communities.Community
}

func (s legacyDeleteCommunities) GetByDID(_ context.Context, _ string) (*communities.Community, error) {
	return s.community, nil
}

func (s legacyDeleteCommunities) EnsureFreshToken(_ context.Context, _ *communities.Community) (*communities.Community, error) {
	return s.community, nil
}

type legacyDeletePDSCalls struct {
	mu      sync.Mutex
	gets    int
	deletes int
}

type legacyDeletePDSOutcome int

const (
	legacyDeletePDSSuccess legacyDeletePDSOutcome = iota
	legacyDeletePDSRecordNotFound
	legacyDeletePDSServerError
)

func (c *legacyDeletePDSCalls) recordGet() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
}

func (c *legacyDeletePDSCalls) recordDelete() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++
}

func (c *legacyDeletePDSCalls) counts() (gets, deletes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets, c.deletes
}

func writeLegacyDeletePDSResponse(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode scripted PDS response: %v", err)
	}
}

func TestDeletePost_LegacyCommunityPostTombstonesAppViewDirectly(t *testing.T) {
	legacyURI := "at://" + compensationCommunityDID + "/" + LegacyPostCollection + "/" + compensationRkey

	tests := []struct {
		name           string
		recordAuthor   string
		indexedRow     *Post
		getOutcome     legacyDeletePDSOutcome
		deleteOutcome  legacyDeletePDSOutcome
		wantError      bool
		wantErr        error
		wantPDSDeletes int
		wantTombstones []string
	}{
		{
			name:           "successful PDS delete",
			recordAuthor:   compensationAuthorDID,
			wantPDSDeletes: 1,
			wantTombstones: []string{legacyURI},
		},
		{
			name:           "record already absent from PDS",
			indexedRow:     &Post{AuthorDID: compensationAuthorDID},
			getOutcome:     legacyDeletePDSRecordNotFound,
			wantTombstones: []string{legacyURI},
		},
		{
			name:       "absent PDS record belongs to another author in the index",
			indexedRow: &Post{AuthorDID: "did:plc:somebodyelsexxxxxxxxxx"},
			getOutcome: legacyDeletePDSRecordNotFound,
			wantErr:    ErrNotAuthorized,
		},
		{
			name:         "caller is not the record author",
			recordAuthor: "did:plc:somebodyelsexxxxxxxxxx",
			wantErr:      ErrNotAuthorized,
		},
		{
			name:           "record disappears before PDS delete",
			recordAuthor:   compensationAuthorDID,
			indexedRow:     &Post{AuthorDID: compensationAuthorDID},
			deleteOutcome:  legacyDeletePDSRecordNotFound,
			wantPDSDeletes: 1,
			wantTombstones: []string{legacyURI},
		},
		{
			name:           "PDS delete fails",
			recordAuthor:   compensationAuthorDID,
			deleteOutcome:  legacyDeletePDSServerError,
			wantError:      true,
			wantPDSDeletes: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := &legacyDeletePDSCalls{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/xrpc/com.atproto.repo.getRecord":
					calls.recordGet()
					switch tt.getOutcome {
					case legacyDeletePDSRecordNotFound:
						writeLegacyDeletePDSResponse(t, w, http.StatusNotFound, map[string]string{
							"error":   "RecordNotFound",
							"message": "record does not exist",
						})
						return
					case legacyDeletePDSServerError:
						writeLegacyDeletePDSResponse(t, w, http.StatusInternalServerError, map[string]string{
							"error":   "InternalError",
							"message": "get failed",
						})
						return
					}
					writeLegacyDeletePDSResponse(t, w, http.StatusOK, map[string]any{
						"uri": legacyURI,
						"cid": "bafyreilegacydelete",
						"value": map[string]any{
							"$type":  LegacyPostCollection,
							"author": tt.recordAuthor,
						},
					})
				case "/xrpc/com.atproto.repo.deleteRecord":
					calls.recordDelete()
					switch tt.deleteOutcome {
					case legacyDeletePDSRecordNotFound:
						writeLegacyDeletePDSResponse(t, w, http.StatusNotFound, map[string]string{
							"error":   "RecordNotFound",
							"message": "record does not exist",
						})
					case legacyDeletePDSServerError:
						writeLegacyDeletePDSResponse(t, w, http.StatusInternalServerError, map[string]string{
							"error":   "InternalError",
							"message": "delete failed",
						})
					default:
						writeLegacyDeletePDSResponse(t, w, http.StatusOK, map[string]any{})
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			h := newDeleteHarness(t)
			h.withCommunityService(legacyDeleteCommunities{community: &communities.Community{
				DID:            compensationCommunityDID,
				PDSURL:         server.URL,
				PDSAccessToken: "legacy-delete-test-token",
			}}, pds.PrivateHostOptions(true)...)
			h.repo.rawIndexedRow = tt.indexedRow

			err := h.delete(legacyURI)

			gets, deletes := calls.counts()
			require.Equal(t, 1, gets, "the legacy delete must verify the record through the real PDS client")
			require.Equal(t, tt.wantPDSDeletes, deletes)
			assert.Equal(t, tt.wantTombstones, h.repo.softDeleted,
				"a completed legacy delete must tombstone the AppView row without waiting for retired ingestion")

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
