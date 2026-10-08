//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func retentionSweeper(t *testing.T, db *sql.DB) notifications.RetentionSweeper {
	t.Helper()
	sweeper, ok := NewNotificationRepository(db).(notifications.RetentionSweeper)
	require.True(t, ok, "notification repository must implement RetentionSweeper")
	return sweeper
}

func retentionUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	uniqueID := testkit.UniqueID(t)
	did := "did:plc:" + uniqueID
	createTestUser(t, db, uniqueID+".test", did)
	return did
}

func retentionSeenAt(t *testing.T, db *sql.DB, recipient string, seenAt any) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO notification_state (did, seen_at) VALUES ($1, $2)`, recipient, seenAt)
	require.NoError(t, err)
}

func retentionRow(t *testing.T, db *sql.DB, ids map[string]int64, label, recipient, reason string, sortAt time.Time) {
	t.Helper()
	recordURI := "at://" + recipient + "/social.coves.community.comment/" + label
	subjectURI := "at://" + recipient + "/social.coves.community.postv2/" + label
	var id int64
	if reason == "upvote" {
		require.NoError(t, db.QueryRow(`INSERT INTO notifications
			(recipient_did, reason, subject_uri, root_post_uri, sort_at)
			VALUES ($1, 'upvote', $2, $2, $3) RETURNING id`, recipient, subjectURI, sortAt).Scan(&id))
	} else {
		require.NoError(t, db.QueryRow(`INSERT INTO notifications
			(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
			VALUES ($1, 'postReply', $2, 'bafyretentionreply', $1, $3, $3, $4, $4) RETURNING id`,
			recipient, recordURI, subjectURI, sortAt).Scan(&id))
	}
	ids[label] = id
}

func retentionSurvivors(t *testing.T, db *sql.DB, ids map[string]int64) map[string]bool {
	t.Helper()
	labels := make(map[int64]string, len(ids))
	for label, id := range ids {
		labels[id] = label
	}
	rows, err := db.Query(`SELECT id FROM notifications`)
	require.NoError(t, err)
	defer rows.Close()
	survivors := make(map[string]bool)
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		label, ok := labels[id]
		require.True(t, ok, "unexpected notification id %d", id)
		survivors[label] = true
	}
	require.NoError(t, rows.Err())
	return survivors
}

func retentionNewYorkTimezone(t *testing.T, db *sql.DB) {
	t.Helper()
	var database string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&database))
	_, err := db.Exec("ALTER DATABASE " + pq.QuoteIdentifier(database) + " SET timezone = 'America/New_York'")
	require.NoError(t, err)
	db.SetMaxIdleConns(0)
	connection, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer connection.Close()
	var timezone string
	require.NoError(t, connection.QueryRowContext(context.Background(), `SHOW timezone`).Scan(&timezone))
	require.Equal(t, "America/New_York", timezone, "new connections must use the DST-crossing timezone")
}

func TestNotificationRetention_ReadSeenAtUsesExactHoursAndEachRecipient(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	retentionNewYorkTimezone(t, db)
	userA := retentionUser(t, db)
	userB := retentionUser(t, db)
	seenAt := time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	retentionSeenAt(t, db, userA, seenAt)
	retentionSeenAt(t, db, userB, seenAt.Add(-960*time.Hour))
	ids := make(map[string]int64)
	retentionRow(t, db, ids, "a_boundary", userA, "postReply", seenAt.Add(-720*time.Hour))
	retentionRow(t, db, ids, "a_old_group", userA, "upvote", seenAt.Add(-720*time.Hour-time.Microsecond))
	retentionRow(t, db, ids, "a_old_reply", userA, "postReply", seenAt.Add(-720*time.Hour-time.Microsecond))
	retentionRow(t, db, ids, "a_unread", userA, "postReply", seenAt.Add(time.Hour))
	retentionRow(t, db, ids, "b_unread", userB, "postReply", seenAt.Add(-720*time.Hour-time.Microsecond))
	retentionRow(t, db, ids, "b_old", userB, "postReply", seenAt.Add(-1680*time.Hour-time.Microsecond))

	deleted, err := retentionSweeper(t, db).SweepReadNotifications(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]bool{
		"a_boundary": true, "a_unread": true, "b_unread": true,
	}, retentionSurvivors(t, db, ids))
	require.EqualValues(t, 3, deleted)
}

func TestNotificationRetention_ReadNullOrMissingStateUsesRecipientNewest(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	userC := retentionUser(t, db)
	userD := retentionUser(t, db)
	retentionSeenAt(t, db, userC, nil)
	newestC := time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	newestD := newestC.Add(240 * time.Hour)
	shared := newestD.Add(-720*time.Hour - time.Microsecond)
	ids := make(map[string]int64)
	retentionRow(t, db, ids, "c_boundary", userC, "postReply", newestC.Add(-720*time.Hour))
	retentionRow(t, db, ids, "c_old", userC, "postReply", newestC.Add(-720*time.Hour-time.Microsecond))
	retentionRow(t, db, ids, "c_newest", userC, "postReply", newestC)
	retentionRow(t, db, ids, "c_shared", userC, "postReply", shared)
	retentionRow(t, db, ids, "d_boundary", userD, "postReply", newestD.Add(-720*time.Hour))
	retentionRow(t, db, ids, "d_old", userD, "postReply", newestD.Add(-720*time.Hour-time.Microsecond))
	retentionRow(t, db, ids, "d_newest", userD, "postReply", newestD)
	retentionRow(t, db, ids, "d_shared", userD, "postReply", shared)

	deleted, err := retentionSweeper(t, db).SweepReadNotifications(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]bool{
		"c_boundary": true, "c_newest": true, "c_shared": true,
		"d_boundary": true, "d_newest": true,
	}, retentionSurvivors(t, db, ids))
	require.EqualValues(t, 3, deleted)
}

// All bulk notifications have distinct record URIs; their labels are recorded
// from RETURNING so the survivor assertion identifies individual old rows.
func retentionBulkReplies(t *testing.T, db *sql.DB, ids map[string]int64, labelPrefix, recipient string,
	total, oldCount int, oldAt time.Time, boundaryIndex int, boundaryAt time.Time,
	pastBoundaryIndex int, pastBoundaryAt, recentAt, newestAt time.Time,
) {
	t.Helper()
	uriPrefix := "at://" + recipient + "/social.coves.community.comment/" + labelPrefix + "-"
	rows, err := db.Query(`INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, sort_at)
		SELECT $1, 'postReply', $2 || number::text, 'bafyretentionbulk', $1, $3, $3,
			CASE WHEN number <= $5 THEN $6::timestamptz
				WHEN number = $7 THEN $8::timestamptz
				WHEN number = $9 THEN $10::timestamptz
				WHEN number = $4 THEN $12::timestamptz ELSE $11::timestamptz END
		FROM generate_series(1, $4) AS number RETURNING id, record_uri`,
		recipient, uriPrefix, "at://"+recipient+"/social.coves.community.postv2/retention",
		total, oldCount, oldAt, boundaryIndex, boundaryAt, pastBoundaryIndex, pastBoundaryAt, recentAt, newestAt)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id int64
		var uri string
		require.NoError(t, rows.Scan(&id, &uri))
		ids[labelPrefix+"/"+strings.TrimPrefix(uri, uriPrefix)] = id
	}
	require.NoError(t, rows.Err())
	require.Len(t, ids, total, "one labelled id per bulk row")
}

func TestNotificationRetention_UnreadOverflowUsesStrictCapAndExactHours(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	retentionNewYorkTimezone(t, db)
	newest := time.Date(2026, time.December, 1, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	seenAt := newest.Add(-4800 * time.Hour)
	userE := retentionUser(t, db)
	userF := retentionUser(t, db)
	userG := retentionUser(t, db)
	userH := retentionUser(t, db)
	userI := retentionUser(t, db)
	retentionSeenAt(t, db, userE, seenAt)
	retentionSeenAt(t, db, userF, seenAt)
	retentionSeenAt(t, db, userG, nil)
	ids := make(map[string]int64)
	expected := make(map[string]bool)
	addBulk := func(label, recipient string, total, oldCount int, oldAt time.Time,
		boundaryIndex int, boundaryAt time.Time, pastBoundaryIndex int, pastBoundaryAt time.Time,
	) {
		t.Helper()
		bulk := make(map[string]int64)
		retentionBulkReplies(t, db, bulk, label, recipient, total, oldCount, oldAt,
			boundaryIndex, boundaryAt, pastBoundaryIndex, pastBoundaryAt, newest.Add(-time.Hour), newest)
		for name, id := range bulk {
			ids[name] = id
			expected[name] = true
		}
	}
	addBulk("e", userE, 500, 2, newest.Add(-4560*time.Hour), -1, newest, -1, newest)
	retentionRow(t, db, ids, "e_at_seen", userE, "postReply", seenAt)
	expected["e_at_seen"] = true
	addBulk("f", userF, 500, 1, newest.Add(-4560*time.Hour), 2, newest.Add(-4320*time.Hour),
		3, newest.Add(-4320*time.Hour-time.Microsecond))
	retentionRow(t, db, ids, "f_at_seen", userF, "postReply", seenAt)
	retentionRow(t, db, ids, "f_before_seen", userF, "postReply", seenAt.Add(-time.Hour))
	// More than 180 days before the table-wide newest row (user I's) but within
	// 180 days of F's own newest row, so the window must be per recipient.
	retentionRow(t, db, ids, "f_within_own_window", userF, "postReply", newest.Add(-4000*time.Hour))
	expected["f_at_seen"] = true
	expected["f_before_seen"] = true
	expected["f_within_own_window"] = true
	delete(expected, "f/1")
	delete(expected, "f/3")
	addBulk("g", userG, 600, 50, newest.Add(-4560*time.Hour), -1, newest, -1, newest)
	addBulk("h", userH, 600, 50, newest.Add(-4560*time.Hour), -1, newest, -1, newest)
	retentionRow(t, db, ids, "i_table_newest", userI, "postReply", newest.Add(1000*time.Hour))
	expected["i_table_newest"] = true

	unreadCount := func(recipient string) int {
		t.Helper()
		var count int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM notifications n
			JOIN notification_state state ON state.did = n.recipient_did
			WHERE n.recipient_did = $1 AND n.sort_at > state.seen_at`, recipient).Scan(&count))
		return count
	}
	require.Equal(t, 500, unreadCount(userE), "E must sit exactly at the unread cap")
	require.Equal(t, 501, unreadCount(userF), "F must be exactly one over the unread cap")

	deleted, err := retentionSweeper(t, db).SweepUnreadOverflow(context.Background())
	require.NoError(t, err)
	survivors := retentionSurvivors(t, db, ids)
	require.False(t, survivors["f/1"], "F's 190-day-old unread row must be deleted")
	require.False(t, survivors["f/3"], "F's unread row one microsecond beyond the cutoff must be deleted")
	require.True(t, survivors["f_within_own_window"],
		"F's unread row within 180 days of F's own newest row must survive even when another recipient has newer rows")
	require.Equal(t, expected, survivors, "only F's two old unread rows should be deleted")
	require.EqualValues(t, 2, deleted)
}

func TestNotificationRetention_EmptyUpvoteGroupsRequireQualifyingVoterOnSameSubjectAndRecipient(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	first := retentionUser(t, db)
	second := retentionUser(t, db)
	votes := qualifyingUpvoteFixture{db: db}
	now := time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	ids := make(map[string]int64)
	groupWithRoot := func(label, recipient, subject, rootPost string) {
		t.Helper()
		var id int64
		require.NoError(t, db.QueryRow(`INSERT INTO notifications
			(recipient_did, reason, subject_uri, root_post_uri, sort_at)
			VALUES ($1, 'upvote', $2, $3, $4) RETURNING id`, recipient, subject, rootPost, now).Scan(&id))
		ids[label] = id
	}
	group := func(label, recipient, subject string) {
		t.Helper()
		groupWithRoot(label, recipient, subject, subject)
	}
	subject := func(label string) string {
		return "at://" + first + "/social.coves.community.postv2/" + label
	}
	voter := func() string { return "did:plc:" + testkit.UniqueID(t) }
	retracted, erased, blockedByRecipient, blockingRecipient, mixed, different, sharedVoter := voter(), voter(), voter(), voter(), voter(), voter(), voter()
	commentVoter, rootPostVoter := voter(), voter()
	votes.insertVote(t, retracted, subject("retracted"), "up", now, true)
	votes.insertVote(t, erased, subject("erased"), "up", now, false)
	_, err := db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, erased)
	require.NoError(t, err)
	votes.insertBlock(t, first, blockedByRecipient)
	votes.insertVote(t, blockedByRecipient, subject("blocked_by_recipient"), "up", now, false)
	votes.insertBlock(t, blockingRecipient, first)
	votes.insertVote(t, blockingRecipient, subject("blocking_recipient"), "up", now, false)
	votes.insertVote(t, first, subject("self"), "up", now, false)
	votes.insertVote(t, different, subject("different_vote"), "up", now, false)
	votes.insertVote(t, mixed, subject("mixed"), "up", now, false)
	votes.insertVote(t, blockedByRecipient, subject("mixed"), "up", now, false)
	votes.insertVote(t, sharedVoter, subject("shared"), "up", now, false)
	votes.insertBlock(t, second, sharedVoter)
	for _, label := range []string{"retracted", "erased", "blocked_by_recipient", "blocking_recipient", "self", "different_group", "mixed"} {
		group(label, first, subject(label))
	}
	group("shared_first", first, subject("shared"))
	group("shared_second", second, subject("shared"))
	retentionRow(t, db, ids, "reply_without_votes", first, "postReply", now)
	// Comment groups are judged by votes on the comment, never on its root post.
	comment := func(label string) string {
		return "at://" + first + "/social.coves.community.comment/" + label
	}
	votes.insertVote(t, commentVoter, comment("comment_voted"), "up", now, false)
	groupWithRoot("comment_voted", first, comment("comment_voted"), subject("comment_voted_root"))
	votes.insertVote(t, rootPostVoter, subject("comment_unvoted_root"), "up", now, false)
	groupWithRoot("comment_unvoted", first, comment("comment_unvoted"), subject("comment_unvoted_root"))

	deleted, err := retentionSweeper(t, db).SweepEmptyUpvoteGroups(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]bool{
		"mixed": true, "shared_first": true, "reply_without_votes": true, "comment_voted": true,
	}, retentionSurvivors(t, db, ids))
	require.EqualValues(t, 8, deleted)
}

// A row is swept once a required reference has read hidden for longer than the
// window. Placeholders, and rows hidden only by blocks, preferences or the
// upvote alive rule, are never swept. Each shape returns the notification's
// reason, record, subject and root; every other reference is indexed and live.
func TestNotificationRetention_HiddenReferencesSweptAfterWindow(t *testing.T) {
	t.Parallel()
	type shape func(t *testing.T, f *unreadVisibilityFixture) (reason, record, subject, root string)
	subjectPost := func(prepare func(t *testing.T, f *unreadVisibilityFixture) string) shape {
		return func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			return "postReply", f.comment(t, f.root), prepare(t, f), f.root
		}
	}
	visibleReply := func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
		return "postReply", f.comment(t, f.root), f.root, f.root
	}
	hidden := []struct {
		name  string
		shape shape
	}{
		{"root never-public post", func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			root := f.post(t, posts.AdmissionStatusPending, false)
			return "commentReply", f.comment(t, root), f.comment(t, root), root
		}},
		{"record never-public mention post", func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			return "mention", f.post(t, posts.AdmissionStatusPending, false), "", f.root
		}},
		{"record unindexed reply comment", func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			return "postReply", "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID(), f.root, f.root
		}},
		{"subject never-public post", subjectPost(func(t *testing.T, f *unreadVisibilityFixture) string {
			return f.post(t, posts.AdmissionStatusPending, false)
		})},
		{"subject accepted post with mismatched CID", subjectPost(func(t *testing.T, f *unreadVisibilityFixture) string {
			uri := f.post(t, "", false)
			seedVisibilityAdmissionDriftedCID(t, f.db, f.community, uri)
			return uri
		})},
		{"subject removed by community without withdrawal marker", subjectPost(func(t *testing.T, f *unreadVisibilityFixture) string {
			return f.post(t, posts.AdmissionStatusRemoved, false)
		})},
		{"subject author-deleted without withdrawal marker", subjectPost(func(t *testing.T, f *unreadVisibilityFixture) string {
			uri := f.post(t, posts.AdmissionStatusAccepted, false)
			f.deletePost(t, uri)
			return uri
		})},
		{"subject unindexed comment", func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			return "commentReply", f.comment(t, f.root), "at://" + f.recipient + "/social.coves.community.comment/" + testkit.TID(), f.root
		}},
		{"record in unsupported collection", func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			return "mention", "at://" + f.actor + "/app.bsky.feed.post/" + testkit.TID(), "", f.root
		}},
	}
	type row struct {
		name     string
		shape    shape
		ageHours int
		counted  bool // CountUnread, as a fixture check of the shape's read-time visibility
		swept    bool
		// neverSeen checks the fixture precondition that the recipient has no
		// notification_state row, so no seen_at or read-retention rule applies.
		neverSeen bool
	}
	var rows []row
	for _, h := range hidden {
		rows = append(rows,
			row{name: h.name + " past the window", shape: h.shape, ageHours: 200, swept: true},
			row{name: h.name + " within the window", shape: h.shape, ageHours: 100})
	}
	neverPublicSubject := subjectPost(func(t *testing.T, f *unreadVisibilityFixture) string {
		return f.post(t, posts.AdmissionStatusPending, false)
	})
	rows = append(rows,
		row{name: "never-seen recipient hidden subject far past the window", shape: neverPublicSubject, ageHours: 400, swept: true, neverSeen: true},
		row{name: "never-seen recipient hidden subject at the oldest age", shape: neverPublicSubject, ageHours: 2000, swept: true, neverSeen: true})
	rows = append(rows,
		row{name: "author-deleted placeholder", ageHours: 200, counted: true, shape: subjectPost(func(t *testing.T, f *unreadVisibilityFixture) string {
			uri := f.post(t, posts.AdmissionStatusAccepted, false)
			f.deletePost(t, uri)
			seedWithdrawalMarker(t, f.db, uri, "authorDelete", nil)
			return uri
		})},
		row{name: "deleted comment placeholder", ageHours: 200, counted: true, shape: func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			subject := f.comment(t, f.root)
			f.deleteComment(t, subject)
			return "commentReply", f.comment(t, f.root), subject, f.root
		}},
		row{name: "community-removed placeholder", ageHours: 200, counted: true, shape: subjectPost(func(t *testing.T, f *unreadVisibilityFixture) string {
			uri := f.post(t, posts.AdmissionStatusRemoved, false)
			seedWithdrawalMarker(t, f.db, uri, "communityWithdrawal", "3lqqqqqqqqqq1")
			return uri
		})},
		row{name: "server-admin-removed placeholder", ageHours: 200, counted: true, shape: subjectPost(func(t *testing.T, f *unreadVisibilityFixture) string {
			uri := f.post(t, posts.AdmissionStatusAccepted, false)
			seedModerationDecision(t, f.db, uri, "removal", "", true)
			return uri
		})},
		row{name: "hidden only by a block", ageHours: 200, shape: func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			f.insertBlock(t, f.recipient, f.actor)
			return visibleReply(t, f)
		}},
		row{name: "hidden only by a disabled preference", ageHours: 200, shape: func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			retentionSeenAt(t, f.db, f.recipient, nil)
			f.setDisabledReasons(t, f.recipient, []string{"postReply"})
			return visibleReply(t, f)
		}},
		row{name: "hidden only by a dead upvote group", ageHours: 200, shape: func(t *testing.T, f *unreadVisibilityFixture) (string, string, string, string) {
			return "upvote", "", f.root, f.root
		}},
		row{name: "fully visible", ageHours: 200, counted: true, shape: visibleReply},
	)
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUnreadCountFixture(t)
			reason, record, subject, root := tc.shape(t, f)
			f.listNotificationAt(t, f.recipient, f.actor, reason, record, subject, root, f.sortAt)
			var id int64
			require.NoError(t, f.db.QueryRow(`UPDATE notifications SET sort_at = now() - make_interval(hours => $2)
				WHERE recipient_did = $1 RETURNING id`, f.recipient, tc.ageHours).Scan(&id))
			if tc.neverSeen {
				require.Zero(t, retentionBatchCount(t, f.db, `SELECT count(*) FROM notification_state WHERE did = $1`, f.recipient),
					"the recipient must never have set seen_at")
			}
			counted := 0
			if tc.counted {
				counted = 1
			}
			f.requireCount(t, counted)

			deleted, err := retentionSweeper(t, f.db).SweepHiddenReferenceNotifications(context.Background())
			require.NoError(t, err)
			remaining := retentionBatchCount(t, f.db, `SELECT count(*) FROM notifications WHERE id = $1`, id)
			if tc.swept {
				require.EqualValues(t, 1, deleted, "the hidden row past the window must be swept")
				require.Zero(t, remaining)
				return
			}
			require.Zero(t, deleted)
			require.Equal(t, 1, remaining, "the row must survive the hidden-reference sweep")
		})
	}
}
