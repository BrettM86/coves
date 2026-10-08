//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Set the fixture's current content CID explicitly so the expectations do not
// depend on its generated rkeys. Keep accepted admissions pinned to live posts.
func setListedPostCID(t *testing.T, f *unreadVisibilityFixture, uri, cid string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `UPDATE posts SET cid = $2 WHERE uri = $1`, uri, cid)
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(), `UPDATE community_post_admissions
		SET accepted_cid = $2, evaluated_cid = $2 WHERE post_uri = $1 AND status = 'accepted'`, uri, cid)
	require.NoError(t, err)
}

func setListedCommentCID(t *testing.T, f *unreadVisibilityFixture, uri, cid string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `UPDATE comments SET cid = $2 WHERE uri = $1`, uri, cid)
	require.NoError(t, err)
}

// Each case lists one notification and counts it against a seen_at just before
// it, so a row that is listed is counted exactly once and a hidden row is neither
// listed nor counted. Admin moderation decisions are seeded in main's moderation
// tables; the root post is the subject for postReply and upvote rows.
func TestNotificationList_ReferenceStatesAndCurrentCIDs(t *testing.T) {
	t.Parallel()
	serverAdminRemoved := func(cid string) notifications.ListedReference {
		return notifications.ListedReference{State: notifications.ReferenceRemovedByServerAdmin, CID: cid}
	}
	live := func(cid string) notifications.ListedReference {
		return notifications.ListedReference{State: notifications.ReferenceLive, CID: cid}
	}
	for _, tc := range []struct {
		name, reason, rootState, subjectState, recordState string
		rootCID, subjectCID, recordCID                     string
		hidden                                             bool
		wantRoot, wantSubject, wantRecord                  notifications.ListedReference
	}{
		{
			name: "author-deleted postReply subject and root", reason: "postReply", rootState: "authorDeleted",
			rootCID: "bafydeletedpostcurrent", subjectCID: "bafydeletedpostcurrent", recordCID: "bafydeletedpostreplycurrent",
			wantRoot:    notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeletedpostcurrent"},
			wantSubject: notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeletedpostcurrent"},
			wantRecord:  notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafydeletedpostreplycurrent"},
		},
		{
			name: "community-removed root and deleted comment subject", reason: "commentReply", rootState: "communityRemoved", subjectState: "deleted",
			rootCID: "bafyremovedrootcurrent", subjectCID: "bafydeletedsubjectcurrent", recordCID: "bafyremovedrootreplycurrent",
			wantRoot:    notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyremovedrootcurrent"},
			wantSubject: notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeletedsubjectcurrent"},
			wantRecord:  notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyremovedrootreplycurrent"},
		},
		{
			name: "community-removed and author-deleted postReply root", reason: "postReply", rootState: "communityRemovedAndAuthorDeleted",
			rootCID: "bafyremoveddeletedcurrent", subjectCID: "bafyremoveddeletedcurrent", recordCID: "bafyremoveddeletedreplycurrent",
			wantRoot:    notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyremoveddeletedcurrent"},
			wantSubject: notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyremoveddeletedcurrent"},
			wantRecord:  notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyremoveddeletedreplycurrent"},
		},
		{
			name: "all references live", reason: "commentReply",
			rootCID: "bafyliverootcurrent", subjectCID: "bafylivesubjectcurrent", recordCID: "bafylivereplycurrent",
			wantRoot:    notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyliverootcurrent"},
			wantSubject: notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylivesubjectcurrent"},
			wantRecord:  notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylivereplycurrent"},
		},
		{
			name: "server-admin-removed postReply subject", reason: "postReply", rootState: "instanceRemoved",
			rootCID: "bafyadminremovedpost", subjectCID: "bafyadminremovedpost", recordCID: "bafyadminremovedpostreply",
			wantRoot: serverAdminRemoved("bafyadminremovedpost"), wantSubject: serverAdminRemoved("bafyadminremovedpost"),
			wantRecord: serverAdminRemoved("bafyadminremovedpostreply"),
		},
		{
			name: "server-admin-removed upvote subject", reason: "upvote", rootState: "instanceRemoved",
			rootCID: "bafyadminremovedupvoted", subjectCID: "bafyadminremovedupvoted",
			wantRoot: serverAdminRemoved("bafyadminremovedupvoted"), wantSubject: serverAdminRemoved("bafyadminremovedupvoted"),
		},
		{
			name: "community-scope-only removal of postReply subject", reason: "postReply", rootState: "communityScopeRemoved",
			rootCID: "bafyscoperemovedpost", subjectCID: "bafyscoperemovedpost", recordCID: "bafyscoperemovedpostreply",
			wantRoot:    notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyscoperemovedpost"},
			wantSubject: notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyscoperemovedpost"},
			wantRecord:  notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyscoperemovedpostreply"},
		},
		{
			name: "instance and community-scope removals of postReply subject", reason: "postReply", rootState: "instanceAndCommunityScopeRemoved",
			rootCID: "bafybothremovedpost", subjectCID: "bafybothremovedpost", recordCID: "bafybothremovedpostreply",
			wantRoot: serverAdminRemoved("bafybothremovedpost"), wantSubject: serverAdminRemoved("bafybothremovedpost"),
			wantRecord: serverAdminRemoved("bafybothremovedpostreply"),
		},
		{
			name: "server-admin removal of community-withdrawn postReply subject", reason: "postReply", rootState: "communityRemovedAndInstanceRemoved",
			rootCID: "bafywithdrawnadminpost", subjectCID: "bafywithdrawnadminpost", recordCID: "bafywithdrawnadminpostreply",
			wantRoot: serverAdminRemoved("bafywithdrawnadminpost"), wantSubject: serverAdminRemoved("bafywithdrawnadminpost"),
			wantRecord: serverAdminRemoved("bafywithdrawnadminpostreply"),
		},
		{
			name: "inactive server-admin removal of postReply subject", reason: "postReply", rootState: "instanceRemovalInactive",
			rootCID: "bafyrestoredpost", subjectCID: "bafyrestoredpost", recordCID: "bafyrestoredpostreply",
			wantRoot: live("bafyrestoredpost"), wantSubject: live("bafyrestoredpost"), wantRecord: live("bafyrestoredpostreply"),
		},
		{
			name: "label-only decision on postReply subject", reason: "postReply", rootState: "instanceLabel",
			rootCID: "bafylabeledpost", subjectCID: "bafylabeledpost", recordCID: "bafylabeledpostreply",
			wantRoot: live("bafylabeledpost"), wantSubject: live("bafylabeledpost"), wantRecord: live("bafylabeledpostreply"),
		},
		{
			name: "pending postReply subject with server-admin removal", reason: "postReply", rootState: "pendingInstanceRemoved",
			rootCID: "bafypendingadminpost", subjectCID: "bafypendingadminpost", recordCID: "bafypendingadminpostreply",
			hidden: true,
		},
		{
			name: "author-deleted postReply subject with server-admin removal", reason: "postReply", rootState: "authorDeletedAndInstanceRemoved",
			rootCID: "bafydeletedadminpost", subjectCID: "bafydeletedadminpost", recordCID: "bafydeletedadminpostreply",
			wantRoot:    notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeletedadminpost"},
			wantSubject: notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeletedadminpost"},
			wantRecord:  serverAdminRemoved("bafydeletedadminpostreply"),
		},
		{
			name: "author-deleted postReply subject without marker with server-admin removal", reason: "postReply", rootState: "authorDeletedWithoutMarkerAndInstanceRemoved",
			rootCID: "bafyunmarkedadminpost", subjectCID: "bafyunmarkedadminpost", recordCID: "bafyunmarkedadminpostreply",
			hidden: true,
		},
		{
			name: "commentReply under server-admin-removed root", reason: "commentReply", rootState: "instanceRemoved",
			rootCID: "bafyadminremovedthread", subjectCID: "bafyadminremovedthreadsubject", recordCID: "bafyadminremovedthreadreply",
			wantRoot: serverAdminRemoved("bafyadminremovedthread"), wantSubject: serverAdminRemoved("bafyadminremovedthreadsubject"),
			wantRecord: serverAdminRemoved("bafyadminremovedthreadreply"),
		},
		{
			name: "commentReply under community-scope-only removed root", reason: "commentReply", rootState: "communityScopeRemoved",
			rootCID: "bafyscoperemovedthread", subjectCID: "bafyscoperemovedthreadsubject", recordCID: "bafyscoperemovedthreadreply",
			wantRoot:    notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyscoperemovedthread"},
			wantSubject: notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyscoperemovedthreadsubject"},
			wantRecord:  notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyscoperemovedthreadreply"},
		},
		{
			name: "author-deleted commentReply subject under server-admin-removed root", reason: "commentReply", rootState: "instanceRemoved", subjectState: "deleted",
			rootCID: "bafyadminremoveddeletedthread", subjectCID: "bafyadminremoveddeletedsubject", recordCID: "bafyadminremoveddeletedreply",
			wantRoot:    serverAdminRemoved("bafyadminremoveddeletedthread"),
			wantSubject: notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafyadminremoveddeletedsubject"},
			wantRecord:  serverAdminRemoved("bafyadminremoveddeletedreply"),
		},
		{
			name: "server-admin-removed commentReply subject", reason: "commentReply", subjectState: "instanceRemoved",
			rootCID: "bafyadmincommentroot", subjectCID: "bafyadminremovedcomment", recordCID: "bafyadmincommentreply",
			wantRoot: live("bafyadmincommentroot"), wantSubject: serverAdminRemoved("bafyadminremovedcomment"),
			wantRecord: live("bafyadmincommentreply"),
		},
		{
			name: "author-deleted commentReply subject with server-admin removal", reason: "commentReply", subjectState: "deletedAndInstanceRemoved",
			rootCID: "bafydeletedadmincommentroot", subjectCID: "bafydeletedadmincomment", recordCID: "bafydeletedadmincommentreply",
			wantRoot:    live("bafydeletedadmincommentroot"),
			wantSubject: notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeletedadmincomment"},
			wantRecord:  live("bafydeletedadmincommentreply"),
		},
		{
			name: "server-admin-removed reply comment record", reason: "postReply", recordState: "instanceRemoved",
			rootCID: "bafyadminrecordroot", subjectCID: "bafyadminrecordroot", recordCID: "bafyadminremovedreply",
			wantRoot: live("bafyadminrecordroot"), wantSubject: live("bafyadminrecordroot"),
			wantRecord: serverAdminRemoved("bafyadminremovedreply"),
		},
		{
			name: "server-admin-removed post mention record", reason: "mention", recordState: "instanceRemoved",
			rootCID: "bafyadminmentionroot", recordCID: "bafyadminremovedmention",
			wantRoot: live("bafyadminmentionroot"), wantRecord: serverAdminRemoved("bafyadminremovedmention"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadCountFixture(t)
			retentionSeenAt(t, f.db, f.recipient, f.sortAt.Add(-time.Microsecond))
			root := f.root
			switch tc.rootState {
			case "communityRemoved", "communityRemovedAndAuthorDeleted", "communityRemovedAndInstanceRemoved":
				root = f.post(t, posts.AdmissionStatusRemoved, false)
			case "pendingInstanceRemoved":
				root = f.post(t, posts.AdmissionStatusPending, false)
			}
			setListedPostCID(t, f, root, tc.rootCID)
			switch tc.rootState {
			case "authorDeleted":
				f.deletePost(t, root)
				seedWithdrawalMarker(t, f.db, root, "authorDelete", nil)
			case "communityRemoved":
				seedWithdrawalMarker(t, f.db, root, "communityWithdrawal", "3lqqqqqqqqqq1")
			case "communityRemovedAndAuthorDeleted":
				f.deletePost(t, root)
				seedWithdrawalMarker(t, f.db, root, "communityWithdrawal", "3lqqqqqqqqqq1")
				seedWithdrawalMarker(t, f.db, root, "authorDelete", nil)
			case "instanceRemoved", "pendingInstanceRemoved":
				seedModerationDecision(t, f.db, root, "removal", "", true)
			case "communityScopeRemoved":
				seedModerationDecision(t, f.db, root, "removal", f.community, true)
			case "instanceAndCommunityScopeRemoved":
				seedModerationDecision(t, f.db, root, "removal", "", true)
				seedModerationDecision(t, f.db, root, "removal", f.community, true)
			case "communityRemovedAndInstanceRemoved":
				seedWithdrawalMarker(t, f.db, root, "communityWithdrawal", "3lqqqqqqqqqq1")
				seedModerationDecision(t, f.db, root, "removal", "", true)
			case "instanceRemovalInactive":
				seedModerationDecision(t, f.db, root, "removal", "", false)
			case "instanceLabel":
				seedModerationDecision(t, f.db, root, "label", "", true)
			case "authorDeletedAndInstanceRemoved":
				f.deletePost(t, root)
				seedWithdrawalMarker(t, f.db, root, "authorDelete", nil)
				seedModerationDecision(t, f.db, root, "removal", "", true)
			case "authorDeletedWithoutMarkerAndInstanceRemoved":
				f.deletePost(t, root)
				seedModerationDecision(t, f.db, root, "removal", "", true)
			}
			subject := root
			switch tc.reason {
			case "commentReply":
				subject = f.comment(t, root)
				setListedCommentCID(t, f, subject, tc.subjectCID)
				switch tc.subjectState {
				case "deleted":
					f.deleteComment(t, subject)
				case "instanceRemoved":
					seedModerationDecision(t, f.db, subject, "removal", "", true)
				case "deletedAndInstanceRemoved":
					f.deleteComment(t, subject)
					seedModerationDecision(t, f.db, subject, "removal", "", true)
				}
			case "mention":
				subject = ""
			}
			var record string
			switch tc.reason {
			case "upvote":
				f.insertVote(t, "did:plc:referencevoter"+testkit.UniqueID(t), subject, false)
			case "mention":
				record = f.post(t, posts.AdmissionStatusAccepted, false)
				setListedPostCID(t, f, record, tc.recordCID)
			default:
				record = f.comment(t, root)
				setListedCommentCID(t, f, record, tc.recordCID)
			}
			if tc.recordState == "instanceRemoved" {
				seedModerationDecision(t, f.db, record, "removal", "", true)
			}
			// listNotificationAt stores bafyunreadrecord, never the current record CID.
			f.listNotificationAt(t, f.recipient, f.actor, tc.reason, record, subject, root, f.sortAt)
			page := f.listPage(t, f.recipient, "", 10)
			if tc.hidden {
				require.Empty(t, page.Notifications)
				f.requireCount(t, 0)
				return
			}
			require.Len(t, page.Notifications, 1)
			listed := page.Notifications[0]
			require.Equal(t, record, listed.RecordURI)
			require.Equal(t, subject, listed.SubjectURI)
			require.Equal(t, tc.wantRoot, listed.RootPost)
			require.Equal(t, tc.wantSubject, listed.Subject)
			require.Equal(t, tc.wantRecord, listed.Record)
			f.requireCount(t, 1)
		})
	}
}
