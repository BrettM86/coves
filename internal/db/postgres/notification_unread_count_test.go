//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unlike newUnreadVisibilityFixture, these cases need to choose whether state
// and the visible control row exist at all.
func newUnreadCountFixture(t *testing.T) *unreadVisibilityFixture {
	t.Helper()
	db := testkit.DB(t)
	f := &unreadVisibilityFixture{
		db:        db,
		recipient: retentionUser(t, db),
		actor:     "did:plc:unreadactor" + testkit.UniqueID(t),
		community: visibilityCommunity(t, db, testkit.UniqueID(t)),
		sortAt:    time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond),
	}
	f.root = f.post(t, posts.AdmissionStatusAccepted, false)
	return f
}

func (f *unreadVisibilityFixture) countReplyAt(t *testing.T, sortAt time.Time, root string) string {
	t.Helper()
	f.sortAt = sortAt.Truncate(time.Microsecond)
	record := f.comment(t, root)
	f.notify(t, "postReply", record, root, root)
	return record
}

func TestNotificationUnreadCount_NeverSeenUsesNewestVisible(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		stateRow bool
		caseName string
		want     int
	}{
		{"three visible/null state", true, "three visible", 1},
		{"three visible/no state", false, "three visible", 1},
		{"newest pending/null state", true, "newest pending", 1},
		{"newest pending/no state", false, "newest pending", 1},
		{"newest unindexed record/null state", true, "newest unindexed", 1},
		{"newest unindexed record/no state", false, "newest unindexed", 1},
		{"deleted comment placeholder/null state", true, "deleted comment", 1},
		{"deleted comment placeholder/no state", false, "deleted comment", 1},
		{"removed post placeholder/null state", true, "removed post", 1},
		{"removed post placeholder/no state", false, "removed post", 1},
		{"only hidden/null state", true, "only hidden", 0},
		{"only hidden/no state", false, "only hidden", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadCountFixture(t)
			if tc.stateRow {
				retentionSeenAt(t, f.db, f.recipient, nil)
			}
			base := f.sortAt
			switch tc.caseName {
			case "three visible":
				for _, offset := range []time.Duration{0, time.Second, 2 * time.Second} {
					f.countReplyAt(t, base.Add(offset), f.root)
				}
			case "newest pending":
				f.countReplyAt(t, base, f.root)
				pending := f.post(t, posts.AdmissionStatusPending, false)
				f.countReplyAt(t, base.Add(time.Second), pending)
			case "newest unindexed":
				f.countReplyAt(t, base, f.root)
				f.sortAt = base.Add(time.Second)
				missing := "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID()
				f.notify(t, "commentReply", missing, f.comment(t, f.root), f.root)
			case "deleted comment":
				pending := f.post(t, posts.AdmissionStatusPending, false)
				f.countReplyAt(t, base, pending)
				f.sortAt = base.Add(time.Second)
				record := f.comment(t, f.root)
				f.deleteComment(t, record)
				f.notify(t, "commentReply", record, f.comment(t, f.root), f.root)
			case "removed post":
				removed := f.post(t, posts.AdmissionStatusRemoved, false)
				seedWithdrawalMarker(t, f.db, removed, "communityWithdrawal", "3lqqqqqqqqqq1")
				f.countReplyAt(t, base, removed)
				pending := f.post(t, posts.AdmissionStatusPending, false)
				f.countReplyAt(t, base.Add(time.Second), pending)
			case "only hidden":
				pending := f.post(t, posts.AdmissionStatusPending, false)
				f.countReplyAt(t, base, pending)
				f.countReplyAt(t, base.Add(time.Second), pending)
			}
			f.requireCount(t, tc.want)
		})
	}
}

func TestNotificationUnreadCount_SeenAtIsExclusive(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	seenAt := f.sortAt
	retentionSeenAt(t, f.db, f.recipient, seenAt)
	for _, offset := range []time.Duration{-time.Second, 0, time.Second, 2 * time.Second} {
		f.countReplyAt(t, seenAt.Add(offset), f.root)
	}
	f.requireCount(t, 2)
}

// Seed indexed record comments and matching notifications together; the caller
// chooses which rows point at the pending root. Every generated URI is distinct.
func insertUnreadCountRows(t *testing.T, db *sql.DB, recipient, actor, prefix, visibleRoot, hiddenRoot string, base time.Time, total int, hiddenMode string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		WITH records AS MATERIALIZED (
			SELECT number, $2::text || number::text AS uri,
				CASE WHEN $8 = 'all' OR ($8 = 'interleaved' AND (number > 9500 OR number % 19 NOT IN (0, 1)))
					THEN $5::text ELSE $4::text END AS root,
				$6::timestamptz + number * interval '1 microsecond' AS sort_at
			FROM generate_series(1, $7) AS number
		), inserted AS (
			INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
			SELECT uri, 'bafycountcomment', number::text, $3, root, 'bafycountroot', root, 'bafycountroot', 'count comment', sort_at
			FROM records RETURNING uri
		)
		INSERT INTO notifications
			(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		SELECT $1, 'postReply', records.uri, 'bafycountcomment', $3, records.root, records.root, records.sort_at, records.sort_at
		FROM records JOIN inserted ON inserted.uri = records.uri`,
		recipient, prefix, actor, visibleRoot, hiddenRoot, base.Truncate(time.Microsecond), total, hiddenMode)
	require.NoError(t, err)
}

func TestNotificationUnreadCount_BoundedVisibleRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, hiddenMode      string
		visible, hidden, want int
	}{
		{"150 visible", "none", 150, 0, 101},
		{"100 visible", "none", 100, 0, 100},
		{"150 newer hidden before 120 visible", "all", 120, 150, 101},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadCountFixture(t)
			base := f.sortAt
			retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Second))
			prefix := "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID() + "-"
			insertUnreadCountRows(t, f.db, f.recipient, f.actor, prefix, f.root, f.root, base, tc.visible, "none")
			if tc.hidden > 0 {
				pending := f.post(t, posts.AdmissionStatusPending, false)
				insertUnreadCountRows(t, f.db, f.recipient, f.actor, prefix+"hidden-", f.root, pending,
					base.Add(time.Second), tc.hidden, tc.hiddenMode)
			}
			f.requireCount(t, tc.want)
		})
	}
}

func unreadCountPlanHasIndex(node map[string]any, index string) bool {
	if node["Index Name"] == index {
		return true
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		plan, ok := child.(map[string]any)
		if ok && unreadCountPlanHasIndex(plan, index) {
			return true
		}
	}
	return false
}

func TestNotificationUnreadCount_RecipientSortIndex(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := f.sortAt
	retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Second))
	pending := f.post(t, posts.AdmissionStatusPending, false)
	prefix := "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID() + "-"
	insertUnreadCountRows(t, f.db, f.recipient, f.actor, prefix, f.root, pending, base, 10000, "interleaved")

	otherPrefix := "did:plc:unreadother" + testkit.UniqueID(t)
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO users (did, handle, pds_url)
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
			SELECT uri, 'bafycountcomment', number::text, $3, $5, 'bafycountroot', $5, 'bafycountroot', 'count comment', sort_at
			FROM records RETURNING uri
		)
		INSERT INTO notifications
			(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		SELECT records.recipient, 'postReply', records.uri, 'bafycountcomment', $3, $5, $5, records.sort_at, records.sort_at
		FROM records JOIN inserted ON inserted.uri = records.uri`,
		prefix+"other-", otherPrefix, f.actor, base, f.root)
	require.NoError(t, err)

	for _, table := range []string{"notifications", "notification_state", "posts", "community_post_admissions", "comments", "notification_public_post_withdrawals"} {
		_, err := f.db.ExecContext(context.Background(), "ANALYZE "+table)
		require.NoError(t, err, "analyzing %s", table)
	}
	var raw []byte
	require.NoError(t, f.db.QueryRowContext(context.Background(), "EXPLAIN (FORMAT JSON) "+countUnreadNotificationsSQL, f.recipient).Scan(&raw))
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &plans))
	require.Len(t, plans, 1)
	assert.True(t, unreadCountPlanHasIndex(plans[0].Plan, "idx_notifications_recipient_sort"), "EXPLAIN plan: %s", raw)
	f.requireCount(t, 101)
}

func unreadCountPlanIndexNode(node map[string]any, index string) map[string]any {
	if node["Index Name"] == index {
		return node
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		if plan, ok := child.(map[string]any); ok {
			if found := unreadCountPlanIndexNode(plan, index); found != nil {
				return found
			}
		}
	}
	return nil
}

// A recipient with a long read history must not walk it on every badge poll:
// the recipient index scan has to stop at the seen_at boundary.
func TestNotificationUnreadCount_SeenAtBoundsRecipientIndexScan(t *testing.T) {
	t.Parallel()
	const readRows, unreadRows = 10000, 5
	f := newUnreadCountFixture(t)
	base := f.sortAt
	seenAt := base.Add(readRows * time.Microsecond)
	retentionSeenAt(t, f.db, f.recipient, seenAt)
	prefix := "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID() + "-"
	insertUnreadCountRows(t, f.db, f.recipient, f.actor, prefix, f.root, f.root, base, readRows, "none")
	insertUnreadCountRows(t, f.db, f.recipient, f.actor, prefix+"unread-", f.root, f.root, seenAt, unreadRows, "none")

	for _, table := range []string{"notifications", "notification_state", "posts", "community_post_admissions", "comments", "notification_public_post_withdrawals"} {
		_, err := f.db.ExecContext(context.Background(), "ANALYZE "+table)
		require.NoError(t, err, "analyzing %s", table)
	}
	var raw []byte
	require.NoError(t, f.db.QueryRowContext(context.Background(), "EXPLAIN (ANALYZE, FORMAT JSON) "+countUnreadNotificationsSQL, f.recipient).Scan(&raw))
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &plans))
	require.Len(t, plans, 1)
	node := unreadCountPlanIndexNode(plans[0].Plan, "idx_notifications_recipient_sort")
	require.NotNil(t, node, "EXPLAIN plan: %s", raw)
	indexCond, _ := node["Index Cond"].(string)
	assert.Contains(t, indexCond, "sort_at", "EXPLAIN plan: %s", raw)
	actualRows, _ := node["Actual Rows"].(float64)
	actualLoops, _ := node["Actual Loops"].(float64)
	assert.LessOrEqual(t, actualRows*actualLoops, float64(unreadRows), "rows read from recipient index; EXPLAIN plan: %s", raw)
	f.requireCount(t, unreadRows)
}
