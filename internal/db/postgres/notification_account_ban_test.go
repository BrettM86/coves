//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedNotificationAccountBan records an active permanent instance account ban
// on accountDID in the shape BanAccount writes (an action with no subject
// collection and the instance removal decision it made active), and returns
// the unban that makes the decision inactive. tests/fixtures/accountbanfixture
// cannot be imported here: it imports this package.
func seedNotificationAccountBan(t *testing.T, db *sql.DB, accountDID string) (unban func()) {
	t.Helper()
	subject := "at://" + accountDID
	actionID := "notification-account-ban-" + testkit.TID()
	_, err := db.ExecContext(context.Background(), `INSERT INTO moderation_actions
		(id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, subject_kind, action, reason, origin, created_at)
		VALUES ($1, 'did:plc:notificationmoderator', $2, 'instance', $3, NULL, 'account', 'remove',
		        'social.coves.moderation.defs#reasonSpam', 'local', NOW())`,
		actionID, fixtures.InstanceDID(), subject)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO moderation_decisions
		(authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'instance', NULL, $2, 'removal', NULL, $3, TRUE)`,
		fixtures.InstanceDID(), subject, actionID)
	require.NoError(t, err)
	return func() {
		t.Helper()
		_, err := db.ExecContext(context.Background(), `UPDATE moderation_decisions SET active = FALSE
			WHERE subject_uri = $1 AND scope_kind = 'instance' AND kind = 'removal'`, subject)
		require.NoError(t, err)
	}
}

// markCommentAccountBanHidden records the comment as hidden at index time by
// an account ban, as the comment consumer does for a comment written during
// the ban.
func markCommentAccountBanHidden(t *testing.T, db *sql.DB, uri string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE comments SET hide_reason = 'account_ban' WHERE uri = $1`, uri)
	require.NoError(t, err)
}

// TestNotificationList_AccountBanReferenceStates lists, for recipient B, one
// notification per way a banned account A's content can be referenced. While
// A is banned the reference to A's content reads removedByServerAdmin; after
// unban it reads live, except a comment A wrote during the ban (recorded
// hidden as account_ban), which keeps reading removedByServerAdmin. Only the
// references named per row are asserted.
func TestNotificationList_AccountBanReferenceStates(t *testing.T) {
	t.Parallel()
	removed, live := notifications.ReferenceRemovedByServerAdmin, notifications.ReferenceLive
	state := func(value notifications.ReferenceState) *notifications.ReferenceState { return &value }
	type references struct{ reason, record, subject, root string }
	for _, tc := range []struct {
		name string
		// seed returns the notification's references given the fixture, the
		// banned account A and another unbanned account C.
		seed                              func(t *testing.T, f *unreadVisibilityFixture, banned, other string) references
		wantRoot, wantSubject, wantRecord *notifications.ReferenceState
		// stillRemovedAfterUnban: the asserted references keep their
		// during-ban states after unban.
		stillRemovedAfterUnban bool
	}{
		{
			name: "reply whose record is A's comment",
			seed: func(t *testing.T, f *unreadVisibilityFixture, banned, _ string) references {
				return references{"postReply", seedActorComment(t, f.db, banned, f.root, testkit.TID(), f.sortAt), f.root, f.root}
			},
			wantRoot: state(live), wantSubject: state(live), wantRecord: state(removed),
		},
		{
			name: "mention from A's post",
			seed: func(t *testing.T, f *unreadVisibilityFixture, banned, _ string) references {
				post := seedVisibilityPost(t, f.db, f.community, banned, testkit.TID(), "a post by the banned account", f.sortAt)
				seedVisibilityAdmission(t, f.db, f.community, post, posts.AdmissionStatusAccepted, "", "")
				return references{"mention", post, "", f.root}
			},
			wantRoot: state(live), wantRecord: state(removed),
		},
		{
			name: "C's reply in a thread whose root post is A's",
			seed: func(t *testing.T, f *unreadVisibilityFixture, banned, other string) references {
				root := seedVisibilityPost(t, f.db, f.community, banned, testkit.TID(), "a thread the banned account started", f.sortAt)
				seedVisibilityAdmission(t, f.db, f.community, root, posts.AdmissionStatusAccepted, "", "")
				subject := seedActorComment(t, f.db, f.recipient, root, testkit.TID(), f.sortAt)
				return references{"commentReply", seedActorComment(t, f.db, other, root, testkit.TID(), f.sortAt), subject, root}
			},
			wantRoot: state(removed),
		},
		{
			name: "C's reply whose subject is A's comment",
			seed: func(t *testing.T, f *unreadVisibilityFixture, banned, other string) references {
				subject := seedActorComment(t, f.db, banned, f.root, testkit.TID(), f.sortAt)
				return references{"commentReply", seedActorComment(t, f.db, other, f.root, testkit.TID(), f.sortAt), subject, f.root}
			},
			wantRoot: state(live), wantSubject: state(removed), wantRecord: state(live),
		},
		{
			name: "C's reply whose subject is A's comment written during the ban",
			seed: func(t *testing.T, f *unreadVisibilityFixture, banned, other string) references {
				subject := seedActorComment(t, f.db, banned, f.root, testkit.TID(), f.sortAt)
				markCommentAccountBanHidden(t, f.db, subject)
				return references{"commentReply", seedActorComment(t, f.db, other, f.root, testkit.TID(), f.sortAt), subject, f.root}
			},
			wantRoot: state(live), wantSubject: state(removed), wantRecord: state(live),
			stillRemovedAfterUnban: true,
		},
		{
			name: "reply whose record is A's comment written during the ban",
			seed: func(t *testing.T, f *unreadVisibilityFixture, banned, _ string) references {
				record := seedActorComment(t, f.db, banned, f.root, testkit.TID(), f.sortAt)
				markCommentAccountBanHidden(t, f.db, record)
				return references{"postReply", record, f.root, f.root}
			},
			wantRoot: state(live), wantSubject: state(live), wantRecord: state(removed),
			stillRemovedAfterUnban: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadCountFixture(t)
			banned := "did:plc:notifybanned" + testkit.UniqueID(t)
			other := "did:plc:notifyother" + testkit.UniqueID(t)
			refs := tc.seed(t, f, banned, other)
			f.listNotificationAt(t, f.recipient, banned, refs.reason, refs.record, refs.subject, refs.root, f.sortAt.Add(time.Second))
			unban := seedNotificationAccountBan(t, f.db, banned)

			listed := func(t *testing.T) notifications.ListedNotification {
				t.Helper()
				page := f.listPage(t, f.recipient, "", 10)
				require.Len(t, page.Notifications, 1, "the notification stays listed")
				require.Equal(t, refs.record, page.Notifications[0].RecordURI)
				return page.Notifications[0]
			}
			check := func(t *testing.T, got notifications.ListedNotification, root, subject, record *notifications.ReferenceState) {
				t.Helper()
				if root != nil {
					assert.Equal(t, *root, got.RootPost.State, "rootPost state")
				}
				if subject != nil {
					assert.Equal(t, *subject, got.Subject.State, "subject state")
				}
				if record != nil {
					assert.Equal(t, *record, got.Record.State, "record state")
				}
			}
			liveIfAsserted := func(want *notifications.ReferenceState) *notifications.ReferenceState {
				if want == nil {
					return nil
				}
				return state(live)
			}

			t.Run("while banned", func(t *testing.T) {
				check(t, listed(t), tc.wantRoot, tc.wantSubject, tc.wantRecord)
			})
			unban()
			t.Run("after unban", func(t *testing.T) {
				if tc.stillRemovedAfterUnban {
					check(t, listed(t), tc.wantRoot, tc.wantSubject, tc.wantRecord)
					return
				}
				check(t, listed(t), liveIfAsserted(tc.wantRoot), liveIfAsserted(tc.wantSubject), liveIfAsserted(tc.wantRecord))
			})
		})
	}
}
