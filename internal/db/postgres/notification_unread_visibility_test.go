//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Each visibility case has its own recipient and an accepted commentReply control.
// Keeping the control in the same database makes hidden cases fail against a
// CountUnread implementation that always returns zero.
type unreadVisibilityFixture struct {
	db        *sql.DB
	recipient string
	actor     string
	community string
	root      string
	sortAt    time.Time
}

func newUnreadVisibilityFixture(t *testing.T) *unreadVisibilityFixture {
	t.Helper()
	db := testkit.DB(t)
	recipient := retentionUser(t, db)
	fixture := &unreadVisibilityFixture{
		db: db, recipient: recipient,
		actor:     "did:plc:unreadactor" + testkit.UniqueID(t),
		community: visibilityCommunity(t, db, testkit.UniqueID(t)),
		sortAt:    time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond),
	}
	retentionSeenAt(t, db, recipient, fixture.sortAt.Add(-time.Microsecond))
	fixture.root = fixture.post(t, posts.AdmissionStatusAccepted, false)
	fixture.notify(t, "commentReply", fixture.comment(t, fixture.root), fixture.comment(t, fixture.root), fixture.root)
	return fixture
}

func (f *unreadVisibilityFixture) post(t *testing.T, status posts.AdmissionStatus, legacy bool) string {
	t.Helper()
	rkey := testkit.TID()
	var uri string
	if legacy {
		uri = seedLegacyPost(t, f.db, f.community, f.recipient, rkey, "legacy root", f.sortAt)
	} else {
		uri = seedVisibilityPost(t, f.db, f.community, f.recipient, rkey, "notification post", f.sortAt)
		if status != "" {
			seedVisibilityAdmission(t, f.db, f.community, uri, status, "", "")
		}
	}
	return uri
}

func (f *unreadVisibilityFixture) comment(t *testing.T, root string) string {
	t.Helper()
	return seedActorComment(t, f.db, f.actor, root, testkit.TID(), f.sortAt)
}

func (f *unreadVisibilityFixture) notify(t *testing.T, reason, record, subject, root string) {
	t.Helper()
	var recordURI, recordCID, actor, subjectURI, recordCreatedAt any = record, "bafyunreadrecord", f.actor, subject, f.sortAt
	switch reason {
	case "mention":
		subjectURI = nil
	case "upvote":
		// Upvote groups carry no record, actor, or record time (schema CHECK).
		recordURI, recordCID, actor, recordCreatedAt = nil, nil, nil, nil
	}
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		f.recipient, reason, recordURI, recordCID, actor, subjectURI, root, recordCreatedAt, f.sortAt)
	require.NoError(t, err)
}

// Isolate one post reference at a time; the other references are indexed and
// accepted, so a hidden case can only disappear because of the selected post.
func (f *unreadVisibilityFixture) notifyPostReference(t *testing.T, position, postURI string) {
	t.Helper()
	switch position {
	case "subject":
		f.notify(t, "postReply", f.comment(t, f.root), postURI, f.root)
	case "source":
		f.notify(t, "mention", postURI, "", f.root)
	case "root":
		f.notify(t, "commentReply", f.comment(t, postURI), f.comment(t, postURI), postURI)
	default:
		t.Fatalf("unknown post reference position %q", position)
	}
}

func (f *unreadVisibilityFixture) requireCount(t *testing.T, expected int) {
	t.Helper()
	count, err := NewNotificationRepository(f.db).(notifications.ReadRepository).CountUnread(context.Background(), f.recipient)
	require.NoError(t, err)
	require.Equal(t, expected, count)
}

func (f *unreadVisibilityFixture) deletePost(t *testing.T, uri string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `UPDATE posts SET deleted_at = $2 WHERE uri = $1`, uri, f.sortAt)
	require.NoError(t, err)
}

func (f *unreadVisibilityFixture) deleteComment(t *testing.T, uri string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `UPDATE comments SET deleted_at = $2 WHERE uri = $1`, uri, f.sortAt)
	require.NoError(t, err)
}

func TestNotificationUnreadVisibility_PostAdmission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, position  string
		status          posts.AdmissionStatus
		legacy, drifted bool
		want            int
	}{
		{"subject pending", "subject", posts.AdmissionStatusPending, false, false, 1},
		{"subject rejected", "subject", posts.AdmissionStatusRejected, false, false, 1},
		{"subject pending reacceptance", "subject", posts.AdmissionStatusPendingReacceptance, false, false, 1},
		{"subject no admission", "subject", "", false, false, 1},
		{"subject edited after acceptance", "subject", "", false, true, 1},
		{"subject accepted at current CID", "subject", posts.AdmissionStatusAccepted, false, false, 2},
		{"source pending", "source", posts.AdmissionStatusPending, false, false, 1},
		{"source accepted at current CID", "source", posts.AdmissionStatusAccepted, false, false, 2},
		{"root pending", "root", posts.AdmissionStatusPending, false, false, 1},
		{"root accepted at current CID", "root", posts.AdmissionStatusAccepted, false, false, 2},
		{"legacy root without admission", "root", "", true, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			uri := f.post(t, tc.status, tc.legacy)
			if tc.drifted {
				seedVisibilityAdmissionDriftedCID(t, f.db, f.community, uri)
			}
			f.notifyPostReference(t, tc.position, uri)
			f.requireCount(t, tc.want)
		})
	}
}

func TestNotificationUnreadVisibility_ModeratorRemoval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, position                                        string
		status                                                posts.AdmissionStatus
		marker, deleted, foreignRemoval, authorDelete, legacy bool
		want                                                  int
	}{
		{"subject removed with marker", "subject", posts.AdmissionStatusRemoved, true, false, false, false, false, 2},
		{"source removed with marker", "source", posts.AdmissionStatusRemoved, true, false, false, false, false, 2},
		{"source removed with marker and deleted", "source", posts.AdmissionStatusRemoved, true, true, false, false, false, 2},
		{"root removed with marker", "root", posts.AdmissionStatusRemoved, true, false, false, false, false, 2},
		{"subject removed with marker and deleted", "subject", posts.AdmissionStatusRemoved, true, true, false, false, false, 2},
		{"root removed with marker and deleted", "root", posts.AdmissionStatusRemoved, true, true, false, false, false, 2},
		{"subject removed without marker", "subject", posts.AdmissionStatusRemoved, false, false, false, false, false, 1},
		{"subject removal lifted with stale marker", "subject", posts.AdmissionStatusPending, true, false, false, false, false, 1},
		{"subject foreign removal cannot qualify", "subject", posts.AdmissionStatusPending, true, false, true, false, false, 1},
		{"subject removed with stale rev and both markers", "subject", posts.AdmissionStatusRemoved, true, true, false, true, false, 2},
		{"legacy root removed without marker", "root", posts.AdmissionStatusRemoved, false, false, false, false, true, 1},
		{"subject unaccepted then deleted with only community withdrawal marker", "subject", posts.AdmissionStatusPending, true, true, false, false, false, 1},
		{"subject removed undeleted with only author delete marker", "subject", posts.AdmissionStatusRemoved, false, false, false, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			uri := f.post(t, tc.status, tc.legacy)
			if tc.legacy {
				seedVisibilityAdmission(t, f.db, f.community, uri, tc.status, "", "")
			}
			if tc.foreignRemoval {
				otherCommunity := visibilityCommunity(t, f.db, testkit.UniqueID(t))
				seedVisibilityAdmission(t, f.db, otherCommunity, uri, posts.AdmissionStatusRemoved, "", "")
			}
			if tc.marker {
				// The differing rev in the both-markers case is deliberate: the
				// placeholder depends on the marker's existence, not rev equality.
				seedWithdrawalMarker(t, f.db, uri, "communityWithdrawal", "3lqqqqqqqqqq1")
			}
			if tc.authorDelete {
				seedWithdrawalMarker(t, f.db, uri, "authorDelete", nil)
			}
			if tc.deleted {
				f.deletePost(t, uri)
			}
			f.notifyPostReference(t, tc.position, uri)
			f.requireCount(t, tc.want)
		})
	}
}

func TestNotificationUnreadVisibility_AuthorDeletion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, position string
		status         posts.AdmissionStatus
		marker, legacy bool
		want           int
	}{
		{"subject accepted with author delete marker", "subject", posts.AdmissionStatusAccepted, true, false, 2},
		{"source pending with author delete marker", "source", posts.AdmissionStatusPending, true, false, 2},
		{"root accepted with author delete marker", "root", posts.AdmissionStatusAccepted, true, false, 2},
		{"subject pending with author delete marker", "subject", posts.AdmissionStatusPending, true, false, 2},
		{"source accepted with author delete marker", "source", posts.AdmissionStatusAccepted, true, false, 2},
		{"root pending with author delete marker", "root", posts.AdmissionStatusPending, true, false, 2},
		{"subject accepted without marker", "subject", posts.AdmissionStatusAccepted, false, false, 1},
		{"subject pending without marker", "subject", posts.AdmissionStatusPending, false, false, 1},
		{"subject rejected without marker", "subject", posts.AdmissionStatusRejected, false, false, 1},
		{"subject pending reacceptance without marker", "subject", posts.AdmissionStatusPendingReacceptance, false, false, 1},
		{"subject no admission without marker", "subject", "", false, false, 1},
		{"legacy root without marker", "root", "", false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			uri := f.post(t, tc.status, tc.legacy)
			f.deletePost(t, uri)
			if tc.marker {
				seedWithdrawalMarker(t, f.db, uri, "authorDelete", nil)
			}
			f.notifyPostReference(t, tc.position, uri)
			f.requireCount(t, tc.want)
		})
	}
}

func TestNotificationUnreadVisibility_DeletedCommentsRemainVisible(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, reason, deletedReference string }{
		{"commentReply deleted record", "commentReply", "record"},
		{"commentReply deleted subject", "commentReply", "subject"},
		{"postReply deleted record", "postReply", "record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			record := f.comment(t, f.root)
			subject := f.root
			if tc.reason == "commentReply" {
				subject = f.comment(t, f.root)
			}
			if tc.deletedReference == "record" {
				f.deleteComment(t, record)
			} else {
				f.deleteComment(t, subject)
			}
			f.notify(t, tc.reason, record, subject, f.root)
			f.requireCount(t, 2)
		})
	}
}

func TestNotificationUnreadVisibility_UnindexedAndNonPostReferences(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, reason, missing string }{
		{"missing record comment", "commentReply", "recordComment"},
		{"missing post mention source", "mention", "recordPost"},
		{"missing subject comment", "commentReply", "subjectComment"},
		{"missing subject post", "postReply", "subjectPost"},
		{"missing root post", "commentReply", "rootPost"},
		{"root resolves to indexed comment", "commentReply", "rootComment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			record := f.comment(t, f.root)
			subject := f.comment(t, f.root)
			root := f.root
			switch tc.missing {
			case "recordComment":
				record = "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID()
			case "recordPost":
				record = postV2URI(f.recipient, testkit.TID())
				subject = ""
			case "subjectComment":
				subject = "at://" + f.recipient + "/social.coves.community.comment/" + testkit.TID()
			case "subjectPost":
				subject = postV2URI(f.recipient, testkit.TID())
			case "rootPost":
				root = postV2URI(f.recipient, testkit.TID())
			case "rootComment":
				root = f.comment(t, f.root)
			}
			f.notify(t, tc.reason, record, subject, root)
			f.requireCount(t, 1)
		})
	}
}

// Upvote groups have no record; only the subject (and the accepted root) is
// referenced. The root stays the fixture's accepted post so each case isolates
// the subject's state. Every group has a qualifying vote so only the subject's
// visibility changes the expected count.
func TestNotificationUnreadVisibility_Upvote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, subject string
		want          int
	}{
		{"subject post accepted", "acceptedPost", 2},
		{"subject post pending", "pendingPost", 1},
		{"subject comment deleted", "deletedComment", 2},
		{"subject comment unindexed", "unindexedComment", 1},
		{"subject in unsupported collection", "unsupportedCollection", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			var subject string
			switch tc.subject {
			case "acceptedPost":
				subject = f.post(t, posts.AdmissionStatusAccepted, false)
			case "pendingPost":
				subject = f.post(t, posts.AdmissionStatusPending, false)
			case "deletedComment":
				subject = seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), f.sortAt)
				f.deleteComment(t, subject)
			case "unindexedComment":
				subject = "at://" + f.recipient + "/social.coves.community.comment/" + testkit.TID()
			case "unsupportedCollection":
				subject = "at://" + f.recipient + "/social.coves.feed.vote/" + testkit.TID()
			}
			f.insertVote(t, "did:plc:unreadvoter"+testkit.UniqueID(t), subject, false)
			f.notify(t, "upvote", "", subject, f.root)
			f.requireCount(t, tc.want)
		})
	}
}

func TestNotificationUnreadVisibility_CommentMention(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{"comment source live", "live", 2},
		{"comment source deleted", "deleted", 2},
		{"comment source unindexed", "unindexed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			record := f.comment(t, f.root)
			switch tc.source {
			case "deleted":
				f.deleteComment(t, record)
			case "unindexed":
				record = "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID()
			}
			f.notify(t, "mention", record, "", f.root)
			f.requireCount(t, tc.want)
		})
	}
}
