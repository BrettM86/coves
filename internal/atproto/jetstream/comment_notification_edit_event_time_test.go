//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommentConsumer_MentionEdit_ZeroEventTimeNotifiesAddedMention(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	added := fixture.recipient(t)
	fixture.create(t)
	require.Zero(t, mentionEditRows(t, fixture, added, "mention"))
	require.NoError(t, fixture.update(t, fixture.createdAt, 0, added))
	var cid, content, revision string
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT cid, content FROM comments WHERE uri = $1`, fixture.uri).Scan(&cid, &content))
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, fixture.uri).Scan(&revision))
	require.Equal(t, "bafyreimentioneditupdate", cid, "zero-time edit must apply")
	require.Equal(t, "A's edited comment @"+added.handle, content)
	require.Equal(t, fixture.revision, revision)
	require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`,
		fixture.uri, added.did, cid), "zero time must use index-time freshness, not the Unix epoch")
}

func TestCommentConsumer_ActiveRecreateOldEventAppliesContentWithoutMention(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	added := fixture.recipient(t)
	fixture.create(t)
	require.Zero(t, mentionEditRows(t, fixture, added, "mention"))
	var oldTime, databaseNow time.Time
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT now(), now() - INTERVAL '8 days'`).Scan(&databaseNow, &oldTime))
	require.True(t, oldTime.Before(databaseNow.Add(-7*24*time.Hour)), "fixture: re-create event predates the freshness window")
	_, err := fixture.gate.db.Exec(`UPDATE comments SET indexed_at = $1 WHERE uri = $2`, oldTime.Add(-time.Hour), fixture.uri)
	require.NoError(t, err)
	var storedIndexedAt time.Time
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT indexed_at FROM comments WHERE uri = $1`, fixture.uri).Scan(&storedIndexedAt))
	require.True(t, storedIndexedAt.Before(oldTime), "fixture: old re-create is not rejected by a recency watermark")
	event, content := activeRecreateEvent(t, fixture, fixture.createdAt, added)
	event.TimeUS = oldTime.UnixMicro()
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
	requireActiveRecreateApplied(t, fixture, content)
	requireStoredMentionFacets(t, fixture.gate.db, fixture.uri, added.did)
	var revision string
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, fixture.uri).Scan(&revision))
	require.Equal(t, event.Commit.Rev, revision, "the old re-create must advance the rev")
	require.Zero(t, mentionEditRows(t, fixture, added, "mention"), "an active re-create eight days old must not notify its new mention")
}
