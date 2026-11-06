//go:build integration

package postgres

import (
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// In recipient X's own notification list, X's own content is exempt from X's
// account ban: a reply to X's comment stays listed and X's comment and thread
// root read live while X is banned. The exemption is per reference: a reply by
// another banned account Z still reads removedByServerAdmin to X. A direct
// instance removal of X's own comment is not exempt. A comment X wrote while
// banned (recorded hidden as account_ban) reads live to X after unban; one
// recorded hidden by a community ban is not exempt and reads
// removedByModerator to X.
func TestNotificationList_RecipientSelfViewReferenceStates(t *testing.T) {
	t.Parallel()
	removed, live := notifications.ReferenceRemovedByServerAdmin, notifications.ReferenceLive
	for _, tc := range []struct {
		name                         string
		recipientBanned, replierBans bool
		directRemoval                bool
		subjectAccountBanHidden      bool
		subjectCommunityBanHidden    bool
		wantRoot, wantSubject        notifications.ReferenceState
		wantRecord                   notifications.ReferenceState
	}{
		{
			name:            "Y's reply to banned X's comment",
			recipientBanned: true,
			wantRoot:        live, wantSubject: live, wantRecord: live,
		},
		{
			name:            "banned Z's reply to banned X's comment",
			recipientBanned: true, replierBans: true,
			wantRoot: live, wantSubject: live, wantRecord: removed,
		},
		{
			name:          "Y's reply to X's comment under a direct instance removal",
			directRemoval: true,
			wantRoot:      live, wantSubject: removed, wantRecord: live,
		},
		{
			name:                    "Y's reply to unbanned X's comment written during the ban",
			subjectAccountBanHidden: true,
			wantRoot:                live, wantSubject: live, wantRecord: live,
		},
		{
			name:                      "Y's reply to X's comment hidden by a community ban",
			subjectCommunityBanHidden: true,
			wantRoot:                  live, wantSubject: notifications.ReferenceRemovedByModerator, wantRecord: live,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUnreadCountFixture(t)
			replier := "did:plc:selfviewreplier" + testkit.UniqueID(t)
			subject := seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), f.sortAt)
			record := seedActorComment(t, f.db, replier, f.root, testkit.TID(), f.sortAt)
			f.listNotificationAt(t, f.recipient, replier, "commentReply", record, subject, f.root, f.sortAt.Add(time.Second))
			if tc.recipientBanned {
				seedNotificationAccountBan(t, f.db, f.recipient)
			}
			if tc.replierBans {
				seedNotificationAccountBan(t, f.db, replier)
			}
			if tc.subjectAccountBanHidden {
				markCommentAccountBanHidden(t, f.db, subject)
			}
			if tc.subjectCommunityBanHidden {
				_, err := f.db.ExecContext(t.Context(), `UPDATE comments SET hide_reason = 'community_ban' WHERE uri = $1`, subject)
				require.NoError(t, err)
			}
			if tc.directRemoval {
				seedModerationDecision(t, f.db, subject, "removal", "", true)
			}

			page := f.listPage(t, f.recipient, "", 10)
			require.Len(t, page.Notifications, 1, "the reply to X's comment stays listed to X")
			got := page.Notifications[0]
			require.Equal(t, record, got.RecordURI)
			assert.Equal(t, tc.wantRoot, got.RootPost.State, "rootPost state (X's post)")
			assert.Equal(t, tc.wantSubject, got.Subject.State, "subject state (X's comment)")
			assert.Equal(t, tc.wantRecord, got.Record.State, "record state (the reply)")
		})
	}
}
