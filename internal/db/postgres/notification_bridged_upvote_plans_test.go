//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func bridgedCountPlan(t *testing.T, db *sql.DB, statement string, args ...any) (map[string]any, []byte) {
	t.Helper()
	var raw []byte
	require.NoError(t, db.QueryRowContext(context.Background(), "EXPLAIN (ANALYZE, FORMAT JSON) "+statement, args...).Scan(&raw))
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &plans))
	require.Len(t, plans, 1)
	return plans[0].Plan, raw
}

// This is a gate-on guard of the existing seen-at access-path contract; it can
// pass while the builder still ignores the gate.
func TestNotificationUnreadCount_BridgedSeenAtBoundsRecipientIndexScan(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := f.sortAt
	seenAt := base.Add(10000 * time.Microsecond)
	retentionSeenAt(t, f.db, f.recipient, seenAt)
	prefix := "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID() + "-"
	insertUnreadCountRows(t, f.db, f.recipient, f.actor, prefix, f.root, f.root, base, 10000, "none")
	insertUnreadCountRows(t, f.db, f.recipient, f.actor, prefix+"unread-", f.root, f.root, seenAt, 5, "none")
	for _, table := range []string{"notifications", "notification_state", "posts", "community_post_admissions", "comments", "notification_public_post_withdrawals"} {
		_, err := f.db.ExecContext(context.Background(), "ANALYZE "+table)
		require.NoError(t, err)
	}
	plan, raw := bridgedCountPlan(t, f.db, buildCountUnreadNotificationsSQL(true), f.recipient)
	node := unreadCountPlanIndexNode(plan, "idx_notifications_recipient_sort")
	require.NotNil(t, node, "EXPLAIN plan: %s", raw)
	indexCond, _ := node["Index Cond"].(string)
	require.Contains(t, indexCond, "sort_at", "EXPLAIN plan: %s", raw)
	actualRows, _ := node["Actual Rows"].(float64)
	actualLoops, _ := node["Actual Loops"].(float64)
	require.LessOrEqual(t, actualRows*actualLoops, float64(5), "rows read from recipient index; EXPLAIN plan: %s", raw)
}

func bridgedListCountingRepository(t *testing.T, source *sql.DB) (notifications.ReadRepository, *listStatementCounter) {
	t.Helper()
	var database string
	require.NoError(t, source.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&database))
	connector, err := pq.NewConnector(testkit.Endpoints().Postgres.URL(database))
	require.NoError(t, err)
	counter := &listStatementCounter{}
	db := sql.OpenDB(&listCountingConnector{underlying: connector, counter: counter})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return NewNotificationRepository(db, WithBridgedUpvoteTotals()).(notifications.ReadRepository), counter
}

func TestNotificationList_BridgedAggregatesUseOneBoundedStatement(t *testing.T) {
	t.Parallel()
	statementTotals := make([]int, 0, 2)
	for _, size := range []int{50, 8} {
		t.Run(strconv.Itoa(size)+" upvote groups", func(t *testing.T) {
			f := newUnreadCountFixture(t)
			retentionSeenAt(t, f.db, f.recipient, f.sortAt.Add(-time.Second))
			for i := 0; i < size; i++ {
				subject := f.post(t, posts.AdmissionStatusAccepted, false)
				if i%2 == 0 {
					setBridgedGroupTotals(t, f.db, "post", subject, 4, 0)
				} else {
					setBridgedGroupTotals(t, f.db, "post", subject, 5, 0)
					f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), subject, "up", f.sortAt)
					f.listVoteAt(t, "did:plc:listvoter"+testkit.UniqueID(t), subject, "up", f.sortAt.Add(time.Microsecond))
				}
				f.listGroupAt(t, subject, subject, f.sortAt.Add(time.Duration(i+1)*time.Second))
			}
			repository, counter := bridgedListCountingRepository(t, f.db)
			page, err := repository.List(context.Background(), f.recipient, "", size)
			require.NoError(t, err)
			require.Equal(t, size, len(page.Notifications), "bridged-only groups must fill the page")
			for _, row := range page.Notifications {
				require.Equal(t, notifications.ReasonUpvote, row.Reason)
				if row.SortAt.Sub(f.sortAt)/time.Second%2 == 1 {
					require.Equal(t, 4, row.UpvoteCount, "bridged-only group %s", row.SubjectURI)
					require.Empty(t, row.RecentUpvoterDIDs)
				} else {
					require.Equal(t, 7, row.UpvoteCount, "mixed group %s", row.SubjectURI)
					require.Len(t, row.RecentUpvoterDIDs, 2)
				}
			}
			require.Equal(t, 3, counter.total(), "one state, one page, exactly one aggregate statement")
			statementTotals = append(statementTotals, counter.total())
		})
	}
	if len(statementTotals) == 2 {
		require.Equal(t, statementTotals[0], statementTotals[1], "50 and 8 rows must cost the same number of statements")
	}
}

// SubPlan and InitPlan children are deliberately included: both can contain
// the lookup for a bridged subject even when the outer scan uses other aliases.
func bridgedPlanAliasNodes(node map[string]any, alias string) []map[string]any {
	var matches []map[string]any
	if node["Alias"] == alias {
		matches = append(matches, node)
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		if plan, ok := child.(map[string]any); ok {
			matches = append(matches, bridgedPlanAliasNodes(plan, alias)...)
		}
	}
	return matches
}

func bridgedURIIndexes(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT indexname FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = $1 AND indexdef LIKE '%(uri)%'`, table)
	require.NoError(t, err)
	defer rows.Close()
	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		indexes[name] = true
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, indexes, "%s must have a uri index", table)
	return indexes
}

func requireBridgedPointLookups(t *testing.T, plan map[string]any, raw []byte, indexes map[string]map[string]bool) {
	t.Helper()
	for alias, table := range map[string]string{"bridged_post": "posts", "bridged_comment": "comments"} {
		nodes := bridgedPlanAliasNodes(plan, alias)
		require.NotEmpty(t, nodes, "missing %s alias node in plan: %s", alias, raw)
		for _, node := range nodes {
			require.Contains(t, []string{"Index Scan", "Index Only Scan"}, node["Node Type"], "%s must use point lookup: %s", alias, raw)
			name, _ := node["Index Name"].(string)
			require.True(t, indexes[table][name], "%s must use %s.uri index, got %q: %s", alias, table, name, raw)
			loops, _ := node["Actual Loops"].(float64)
			require.Greater(t, loops, float64(0), "%s lookup must execute: %s", alias, raw)
		}
	}
}

func TestNotificationRepository_BridgedStatementsProbeSubjectURIIndexes(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := f.sortAt
	post := f.post(t, posts.AdmissionStatusAccepted, false)
	comment := seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), base)
	setBridgedGroupTotals(t, f.db, "post", post, 3, 0)
	setBridgedGroupTotals(t, f.db, "comment", comment, 5, 0)
	f.listGroupAt(t, post, post, base.Add(time.Second))
	f.listGroupAt(t, comment, f.root, base.Add(3*time.Second)) // newest visible row drives the NULL-seen_at InitPlan
	pending := f.post(t, posts.AdmissionStatusPending, false)
	f.listGroupAt(t, pending, pending, base.Add(2*time.Second))
	f.listNotificationAt(t, f.recipient, f.actor, "postReply", f.comment(t, f.root), pending, pending, base.Add(4*time.Second))
	hiddenPrefix := "at://" + f.actor + "/social.coves.community.comment/" + testkit.TID() + "-hidden-"
	insertUnreadCountRows(t, f.db, f.recipient, f.actor, hiddenPrefix, f.root, pending, base.Add(2*time.Second), 40, "all")
	// Add unrelated subjects so scanning posts/comments instead of probing uri
	// indexes is distinguishable after ANALYZE.
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		SELECT $1::text || number::text, 'bafybulkpost', number::text, $2, $3, 'unrelated', $4::timestamptz
		FROM generate_series(1, 2000) number`, post+"-unrelated-", f.recipient, f.community, base)
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(), `INSERT INTO comments
		(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		SELECT $1::text || number::text, 'bafybulkcomment', number::text, $2, $3, 'bafybulkroot',
			$3, 'bafybulkroot', 'unrelated', $4::timestamptz
		FROM generate_series(1, 2000) number`, comment+"-unrelated-", f.actor, f.root, base)
	require.NoError(t, err)
	for _, table := range []string{"notifications", "notification_state", "posts", "comments"} {
		_, err := f.db.ExecContext(context.Background(), "ANALYZE "+table)
		require.NoError(t, err)
	}
	indexes := map[string]map[string]bool{
		"posts": bridgedURIIndexes(t, f.db, "posts"), "comments": bridgedURIIndexes(t, f.db, "comments"),
	}
	countSQL := buildCountUnreadNotificationsSQL(true)
	pageSQL := buildListNotificationsSQL(true)
	aggregateSQL := buildListNotificationUpvotesSQL(true)

	t.Run("count", func(t *testing.T) {
		retentionSeenAt(t, f.db, f.recipient, base) // before both visible groups
		var count int
		require.NoError(t, f.db.QueryRowContext(context.Background(), countSQL, f.recipient).Scan(&count))
		require.Equal(t, 2, count, "only post and comment bridged groups are unread")
		plan, raw := bridgedCountPlan(t, f.db, countSQL, f.recipient)
		node := unreadCountPlanIndexNode(plan, "idx_notifications_recipient_sort")
		require.NotNil(t, node, "recipient index: %s", raw)
		condition, _ := node["Index Cond"].(string)
		require.Contains(t, condition, "sort_at", "recipient index must bound seen_at: %s", raw)
		requireBridgedPointLookups(t, plan, raw, indexes)
	})
	t.Run("page with NULL seen_at", func(t *testing.T) {
		_, err := f.db.ExecContext(context.Background(), `UPDATE notification_state SET seen_at = NULL WHERE did = $1`, f.recipient)
		require.NoError(t, err)
		rows, err := f.db.QueryContext(context.Background(), pageSQL, f.recipient, nil, nil, 11)
		require.NoError(t, err)
		readStates := map[string]bool{}
		for rows.Next() {
			var id int64
			var reason, record, actor, subject, root, rootState, rootCID, subjectState, subjectCID, recordState, recordCID sql.NullString
			var created sql.NullTime
			var sortAt time.Time
			var isRead bool
			require.NoError(t, rows.Scan(&id, &reason, &record, &actor, &subject, &root, &created, &sortAt,
				&isRead, &rootState, &rootCID, &subjectState, &subjectCID, &recordState, &recordCID))
			readStates[subject.String] = isRead
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Equal(t, map[string]bool{post: true, comment: false}, readStates, "newest visible comment is unread")
		plan, raw := bridgedCountPlan(t, f.db, pageSQL, f.recipient, nil, nil, 11)
		requireBridgedPointLookups(t, plan, raw, indexes)
	})
	t.Run("aggregate", func(t *testing.T) {
		rows, err := f.db.QueryContext(context.Background(), aggregateSQL, f.recipient, pq.Array([]string{post, comment}))
		require.NoError(t, err)
		totals := map[string]int{}
		for rows.Next() {
			var subject string
			var total int
			var voters pq.StringArray
			require.NoError(t, rows.Scan(&subject, &total, &voters))
			require.Empty(t, voters, "bridged-only subjects have no native recent voters")
			totals[subject] = total
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Equal(t, map[string]int{post: 3, comment: 5}, totals)
		plan, raw := bridgedCountPlan(t, f.db, aggregateSQL, f.recipient, pq.Array([]string{post, comment}))
		requireBridgedPointLookups(t, plan, raw, indexes)
	})
}
