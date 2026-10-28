package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/users"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingProfileIndexer struct {
	err   error
	did   string
	input users.UpdateProfileInput
	calls int
}

func (r *recordingProfileIndexer) UpdateProfile(_ context.Context, did string, input users.UpdateProfileInput) (*users.User, error) {
	r.calls++
	r.did = did
	r.input = input
	return &users.User{DID: did}, r.err
}

const indexedAvatarCID = "bafkreifgzin5g3aeh5d6f6v54pcj66g425f5lwwjfj6d4ctc23xjeqyd6q"

func indexTestRequest(t *testing.T, body UpdateProfileRequest) *http.Request {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	return setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))
}

func indexTestPDSClient() *mockPDSClient {
	return &mockPDSClient{
		getRecordError: pds.ErrNotFound,
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      map[string]string{"$link": indexedAvatarCID},
			MimeType: "image/png",
			Size:     100,
		},
		putRecordURI: "at://did:plc:testuser123/social.coves.actor.profile/self",
		putRecordCID: "bafyupdated",
	}
}

// The AppView only learns of a profile write when Jetstream indexes it, so a
// client that refetches its own profile right after saving would read the old
// avatar. The handler writes the change through to the users table itself.
func TestUpdateProfileHandler_IndexesTheWrittenProfile(t *testing.T) {
	indexer := &recordingProfileIndexer{}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(indexTestPDSClient(), nil), testMediaPipeline(t)).
		WithProfileIndexer(indexer)
	displayName := "New name"
	description := "New bio"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, indexTestRequest(t, UpdateProfileRequest{
		DisplayName:    &displayName,
		Description:    &description,
		AvatarBlob:     testkit.TestPNG(4, 4),
		AvatarMimeType: "image/png",
	}))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, indexer.calls)
	assert.Equal(t, "did:plc:testuser123", indexer.did)
	require.NotNil(t, indexer.input.DisplayName)
	assert.Equal(t, displayName, *indexer.input.DisplayName)
	require.NotNil(t, indexer.input.Description)
	assert.Equal(t, description, *indexer.input.Description)
	require.NotNil(t, indexer.input.AvatarCID)
	assert.Equal(t, indexedAvatarCID, *indexer.input.AvatarCID)
	require.NotNil(t, indexer.input.BannerCID)
	assert.Empty(t, *indexer.input.BannerCID, "the record has no banner, so the indexed one is cleared")
	assert.Nil(t, indexer.input.Origin, "a user edit must not touch bridge origin fields")
}

// Jetstream may skip this commit as already applied, so the write-through
// must carry the whole record, including fields the request left unchanged.
func TestUpdateProfileHandler_IndexesTheWholeCommittedRecord(t *testing.T) {
	const existingBannerCID = "bafkreicxaeeaokhsdh4tafqquirxkclbhkog3ab7wh7o5mod7rqnsccr4m"
	client := indexTestPDSClient()
	client.getRecordError = nil
	client.getRecordResponse = &pds.RecordResponse{
		CID: "bafyexisting",
		Value: map[string]any{
			"$type":       CovesProfileCollection,
			"displayName": "Old name",
			"description": "Kept bio",
			"banner": map[string]any{
				"$type":    "blob",
				"ref":      map[string]any{"$link": existingBannerCID},
				"mimeType": "image/png",
				"size":     float64(100),
			},
		},
	}
	indexer := &recordingProfileIndexer{}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(client, nil), testMediaPipeline(t)).
		WithProfileIndexer(indexer)
	displayName := "Only the name"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, indexTestRequest(t, UpdateProfileRequest{DisplayName: &displayName}))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, indexer.calls)
	require.NotNil(t, indexer.input.DisplayName)
	assert.Equal(t, displayName, *indexer.input.DisplayName)
	require.NotNil(t, indexer.input.Description)
	assert.Equal(t, "Kept bio", *indexer.input.Description)
	require.NotNil(t, indexer.input.BannerCID)
	assert.Equal(t, existingBannerCID, *indexer.input.BannerCID)
	require.NotNil(t, indexer.input.AvatarCID)
	assert.Empty(t, *indexer.input.AvatarCID, "the record has no avatar, so the indexed one is cleared")
	assert.Nil(t, indexer.input.Origin)
}

// The record is already on the PDS, and Jetstream indexes it later, so a
// failed write-through must not report the save as failed.
func TestUpdateProfileHandler_IndexFailureStillSucceeds(t *testing.T) {
	indexer := &recordingProfileIndexer{err: errors.New("database unavailable")}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(indexTestPDSClient(), nil), testMediaPipeline(t)).
		WithProfileIndexer(indexer)
	displayName := "New name"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, indexTestRequest(t, UpdateProfileRequest{DisplayName: &displayName}))

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, indexer.calls)
}

func TestUpdateProfileHandler_PDSFailureIsNotIndexed(t *testing.T) {
	indexer := &recordingProfileIndexer{}
	client := indexTestPDSClient()
	client.putRecordWithCommitError = errors.New("pds down")
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(client, nil), testMediaPipeline(t)).
		WithProfileIndexer(indexer)
	displayName := "New name"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, indexTestRequest(t, UpdateProfileRequest{DisplayName: &displayName}))

	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, indexer.calls)
}
