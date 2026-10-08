//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Hold the post row while the consumer uses a separate pool connection. The
// fixture transaction's backend also observes the consumer's lock wait.
func postEditRowTransaction(t *testing.T, ctx context.Context, db *sql.DB, uri string) (*sql.Tx, int) {
	t.Helper()
	connection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	transaction, err := connection.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackError := transaction.Rollback()
		require.True(t, rollbackError == nil || errors.Is(rollbackError, sql.ErrTxDone),
			"rolling back post-row fixture transaction: %v", rollbackError)
	})
	var processID int
	var lockedURI string
	require.NoError(t, transaction.QueryRowContext(ctx,
		`SELECT pg_backend_pid(), uri FROM posts WHERE uri = $1 FOR UPDATE`, uri).Scan(&processID, &lockedURI))
	require.Equal(t, uri, lockedURI)
	return transaction, processID
}

func TestPostNotificationEdit_NonqualifyingUpdatesPreserveNotifications(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"stale revision", "recency guard", "already soft deleted", "concurrent delete", "concurrent recency advancement"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			db := testkit.DB(t)
			fixture := newPV2Fixture(t, db)
			handles, recipients := postMentionRecipients(t, db, 2)
			createdAt := activatedCommentNotificationTime(t, db, ctx)
			consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
			key := testkit.TID()
			uri := pv2URI(pv2Author, key)
			revisions := increasingTIDs(t, 3)
			base := time.Now().UnixMicro()
			const createCID = "bafyreiposteditguardscreate"
			require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revisions[1],
				createCID, base, postMentionRecord(t, createdAt, handles[:1], recipients[:1]))))
			requirePostMentionFacets(t, db, uri, recipients[0])
			before := postMentionRows(t, db, uri)
			require.Len(t, before, 1, "fixture: original post must notify B")
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri))
			require.Equal(t, revisions[1], readPostV2MechanismRev(t, db, uri))
			var indexedAt time.Time
			require.NoError(t, db.QueryRowContext(ctx, `SELECT indexed_at FROM posts WHERE uri = $1`, uri).Scan(&indexedAt))
			event := pv2Event(pv2Author, "update", key, revisions[2], "bafyreiposteditguardsskipped",
				base+1_000_000, postMentionRecord(t, createdAt, handles, recipients))

			switch name {
			case "stale revision":
				event.Commit.Rev = revisions[0]
				require.Less(t, event.Commit.Rev, revisions[1])
				require.NoError(t, consumer.HandleEvent(ctx, event))
			case "recency guard":
				event.TimeUS = indexedAt.Add(-time.Second).UnixMicro()
				require.NoError(t, consumer.HandleEvent(ctx, event))
			case "already soft deleted":
				_, err := db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, uri)
				require.NoError(t, err, "fixture: leave the original notification present on the deleted row")
				require.NoError(t, consumer.HandleEvent(ctx, event))
			case "concurrent delete", "concurrent recency advancement":
				results := make(chan error, 1)
				started, finished := false, false
				t.Cleanup(func() {
					if started && !finished {
						commentErasureResult(t, ctx, results, "HandleEvent(post edit) after fixture rollback")
					}
				})
				transaction, processID := postEditRowTransaction(t, ctx, db, uri)
				if name == "concurrent delete" {
					_, err := transaction.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, uri)
					require.NoError(t, err)
				} else {
					_, err := transaction.ExecContext(ctx, `UPDATE posts SET indexed_at = $1 WHERE uri = $2`,
						time.UnixMicro(event.TimeUS).Add(time.Hour), uri)
					require.NoError(t, err)
				}
				started = true
				go func() { results <- consumer.HandleEvent(ctx, event) }()
				commentErasureBlockedByFixture(t, ctx, transaction, processID, "FROM posts WHERE id")
				require.NoError(t, transaction.Commit())
				commentErasureResult(t, ctx, results, "HandleEvent(post edit)")
				finished = true
				if name == "concurrent recency advancement" {
					var content string
					require.NoError(t, db.QueryRowContext(ctx, `SELECT content FROM posts WHERE uri = $1`, uri).Scan(&content))
					require.Equal(t, "mentions @"+handles[0], content, "zero-row edit must preserve original content")
				}
			default:
				t.Fatalf("unhandled guard case %q", name)
			}
			require.Equal(t, before, postMentionRows(t, db, uri), "skipped edit must preserve notification IDs and CIDs")
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri),
				"skipped edit must not add any other notification")
			require.Equal(t, revisions[1], readPostV2MechanismRev(t, db, uri), "skipped edit must not advance the rev")
			require.Equal(t, createCID, readPostV2MechanismRow(t, db, uri).CID)
		})
	}
}

func TestPostNotificationEdit_CapCountsExistingEightMentionRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 12)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revisions := increasingTIDs(t, 2)
	base := time.Now().UnixMicro()
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revisions[0],
		"bafyreiposteditcapcreate", base, postMentionRecord(t, createdAt, handles[:8], recipients[:8]))))
	before := postMentionRows(t, db, uri)
	require.Len(t, before, 8, "fixture: eight eligible recipients must already hold rows")
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revisions[1],
		"bafyreiposteditcapupdate", base+1_000_000, postMentionRecord(t, createdAt, handles, recipients))))
	requirePostMentionFacets(t, db, uri, recipients...)
	after := postMentionRows(t, db, uri)
	require.Len(t, after, 10, "the cap applies across create and edit, not independently per edit")
	require.Equal(t, before, after[:8], "original mention rows must survive the edit")
	for index := 8; index < 10; index++ {
		require.Equal(t, recipients[index], after[index].recipient, "facet order chooses the two available slots")
		require.Equal(t, "bafyreiposteditcapupdate", after[index].cid)
	}
	for _, recipient := range recipients[10:] {
		require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, uri, recipient))
	}
	require.Equal(t, revisions[1], readPostV2MechanismRev(t, db, uri), "fixture: the edit must win rather than silently skip")
}

func TestPostNotificationEdit_DiffAndCapUseLockedStoredFacets(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	// X, Y, Z are the incoming facets. Nine distinct pre-existing recipients
	// occupy the budget after the fixture transaction commits.
	handles, recipients := postMentionRecipients(t, db, 12)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revisions := increasingTIDs(t, 2)
	base := time.Now().UnixMicro()
	create := pv2Record(pv2Community, "Locked post", "No mentions yet")
	create["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revisions[0],
		"bafyreiposteditlockedcreate", base, create)))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri))
	event := pv2Event(pv2Author, "update", key, revisions[1], "bafyreiposteditlockedupdate",
		base+1_000_000, postMentionRecord(t, createdAt, handles[:3], recipients[:3]))
	xFacetRecord := postMentionRecord(t, createdAt, handles[:1], recipients[:1])
	xFacets, err := json.Marshal(xFacetRecord["facets"])
	require.NoError(t, err)
	results := make(chan error, 1)
	started, finished := false, false
	t.Cleanup(func() {
		if started && !finished {
			commentErasureResult(t, ctx, results, "HandleEvent(post edit) after fixture rollback")
		}
	})
	transaction, processID := postEditRowTransaction(t, ctx, db, uri)
	_, err = transaction.ExecContext(ctx, `UPDATE posts SET content_facets = $1::jsonb WHERE uri = $2`, string(xFacets), uri)
	require.NoError(t, err)
	for _, recipient := range recipients[3:] {
		_, err = transaction.ExecContext(ctx, `INSERT INTO notifications
			(recipient_did, reason, record_uri, record_cid, actor_did, root_post_uri, record_created_at)
			VALUES ($1, 'mention', $2, $3, $4, $2, NOW())`, recipient, uri,
			"bafyreiposteditlockedcreate", pv2Author)
		require.NoError(t, err)
	}
	started = true
	go func() { results <- consumer.HandleEvent(ctx, event) }()
	commentErasureBlockedByFixture(t, ctx, transaction, processID, "FROM posts WHERE id")
	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, results, "HandleEvent(post edit)")
	finished = true
	require.Equal(t, revisions[1], readPostV2MechanismRev(t, db, uri), "fixture: edit must apply")
	requirePostMentionFacets(t, db, uri, recipients[:3]...)
	rows := postMentionRows(t, db, uri)
	require.Len(t, rows, 10, "nine committed rows leave exactly one edit slot")
	for _, recipient := range recipients[3:] {
		require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, uri, recipient))
	}
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, uri, recipients[0]),
		"X was already in facets committed under the post lock")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2 AND record_cid = $3`,
		uri, recipients[1], "bafyreiposteditlockedupdate"), "Y gets the one available slot")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, uri, recipients[2]),
		"Z follows Y in facet order and exceeds the cap")
}
