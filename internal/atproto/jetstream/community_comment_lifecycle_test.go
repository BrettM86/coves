//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type communityCommentLifecycleFixture struct {
	db       *sql.DB
	consumer *CommentEventConsumer
	postURI  string
	postCID  string
}

func newCommunityCommentLifecycleFixture(t *testing.T) communityCommentLifecycleFixture {
	t.Helper()
	db := testkit.DB(t)
	insertBridgedUser(t, db, revTestAuthor, "revauthor.test")
	insertBridgedUser(t, db, revTestCommenter, "revcommenter.test")
	insertBridgedCommunity(t, db, revTestCommunity, "revcommunity.test", revTestAuthor)
	fixture := communityCommentLifecycleFixture{
		db: db, postURI: pv2URI(revTestAuthor, "lifecyclepost"), postCID: "baflifecyclepost",
		consumer: NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
			WithCommentBlobReferences(postgres.NewBlobReferenceRepository(db))),
	}
	_, err := db.ExecContext(t.Context(), `INSERT INTO posts
		(uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, 'lifecyclepost', $3, $4, 'community lifecycle', NOW())`,
		fixture.postURI, fixture.postCID, revTestAuthor, revTestCommunity)
	require.NoError(t, err)
	return fixture
}

func (fixture communityCommentLifecycleFixture) event(operation, did, key, revision string, eventTime int64, parentURI string) *JetstreamEvent {
	var record map[string]interface{}
	if operation != "delete" {
		record = revCommentRecord("community image comment", fixture.postURI, fixture.postCID, parentURI, fixture.postCID)
		record["embed"] = postBlobImages(commentImageA)
	}
	return revCommitEvent(did, CommentCollection, operation, key, revision, "baflifecyclecomment", eventTime, record)
}

func (fixture communityCommentLifecycleFixture) delete(ctx context.Context) error {
	return postgres.NewCommunityRepository(fixture.db, credentialciphertest.Fixed()).Delete(ctx, revTestCommunity)
}

// Results are drained after fixture-lock rollback and before the clone closes,
// including assertion failures while a worker is waiting on a barrier.
type communityCommentLifecycleWorker struct {
	result  chan error
	started bool
	read    bool
}

func lifecycleWorker(t *testing.T, ctx context.Context) *communityCommentLifecycleWorker {
	t.Helper()
	worker := &communityCommentLifecycleWorker{result: make(chan error, 1)}
	t.Cleanup(func() {
		if worker.started && !worker.read {
			select {
			case err := <-worker.result:
				t.Logf("community lifecycle worker returned during cleanup: %v", err)
			case <-ctx.Done():
				t.Errorf("community lifecycle worker cleanup: %v", ctx.Err())
			}
		}
	})
	return worker
}

func (worker *communityCommentLifecycleWorker) start(operation func() error) {
	worker.started = true
	go func() { worker.result <- operation() }()
}

func (worker *communityCommentLifecycleWorker) finish(t *testing.T, ctx context.Context) error {
	t.Helper()
	select {
	case err := <-worker.result:
		worker.read = true
		return err
	case <-ctx.Done():
		t.Fatalf("community lifecycle worker did not finish: %v", ctx.Err())
		return ctx.Err()
	}
}

func lifecycleBarrier(t *testing.T, ctx context.Context, db *sql.DB) (*sql.Tx, int) {
	t.Helper()
	transaction, processID := commentErasureLockTransaction(t, ctx, db, revTestAuthor)
	_, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(4242, 1)`)
	require.NoError(t, err)
	return transaction, processID
}

func TestCommunityCommentLifecycle_PendingCreateCannotPublishAfterDelete(t *testing.T) {
	fixture := newCommunityCommentLifecycleFixture(t)
	_, err := fixture.db.ExecContext(t.Context(), `CREATE FUNCTION hold_community_delete() RETURNS trigger AS $$
		BEGIN PERFORM pg_advisory_xact_lock(4242, 1); RETURN OLD; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER hold_community_delete BEFORE DELETE ON communities
		FOR EACH ROW EXECUTE FUNCTION hold_community_delete()`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	deletion, creation := lifecycleWorker(t, ctx), lifecycleWorker(t, ctx)
	barrier, barrierProcessID := lifecycleBarrier(t, ctx, fixture.db)

	deletion.start(func() error { return fixture.delete(ctx) })
	deletionProcessID := commentErasureBlockedByFixture(t, ctx, barrier, barrierProcessID, "")
	key := testkit.TID()
	event := fixture.event("create", revTestCommenter, key, testkit.TID(), time.Now().UnixMicro(), fixture.postURI)
	creation.start(func() error { return fixture.consumer.HandleEvent(ctx, event) })
	// Observe a waiter on deletion, whether publication is guarded before the
	// insert or the old path has already inserted and reached the post counter.
	commentErasureBlockedByFixture(t, ctx, barrier, deletionProcessID, "")
	require.NoError(t, barrier.Commit())
	deletionError, creationError := deletion.finish(t, ctx), creation.finish(t, ctx)
	require.NoError(t, deletionError)
	require.NoError(t, creationError)
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM posts WHERE uri = $1`, fixture.postURI))
	uri := "at://" + revTestCommenter + "/" + CommentCollection + "/" + key
	require.Empty(t, activePostBlobRows(readPostBlobRows(t, fixture.db, uri)),
		"a create queued behind successful community deletion must leave no active image references")
}

func TestCommunityCommentLifecycle_NewerEditCannotReactivateAfterDelete(t *testing.T) {
	fixture := newCommunityCommentLifecycleFixture(t)
	key := testkit.TID()
	revisions := increasingTIDs(t, 2)
	base := time.Now().UnixMicro()
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), fixture.event("create", revTestCommenter, key, revisions[0], base, fixture.postURI)))
	uri := "at://" + revTestCommenter + "/" + CommentCollection + "/" + key
	require.ElementsMatch(t, []postBlobRow{{revTestCommenter, commentImageA, "comment_image", true}}, readPostBlobRows(t, fixture.db, uri))
	require.NoError(t, fixture.delete(t.Context()))
	require.ElementsMatch(t, []postBlobRow{{revTestCommenter, commentImageA, "comment_image", false}}, readPostBlobRows(t, fixture.db, uri))
	event := fixture.event("update", revTestCommenter, key, revisions[1], base+1_000_000, fixture.postURI)
	event.Commit.Record["embed"] = postBlobImages(commentImageA, commentImageB)
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), event))
	require.Empty(t, activePostBlobRows(readPostBlobRows(t, fixture.db, uri)),
		"a newer edit cannot reactivate old or new comment images after community deletion")
}

func TestCommunityCommentLifecycle_ChangedParentResurrectionDoesNotDeadlockDelete(t *testing.T) {
	fixture := newCommunityCommentLifecycleFixture(t)
	base := time.Now().UnixMicro()
	parentKey, replyKey := testkit.TID(), testkit.TID()
	parentURI := "at://" + revTestAuthor + "/" + CommentCollection + "/" + parentKey
	replyURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + replyKey
	require.Less(t, parentURI, replyURI, "the new parent's URI sorts before the resurrecting reply")
	require.NotEqual(t, revTestCommunity, revTestCommenter, "erasure locks for different accounts do not serialize this race")
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), fixture.event("create", revTestAuthor, parentKey, testkit.TID(), base, fixture.postURI)))
	revisions := increasingTIDs(t, 3)
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), fixture.event("create", revTestCommenter, replyKey, revisions[0], base+1_000_000, fixture.postURI)))
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), fixture.event("delete", revTestCommenter, replyKey, revisions[1], base+2_000_000, fixture.postURI)))
	require.NotNil(t, readCommentBlobState(t, fixture.db, replyURI).deletedAt)
	_, err := fixture.db.ExecContext(t.Context(), `CREATE FUNCTION hold_reply_resurrection() RETURNS trigger AS $$
		BEGIN
			IF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
				PERFORM pg_advisory_xact_lock(4242, 1);
			END IF;
			RETURN NEW;
		END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER hold_reply_resurrection AFTER UPDATE ON comments
		FOR EACH ROW EXECUTE FUNCTION hold_reply_resurrection()`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	resurrection, deletion := lifecycleWorker(t, ctx), lifecycleWorker(t, ctx)
	barrier, barrierProcessID := lifecycleBarrier(t, ctx, fixture.db)
	event := fixture.event("create", revTestCommenter, replyKey, revisions[2], base+3_000_000, parentURI)
	resurrection.start(func() error { return fixture.consumer.HandleEvent(ctx, event) })
	resurrectionProcessID := commentErasureBlockedByFixture(t, ctx, barrier, barrierProcessID, "")
	deletion.start(func() error { return fixture.delete(ctx) })
	// On the old path Delete has locked the earlier parent and waits for the
	// reply. A lifecycle guard may instead make it wait before touching rows.
	commentErasureBlockedByFixture(t, ctx, barrier, resurrectionProcessID, "")
	require.NoError(t, barrier.Commit())
	resurrectionError, deletionError := resurrection.finish(t, ctx), deletion.finish(t, ctx)
	require.NoError(t, resurrectionError, "changed-parent resurrection must not deadlock community deletion")
	require.NoError(t, deletionError, "community deletion must not deadlock changed-parent resurrection")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM communities WHERE did = $1`, revTestCommunity))
	require.Empty(t, activePostBlobRows(readPostBlobRows(t, fixture.db, replyURI)))
}

func TestCommunityCommentLifecycle_MerelyMissingRootStillIndexesImages(t *testing.T) {
	fixture := newCommunityCommentLifecycleFixture(t)
	fixture.postURI = pv2URI(revTestAuthor, "notyetindexed")
	key := testkit.TID()
	base := time.Now().UnixMicro()
	revisions := increasingTIDs(t, 2)
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM posts WHERE uri = $1`, fixture.postURI))
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), fixture.event("create", revTestCommenter, key, revisions[0], base, fixture.postURI)),
		"a missing root alone is ordinary out-of-order indexing")
	uri := "at://" + revTestCommenter + "/" + CommentCollection + "/" + key
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, uri))
	require.ElementsMatch(t, []postBlobRow{{revTestCommenter, commentImageA, "comment_image", true}}, readPostBlobRows(t, fixture.db, uri))
	event := fixture.event("update", revTestCommenter, key, revisions[1], base+1_000_000, fixture.postURI)
	event.Commit.Record["embed"] = postBlobImages(commentImageB)
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), event))
	require.ElementsMatch(t, []postBlobRow{
		{revTestCommenter, commentImageA, "comment_image", false},
		{revTestCommenter, commentImageB, "comment_image", true},
	}, readPostBlobRows(t, fixture.db, uri), "a newer edit also remains valid while its root has not arrived")
}
