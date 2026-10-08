//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func (f *unreadVisibilityFixture) listControlRecord(t *testing.T) string {
	t.Helper()
	var record string
	require.NoError(t, f.db.QueryRowContext(context.Background(),
		`SELECT record_uri FROM notifications WHERE recipient_did = $1`, f.recipient).Scan(&record))
	return record
}

// Insert a notification at an explicit time, preserving the fixture's time for
// seeding its referenced posts and comments. Upvote groups have no record/actor.
func (f *unreadVisibilityFixture) listNotificationAt(t *testing.T, recipient, actor, reason, record, subject, root string, at time.Time) {
	t.Helper()
	var recordURI, recordCID, actorDID, subjectURI, createdAt any = record, "bafyunreadrecord", actor, subject, at
	if reason == "mention" {
		subjectURI = nil
	}
	if reason == "upvote" {
		recordURI, recordCID, actorDID, createdAt = nil, nil, nil, nil
	}
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		recipient, reason, recordURI, recordCID, actorDID, subjectURI, root, createdAt, at.Truncate(time.Microsecond))
	require.NoError(t, err)
}

func TestNotificationList_AgreesWithCountForReplyRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, position string
		status               posts.AdmissionStatus
		legacy, drifted      bool
		wantLabels           []string
		wantCount            int
	}{
		{"subject pending", "post", "subject", posts.AdmissionStatusPending, false, false, []string{"control"}, 1},
		{"subject rejected", "post", "subject", posts.AdmissionStatusRejected, false, false, []string{"control"}, 1},
		{"subject pending reacceptance", "post", "subject", posts.AdmissionStatusPendingReacceptance, false, false, []string{"control"}, 1},
		{"subject no admission", "post", "subject", "", false, false, []string{"control"}, 1},
		{"subject CID drifted", "post", "subject", "", false, true, []string{"control"}, 1},
		{"subject accepted current CID", "post", "subject", posts.AdmissionStatusAccepted, false, false, []string{"case", "control"}, 2},
		{"root pending", "post", "root", posts.AdmissionStatusPending, false, false, []string{"control"}, 1},
		{"root accepted", "post", "root", posts.AdmissionStatusAccepted, false, false, []string{"case", "control"}, 2},
		{"legacy root live", "post", "root", "", true, false, []string{"case", "control"}, 2},
		{"subject deleted without marker", "deletedPost", "subject", posts.AdmissionStatusAccepted, false, false, []string{"control"}, 1},
		{"subject removed without marker", "post", "subject", posts.AdmissionStatusRemoved, false, false, []string{"control"}, 1},
		{"legacy root deleted without marker", "deletedPost", "root", "", true, false, []string{"control"}, 1},
		{"author-deleted subject accepted with marker", "deletedPostMarked", "subject", posts.AdmissionStatusAccepted, false, false, []string{"case", "control"}, 2},
		{"author-deleted subject admission withdrawn with marker", "deletedPostMarked", "subject", "", false, false, []string{"case", "control"}, 2},
		{"author-deleted root with marker", "deletedPostMarked", "root", posts.AdmissionStatusAccepted, false, false, []string{"case", "control"}, 2},
		{"community-removed subject with marker", "removedPostMarked", "subject", posts.AdmissionStatusRemoved, false, false, []string{"case", "control"}, 2},
		{"community-removed root with marker", "removedPostMarked", "root", posts.AdmissionStatusRemoved, false, false, []string{"case", "control"}, 2},
		{"deleted record comment", "deletedRecord", "record", "", false, false, []string{"case", "control"}, 2},
		{"deleted subject comment", "deletedSubject", "subject", "", false, false, []string{"case", "control"}, 2},
		{"community withdrawal lifted with stale marker", "liftedRemoval", "subject", posts.AdmissionStatusPending, false, false, []string{"control"}, 1},
		{"recipient blocks placeholder actor", "placeholderBlock", "recipient", "", false, false, []string{"control"}, 1},
		{"placeholder actor blocks recipient", "placeholderBlock", "actor", "", false, false, []string{"control"}, 1},
		{"disabled placeholder postReply", "placeholderDisabled", "postReply", "", false, false, []string{"control"}, 1},
		{"unindexed subject post", "unindexed", "subjectPost", "", false, false, []string{"control"}, 1},
		{"unindexed root post", "unindexed", "rootPost", "", false, false, []string{"control"}, 1},
		{"unindexed record comment", "unindexed", "recordComment", "", false, false, []string{"control"}, 1},
		{"unindexed subject comment", "unindexed", "subjectComment", "", false, false, []string{"control"}, 1},
		{"recipient blocks actor", "block", "recipient", "", false, false, []string{"control"}, 1},
		{"actor blocks recipient", "block", "actor", "", false, false, []string{"control"}, 1},
		{"disabled postReply", "disabled", "postReply", "", false, false, []string{"control"}, 1},
		{"disabled commentReply", "disabled", "commentReply", "", false, false, []string{"post control"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			control := f.listControlRecord(t)
			labels := map[string]string{control: "control"}
			actor := f.actor
			if tc.kind == "block" || tc.kind == "placeholderBlock" || tc.position == "commentReply" {
				actor = "did:plc:listcase" + testkit.UniqueID(t)
			}
			record := seedActorComment(t, f.db, actor, f.root, testkit.TID(), f.sortAt)
			subject, root, reason := f.root, f.root, "postReply"
			switch tc.kind {
			case "post", "deletedPost", "deletedPostMarked", "removedPostMarked", "liftedRemoval":
				post := f.post(t, tc.status, tc.legacy)
				if tc.drifted {
					seedVisibilityAdmissionDriftedCID(t, f.db, f.community, post)
				}
				if tc.kind == "deletedPost" || tc.kind == "deletedPostMarked" {
					f.deletePost(t, post)
				}
				if tc.kind == "deletedPostMarked" {
					seedWithdrawalMarker(t, f.db, post, "authorDelete", nil)
				}
				if tc.kind == "removedPostMarked" || tc.kind == "liftedRemoval" {
					seedWithdrawalMarker(t, f.db, post, "communityWithdrawal", "3lqqqqqqqqqq1")
				}
				if tc.position == "subject" {
					subject = post
				} else {
					root, subject, reason = post, f.comment(t, post), "commentReply"
				}
			case "unindexed":
				switch tc.position {
				case "subjectPost":
					subject = postV2URI(f.recipient, testkit.TID())
				case "rootPost":
					root = postV2URI(f.recipient, testkit.TID())
				case "recordComment":
					record = "at://" + actor + "/social.coves.community.comment/" + testkit.TID()
				case "subjectComment":
					subject = "at://" + f.recipient + "/social.coves.community.comment/" + testkit.TID()
				}
				if tc.position == "subjectComment" || tc.position == "recordComment" || tc.position == "rootPost" {
					reason = "commentReply"
					if tc.position != "subjectComment" {
						subject = f.comment(t, f.root)
					}
				}
			case "block":
				if tc.position == "recipient" {
					f.insertBlock(t, f.recipient, actor)
				} else {
					f.insertBlock(t, actor, f.recipient)
				}
			case "placeholderBlock":
				f.deleteComment(t, record)
				if tc.position == "recipient" {
					f.insertBlock(t, f.recipient, actor)
				} else {
					f.insertBlock(t, actor, f.recipient)
				}
			case "placeholderDisabled":
				f.deleteComment(t, record)
				f.setDisabledReasons(t, f.recipient, []string{tc.position})
			case "deletedRecord":
				f.deleteComment(t, record)
			case "deletedSubject":
				reason = "commentReply"
				subject = f.comment(t, f.root)
				f.deleteComment(t, subject)
			case "disabled":
				if tc.position == "commentReply" {
					reason = "commentReply"
					subject = f.comment(t, f.root)
					postControl, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, f.sortAt.Add(2*time.Second), f.sortAt)
					labels[postControl] = "post control"
				}
				f.setDisabledReasons(t, f.recipient, []string{tc.position})
			}
			f.listNotificationAt(t, f.recipient, actor, reason, record, subject, root, f.sortAt.Add(time.Second))
			labels[record] = "case"
			f.requireCount(t, tc.wantCount)
			page := f.listPage(t, f.recipient, "", 10)
			require.Equal(t, tc.wantLabels, f.recordLabels(t, page, labels))
			require.Empty(t, page.Cursor)
		})
	}
}

func TestNotificationList_NeverSeenUnreadIsNewestVisibleRow(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"newest hidden", "newest upvote with null state", "newest placeholder", "tied replies across pages", "foreign newer reply"} {
		t.Run(scenario, func(t *testing.T) {
			f := newUnreadCountFixture(t)
			base := f.sortAt
			labels := make(map[string]string)
			add := func(label string, at time.Time) {
				record, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, at, base)
				labels[record] = label
			}
			add("R1", base)
			switch scenario {
			case "newest hidden":
				add("R2", base.Add(time.Second))
				missingRoot := postV2URI(f.recipient, testkit.TID())
				f.listNotificationAt(t, f.recipient, f.actor, "postReply", f.comment(t, f.root), missingRoot, missingRoot, base.Add(2*time.Second))
			case "newest upvote with null state":
				retentionSeenAt(t, f.db, f.recipient, nil)
				f.insertVote(t, "did:plc:listvoter"+testkit.UniqueID(t), f.root, false)
				f.listNotificationAt(t, f.recipient, "", "upvote", "", f.root, f.root, base.Add(time.Second))
			case "newest placeholder":
				record := f.comment(t, f.root)
				f.deleteComment(t, record)
				labels[record] = "P"
				f.listNotificationAt(t, f.recipient, f.actor, "commentReply", record, f.comment(t, f.root), f.root, base.Add(time.Second))
			case "tied replies across pages":
				add("R2a", base.Add(time.Second))
				add("R2b", base.Add(time.Second))
			case "foreign newer reply":
				other := retentionUser(t, f.db)
				f.insertListedReply(t, other, "postReply", f.root, base.Add(time.Second), base)
			}
			f.requireCount(t, 1)
			if scenario == "tied replies across pages" {
				cursor := ""
				for _, want := range []struct {
					label, next string
					isRead      bool
				}{{"R2b", "more", false}, {"R2a", "more", true}, {"R1", "end", true}} {
					page := f.listPage(t, f.recipient, cursor, 1)
					require.Equal(t, []string{want.label}, f.recordLabels(t, page, labels))
					require.Equal(t, want.isRead, page.Notifications[0].IsRead)
					require.Nil(t, page.SeenAt)
					if want.next == "more" {
						require.NotEmpty(t, page.Cursor)
					} else {
						require.Empty(t, page.Cursor)
					}
					cursor = page.Cursor
				}
				return
			}
			page := f.listPage(t, f.recipient, "", 10)
			switch scenario {
			case "newest upvote with null state":
				require.Len(t, page.Notifications, 2)
				require.Equal(t, "upvote", string(page.Notifications[0].Reason))
				require.Equal(t, f.root, page.Notifications[0].SubjectURI)
				require.False(t, page.Notifications[0].IsRead)
				require.Equal(t, "R1", labels[page.Notifications[1].RecordURI])
				require.True(t, page.Notifications[1].IsRead)
			case "newest placeholder":
				require.Equal(t, []string{"P", "R1"}, f.recordLabels(t, page, labels))
				require.Equal(t, []bool{false, true}, []bool{page.Notifications[0].IsRead, page.Notifications[1].IsRead})
			case "newest hidden":
				require.Equal(t, []string{"R2", "R1"}, f.recordLabels(t, page, labels))
				require.Equal(t, []bool{false, true}, []bool{page.Notifications[0].IsRead, page.Notifications[1].IsRead})
			case "foreign newer reply":
				require.Equal(t, []string{"R1"}, f.recordLabels(t, page, labels))
				require.False(t, page.Notifications[0].IsRead)
			default:
				require.Equal(t, []string{"R1"}, f.recordLabels(t, page, labels))
				require.True(t, page.Notifications[0].IsRead)
			}
			require.Nil(t, page.SeenAt)
			require.Empty(t, page.Cursor)
		})
	}
}

func TestNotificationList_FullPageThroughHiddenRows(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := f.sortAt
	retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Microsecond))
	pendingRoot := f.post(t, posts.AdmissionStatusPending, false)
	labels := make(map[string]string)
	for number := 1; number <= 60; number++ {
		at := base.Add(time.Duration(number) * time.Microsecond)
		switch number % 3 {
		case 1:
			record, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, at, base)
			labels[record] = "V" + strconv.Itoa((number+2)/3)
		case 2:
			record := f.comment(t, f.root)
			f.listNotificationAt(t, f.recipient, f.actor, "postReply", record, pendingRoot, pendingRoot, at)
		case 0:
			missing := "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID()
			f.listNotificationAt(t, f.recipient, f.actor, "commentReply", missing, f.comment(t, f.root), f.root, at)
		}
	}
	page := f.listPage(t, f.recipient, "", 20)
	require.Equal(t, []string{
		"V20", "V19", "V18", "V17", "V16", "V15", "V14", "V13", "V12", "V11",
		"V10", "V9", "V8", "V7", "V6", "V5", "V4", "V3", "V2", "V1",
	}, f.recordLabels(t, page, labels))
	require.Empty(t, page.Cursor)
}

// Subplans used to decide read state or visibility do not satisfy the outer
// recipient-page access-path contract and cannot introduce an outer sort.
func listMainPlanAccess(node map[string]any) (index, sort bool) {
	if node["Index Name"] == "idx_notifications_recipient_sort" &&
		(node["Node Type"] == "Index Scan" || node["Node Type"] == "Index Only Scan") {
		index = true
	}
	if node["Node Type"] == "Sort" || node["Node Type"] == "Incremental Sort" {
		sort = true
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		plan, ok := child.(map[string]any)
		if !ok || plan["Parent Relationship"] == "InitPlan" || plan["Parent Relationship"] == "SubPlan" {
			continue
		}
		childIndex, childSort := listMainPlanAccess(plan)
		index, sort = index || childIndex, sort || childSort
	}
	return index, sort
}

func TestNotificationList_RecipientSortIndexWithMostlyBlocked(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := f.sortAt
	retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Second))
	blocked := "did:plc:listblocked" + testkit.UniqueID(t)
	f.insertBlock(t, f.recipient, blocked)
	prefix := "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID() + "-"
	_, err := f.db.ExecContext(context.Background(), `
		WITH records AS MATERIALIZED (
			SELECT number, $2::text || number::text AS uri,
				CASE WHEN number % 10 = 0 THEN $3::text ELSE $4::text END AS actor,
				$6::timestamptz + number * interval '1 microsecond' AS sort_at
			FROM generate_series(1, 10000) AS number
		), inserted AS (
			INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
			SELECT uri, 'bafylistcomment', number::text, actor, $5, 'bafylistroot', $5, 'bafylistroot', 'list comment', sort_at
			FROM records RETURNING uri
		)
		INSERT INTO notifications (recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		SELECT $1, 'postReply', records.uri, 'bafylistcomment', records.actor, $5, $5, records.sort_at, records.sort_at
		FROM records JOIN inserted ON inserted.uri = records.uri`,
		f.recipient, prefix, f.actor, blocked, f.root, base)
	require.NoError(t, err)

	otherPrefix := "did:plc:listother" + testkit.UniqueID(t)
	_, err = f.db.ExecContext(context.Background(), `INSERT INTO users (did, handle, pds_url)
		SELECT $1 || number::text, $2 || number::text || '.test', 'https://pds.test'
		FROM generate_series(1, 20) AS number`, otherPrefix, testkit.UniqueID(t))
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(), `
		WITH records AS MATERIALIZED (
			SELECT number, $1::text || number::text AS uri,
				$2::text || (((number - 1) % 20) + 1)::text AS recipient,
				$4::timestamptz + number * interval '1 microsecond' AS sort_at
			FROM generate_series(1, 10000) AS number
		), inserted AS (
			INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
			SELECT uri, 'bafylistcomment', number::text, $3, $5, 'bafylistroot', $5, 'bafylistroot', 'list comment', sort_at
			FROM records RETURNING uri
		)
		INSERT INTO notifications (recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		SELECT records.recipient, 'postReply', records.uri, 'bafylistcomment', $3, $5, $5, records.sort_at, records.sort_at
		FROM records JOIN inserted ON inserted.uri = records.uri`, prefix+"other-", otherPrefix, f.actor, base.Add(time.Second), f.root)
	require.NoError(t, err)

	for _, table := range []string{"notifications", "notification_state", "posts", "community_post_admissions", "comments", "notification_public_post_withdrawals", "user_blocks", "votes"} {
		_, err := f.db.ExecContext(context.Background(), "ANALYZE "+table)
		require.NoError(t, err, "analyzing %s", table)
	}
	var midID int64
	require.NoError(t, f.db.QueryRowContext(context.Background(), `SELECT id FROM notifications WHERE record_uri = $1`, prefix+"5000").Scan(&midID))
	for _, tc := range []struct {
		name   string
		sortAt any
		id     any
	}{
		{"first page", nil, nil},
		{"mid-history cursor", base.Add(5000 * time.Microsecond), midID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			require.NoError(t, f.db.QueryRowContext(context.Background(), "EXPLAIN (FORMAT JSON) "+listNotificationsSQL,
				f.recipient, tc.sortAt, tc.id, 51).Scan(&raw))
			var plans []struct {
				Plan map[string]any `json:"Plan"`
			}
			require.NoError(t, json.Unmarshal(raw, &plans))
			require.Len(t, plans, 1)
			index, sort := listMainPlanAccess(plans[0].Plan)
			require.True(t, index, "outer path must scan idx_notifications_recipient_sort: %s", raw)
			require.False(t, sort, "outer path must not sort: %s", raw)
		})
	}
	labels := make(map[string]string)
	want := make([]string, 0, 50)
	for number := 10000; number >= 9510; number -= 10 {
		label := "V" + strconv.Itoa(number)
		labels[prefix+strconv.Itoa(number)] = label
		want = append(want, label)
	}
	page := f.listPage(t, f.recipient, "", 50)
	require.Equal(t, want, f.recordLabels(t, page, labels))
	require.NotEmpty(t, page.Cursor)
}
