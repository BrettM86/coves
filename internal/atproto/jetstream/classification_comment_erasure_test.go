//go:build integration

package jetstream

import (
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clsControlCommenter is a commenter who is never erased.
const clsControlCommenter = accPrefix + "controlcommenter"

// controlComment consumes a comment create or update by clsControlCommenter
// for rkey at cid through the classification-wired comment consumer.
func (f *clsFixture) controlComment(t *testing.T, op, rkey, cid string, record map[string]interface{}) string {
	t.Helper()
	f.timeUS += 1_000_000
	require.NoError(t, f.comments.HandleEvent(t.Context(),
		revCommitEvent(clsControlCommenter, CommentCollection, op, rkey, testkit.TID(), cid, f.timeUS, record)))
	return "at://" + clsControlCommenter + "/" + CommentCollection + "/" + rkey
}

// TestClassificationEnqueue_CommentByErasedAuthor: with the comment
// classification enqueuer wired and no notifications repository, a comment
// create or update from an author with an erasure marker enqueues no job,
// while the same events from a commenter who was not erased do (E1).
func TestClassificationEnqueue_CommentByErasedAuthor(t *testing.T) {
	t.Parallel()
	markErased := func(t *testing.T, f *clsFixture) {
		t.Helper()
		_, err := f.db.ExecContext(t.Context(),
			`INSERT INTO deleted_accounts (did, deleted_at) VALUES ($1, NOW())`, clsCommenter)
		require.NoError(t, err)
	}
	newFixture := func(t *testing.T) (*clsFixture, string, string) {
		t.Helper()
		f := newClsCommentFixture(t)
		insertBridgedUser(t, f.db, clsControlCommenter, "acccontrolcommenter.test")
		const postCID = "bafyclserasedcmtpost"
		postURI := f.commit(t, "create", "clserasedcmtpost", postCID, clsRecord("Kelp forest", "The kelp forest thinned after the storm"))
		f.accept(t, "create", postURI, postCID)
		return f, postURI, postCID
	}

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		f, postURI, postCID := newFixture(t)
		markErased(t, f)

		erasedURI := f.comment(t, "create", "clserasedcmtc", "bafyclserasedcmtc",
			clsCommentRecord("Urchins moved into the gaps", postURI, postCID))
		controlURI := f.controlComment(t, "create", "clserasedctlc", "bafyclserasedctlc",
			clsCommentRecord("Otters are eating the urchins", postURI, postCID))

		assert.Equal(t, 0, f.jobCount(t, erasedURI), "an erased commenter's create enqueues no job")
		assert.Equal(t, 1, f.jobCount(t, controlURI), "a create by a commenter who was not erased enqueues a job")
	})

	t.Run("update", func(t *testing.T) {
		t.Parallel()
		f, postURI, postCID := newFixture(t)
		erasedURI := f.comment(t, "create", "clserasedcmtu", "bafyclserasedcmtua",
			clsCommentRecord("Sea stars returned to the reef", postURI, postCID))
		controlURI := f.controlComment(t, "create", "clserasedctlu", "bafyclserasedctlua",
			clsCommentRecord("Sea stars are back on the pilings", postURI, postCID))
		// Erasure removes the commenter's jobs and writes the marker; the
		// comment row is left so the late update reaches the consumer.
		_, err := f.db.ExecContext(t.Context(), `DELETE FROM classification_jobs WHERE author_did = $1`, clsCommenter)
		require.NoError(t, err)
		markErased(t, f)

		f.comment(t, "update", "clserasedcmtu", "bafyclserasedcmtub",
			clsCommentRecord("Sea stars spread along the whole reef", postURI, postCID))
		f.controlComment(t, "update", "clserasedctlu", "bafyclserasedctlub",
			clsCommentRecord("Sea stars spread along every piling", postURI, postCID))

		assert.Equal(t, 0, f.jobCount(t, erasedURI), "an erased commenter's update enqueues no job")
		assert.Equal(t, 2, f.jobCount(t, controlURI), "a create and an update by a commenter who was not erased each enqueue a job")
	})
}
