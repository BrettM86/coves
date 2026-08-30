//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostConsumer_LegacyCommunityPost_IsIgnored(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 3)
	const (
		collection = "social.coves.community.post"
		rkey       = "retiredlegacy"
	)
	uri := "at://" + pv2Community + "/" + collection + "/" + rkey

	record := func(title, content string) map[string]interface{} {
		return map[string]interface{}{
			"$type":     collection,
			"community": pv2Community,
			"author":    pv2Author,
			"title":     title,
			"content":   content,
			"createdAt": "2026-03-01T00:00:00Z",
		}
	}

	require.NoError(t, f.consumer.HandleEvent(ctx, revCommitEvent(
		pv2Community, collection, "create", rkey, revs[0], "bafyreiretiredlegacy1", base,
		record("legacy title", "legacy content"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, revCommitEvent(
		pv2Community, collection, "update", rkey, revs[1], "bafyreiretiredlegacy2", base+1_000_000,
		record("updated legacy title", "updated legacy content"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, revCommitEvent(
		pv2Community, collection, "delete", rkey, revs[2], "", base+2_000_000, nil,
	)))

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"retired legacy post commits must not write posts")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM jetstream_record_revs WHERE record_uri = $1`, uri),
		"retired legacy post commits must not advance the rev gate")
}
