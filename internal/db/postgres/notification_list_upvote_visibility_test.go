//go:build integration

package postgres

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Both vote timestamps increase together; tests do not choose which timestamp
// the read path uses to define the newest voter.
func (f *unreadVisibilityFixture) listVoteAt(t *testing.T, voter, subject, direction string, at time.Time) string {
	t.Helper()
	key := testkit.TID()
	uri := "at://" + voter + "/social.coves.feed.vote/" + key
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO votes
		(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, 'bafylistvote', $2, $3, $4, 'bafylistvotesubject', $5, $6, $6)`,
		uri, key, voter, subject, direction, at.Truncate(time.Microsecond))
	require.NoError(t, err)
	return uri
}

// Upvote rows have no RecordURI; never feed them to recordLabels.
func (f *unreadVisibilityFixture) listRowLabels(t *testing.T, page notifications.ListPage, records, subjects map[string]string) []string {
	t.Helper()
	got := make([]string, 0, len(page.Notifications))
	for _, row := range page.Notifications {
		var label string
		var ok bool
		if row.Reason == notifications.ReasonUpvote {
			label, ok = subjects[row.SubjectURI]
		} else {
			label, ok = records[row.RecordURI]
		}
		require.True(t, ok, "unlabeled notification id=%d reason=%s record=%s subject=%s", row.ID, row.Reason, row.RecordURI, row.SubjectURI)
		got = append(got, label)
	}
	return got
}

func (f *unreadVisibilityFixture) listGroupAt(t *testing.T, subject, root string, at time.Time) {
	t.Helper()
	f.listNotificationAt(t, f.recipient, "", "upvote", "", subject, root, at)
}

func TestNotificationList_AgreesWithCountForUpvoteGroups(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind string
		status     posts.AdmissionStatus
		listed     bool
	}{
		{"voter blocked by recipient", "blocked", "", false},
		{"voter blocks recipient", "blocks", "", false},
		{"voter erased", "erased", "", false},
		{"vote retracted", "retracted", "", false},
		{"recipient own vote", "self", "", false},
		{"post pending", "post", posts.AdmissionStatusPending, false},
		{"post rejected", "post", posts.AdmissionStatusRejected, false},
		{"post pending reacceptance", "post", posts.AdmissionStatusPendingReacceptance, false},
		{"post no admission", "post", "", false},
		{"post CID drifted", "drifted", "", false},
		{"post deleted without marker", "deletedPost", posts.AdmissionStatusAccepted, false},
		{"post removed without marker", "post", posts.AdmissionStatusRemoved, false},
		{"post unindexed", "unindexedPost", "", false},
		{"comment pending root", "comment", posts.AdmissionStatusPending, false},
		{"comment unindexed", "unindexedComment", "", false},
		{"post live", "post", posts.AdmissionStatusAccepted, true},
		{"comment live", "comment", posts.AdmissionStatusAccepted, true},
		{"post author deleted with marker", "markedDelete", posts.AdmissionStatusAccepted, true},
		{"post community removed with marker", "markedRemoval", posts.AdmissionStatusRemoved, true},
		{"comment root author deleted with marker", "commentDeletedRoot", posts.AdmissionStatusAccepted, true},
		{"comment root removed with marker", "commentRemovedRoot", posts.AdmissionStatusRemoved, true},
		{"comment subject deleted", "deletedComment", posts.AdmissionStatusAccepted, true},
		{"disabled upvote", "disabled", posts.AdmissionStatusAccepted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			controlReply := f.listControlRecord(t)
			controlPostKey := testkit.TID()
			controlPost := seedVisibilityPost(t, f.db, f.community, f.recipient, controlPostKey, "control", f.sortAt)
			seedVisibilityAdmission(t, f.db, f.community, controlPost, posts.AdmissionStatusAccepted, "", "")
			controlVoter := "did:plc:listvoter" + testkit.UniqueID(t)
			f.listVoteAt(t, controlVoter, controlPost, "up", f.sortAt)
			f.listGroupAt(t, controlPost, controlPost, f.sortAt.Add(time.Second))

			subject, root := f.root, f.root
			casePostCID, caseCommentCID, caseRootCID := "", "", ""
			voter := "did:plc:listvoter" + testkit.UniqueID(t)
			if tc.kind == "self" {
				voter = f.recipient
			}
			switch tc.kind {
			case "post", "drifted", "deletedPost", "markedDelete", "markedRemoval", "unindexedPost":
				if tc.kind == "unindexedPost" {
					subject = postV2URI(f.recipient, testkit.TID())
				} else {
					key := testkit.TID()
					subject = seedVisibilityPost(t, f.db, f.community, f.recipient, key, "case post", f.sortAt)
					casePostCID = "bafypostv2" + key
					if tc.status != "" {
						seedVisibilityAdmission(t, f.db, f.community, subject, tc.status, "", "")
					}
				}
				root = subject
				if tc.kind == "drifted" {
					seedVisibilityAdmissionDriftedCID(t, f.db, f.community, subject)
				}
				if tc.kind == "deletedPost" || tc.kind == "markedDelete" {
					f.deletePost(t, subject)
				}
				if tc.kind == "markedDelete" {
					seedWithdrawalMarker(t, f.db, subject, "authorDelete", nil)
				}
				if tc.kind == "markedRemoval" {
					seedWithdrawalMarker(t, f.db, subject, "communityWithdrawal", "3lqqqqqqqqqq1")
				}
			case "comment", "commentDeletedRoot", "commentRemovedRoot", "deletedComment", "unindexedComment":
				if tc.kind != "deletedComment" && tc.kind != "unindexedComment" {
					key := testkit.TID()
					root = seedVisibilityPost(t, f.db, f.community, f.recipient, key, "comment root", f.sortAt)
					caseRootCID = "bafypostv2" + key
					seedVisibilityAdmission(t, f.db, f.community, root, tc.status, "", "")
				} else {
					caseRootCID = "bafypostv2" + f.root[strings.LastIndex(f.root, "/")+1:]
				}
				if tc.kind == "unindexedComment" {
					subject = "at://" + f.recipient + "/social.coves.community.comment/" + testkit.TID()
				} else {
					key := testkit.TID()
					subject = seedActorComment(t, f.db, f.recipient, root, key, f.sortAt)
					caseCommentCID = "bafycmt" + key
				}
				if tc.kind == "deletedComment" {
					f.deleteComment(t, subject)
				}
				if tc.kind == "commentDeletedRoot" {
					f.deletePost(t, root)
					seedWithdrawalMarker(t, f.db, root, "authorDelete", nil)
				}
				if tc.kind == "commentRemovedRoot" {
					seedWithdrawalMarker(t, f.db, root, "communityWithdrawal", "3lqqqqqqqqqq1")
				}
			}
			vote := f.listVoteAt(t, voter, subject, "up", f.sortAt.Add(time.Second))
			switch tc.kind {
			case "blocked":
				f.insertBlock(t, f.recipient, voter)
			case "blocks":
				f.insertBlock(t, voter, f.recipient)
			case "erased":
				f.eraseDID(t, voter)
			case "retracted":
				_, err := f.db.ExecContext(context.Background(), `UPDATE votes SET deleted_at = $2 WHERE uri = $1`, vote, f.sortAt.Add(2*time.Second))
				require.NoError(t, err)
			}
			f.listGroupAt(t, subject, root, f.sortAt.Add(2*time.Second))
			// The independent live group is present even when the case group is hidden.
			f.requireCount(t, map[bool]int{true: 3, false: 2}[tc.listed])
			page := f.listPage(t, f.recipient, "", 10)
			want := []string{"control upvote", "reply"}
			if tc.listed {
				want = []string{"case", "control upvote", "reply"}
			}
			require.Equal(t, want, f.listRowLabels(t, page,
				map[string]string{controlReply: "reply"}, map[string]string{subject: "case", controlPost: "control upvote"}))
			require.Empty(t, page.Cursor)
			for _, row := range page.Notifications {
				if row.Reason != notifications.ReasonUpvote {
					continue
				}
				require.Empty(t, row.RecordURI)
				require.Empty(t, row.ActorDID)
				require.True(t, row.RecordCreatedAt.IsZero())
				if row.SubjectURI == controlPost {
					require.Equal(t, "bafypostv2"+controlPostKey, row.Subject.CID)
					require.Equal(t, "bafypostv2"+controlPostKey, row.RootPost.CID)
					require.Equal(t, 1, row.UpvoteCount)
					require.Equal(t, []string{controlVoter}, row.RecentUpvoterDIDs)
				} else {
					require.Equal(t, 1, row.UpvoteCount)
					require.Equal(t, []string{voter}, row.RecentUpvoterDIDs)
					if casePostCID != "" {
						require.Equal(t, casePostCID, row.Subject.CID)
						require.Equal(t, casePostCID, row.RootPost.CID)
					}
					if tc.kind == "comment" || tc.kind == "commentDeletedRoot" || tc.kind == "commentRemovedRoot" || tc.kind == "deletedComment" {
						require.NotEqual(t, row.Subject.CID, row.RootPost.CID)
						require.Equal(t, caseCommentCID, row.Subject.CID)
						require.Equal(t, caseRootCID, row.RootPost.CID)
					}
				}
			}
			if tc.kind == "disabled" {
				f.setDisabledReasons(t, f.recipient, []string{"upvote"})
				f.requireCount(t, 1)
				page = f.listPage(t, f.recipient, "", 10)
				require.Equal(t, []string{"reply"}, f.listRowLabels(t, page, map[string]string{controlReply: "reply"}, nil))
			}
		})
	}
}

func TestNotificationList_EveryReasonFillsPagesAndNeverSeenGroup(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := f.sortAt
	retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Microsecond))
	records, subjects := map[string]string{}, map[string]string{}
	for number := 1; number <= 12; number++ {
		record, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, base.Add(time.Duration(number)*time.Second), base)
		records[record] = "R" + strconv.Itoa(number)
	}
	postMention := seedVisibilityPost(t, f.db, f.community, f.actor, testkit.TID(), "mention post", base)
	seedVisibilityAdmission(t, f.db, f.community, postMention, posts.AdmissionStatusAccepted, "", "")
	f.listNotificationAt(t, f.recipient, f.actor, "mention", postMention, "", postMention, base.Add(13*time.Second))
	records[postMention] = "post mention"
	commentMention := f.comment(t, f.root)
	f.listNotificationAt(t, f.recipient, f.actor, "mention", commentMention, "", f.root, base.Add(14*time.Second))
	records[commentMention] = "comment mention"
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), f.root, "up", base)
	f.listGroupAt(t, f.root, f.root, base.Add(15*time.Second))
	subjects[f.root] = "post group"
	commentSubject := seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), base)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), commentSubject, "up", base)
	f.listGroupAt(t, commentSubject, f.root, base.Add(16*time.Second))
	subjects[commentSubject] = "comment group"
	deletedRecord := f.comment(t, f.root)
	f.deleteComment(t, deletedRecord)
	f.listNotificationAt(t, f.recipient, f.actor, "commentReply", deletedRecord, f.comment(t, f.root), f.root, base.Add(17*time.Second))
	records[deletedRecord] = "deleted record"
	deletedSubjectPost := f.post(t, posts.AdmissionStatusAccepted, false)
	f.deletePost(t, deletedSubjectPost)
	seedWithdrawalMarker(t, f.db, deletedSubjectPost, "authorDelete", nil)
	deletedSubjectPostRecord := f.comment(t, f.root)
	f.listNotificationAt(t, f.recipient, f.actor, "postReply", deletedSubjectPostRecord, deletedSubjectPost, f.root, base.Add(18*time.Second))
	records[deletedSubjectPostRecord] = "deleted subject post"
	deletedSubjectComment := f.comment(t, f.root)
	f.deleteComment(t, deletedSubjectComment)
	deletedSubjectCommentRecord := f.comment(t, f.root)
	f.listNotificationAt(t, f.recipient, f.actor, "commentReply", deletedSubjectCommentRecord, deletedSubjectComment, f.root, base.Add(19*time.Second))
	records[deletedSubjectCommentRecord] = "deleted subject comment"
	removedRoot := f.post(t, posts.AdmissionStatusRemoved, false)
	seedWithdrawalMarker(t, f.db, removedRoot, "communityWithdrawal", "3lqqqqqqqqqq1")
	removedRootRecord := f.comment(t, removedRoot)
	f.listNotificationAt(t, f.recipient, f.actor, "commentReply", removedRootRecord, f.comment(t, removedRoot), removedRoot, base.Add(20*time.Second))
	records[removedRootRecord] = "removed root"
	deletedRoot := f.post(t, posts.AdmissionStatusAccepted, false)
	f.deletePost(t, deletedRoot)
	seedWithdrawalMarker(t, f.db, deletedRoot, "authorDelete", nil)
	deletedRootRecord := f.comment(t, deletedRoot)
	f.listNotificationAt(t, f.recipient, f.actor, "commentReply", deletedRootRecord, f.comment(t, deletedRoot), deletedRoot, base.Add(21*time.Second))
	records[deletedRootRecord] = "deleted root"
	deletedGroupComment := seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), base)
	f.deleteComment(t, deletedGroupComment)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), deletedGroupComment, "up", base)
	f.listGroupAt(t, deletedGroupComment, f.root, base.Add(22*time.Second))
	subjects[deletedGroupComment] = "deleted comment group"
	removedGroupPost := f.post(t, posts.AdmissionStatusRemoved, false)
	seedWithdrawalMarker(t, f.db, removedGroupPost, "communityWithdrawal", "3lqqqqqqqqqq1")
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), removedGroupPost, "up", base)
	f.listGroupAt(t, removedGroupPost, removedGroupPost, base.Add(23*time.Second))
	subjects[removedGroupPost] = "removed post group"
	// Hidden rows interleave with visible rows and must consume no page slots.
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
	f.requireCount(t, 23)
	cursor, got := "", []string{}
	for _, size := range []int{7, 5, 7, 4} {
		page := f.listPage(t, f.recipient, cursor, size)
		if cursor == "" {
			require.Equal(t, []string{"removed post group", "deleted comment group", "deleted root", "removed root", "deleted subject comment", "deleted subject post", "deleted record"}, f.listRowLabels(t, page, records, subjects))
		}
		require.Len(t, page.Notifications, size)
		got = append(got, f.listRowLabels(t, page, records, subjects)...)
		if size == 4 {
			require.Empty(t, page.Cursor)
		} else {
			require.NotEmpty(t, page.Cursor)
		}
		cursor = page.Cursor
	}
	require.Equal(t, []string{"removed post group", "deleted comment group", "deleted root", "removed root", "deleted subject comment", "deleted subject post", "deleted record", "comment group", "post group", "comment mention", "post mention", "R12", "R11", "R10", "R9", "R8", "R7", "R6", "R5", "R4", "R3", "R2", "R1"}, got)
	_, clearSeenErr := f.db.Exec(`UPDATE notification_state SET seen_at = NULL WHERE did = $1`, f.recipient)
	require.NoError(t, clearSeenErr)
	newestSubject := f.post(t, posts.AdmissionStatusAccepted, false)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), newestSubject, "up", base)
	f.listGroupAt(t, newestSubject, newestSubject, base.Add(24*time.Second))
	subjects[newestSubject] = "newest group"
	f.listNotificationAt(t, f.recipient, f.actor, "postReply", f.comment(t, f.root), pending, pending, base.Add(25*time.Second))
	f.requireCount(t, 1)
	page := f.listPage(t, f.recipient, "", 100)
	require.Equal(t, "newest group", f.listRowLabels(t, page, records, subjects)[0])
	unread := []string{}
	for _, row := range page.Notifications {
		if !row.IsRead {
			unread = append(unread, subjects[row.SubjectURI])
		}
	}
	require.Equal(t, []string{"newest group"}, unread)
}

func TestNotificationList_BumpedGroupMovesAboveOldCursor(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	var now time.Time
	require.NoError(t, f.db.QueryRowContext(context.Background(), `SELECT now()`).Scan(&now))
	base := now.UTC().Truncate(time.Microsecond).Add(-time.Hour)
	retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Second))
	records, subjects := map[string]string{}, map[string]string{}
	for i := 1; i <= 4; i++ {
		record, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, base.Add(time.Duration(i*2)*time.Second), base)
		records[record] = "R" + strconv.Itoa(i)
	}
	old := f.post(t, posts.AdmissionStatusAccepted, false)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), old, "up", base)
	f.listGroupAt(t, old, old, base.Add(time.Second))
	subjects[old] = "moved"
	other := f.post(t, posts.AdmissionStatusAccepted, false)
	f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), other, "up", base)
	f.listGroupAt(t, other, other, base.Add(3*time.Second))
	subjects[other] = "other group"
	first := f.listPage(t, f.recipient, "", 2)
	require.Equal(t, []string{"R4", "R3"}, f.listRowLabels(t, first, records, subjects))
	require.NotEmpty(t, first.Cursor)
	tx, err := f.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, NewNotificationRepository(f.db).ApplyUpvoteGroupTx(context.Background(), tx, notifications.UpvoteGroupIntent{
		Action: notifications.UpvoteGroupBump, RecipientDID: f.recipient, SubjectURI: old, RootPostURI: old,
	}))
	require.NoError(t, tx.Commit())
	second := f.listPage(t, f.recipient, first.Cursor, 2)
	require.Equal(t, []string{"R2", "other group"}, f.listRowLabels(t, second, records, subjects))
	require.NotEmpty(t, second.Cursor)
	third := f.listPage(t, f.recipient, second.Cursor, 2)
	require.Equal(t, []string{"R1"}, f.listRowLabels(t, third, records, subjects))
	require.Empty(t, third.Cursor)
	fresh := f.listPage(t, f.recipient, "", 3)
	require.Equal(t, []string{"moved", "R4", "R3"}, f.listRowLabels(t, fresh, records, subjects))
}
