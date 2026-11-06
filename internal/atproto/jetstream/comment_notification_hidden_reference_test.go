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

// A comment recorded hidden at index time stays withdrawn after the ban that
// hid it ends: hide_reason is permanent, so fan-out that names the comment
// (its own edit's mentions, or a reply to it) notifies no one. Every other
// gate is open: the ban is lifted before the notifying write, recipients are
// indexed native users, and createdAt is inside the activation and freshness
// windows.
func TestCommentConsumer_HiddenCommentReferenceSuppressesFanout(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name       string
		hideReason string
		reply      bool // false: A edits the hidden comment to add a mention
	}{
		{name: "edit adds a mention after the account ban is lifted", hideReason: "account_ban"},
		{name: "reply to a comment hidden by an account ban", hideReason: "account_ban", reply: true},
		{name: "reply to a comment hidden by a community ban", hideReason: "community_ban", reply: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fixture, moderator := abhMentionFixture(t)
			db := fixture.gate.db
			commenter := fixture.gate.commenterDID

			// A's comment on the post is indexed while A is banned, then the
			// ban is lifted.
			if row.hideReason == "account_ban" {
				moderator.Ban(t, commenter, nil)
				fixture.create(t)
				moderator.Unban(t, commenter)
			} else {
				var community string
				require.NoError(t, db.QueryRow(`SELECT community_did FROM posts WHERE uri = $1`, fixture.post.uri).Scan(&community))
				_, err := db.Exec(`INSERT INTO community_ban_history
					(community_did, subject_did, record_key, record_cid, index_order)
					VALUES ($1, $2, 'hiddenreferenceban', 'bafyreihiddenreferenceban', nextval('coves_index_order'))`,
					community, commenter)
				require.NoError(t, err)
				fixture.create(t)
				_, err = db.Exec(`UPDATE community_ban_history SET lifted_order = nextval('coves_index_order')
					WHERE community_did = $1 AND subject_did = $2`, community, commenter)
				require.NoError(t, err)
			}
			require.Equal(t, row.hideReason, readStoredHideReason(t, db, "comments", fixture.uri).String,
				"fixture: A's comment must be recorded hidden")

			if !row.reply {
				added := fixture.recipient(t)
				require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().UnixMicro(), added))
				require.Equal(t, row.hideReason, readStoredHideReason(t, db, "comments", fixture.uri).String,
					"fixture: the edit keeps the comment hidden")
				assert.Equal(t, 0, mentionEditRows(t, fixture, added, "mention"),
					"an edit of a hidden comment adds no mention notification")
				return
			}

			var parentCID string
			require.NoError(t, db.QueryRow(`SELECT cid FROM comments WHERE uri = $1`, fixture.uri).Scan(&parentCID))
			replier := fixture.postAuthor(t)
			replyKey := testkit.TID()
			replyURI := "at://" + replier.did + "/" + CommentCollection + "/" + replyKey
			record := revCommentRecord("B replies to A's comment", fixture.post.uri, fixture.post.cid, fixture.uri, parentCID)
			record["createdAt"] = fixture.createdAt
			require.NoError(t, fixture.consumer.HandleEvent(ctx, revCommitEvent(replier.did, CommentCollection, "create", replyKey,
				testkit.TID(), "bafyreihiddenreferencereply", time.Now().UnixMicro(), record)))
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1 AND hide_reason IS NULL`, replyURI),
				"fixture: B's reply is indexed and not hidden")
			assert.Equal(t, 0, countRows(t, db, `SELECT count(*) FROM notifications
				WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'commentReply'`, replyURI, commenter),
				"a reply to a hidden comment does not notify its author")
		})
	}
}
