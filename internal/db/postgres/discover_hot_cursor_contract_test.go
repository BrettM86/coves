//go:build integration

package postgres

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/internal/core/feedgen"
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

	read := func(viewer string, cursor *string) ([]*discoverHotTestItem, *string, error) {
		t.Helper()
		return getDiscoverHotViaGenerator(ctx, repository, discoverHotTestRequest{
			ViewerDID: viewer,
			Limit:     2,
			Cursor:    cursor,
		})
	}
	requireInvalid := func(name, viewer, cursor string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			feed, next, err := read(viewer, &cursor)
			assert.ErrorIs(t, err, discover.ErrInvalidCursor)
			assert.Empty(t, feed, "an invalid cursor must not silently restart from the first page")
			assert.Nil(t, next, "an invalid cursor must not mint a replacement cursor")
		})
	}

	firstPage, cursor, err := read(viewerDID, nil)
	require.NoError(t, err)
	require.Equal(t, expected[:2], discoverURIs(firstPage))
	require.NotNil(t, cursor, "a partial snapshot page must emit a continuation cursor")
	assert.LessOrEqual(t, len(*cursor), 500, "Discover Hot cursors must fit the public 500-character contract")

	secondPage, finalCursor, err := read(viewerDID, cursor)
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
		requireInvalid(testCase.name, testCase.viewer, testCase.cursor)
	}

	// A Discover Hot cursor is rejected under New and Top. Discover is served
	// by getFeed, whose cursor names the feed it was issued for; a distinct
	// viewer keeps the expiry below to the one snapshot referenced above.
	const crossFeedViewerDID = "did:plc:discoverhotcursorcrossfeed"
	feeds := newDiscoverViaFeed(db)
	_, hotFeedCursor, err := feeds.GetDiscoverViaFeed(ctx, discoverViaFeedRequest{ViewerDID: crossFeedViewerDID, Sort: "hot", Limit: 2})
	require.NoError(t, err)
	require.NotNil(t, hotFeedCursor, "a partial Discover Hot getFeed page must emit a continuation cursor")
	for _, testCase := range []struct{ name, sort string }{
		{name: "Discover Hot cursor under New", sort: "new"},
		{name: "Discover Hot cursor under Top", sort: "top"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			feed, next, err := feeds.GetDiscoverViaFeed(ctx, discoverViaFeedRequest{ViewerDID: crossFeedViewerDID, Sort: testCase.sort, Limit: 2, Cursor: hotFeedCursor})
			assert.ErrorIs(t, err, feedgen.ErrInvalidCursor)
			assert.Empty(t, feed, "an invalid cursor must not silently restart from the first page")
			assert.Nil(t, next, "an invalid cursor must not mint a replacement cursor")
		})
	}

	result, err := db.ExecContext(ctx, `
		UPDATE discover_hot_snapshots
		SET expires_at = NOW() - INTERVAL '1 second'
		WHERE viewer_scope = $1
	`, discoverHotViewerScope(viewerDID))
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected, "the fixture must expire exactly the snapshot referenced by the cursor")
	requireInvalid("expired snapshot", viewerDID, *cursor)
}
