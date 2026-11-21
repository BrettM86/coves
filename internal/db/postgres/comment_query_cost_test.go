//go:build integration

package postgres

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commentHydrateIndexes are the only indexes the hydrate query may read the
// comments table through: the primary key (migration 016, id BIGSERIAL
// PRIMARY KEY) and the unique uri constraint's index (016, uri TEXT UNIQUE;
// migration 049 dropped the plain duplicates idx_comments_uri and
// idx_comments_uri_lookup in its favour).
var commentHydrateIndexes = map[string]bool{
	"comments_pkey":    true,
	"comments_uri_key": true,
}

// TestCommentQueryCost_OldVersusRankAndHydrate is the cost evidence for
// replacing ListByParentWithHotRank / ListByParentsBatch with a generator rank
// query plus a hydrate-by-URI query.
//
// It seeds a little over 20,000 comments (one busy thread of 600 top-level
// comments, 25 of them with 20 replies each, plus 200 other threads of 100
// comments, half nested), ANALYZEs, and runs EXPLAIN (ANALYZE, BUFFERS) on the
// old query (the oracle), the generator's rank query and the hydrate query for
// the URIs the generator returned, for:
//
//   - a top-level hot page (limit 50) on the busy thread, and
//   - one nested batch level (25 parents, 5 replies each).
//
// The contract is the hydrate plan's shape: it reads comments only through the
// primary key or the unique uri index, and never more rows than it was asked
// for. The timings, buffers and rows are logged for docs/FEEDS.md.
func TestCommentQueryCost_OldVersusRankAndHydrate(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	ctx := env.ctx
	prefix := "did:plc:cost" + env.id + "u"
	viewer := "did:plc:cost" + env.id + "viewer"
	createTestUser(t, env.db, "cost"+env.id+"viewer.test", viewer)

	// 400 commenters, all indexed, so the users join has real work.
	_, err := env.db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url)
		SELECT $1::text || n, 'cost' || $2::text || 'u' || n || '.test', $3
		FROM generate_series(0, 399) AS n
	`, prefix, env.id, testkit.Endpoints().PDS.BaseURL)
	require.NoError(t, err, "seeding commenters")
	insertUserBlock(t, env.db, viewer, prefix+"7")

	commentURI := func(didExpr, rkeyExpr string) string {
		return `'at://' || ` + didExpr + ` || '/social.coves.community.comment/' || ` + rkeyExpr
	}

	// Busy thread: 600 top-level comments under the env root post.
	_, err = env.db.ExecContext(ctx, `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid,
			content, langs, created_at, upvote_count, downvote_count, score, reply_count)
		SELECT `+commentURI(`$1::text || (g % 400)`, `'costtop' || g`)+`, 'bafycosttop' || g, 'costtop' || g, $1::text || (g % 400),
			$2, 'bafyroot', $2, 'bafyroot',
			'top-level comment ' || g, ARRAY['en'], NOW() - g * INTERVAL '7 minutes',
			GREATEST((g * 37) % 300 - 30, 0), GREATEST(30 - (g * 37) % 300, 0), (g * 37) % 300 - 30,
			CASE WHEN g <= 25 THEN 20 ELSE 0 END
		FROM generate_series(1, 600) AS g
	`, prefix, env.root)
	require.NoError(t, err, "seeding the busy thread's top-level comments")

	// 20 replies to each of the busy thread's first 25 top-level comments.
	_, err = env.db.ExecContext(ctx, `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid,
			content, langs, created_at, upvote_count, downvote_count, score)
		SELECT `+commentURI(`$1::text || (g % 400)`, `'costrep' || g`)+`, 'bafycostrep' || g, 'costrep' || g, $1::text || (g % 400),
			$2, 'bafyroot', `+commentURI(`$1::text || (((g - 1) / 20 + 1) % 400)`, `'costtop' || ((g - 1) / 20 + 1)`)+`, 'bafyparent',
			'reply ' || g, ARRAY['en'], NOW() - g * INTERVAL '3 minutes',
			(g * 13) % 50, 0, (g * 13) % 50
		FROM generate_series(1, 500) AS g
	`, prefix, env.root)
	require.NoError(t, err, "seeding the busy thread's replies")

	// 200 other threads of 100 comments: the first 10,000 top-level, the next
	// 10,000 each replying to the comment 10,000 before it (the same thread);
	// one in fifty soft-deleted.
	_, err = env.db.ExecContext(ctx, `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid,
			content, langs, created_at, deleted_at, upvote_count, downvote_count, score)
		SELECT `+commentURI(`$1::text || (g % 400)`, `'costother' || g`)+`, 'bafycostother' || g, 'costother' || g, $1::text || (g % 400),
			$2::text || (g % 200), 'bafyroot',
			CASE WHEN g <= 10000 THEN $2::text || (g % 200)
			     ELSE `+commentURI(`$1::text || ((g - 10000) % 400)`, `'costother' || (g - 10000)`)+` END,
			'bafyparent',
			'other comment ' || g, ARRAY['en'], NOW() - (g % 5000) * INTERVAL '1 minute',
			CASE WHEN g % 50 = 0 THEN NOW() END,
			(g * 7) % 120, (g * 3) % 10, (g * 7) % 120 - (g * 3) % 10
		FROM generate_series(1, 20000) AS g
	`, prefix, "at://did:plc:costroot"+env.id+"/social.coves.community.post/r")
	require.NoError(t, err, "seeding the other threads")

	for _, table := range []string{"comments", "users", "user_blocks"} {
		_, err = env.db.ExecContext(ctx, "ANALYZE "+table)
		require.NoError(t, err, "ANALYZE %s", table)
	}
	var total int
	require.NoError(t, env.db.QueryRowContext(ctx, `SELECT count(*) FROM comments`).Scan(&total))
	require.GreaterOrEqual(t, total, 20000, "THE FIXTURE IS WRONG, not the code: the cost test needs 20,000+ comments")
	t.Logf("cost fixture: %d comments, 200 other threads, busy thread 600 top-level + 500 replies, 401 users, PostgreSQL test database (%s)",
		total, time.Now().UTC().Format("2006-01-02"))

	busyParents := make([]string, 0, 25)
	for p := 1; p <= 25; p++ {
		busyParents = append(busyParents, fmt.Sprintf("at://%s%d/social.coves.community.comment/costtop%d", prefix, p%400, p))
	}

	generator := NewCommentsHotGenerator(env.db)

	cases := []struct {
		label    string
		oracle   func() (string, []any)
		req      feedgen.CommentSkeletonRequest
		wantURIs int
	}{
		{
			label: "top-level hot page (limit 50)",
			oracle: func() (string, []any) {
				query, args, _, err := oracleListByParentQuery(env.impl, env.root, "hot", "", 50, nil, viewer,
					time.Now().UTC().Truncate(time.Microsecond))
				require.NoError(t, err)
				return query, args
			},
			req:      feedgen.CommentSkeletonRequest{ViewerDID: viewer, Parents: []string{env.root}, LimitPerParent: 50},
			wantURIs: 50,
		},
		{
			label: "nested batch level (25 parents x 5)",
			oracle: func() (string, []any) {
				return oracleListByParentsBatchQuery(env.impl, busyParents, "hot", 5, viewer)
			},
			req:      feedgen.CommentSkeletonRequest{ViewerDID: viewer, Parents: busyParents, LimitPerParent: 5},
			wantURIs: 125,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			oracleQuery, oracleArgs := tc.oracle()
			oraclePlan := explainCommentQuery(t, env, "old "+tc.label, oracleQuery, oracleArgs)
			require.NotNil(t, oraclePlan)

			rankQuery, rankArgs, err := generator.skeletonQuery(tc.req)
			require.NoError(t, err)
			require.NotEmpty(t, rankQuery, "skeletonQuery must return the rank SQL the generator runs")
			explainCommentQuery(t, env, "rank "+tc.label, rankQuery, rankArgs)

			page, err := generator.CommentSkeleton(env.ctx, tc.req)
			require.NoError(t, err)
			var uris []string
			for _, replies := range page.Replies {
				uris = append(uris, replies.URIs...)
			}
			require.Len(t, uris, tc.wantURIs, "the generator must return a full page: every parent has more visible replies than the limit")

			hydrateQuery, hydrateArgs := env.impl.hydrateQuery(uris, viewer)
			require.NotEmpty(t, hydrateQuery, "hydrateQuery must return the SQL HydrateComments runs")
			hydratePlan := explainCommentQuery(t, env, "hydrate "+tc.label, hydrateQuery, hydrateArgs)
			assertHydrateReadsCommentsByKey(t, hydratePlan, len(uris))
		})
	}
}

// explainCommentQuery runs EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) of query,
// logs one compact line for docs/FEEDS.md and returns the root plan node.
func explainCommentQuery(t *testing.T, env *commentEnv, label, query string, args []any) map[string]any {
	t.Helper()
	var raw []byte
	err := env.db.QueryRowContext(env.ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&raw)
	require.NoError(t, err, "EXPLAIN of the %s query must execute", label)

	var explained []map[string]any
	require.NoError(t, json.Unmarshal(raw, &explained))
	require.Len(t, explained, 1)
	plan, ok := explained[0]["Plan"].(map[string]any)
	require.True(t, ok, "EXPLAIN JSON has no Plan: %s", raw)

	t.Logf("cost %-45s exec=%.3fms planning=%.3fms sharedHit=%v sharedRead=%v rows=%v",
		label, explained[0]["Execution Time"], explained[0]["Planning Time"],
		plan["Shared Hit Blocks"], plan["Shared Read Blocks"], plan["Actual Rows"])
	return plan
}

// assertHydrateReadsCommentsByKey checks every plan node that reads the
// comments table: an index scan on the primary key or the unique uri index (or
// a bitmap heap scan fed only by those), returning at most requested rows.
func assertHydrateReadsCommentsByKey(t *testing.T, root map[string]any, requested int) {
	t.Helper()
	found := 0
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == "comments" {
			found++
			nodeType, _ := node["Node Type"].(string)
			switch nodeType {
			case "Index Scan", "Index Only Scan":
				indexName, _ := node["Index Name"].(string)
				assert.Truef(t, commentHydrateIndexes[indexName],
					"hydrate reads comments through %s %q; only comments_pkey or comments_uri_key are allowed", nodeType, indexName)
			case "Bitmap Heap Scan":
				for _, indexName := range bitmapIndexNames(node) {
					assert.Truef(t, commentHydrateIndexes[indexName],
						"hydrate's bitmap scan of comments uses index %q; only comments_pkey or comments_uri_key are allowed", indexName)
				}
			default:
				assert.Failf(t, "hydrate must read comments by key",
					"plan node %q reads comments; only index scans on comments_pkey or comments_uri_key are allowed", nodeType)
			}
			rows, _ := node["Actual Rows"].(float64)
			loops, _ := node["Actual Loops"].(float64)
			assert.LessOrEqualf(t, rows*loops, float64(requested),
				"hydrate's %s of comments returned %.0f rows for %d requested URIs", nodeType, rows*loops, requested)
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if childNode, ok := child.(map[string]any); ok {
				walk(childNode)
			}
		}
	}
	walk(root)
	assert.Positive(t, found, "the hydrate plan must read the comments table")
}

// bitmapIndexNames returns the indexes of every Bitmap Index Scan under node.
func bitmapIndexNames(node map[string]any) []string {
	var names []string
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		childNode, ok := child.(map[string]any)
		if !ok {
			continue
		}
		if childNode["Node Type"] == "Bitmap Index Scan" {
			if name, ok := childNode["Index Name"].(string); ok {
				names = append(names, name)
			}
		}
		names = append(names, bitmapIndexNames(childNode)...)
	}
	return names
}
