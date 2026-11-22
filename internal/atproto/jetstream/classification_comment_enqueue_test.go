//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/classification"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Comment classification through the comment consumer and the worker
// (docs/PRD_CONTENT_CLASSIFICATION.md, chunk 03): a comment indexed through
// the consumer gets a job in the indexing transaction, the worker defers it
// until its root post is admitted, and the request carries the comment body
// alone.

const (
	clsCommenter       = accPrefix + "commenter"
	clsCommenterHandle = "acccommenter.test"
)

func clsCommentURI(rkey string) string {
	return "at://" + clsCommenter + "/" + CommentCollection + "/" + rkey
}

// clsCommentRecord is a top-level comment on the post at (postURI, postCID).
func clsCommentRecord(body, postURI, postCID string) map[string]interface{} {
	return revCommentRecord(body, postURI, postCID, postURI, postCID)
}

func newClsCommentFixture(t *testing.T) *clsFixture {
	t.Helper()
	f := newClsFixture(t)
	insertBridgedUser(t, f.db, clsCommenter, clsCommenterHandle)
	return f
}

// comment consumes a comment create or update by clsCommenter for rkey at cid
// through the classification-wired comment consumer.
func (f *clsFixture) comment(t *testing.T, op, rkey, cid string, record map[string]interface{}) string {
	t.Helper()
	f.timeUS += 1_000_000
	require.NoError(t, f.comments.HandleEvent(t.Context(),
		revCommitEvent(clsCommenter, CommentCollection, op, rkey, testkit.TID(), cid, f.timeUS, record)))
	return clsCommentURI(rkey)
}

// runWorker runs worker once over the queue. A run error is reported and the
// test goes on, so the job states it left are asserted too.
func (f *clsFixture) runWorker(t *testing.T, worker *classification.Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	assert.NoError(t, worker.RunOnce(ctx))
}

// jobStatus is the status of the job of (uri, cid), or "" when there is none.
func (f *clsFixture) jobStatus(t *testing.T, uri, cid string) string {
	t.Helper()
	var status string
	err := f.db.QueryRowContext(t.Context(),
		`SELECT status FROM classification_jobs WHERE subject_uri = $1 AND record_cid = $2`, uri, cid).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err)
	return status
}

type clsResult struct {
	SubjectKind  string
	SubjectCID   string
	AuthorDID    string
	CommunityDID string
}

// results reads every result row stored for uri.
func (f *clsFixture) results(t *testing.T, uri string) []clsResult {
	t.Helper()
	rows, err := f.db.QueryContext(t.Context(), `
		SELECT subject_kind, subject_cid, author_did, community_did
		FROM classification_results WHERE subject_uri = $1 ORDER BY id
	`, uri)
	require.NoError(t, err)
	defer rows.Close()
	var results []clsResult
	for rows.Next() {
		var row clsResult
		require.NoError(t, rows.Scan(&row.SubjectKind, &row.SubjectCID, &row.AuthorDID, &row.CommunityDID))
		results = append(results, row)
	}
	require.NoError(t, rows.Err())
	return results
}

// TestClassificationAcceptance_CommentIsDeferredUntilRootAdmittedThenClassified
// is the chunk's acceptance test (A1).
func TestClassificationAcceptance_CommentIsDeferredUntilRootAdmittedThenClassified(t *testing.T) {
	t.Parallel()

	t.Run("root post pending admission", func(t *testing.T) {
		t.Parallel()
		f := newClsCommentFixture(t)
		const postCID, commentCID = "bafyclscmtpendingpost", "bafyclscmtpendingcomment"
		postURI := f.commit(t, "create", "clscmtpending", postCID, clsRecord("Otter census", "Counted nine otters at the pier"))
		commentURI := f.comment(t, "create", "clscmtpendingc", commentCID,
			clsCommentRecord("Nine seems low for this time of year", postURI, postCID))
		assert.Equal(t, "queued", f.jobStatus(t, commentURI, commentCID), "the comment create enqueues a job")

		runAt := time.Now()
		f.runWorker(t, f.worker)
		assert.Equal(t, "deferred", f.jobStatus(t, commentURI, commentCID), "a comment under a pending post is deferred")
		assert.Empty(t, f.transport.Requests(), "a deferred job makes no request")
		var deferredUntil time.Time
		if err := f.db.QueryRowContext(t.Context(),
			`SELECT next_attempt_at FROM classification_jobs WHERE subject_uri = $1`, commentURI).Scan(&deferredUntil); err == nil {
			assert.False(t, deferredUntil.Before(runAt.Add(10*time.Minute-time.Second)),
				"next_attempt_at %s must be at least run time + PendingDeferral", deferredUntil)
		}

		// Before the deferral expires: only the admission's re-arm makes it due.
		f.accept(t, "create", postURI, postCID)
		f.runWorker(t, f.worker)
		assert.Equal(t, "classified", f.jobStatus(t, commentURI, commentCID), "the root's admission re-arms the comment")
		assert.ElementsMatch(t, []string{
			"Otter census\n\nCounted nine otters at the pier",
			"Nine seems low for this time of year",
		}, f.states(t), "the comment's request carries its body alone")
		assert.Equal(t, []clsResult{{
			SubjectKind:  "comment",
			SubjectCID:   "bafyclscmtpendingcomment",
			AuthorDID:    "did:plc:acccommenter",
			CommunityDID: "did:plc:acccommunity",
		}}, f.results(t, commentURI))
		f.assertNoIdentifiers(t, postURI, clsCommenter, commentURI, clsCommenterHandle)
	})

	t.Run("root post pending re-acceptance", func(t *testing.T) {
		t.Parallel()
		f := newClsCommentFixture(t)
		const cidA, cidB, commentCID = "bafyclscmtreaccepta", "bafyclscmtreacceptb", "bafyclscmtreacceptcomment"
		postURI := f.commit(t, "create", "clscmtreaccept", cidA, clsRecord("Heron nest", "The heron returned to the old nest"))
		f.accept(t, "create", postURI, cidA)
		f.run(t)
		require.Equal(t, "classified", f.job(t, postURI, cidA).Status, "fixture: the post is classified before the edit")

		f.commit(t, "update", "clscmtreaccept", cidB, clsRecord("Heron nest", "Two heron chicks hatched this week"))
		admission, err := f.admissions.Get(t.Context(), accCommunity, postURI)
		require.NoError(t, err)
		require.Equal(t, posts.AdmissionStatusPendingReacceptance, admission.Status, "fixture: the edit reopens the admission")

		commentURI := f.comment(t, "create", "clscmtreacceptc", commentCID,
			clsCommentRecord("Lovely news about the chicks", postURI, cidB))
		f.runWorker(t, f.worker)
		assert.Equal(t, "deferred", f.jobStatus(t, commentURI, commentCID), "a comment under a post awaiting re-acceptance is deferred")
		assert.Len(t, f.transport.Requests(), 1, "no request for the comment before re-acceptance")

		f.accept(t, "update", postURI, cidB)
		f.runWorker(t, f.worker)
		assert.Equal(t, "classified", f.jobStatus(t, commentURI, commentCID), "the root's re-acceptance re-arms the comment")
		assert.ElementsMatch(t, []string{
			"Heron nest\n\nThe heron returned to the old nest",
			"Heron nest\n\nTwo heron chicks hatched this week",
			"Lovely news about the chicks",
		}, f.states(t), "the comment's request carries its body alone")
		assert.Equal(t, []clsResult{{
			SubjectKind:  "comment",
			SubjectCID:   "bafyclscmtreacceptcomment",
			AuthorDID:    "did:plc:acccommenter",
			CommunityDID: "did:plc:acccommunity",
		}}, f.results(t, commentURI))
		f.assertNoIdentifiers(t, postURI, clsCommenter, commentURI, clsCommenterHandle)
	})

	t.Run("comment before its root post is indexed", func(t *testing.T) {
		t.Parallel()
		f := newClsCommentFixture(t)
		const postCID, commentCID = "bafyclscmtearlypost", "bafyclscmtearlycomment"
		postURI := accPostURI("clscmtearly")
		commentURI := f.comment(t, "create", "clscmtearlyc", commentCID,
			clsCommentRecord("First to the kelp thread", postURI, postCID))
		assert.Equal(t, "queued", f.jobStatus(t, commentURI, commentCID), "the comment create enqueues a job")

		f.runWorker(t, f.worker)
		assert.Equal(t, "deferred", f.jobStatus(t, commentURI, commentCID), "a comment whose root is not indexed is deferred")
		assert.Empty(t, f.transport.Requests(), "a deferred job makes no request")

		f.commit(t, "create", "clscmtearly", postCID, clsRecord("Kelp forest", "The kelp grew a metre in a month"))
		f.accept(t, "create", postURI, postCID)
		f.runWorker(t, f.newWorker(func() time.Time { return time.Now().Add(11 * time.Minute) }))
		assert.Equal(t, "classified", f.jobStatus(t, commentURI, commentCID), "after the deferral the comment is classified")
		assert.ElementsMatch(t, []string{
			"Kelp forest\n\nThe kelp grew a metre in a month",
			"First to the kelp thread",
		}, f.states(t), "the comment's request carries its body alone")
		assert.Equal(t, []clsResult{{
			SubjectKind:  "comment",
			SubjectCID:   "bafyclscmtearlycomment",
			AuthorDID:    "did:plc:acccommenter",
			CommunityDID: "did:plc:acccommunity",
		}}, f.results(t, commentURI), "the result names the community the root post resolved to")
		f.assertNoIdentifiers(t, postURI, clsCommenter, commentURI, clsCommenterHandle)
	})
}

// clsCommentEmbedRecord is clsCommentRecord with a quote embed of the post at
// (quotedURI, quotedCID).
func clsCommentEmbedRecord(body, postURI, postCID, quotedURI, quotedCID string) map[string]interface{} {
	record := clsCommentRecord(body, postURI, postCID)
	record["embed"] = map[string]interface{}{
		"$type": "social.coves.embed.post",
		"post":  map[string]interface{}{"uri": quotedURI, "cid": quotedCID},
	}
	return record
}

// clsCommentLabelledRecord is clsCommentRecord with langs and a self-label:
// fields the classifier is not asked about.
func clsCommentLabelledRecord(body, postURI, postCID string) map[string]interface{} {
	record := clsCommentRecord(body, postURI, postCID)
	record["langs"] = []interface{}{"en"}
	record["labels"] = map[string]interface{}{
		"$type":  "com.atproto.label.defs#selfLabels",
		"values": []interface{}{map[string]interface{}{"val": "nsfw"}},
	}
	return record
}

// TestClassificationEnqueue_Comment: which comment writes enqueue a job (C1).
func TestClassificationEnqueue_Comment(t *testing.T) {
	t.Parallel()

	t.Run("redelivered create", func(t *testing.T) {
		t.Parallel()
		f := newClsCommentFixture(t)
		postURI := f.commit(t, "create", "clscmtredeliver", "bafyclscmtredeliverpost", clsRecord("Tide pools", "Anemones closed as the tide went out"))
		event := revCommitEvent(clsCommenter, CommentCollection, "create", "clscmtredeliverc", testkit.TID(),
			"bafyclscmtredelivercomment", time.Now().UnixMicro(),
			clsCommentRecord("They open again at high tide", postURI, "bafyclscmtredeliverpost"))
		require.NoError(t, f.comments.HandleEvent(t.Context(), event))
		require.NoError(t, f.comments.HandleEvent(t.Context(), event))
		assert.Equal(t, 1, f.jobCount(t, clsCommentURI("clscmtredeliverc")), "a redelivered create enqueues one job")
	})

	// Edits of a comment whose job is terminal (classified).
	for _, row := range []struct {
		name    string
		rkey    string
		edited  func(postURI, postCID string) map[string]interface{}
		wantJob bool
	}{
		{
			name: "body-only edit",
			rkey: "clscmtbody",
			edited: func(postURI, postCID string) map[string]interface{} {
				return clsCommentRecord("Gulls moved to the east jetty", postURI, postCID)
			},
			wantJob: true,
		},
		{
			name: "embed-only edit",
			rkey: "clscmtembed",
			edited: func(postURI, postCID string) map[string]interface{} {
				return clsCommentEmbedRecord("Gulls roost on the breakwater", postURI, postCID,
					"at://did:plc:accquoted/social.coves.community.postv2/3kquoted", "bafyclscmtquoted")
			},
			wantJob: true,
		},
		{
			name: "langs and self-label only edit",
			rkey: "clscmtlabel",
			edited: func(postURI, postCID string) map[string]interface{} {
				return clsCommentLabelledRecord("Gulls roost on the breakwater", postURI, postCID)
			},
			wantJob: false,
		},
	} {
		t.Run(row.name+" after classification", func(t *testing.T) {
			t.Parallel()
			f := newClsCommentFixture(t)
			postCID := "bafy" + row.rkey + "post"
			cidA, cidB := "bafy"+row.rkey+"a", "bafy"+row.rkey+"b"
			postURI := f.commit(t, "create", row.rkey, postCID, clsRecord("Gull roost", "Gulls roosted on the breakwater"))
			f.accept(t, "create", postURI, postCID)
			commentURI := f.comment(t, "create", row.rkey+"c", cidA, clsCommentRecord("Gulls roost on the breakwater", postURI, postCID))
			f.runWorker(t, f.worker)
			assert.Equal(t, "classified", f.jobStatus(t, commentURI, cidA), "the comment's create job is classified before the edit")

			f.comment(t, "update", row.rkey+"c", cidB, row.edited(postURI, postCID))
			var storedCID string
			require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT cid FROM comments WHERE uri = $1`, commentURI).Scan(&storedCID))
			require.Equal(t, cidB, storedCID, "fixture: the edit applies")
			if row.wantJob {
				assert.Equal(t, "queued", f.jobStatus(t, commentURI, cidB), "a classified-field edit queues the new CID")
			} else {
				assert.Equal(t, "", f.jobStatus(t, commentURI, cidB), "an edit of unclassified fields queues nothing")
			}
		})
	}

	// Re-creates (a newer-rev create at the same rkey, threading unchanged) of an
	// active comment whose job is terminal (classified) apply content in place
	// like an edit, and follow the same enqueue rule.
	for _, row := range []struct {
		name    string
		rkey    string
		body    string
		wantJob bool
	}{
		{name: "changed body", rkey: "clscmtrecbody", body: "Gulls moved to the east jetty", wantJob: true},
		{name: "unchanged body", rkey: "clscmtrecsame", body: "Gulls roost on the breakwater", wantJob: false},
	} {
		t.Run("re-create with "+row.name+" after classification", func(t *testing.T) {
			t.Parallel()
			f := newClsCommentFixture(t)
			postCID := "bafy" + row.rkey + "post"
			cidA, cidB := "bafy"+row.rkey+"a", "bafy"+row.rkey+"b"
			postURI := f.commit(t, "create", row.rkey, postCID, clsRecord("Gull roost", "Gulls roosted on the breakwater"))
			f.accept(t, "create", postURI, postCID)
			commentURI := f.comment(t, "create", row.rkey+"c", cidA, clsCommentRecord("Gulls roost on the breakwater", postURI, postCID))
			f.runWorker(t, f.worker)
			assert.Equal(t, "classified", f.jobStatus(t, commentURI, cidA), "the comment's create job is classified before the re-create")

			f.comment(t, "create", row.rkey+"c", cidB, clsCommentLabelledRecord(row.body, postURI, postCID))
			var storedCID, storedContent string
			require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT cid, content FROM comments WHERE uri = $1`, commentURI).Scan(&storedCID, &storedContent))
			require.Equal(t, cidB, storedCID, "fixture: the re-create applies")
			require.Equal(t, row.body, storedContent, "fixture: the re-create applies")
			if row.wantJob {
				assert.Equal(t, "queued", f.jobStatus(t, commentURI, cidB), "a re-create with a changed body queues the new CID")
			} else {
				assert.Equal(t, "", f.jobStatus(t, commentURI, cidB), "a re-create with unchanged content and embed queues nothing")
			}
		})
	}

	t.Run("langs and self-label only edit while the job is queued", func(t *testing.T) {
		t.Parallel()
		f := newClsCommentFixture(t)
		const postCID, cidA, cidB = "bafyclscmtqueuedpost", "bafyclscmtqueueda", "bafyclscmtqueuedb"
		postURI := f.commit(t, "create", "clscmtqueued", postCID, clsRecord("Crab moult", "Shore crabs shed their shells"))
		f.accept(t, "create", postURI, postCID)
		commentURI := f.comment(t, "create", "clscmtqueuedc", cidA, clsCommentRecord("Found three shells today", postURI, postCID))
		f.comment(t, "update", "clscmtqueuedc", cidB, clsCommentLabelledRecord("Found three shells today", postURI, postCID))
		assert.Equal(t, "queued", f.jobStatus(t, commentURI, cidB), "a queued predecessor carries the queue to the new CID")

		f.runWorker(t, f.worker)
		assert.Equal(t, "closed", f.jobStatus(t, commentURI, cidA), "the old CID's job is closed")
		if f.jobStatus(t, commentURI, cidA) != "" {
			assert.Equal(t, sql.NullString{String: "superseded", Valid: true}, f.job(t, commentURI, cidA).CloseReason,
				"the old CID's job is superseded")
		}
		assert.Equal(t, "classified", f.jobStatus(t, commentURI, cidB))
		assert.ElementsMatch(t, []string{
			"Crab moult\n\nShore crabs shed their shells",
			"Found three shells today",
		}, f.states(t))
	})

	t.Run("no comment enqueuer installed", func(t *testing.T) {
		t.Parallel()
		f := newClsCommentFixture(t)
		postURI := f.commit(t, "create", "clscmtnooption", "bafyclscmtnooptionpost", clsRecord("Sand dunes", "The dunes shifted after the storm"))
		plain := newCommentConsumer(f.db)
		require.NoError(t, plain.HandleEvent(t.Context(), revCommitEvent(clsCommenter, CommentCollection, "create",
			"clscmtnooptionc", testkit.TID(), "bafyclscmtnooptioncomment", time.Now().UnixMicro(),
			clsCommentRecord("Marram grass holds them in place", postURI, "bafyclscmtnooptionpost"))))
		commentURI := clsCommentURI("clscmtnooptionc")
		require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI), "fixture: the comment is indexed")
		assert.Equal(t, 0, f.jobCount(t, commentURI), "a consumer without the option enqueues nothing")
	})
}

// failingClassificationEnqueuer refuses every enqueue.
type failingClassificationEnqueuer struct{}

func (failingClassificationEnqueuer) EnqueueInTx(context.Context, *sql.Tx, classification.NewJob, bool) error {
	return errors.New("classification enqueue refused")
}

func (failingClassificationEnqueuer) ErasureGateTx(context.Context, *sql.Tx, string) (bool, error) {
	return false, nil
}

// TestClassificationEnqueue_FailureRollsBackIndexing: the comment job is
// inserted in the indexing transaction, so a failing enqueue rolls the
// comment write back (T1, comment rows).
func TestClassificationEnqueue_FailureRollsBackIndexing(t *testing.T) {
	t.Parallel()
	evaluation := classification.Evaluation{
		PolicyVersion: "v1",
		TextModelID:   "@cf/cloudflare/clef-flash",
		ImageModelID:  "@cf/cloudflare/clef",
	}
	const postCID = "bafyclscmtfailpost"
	postURI := accPostURI("clscmtfail")

	t.Run("comment create", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		failing := newCommentConsumer(db, WithCommentClassificationEnqueuer(failingClassificationEnqueuer{}, evaluation))
		err := failing.HandleEvent(t.Context(), revCommitEvent(clsCommenter, CommentCollection, "create",
			"clscmtfailcreate", testkit.TID(), "bafyclscmtfailcreate", time.Now().UnixMicro(),
			clsCommentRecord("Rolled back with its job", postURI, postCID)))
		assert.Error(t, err, "a failing enqueue fails the create")
		assert.Equal(t, 0, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, clsCommentURI("clscmtfailcreate")),
			"the comment row is rolled back with the failed enqueue")
	})

	t.Run("comment update", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		createdAt := time.Now().UnixMicro()
		require.NoError(t, newCommentConsumer(db).HandleEvent(t.Context(), revCommitEvent(clsCommenter, CommentCollection, "create",
			"clscmtfailupdate", testkit.TID(), "bafyclscmtfailupdatea", createdAt,
			clsCommentRecord("The original comment text", postURI, postCID))), "fixture: the comment is indexed")

		failing := newCommentConsumer(db, WithCommentClassificationEnqueuer(failingClassificationEnqueuer{}, evaluation))
		err := failing.HandleEvent(t.Context(), revCommitEvent(clsCommenter, CommentCollection, "update",
			"clscmtfailupdate", testkit.TID(), "bafyclscmtfailupdateb", createdAt+1_000_000,
			clsCommentRecord("The edited comment text", postURI, postCID)))
		assert.Error(t, err, "a failing enqueue fails the update")

		var cid, content string
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT cid, content FROM comments WHERE uri = $1`, clsCommentURI("clscmtfailupdate")).Scan(&cid, &content))
		assert.Equal(t, "bafyclscmtfailupdatea", cid, "the update is rolled back with the failed enqueue")
		assert.Equal(t, "The original comment text", content, "the update is rolled back with the failed enqueue")
	})
}
