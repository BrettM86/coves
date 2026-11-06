//go:build integration

package postgres

import (
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// removeNotificationCommunity removes communityDID at the instance through
// the real moderation service and returns the restore. It mirrors
// tests/fixtures/communityremovalfixture, which cannot be imported here: it
// imports this package.
func removeNotificationCommunity(t *testing.T, f *unreadVisibilityFixture, communityDID string) (restore func()) {
	t.Helper()
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(NewPostRepository(f.db), NewCommentRepository(f.db)),
		NewModerationRepository(f.db),
		moderation.Config{
			InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
			Communities: NewCommunityRepository(f.db, credentialciphertest.Fixed()),
		},
	)
	admin := "did:plc:notificationcommunityremovaladmin"
	removed, err := service.RemoveCommunity(t.Context(), admin, moderation.RemoveCommunityRequest{
		Subject: communityDID, ExpectedVersion: "v0", IdempotencyKey: "notification-community-remove",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome, "fixture: the instance removes the community")
	return func() {
		t.Helper()
		restored, err := service.RestoreCommunity(t.Context(), admin, moderation.RestoreCommunityRequest{
			Subject: communityDID, ExpectedVersion: removed.State.Version, IdempotencyKey: "notification-community-restore",
			Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
		})
		require.NoError(t, err)
		require.Equal(t, moderation.OutcomeApplied, restored.Outcome, "fixture: the instance restores the community")
	}
}

// TestNotificationList_CommunityRemovalReferenceStates lists author A's
// notifications created before A's community C is removed by the instance: a
// reply by X to A's post P, an upvote group on P, and an upvote group on A's
// post P3 that C itself withdrew earlier, plus a reply to A's post L in a live
// community D. While C is removed, A's own posts P and P3 keep the state A
// saw before the removal (A's own content is exempt from the instance
// community removal: P reads live, P3, which C withdrew, removedByModerator),
// while X's reply in C reads removedByServerAdmin. After restore every
// reference reads as before the removal. D's reply stays live throughout.
func TestNotificationList_CommunityRemovalReferenceStates(t *testing.T) {
	t.Parallel()
	removed := notifications.ReferenceRemovedByServerAdmin
	live := notifications.ReferenceLive
	byModerator := notifications.ReferenceRemovedByModerator

	f := newUnreadCountFixture(t)
	post := f.root
	reply := f.comment(t, post)
	f.listNotificationAt(t, f.recipient, f.actor, "postReply", reply, post, post, f.sortAt.Add(time.Second))
	f.listVoteAt(t, "did:plc:communityremovalvoter"+testkit.UniqueID(t), post, "up", f.sortAt)
	f.listGroupAt(t, post, post, f.sortAt.Add(2*time.Second))

	withdrawn := f.post(t, posts.AdmissionStatusRemoved, false)
	seedWithdrawalMarker(t, f.db, withdrawn, "communityWithdrawal", "3lqqqqqqqqqq1")
	f.listVoteAt(t, "did:plc:communityremovalvoter"+testkit.UniqueID(t), withdrawn, "up", f.sortAt)
	f.listGroupAt(t, withdrawn, withdrawn, f.sortAt.Add(3*time.Second))

	liveCommunity := visibilityCommunity(t, f.db, testkit.UniqueID(t)+"live")
	livePost := seedVisibilityPost(t, f.db, liveCommunity, f.recipient, testkit.TID(), "post in the live community", f.sortAt)
	seedVisibilityAdmission(t, f.db, liveCommunity, livePost, posts.AdmissionStatusAccepted, "", "")
	liveReply := seedActorComment(t, f.db, f.actor, livePost, testkit.TID(), f.sortAt)
	f.listNotificationAt(t, f.recipient, f.actor, "postReply", liveReply, livePost, livePost, f.sortAt.Add(4*time.Second))

	type key struct {
		reason  notifications.Reason
		subject string
	}
	listed := func(t *testing.T) map[key]notifications.ListedNotification {
		t.Helper()
		page := f.listPage(t, f.recipient, "", 10)
		got := map[key]notifications.ListedNotification{}
		for _, row := range page.Notifications {
			got[key{row.Reason, row.SubjectURI}] = row
		}
		require.Len(t, got, 4, "every notification stays listed")
		return got
	}
	// record is nil for upvote groups, which carry no record.
	type want struct {
		root, subject notifications.ReferenceState
		record        *notifications.ReferenceState
	}
	state := func(value notifications.ReferenceState) *notifications.ReferenceState { return &value }
	check := func(t *testing.T, rows map[key]notifications.ListedNotification, expected map[string]want) {
		t.Helper()
		for name, row := range map[string]key{
			"reply to P":        {notifications.ReasonPostReply, post},
			"upvote on P":       {notifications.ReasonUpvote, post},
			"upvote on P3":      {notifications.ReasonUpvote, withdrawn},
			"reply to L (live)": {notifications.ReasonPostReply, livePost},
		} {
			got, ok := rows[row]
			require.Truef(t, ok, "%s is listed", name)
			wanted := expected[name]
			assert.Equalf(t, wanted.root, got.RootPost.State, "%s rootPost state", name)
			assert.Equalf(t, wanted.subject, got.Subject.State, "%s subject state", name)
			if wanted.record != nil {
				assert.Equalf(t, *wanted.record, got.Record.State, "%s record state", name)
			}
		}
	}

	t.Run("before removal", func(t *testing.T) {
		check(t, listed(t), map[string]want{
			"reply to P":        {live, live, state(live)},
			"upvote on P":       {live, live, nil},
			"upvote on P3":      {byModerator, byModerator, nil},
			"reply to L (live)": {live, live, state(live)},
		})
	})
	restore := removeNotificationCommunity(t, f, f.community)
	t.Run("while the community is removed", func(t *testing.T) {
		check(t, listed(t), map[string]want{
			"reply to P":        {live, live, state(removed)},
			"upvote on P":       {live, live, nil},
			"upvote on P3":      {byModerator, byModerator, nil},
			"reply to L (live)": {live, live, state(live)},
		})
	})
	restore()
	t.Run("after restore", func(t *testing.T) {
		check(t, listed(t), map[string]want{
			"reply to P":        {live, live, state(live)},
			"upvote on P":       {live, live, nil},
			"upvote on P3":      {byModerator, byModerator, nil},
			"reply to L (live)": {live, live, state(live)},
		})
	})
}

// TestCommunityRemoval_MismatchedRootCommentResolvesThroughParentChain covers
// a comment K by the recipient A whose parent is A's post P in community C but
// whose commenter-chosen root is A's post L in the live community D, and a
// reply R by X to K under the same declared root. K and R belong to C through
// their parent chain, as ban classification resolves them. While the instance
// has removed C, K is absent from A's comment activity for every other viewer,
// and in A's notifications the reply R reads removedByServerAdmin, while K, A's
// own comment, keeps reading live (A's own content is exempt from the instance
// community removal); L itself stays live. After restore K is listed again and
// both read live.
func TestCommunityRemoval_MismatchedRootCommentResolvesThroughParentChain(t *testing.T) {
	t.Parallel()
	removed := notifications.ReferenceRemovedByServerAdmin
	live := notifications.ReferenceLive

	f := newUnreadCountFixture(t)
	postInRemovedCommunity := f.root
	liveCommunity := visibilityCommunity(t, f.db, testkit.UniqueID(t)+"live")
	declaredRoot := seedVisibilityPost(t, f.db, liveCommunity, f.recipient, testkit.TID(), "post in the live community", f.sortAt)
	seedVisibilityAdmission(t, f.db, liveCommunity, declaredRoot, posts.AdmissionStatusAccepted, "", "")

	seedChainComment := func(commenter, parent string, createdAt time.Time) string {
		t.Helper()
		rkey := testkit.TID()
		uri := "at://" + commenter + "/social.coves.community.comment/" + rkey
		_, err := f.db.ExecContext(t.Context(), `
			INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
			VALUES ($1, $2, $3, $4, $5, 'bafymismatchedroot', $6, 'bafymismatchedparent', 'mismatched root comment', $7)`,
			uri, "bafycmt"+rkey, rkey, commenter, declaredRoot, parent, createdAt)
		require.NoError(t, err, "seed a comment under the declared root L")
		return uri
	}
	mismatched := seedChainComment(f.recipient, postInRemovedCommunity, f.sortAt)
	reply := seedChainComment(f.actor, mismatched, f.sortAt.Add(time.Second))
	control := seedChainComment(f.recipient, declaredRoot, f.sortAt.Add(-time.Second))

	f.listVoteAt(t, "did:plc:mismatchedrootvoter"+testkit.UniqueID(t), mismatched, "up", f.sortAt)
	f.listGroupAt(t, mismatched, declaredRoot, f.sortAt.Add(2*time.Second))
	f.listNotificationAt(t, f.recipient, f.actor, "commentReply", reply, mismatched, declaredRoot, f.sortAt.Add(3*time.Second))

	commentRepo := NewCommentRepository(f.db, WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	activity := func(t *testing.T, viewer string) []string {
		t.Helper()
		listed, _, err := commentRepo.ListByCommenterWithCursor(t.Context(), comments.ListByCommenterRequest{
			CommenterDID: f.recipient, ViewerDID: viewer, Limit: 50,
		})
		require.NoError(t, err)
		uris := make([]string, 0, len(listed))
		for _, comment := range listed {
			uris = append(uris, comment.URI)
		}
		return uris
	}
	otherViewer := "did:plc:mismatchedrootviewer" + testkit.UniqueID(t)
	checkActivity := func(t *testing.T, mismatchedListed bool) {
		t.Helper()
		for name, viewer := range map[string]string{"another viewer": otherViewer, "a signed-out viewer": ""} {
			got := activity(t, viewer)
			assert.Containsf(t, got, control, "%s: A's comment in the live community is listed", name)
			if mismatchedListed {
				assert.Containsf(t, got, mismatched, "%s: K is listed in A's activity", name)
			} else {
				assert.NotContainsf(t, got, mismatched, "%s: K, whose parent chain reaches the removed community, is not listed in A's activity", name)
			}
		}
		assert.Contains(t, activity(t, f.recipient), mismatched, "A keeps K in A's own activity")
	}
	type want struct{ root, subject, record notifications.ReferenceState }
	checkNotifications := func(t *testing.T, upvote, commentReply want) {
		t.Helper()
		page := f.listPage(t, f.recipient, "", 10)
		require.Len(t, page.Notifications, 2, "both notifications on K stay listed")
		for _, row := range page.Notifications {
			switch row.Reason {
			case notifications.ReasonUpvote:
				assert.Equal(t, upvote.root, row.RootPost.State, "upvote on K rootPost state")
				assert.Equal(t, upvote.subject, row.Subject.State, "upvote on K subject state")
			case notifications.ReasonCommentReply:
				assert.Equal(t, commentReply.root, row.RootPost.State, "reply R to K rootPost state")
				assert.Equal(t, commentReply.subject, row.Subject.State, "reply R to K subject state")
				assert.Equal(t, commentReply.record, row.Record.State, "reply R to K record state")
			default:
				t.Fatalf("unexpected notification reason %s", row.Reason)
			}
		}
	}

	t.Run("before removal", func(t *testing.T) {
		checkActivity(t, true)
		checkNotifications(t, want{live, live, live}, want{live, live, live})
	})
	restore := removeNotificationCommunity(t, f, f.community)
	t.Run("while the community is removed", func(t *testing.T) {
		checkActivity(t, false)
		checkNotifications(t, want{live, live, live}, want{live, live, removed})
	})
	restore()
	t.Run("after restore", func(t *testing.T) {
		checkActivity(t, true)
		checkNotifications(t, want{live, live, live}, want{live, live, live})
	})
}
