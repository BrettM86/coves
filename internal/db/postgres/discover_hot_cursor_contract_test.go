//go:build integration

package postgres

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverHotCursor_RepositoryContract(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	expected := seedDiscoverHotSnapshotCandidates(t, db, "cursor-contract", []int{400, 300, 200, 100})
	repository := NewDiscoverRepository(db, "discover-hot-cursor-contract-secret").(*postgresDiscoverRepo)
	const viewerDID = "did:plc:discoverhotcursorviewer"

	read := func(sort, viewer string, cursor *string) ([]*discover.FeedViewPost, *string, error) {
		t.Helper()
		return repository.GetDiscover(ctx, discover.GetDiscoverRequest{
			ViewerDID: viewer,
			Sort:      sort,
			Limit:     2,
			Cursor:    cursor,
		})
	}
	requireInvalid := func(name, sort, viewer, cursor string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			feed, next, err := read(sort, viewer, &cursor)
			assert.ErrorIs(t, err, discover.ErrInvalidCursor)
			assert.Empty(t, feed, "an invalid cursor must not silently restart from the first page")
			assert.Nil(t, next, "an invalid cursor must not mint a replacement cursor")
		})
	}

	firstPage, cursor, err := read("hot", viewerDID, nil)
	require.NoError(t, err)
	require.Equal(t, expected[:2], discoverURIs(firstPage))
	require.NotNil(t, cursor, "a partial snapshot page must emit a continuation cursor")
	assert.LessOrEqual(t, len(*cursor), 500, "Discover Hot cursors must fit the public 500-character contract")

	secondPage, finalCursor, err := read("hot", viewerDID, cursor)
	require.NoError(t, err)
	assert.Equal(t, expected[2:], discoverURIs(secondPage), "the emitted cursor must continue from its snapshot checkpoint")
	assert.Nil(t, finalCursor, "the final snapshot page must not claim another continuation")

	decoded, err := base64.StdEncoding.DecodeString(*cursor)
	require.NoError(t, err)
	require.NotEmpty(t, decoded)
	decoded[len(decoded)-1] ^= 0x01
	tampered := base64.StdEncoding.EncodeToString(decoded)
	malformedPayload := repository.signCursorPayload("discover-hot-v1::not-a-snapshot::2::" + discoverHotViewerScope(viewerDID))
	legacyHotCursor := repository.buildCursor(firstPage[0].Post, "hot", 0,
		time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC))

	for _, testCase := range []struct {
		name   string
		cursor string
		viewer string
	}{
		{name: "one-byte tamper", cursor: tampered, viewer: viewerDID},
		{name: "malformed base64", cursor: "not-base64%%%", viewer: viewerDID},
		{name: "malformed signed payload", cursor: malformedPayload, viewer: viewerDID},
		{name: "oversized cursor", cursor: string(make([]byte, 501)), viewer: viewerDID},
		{name: "legacy pre-snapshot Hot cursor", cursor: legacyHotCursor, viewer: viewerDID},
		{name: "wrong viewer", cursor: *cursor, viewer: "did:plc:discoverhotcursorwrongviewer"},
	} {
		requireInvalid(testCase.name, "hot", testCase.viewer, testCase.cursor)
	}

	requireInvalid("Discover Hot cursor under New", "new", viewerDID, *cursor)
	requireInvalid("Discover Hot cursor under Top", "top", viewerDID, *cursor)

	result, err := db.ExecContext(ctx, `
		UPDATE discover_hot_snapshots
		SET expires_at = NOW() - INTERVAL '1 second'
		WHERE viewer_scope = $1
	`, discoverHotViewerScope(viewerDID))
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected, "the fixture must expire exactly the snapshot referenced by the cursor")
	requireInvalid("expired snapshot", "hot", viewerDID, *cursor)
}
