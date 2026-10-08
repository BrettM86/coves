//go:build integration

package postgres

import (
	"context"
	"strconv"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func bridgedAgreementNotificationID(t *testing.T, f *unreadVisibilityFixture, reason, uri string) int64 {
	t.Helper()
	var id int64
	if reason == "upvote" {
		require.NoError(t, f.db.QueryRow(`SELECT id FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`, f.recipient, uri).Scan(&id))
	} else {
		require.NoError(t, f.db.QueryRow(`SELECT id FROM notifications WHERE recipient_did = $1 AND record_uri = $2`, f.recipient, uri).Scan(&id))
	}
	return id
}

func TestNotificationList_BridgedEveryReasonPagesAgreeWithUnread(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := f.sortAt
	retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Microsecond))
	ids := map[string]int64{}
	addRecord := func(label, reason, record, subject, root string, at time.Time) {
		f.listNotificationAt(t, f.recipient, f.actor, reason, record, subject, root, at)
		ids[label] = bridgedAgreementNotificationID(t, f, reason, record)
	}
	addGroup := func(label, subject, root string, at time.Time) int64 {
		f.listGroupAt(t, subject, root, at)
		id := bridgedAgreementNotificationID(t, f, "upvote", subject)
		ids[label] = id
		return id
	}
	for number := 1; number <= 12; number++ {
		record, id := f.insertListedReply(t, f.recipient, "postReply", f.root, base.Add(time.Duration(number)*time.Second), base)
		require.NotEmpty(t, record)
		ids["R"+strconv.Itoa(number)] = id
	}
	postMention := seedVisibilityPost(t, f.db, f.community, f.actor, testkit.TID(), "mention post", base)
	seedVisibilityAdmission(t, f.db, f.community, postMention, posts.AdmissionStatusAccepted, "", "")
	addRecord("post mention", "mention", postMention, "", postMention, base.Add(13*time.Second))
	commentMention := f.comment(t, f.root)
	addRecord("comment mention", "mention", commentMention, "", f.root, base.Add(14*time.Second))
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), f.root, "up", base)
	addGroup("post group", f.root, f.root, base.Add(15*time.Second))
	commentSubject := seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), base)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), commentSubject, "up", base)
	addGroup("comment group", commentSubject, f.root, base.Add(16*time.Second))
	deletedRecord := f.comment(t, f.root)
	f.deleteComment(t, deletedRecord)
	addRecord("deleted record", "commentReply", deletedRecord, f.comment(t, f.root), f.root, base.Add(17*time.Second))
	deletedSubjectPost := f.post(t, posts.AdmissionStatusAccepted, false)
	f.deletePost(t, deletedSubjectPost)
	seedWithdrawalMarker(t, f.db, deletedSubjectPost, "authorDelete", nil)
	addRecord("deleted subject post", "postReply", f.comment(t, f.root), deletedSubjectPost, f.root, base.Add(18*time.Second))
	deletedSubjectComment := f.comment(t, f.root)
	f.deleteComment(t, deletedSubjectComment)
	addRecord("deleted subject comment", "commentReply", f.comment(t, f.root), deletedSubjectComment, f.root, base.Add(19*time.Second))
	removedRoot := f.post(t, posts.AdmissionStatusRemoved, false)
	seedWithdrawalMarker(t, f.db, removedRoot, "communityWithdrawal", "3lqqqqqqqqqq1")
	addRecord("removed root", "commentReply", f.comment(t, removedRoot), f.comment(t, removedRoot), removedRoot, base.Add(20*time.Second))
	deletedRoot := f.post(t, posts.AdmissionStatusAccepted, false)
	f.deletePost(t, deletedRoot)
	seedWithdrawalMarker(t, f.db, deletedRoot, "authorDelete", nil)
	addRecord("deleted root", "commentReply", f.comment(t, deletedRoot), f.comment(t, deletedRoot), deletedRoot, base.Add(21*time.Second))
	deletedGroupComment := seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), base)
	f.deleteComment(t, deletedGroupComment)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), deletedGroupComment, "up", base)
	addGroup("deleted comment group", deletedGroupComment, f.root, base.Add(22*time.Second))
	removedGroupPost := f.post(t, posts.AdmissionStatusRemoved, false)
	seedWithdrawalMarker(t, f.db, removedGroupPost, "communityWithdrawal", "3lqqqqqqqqqq1")
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), removedGroupPost, "up", base)
	addGroup("removed post group", removedGroupPost, removedGroupPost, base.Add(23*time.Second))
	// Hidden records and groups interleave with the visible history.
	pending := f.post(t, posts.AdmissionStatusPending, false)
	f.listNotificationAt(t, f.recipient, f.actor, "postReply", f.comment(t, f.root), pending, pending, base.Add(18*time.Second+time.Microsecond))
	f.listNotificationAt(t, f.recipient, f.actor, "commentReply", "at://"+f.actor+"/social.coves.community.comment/"+testkit.TID(), f.comment(t, f.root), f.root, base.Add(14*time.Second+time.Microsecond))
	blockedSubject := f.post(t, posts.AdmissionStatusAccepted, false)
	blockedVoter := "did:plc:listvoter" + testkit.UniqueID(t)
	f.listVoteAt(t, blockedVoter, blockedSubject, "up", base)
	f.insertBlock(t, f.recipient, blockedVoter)
	f.listGroupAt(t, blockedSubject, blockedSubject, base.Add(20*time.Second+time.Microsecond))
	emptySubject := f.post(t, posts.AdmissionStatusAccepted, false)
	f.listGroupAt(t, emptySubject, emptySubject, base.Add(21*time.Second+time.Microsecond))
	bridgedOnly := f.post(t, posts.AdmissionStatusAccepted, false)
	setBridgedGroupTotals(t, f.db, "post", bridgedOnly, 5, 0)
	bridgedOnlyID := addGroup("bridged only", bridgedOnly, bridgedOnly, base.Add(24*time.Second))
	mixed := seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), base)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), mixed, "up", base)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), mixed, "up", base.Add(time.Second))
	setBridgedGroupTotals(t, f.db, "comment", mixed, 5, 0)
	mixedID := addGroup("mixed", mixed, f.root, base.Add(25*time.Second))
	wantLabels := []string{
		"R1", "R2", "R3", "R4", "R5", "R6", "R7", "R8", "R9", "R10", "R11", "R12",
		"post mention", "comment mention", "post group", "comment group", "deleted record",
		"deleted subject post", "deleted subject comment", "removed root", "deleted root",
		"deleted comment group", "removed post group", "bridged only", "mixed",
	}
	require.Len(t, ids, 25)
	wantIDs := make([]int64, 0, 25)
	for _, label := range wantLabels {
		id, ok := ids[label]
		require.True(t, ok, "missing fixture label %s", label)
		wantIDs = append(wantIDs, id)
	}

	repository := NewNotificationRepository(f.db, WithBridgedUpvoteTotals()).(notifications.ReadRepository)
	count, err := repository.CountUnread(context.Background(), f.recipient)
	require.NoError(t, err)
	var gotIDs []int64
	var bridgedOnlyCount, mixedCount int
	cursor := ""
	for {
		page, err := repository.List(context.Background(), f.recipient, cursor, 6)
		require.NoError(t, err)
		for _, row := range page.Notifications {
			gotIDs = append(gotIDs, row.ID)
			switch row.ID {
			case bridgedOnlyID:
				bridgedOnlyCount = row.UpvoteCount
				require.Empty(t, row.RecentUpvoterDIDs)
			case mixedID:
				mixedCount = row.UpvoteCount
				require.Len(t, row.RecentUpvoterDIDs, 2)
			}
		}
		if page.Cursor == "" {
			break
		}
		require.NotEqual(t, cursor, page.Cursor)
		cursor = page.Cursor
		require.Less(t, len(gotIDs), 101, "pagination must terminate")
	}
	require.ElementsMatch(t, wantIDs, gotIDs, "all 25 seeded visible IDs, including both bridged groups")
	require.Equal(t, 25, count)
	require.Equal(t, count, len(gotIDs), "pages and CountUnread must agree")
	require.Equal(t, 5, bridgedOnlyCount)
	require.Equal(t, 7, mixedCount)
}
