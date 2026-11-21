//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postHydrateIndexes are the only indexes the hydrate query may read the posts
// table through: the primary key (id), the unique uri constraint's index and
// the plain uri index.
var postHydrateIndexes = map[string]bool{
	"posts_pkey":    true,
	"posts_uri_key": true,
	"idx_posts_uri": true,
}

// TestFeedQueryCost_OldVersusRankAndHydrate is the cost evidence for replacing
// GetTimeline, GetCommunityFeed and GetDiscover New/Top with a generator rank
// query plus a hydrate-by-URI query.
//
// It seeds 24,000 posts across 12 communities (one in 97 soft-deleted, half
// with an accepted admission row, the rest legacy community-repo posts), 300
// authors, a viewer subscribed to five communities at levels 1-5 and three
// more at level 3 (one of them blocked), 600 other subscriptions, ANALYZEs,
// and for six reads at the default limit 15 — timeline hot, timeline new,
// community hot, community top (day), discover new, discover top (day) — runs
// EXPLAIN (ANALYZE, BUFFERS) on the old query (the oracle), the generator's
// rank query, and the hydrate query for the URIs the generator returned.
//
// The contract is the hydrate plan's shape: it reads posts only through the
// primary key or a uri index (a bitmap AND with another index is allowed),
// and never more rows than it was asked for. The
// timings, buffers and rows are logged for docs/FEEDS.md.
func TestFeedQueryCost_OldVersusRankAndHydrate(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := t.Context()
	const (
		viewer          = "did:plc:fcostviewer"
		authorPrefix    = "did:plc:fcostu"
		communityPrefix = "did:plc:fcostc"
		limit           = 15
	)
	createTestUser(t, db, "fcostviewer.test", viewer)

	_, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url)
		SELECT $1::text || n, 'fcostu' || n || '.test', $2
		FROM generate_series(0, 299) AS n
	`, authorPrefix, testkit.Endpoints().PDS.BaseURL)
	require.NoError(t, err, "seeding authors")

	_, err = db.ExecContext(ctx, `
		INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
		SELECT $1::text || n, 'c-fcostc' || n || '.coves.social', 'Cost community ' || n, $2, $2, $2, NOW()
		FROM generate_series(1, 12) AS n
	`, communityPrefix, authorPrefix+"0")
	require.NoError(t, err, "seeding communities")

	// The viewer: communities 1-5 at levels 1-5, 6-8 at level 3; 9-12 only on
	// Discover. Community 8 is blocked, as is author 7.
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at, content_visibility)
		SELECT $1, $2::text || n, NOW(), CASE WHEN n <= 5 THEN n ELSE 3 END
		FROM generate_series(1, 8) AS n
	`, viewer, communityPrefix)
	require.NoError(t, err, "seeding the viewer's subscriptions")
	insertCommunityBlock(t, db, viewer, communityPrefix+"8")
	insertUserBlock(t, db, viewer, authorPrefix+"7")

	// Every author subscribes to two communities at level 3.
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at, content_visibility)
		SELECT $1::text || n, $2::text || (n % 12 + 1), NOW(), 3 FROM generate_series(0, 299) AS n
		UNION ALL
		SELECT $1::text || n, $2::text || ((n + 5) % 12 + 1), NOW(), 3 FROM generate_series(0, 299) AS n
	`, authorPrefix, communityPrefix)
	require.NoError(t, err, "seeding other subscriptions")

	// 24,000 posts, two minutes apart over ~28 days (pairs 20,000 apart share
	// a created_at), scores -50..449.
	_, err = db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, deleted_at,
			upvote_count, downvote_count, score)
		SELECT 'at://' || $2::text || (g % 12 + 1) || '/social.coves.community.post/fcost' || g,
			'bafyfcost' || g, 'fcost' || g, $1::text || (g % 300), $2::text || (g % 12 + 1),
			'cost post ' || g, NOW() - (g % 20000) * INTERVAL '2 minutes',
			CASE WHEN g % 97 = 0 THEN NOW() END,
			GREATEST((g * 37) % 500 - 50, 0), GREATEST(50 - (g * 37) % 500, 0), (g * 37) % 500 - 50
		FROM generate_series(1, 24000) AS g
	`, authorPrefix, communityPrefix)
	require.NoError(t, err, "seeding posts")

	_, err = db.ExecContext(ctx, `
		INSERT INTO community_post_admissions (community_did, post_uri, status, accepted_cid, evaluated_cid, created_at, updated_at)
		SELECT p.community_did, p.uri, 'accepted', p.cid, p.cid, NOW(), NOW()
		FROM posts p WHERE p.rkey LIKE 'fcost%' AND substring(p.rkey FROM 6)::int % 2 = 0
	`)
	require.NoError(t, err, "accepting half the posts")

	for _, table := range []string{
		"posts", "users", "communities", "community_subscriptions", "community_post_admissions",
		"user_blocks", "community_blocks", "moderation_decisions",
	} {
		_, err = db.ExecContext(ctx, "ANALYZE "+table)
		require.NoError(t, err, "ANALYZE %s", table)
	}

	var postCount, admissionCount, subscriptionCount, communityCount, userCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM posts`).Scan(&postCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM community_post_admissions`).Scan(&admissionCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM community_subscriptions`).Scan(&subscriptionCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM communities`).Scan(&communityCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&userCount))
	require.GreaterOrEqual(t, postCount, 20000, "THE FIXTURE IS WRONG, not the code: the cost test needs 20,000+ posts")
	var viewerLevels []int
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT content_visibility FROM community_subscriptions WHERE user_did = $1 ORDER BY 1`, viewer)
	require.NoError(t, err)
	for rows.Next() {
		var level int
		require.NoError(t, rows.Scan(&level))
		viewerLevels = append(viewerLevels, level)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []int{1, 2, 3, 4, 5}, viewerLevels, "THE FIXTURE IS WRONG, not the code: the viewer needs subscriptions at levels 1-5")
	t.Logf("cost fixture: date=%s posts=%d admissions=%d communities=%d users=%d subscriptions=%d (viewer: 8 at levels 1-5,3,3,3; 1 community block, 1 user block), PostgreSQL test database",
		time.Now().UTC().Format("2006-01-02"), postCount, admissionCount, communityCount, userCount, subscriptionCount)

	restriction := WithAccountRestrictionAuthority(fixtures.InstanceDID())
	oracle := newFeedOracle(db, getFeedEquivalenceCursorSecret, restriction)
	hot := NewHotGenerator(db, restriction)
	newGenerator := NewNewGenerator(db, restriction)
	top := NewTopGenerator(db, restriction)
	hydrator := NewPostHydrator(db, restriction)

	home := feedgen.Scope{Name: feedgen.ScopeHome}
	community := feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: communityPrefix + "3"}
	all := feedgen.Scope{Name: feedgen.ScopeAll}

	cases := []struct {
		label     string
		surface   oracleFeedSurface
		oracleReq oracleFeedRequest
		rankQuery func(feedgen.SkeletonRequest) (string, []any, error)
		skeleton  func(context.Context, feedgen.SkeletonRequest) (feedgen.SkeletonPage, error)
		req       feedgen.SkeletonRequest
	}{
		{
			label: "timeline-hot", surface: oracleTimeline,
			oracleReq: oracleFeedRequest{ViewerDID: viewer, Sort: "hot", Limit: limit},
			rankQuery: hot.homeQuery, skeleton: hot.Skeleton,
			req: feedgen.SkeletonRequest{ViewerDID: viewer, Scope: home, Limit: limit},
		},
		{
			label: "timeline-new", surface: oracleTimeline,
			oracleReq: oracleFeedRequest{ViewerDID: viewer, Sort: "new", Limit: limit},
			rankQuery: newGenerator.homeQuery, skeleton: newGenerator.Skeleton,
			req: feedgen.SkeletonRequest{ViewerDID: viewer, Scope: home, Limit: limit},
		},
		{
			label: "community-hot", surface: oracleCommunity,
			oracleReq: oracleFeedRequest{ViewerDID: viewer, Community: community.CommunityDID, Sort: "hot", Limit: limit},
			rankQuery: hot.communityQuery, skeleton: hot.Skeleton,
			req: feedgen.SkeletonRequest{ViewerDID: viewer, Scope: community, Limit: limit},
		},
		{
			label: "community-top", surface: oracleCommunity,
			oracleReq: oracleFeedRequest{ViewerDID: viewer, Community: community.CommunityDID, Sort: "top", Timeframe: "day", Limit: limit},
			rankQuery: top.communityQuery, skeleton: top.Skeleton,
			req: feedgen.SkeletonRequest{ViewerDID: viewer, Scope: community, Limit: limit, Timeframe: "day"},
		},
		{
			label: "discover-new", surface: oracleDiscover,
			oracleReq: oracleFeedRequest{ViewerDID: viewer, Sort: "new", Limit: limit},
			rankQuery: newGenerator.allQuery, skeleton: newGenerator.Skeleton,
			req: feedgen.SkeletonRequest{ViewerDID: viewer, Scope: all, Limit: limit},
		},
		{
			label: "discover-top", surface: oracleDiscover,
			oracleReq: oracleFeedRequest{ViewerDID: viewer, Sort: "top", Timeframe: "day", Limit: limit},
			rankQuery: top.allQuery, skeleton: top.Skeleton,
			req: feedgen.SkeletonRequest{ViewerDID: viewer, Scope: all, Limit: limit, Timeframe: "day"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			oracleQuery, oracleArgs, err := oracleFeedQuery(oracle, tc.surface, tc.oracleReq)
			require.NoError(t, err)
			explainFeedQuery(t, db, tc.label+" old", oracleQuery, oracleArgs)

			rankQuery, rankArgs, err := tc.rankQuery(tc.req)
			require.NoError(t, err)
			require.NotEmpty(t, rankQuery, "the generator must return the rank SQL its Skeleton runs")
			explainFeedQuery(t, db, tc.label+" rank", rankQuery, rankArgs)

			page, err := tc.skeleton(ctx, tc.req)
			require.NoError(t, err)
			uris := make([]string, 0, len(page.Items))
			for _, item := range page.Items {
				uris = append(uris, item.URI)
			}
			require.Len(t, uris, limit, "THE FIXTURE IS WRONG, not the code: the generator must return a full page")

			hydrateQuery, hydrateArgs, err := hydrator.hydrateQuery(feedgen.HydrateRequest{
				ViewerDID: viewer, Scope: tc.req.Scope, URIs: uris,
			})
			require.NoError(t, err)
			require.NotEmpty(t, hydrateQuery, "hydrateQuery must return the SQL HydratePosts runs")
			hydratePlan := explainFeedQuery(t, db, tc.label+" hydrate", hydrateQuery, hydrateArgs)
			assertHydrateReadsPostsByKey(t, hydratePlan, len(uris))
		})
	}
}

// explainFeedQuery runs EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) of query,
// logs one compact line for docs/FEEDS.md and returns the root plan node.
func explainFeedQuery(t *testing.T, db *sql.DB, label, query string, args []any) map[string]any {
	t.Helper()
	var raw []byte
	err := db.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&raw)
	require.NoError(t, err, "EXPLAIN of the %s query must execute", label)

	var explained []map[string]any
	require.NoError(t, json.Unmarshal(raw, &explained))
	require.Len(t, explained, 1)
	plan, ok := explained[0]["Plan"].(map[string]any)
	require.True(t, ok, "EXPLAIN JSON has no Plan: %s", raw)

	t.Logf("cost %-22s exec=%.3fms planning=%.3fms sharedHit=%v sharedRead=%v rows=%v",
		label, explained[0]["Execution Time"], explained[0]["Planning Time"],
		plan["Shared Hit Blocks"], plan["Shared Read Blocks"], plan["Actual Rows"])
	return plan
}

// assertHydrateReadsPostsByKey checks every plan node that reads the posts
// table: an index scan on the primary key or a uri index, or a bitmap heap
// scan driven by one of those (other indexes only ANDed with it, no
// BitmapOr), returning at most requested rows; and that at
// least one such node exists.
func assertHydrateReadsPostsByKey(t *testing.T, root map[string]any, requested int) {
	t.Helper()
	found := 0
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == "posts" {
			found++
			nodeType, _ := node["Node Type"].(string)
			switch nodeType {
			case "Index Scan", "Index Only Scan":
				indexName, _ := node["Index Name"].(string)
				assert.Truef(t, postHydrateIndexes[indexName],
					"hydrate reads posts through %s %q; only posts_pkey, posts_uri_key or idx_posts_uri are allowed", nodeType, indexName)
			case "Bitmap Heap Scan":
				// A key index must drive the bitmap. Another index may only
				// narrow it (BitmapAnd with the key index), never widen it
				// (BitmapOr).
				indexNames := bitmapIndexNames(node)
				hasKeyIndex := false
				for _, indexName := range indexNames {
					hasKeyIndex = hasKeyIndex || postHydrateIndexes[indexName]
				}
				assert.Truef(t, hasKeyIndex,
					"hydrate's bitmap scan of posts uses indexes %q; it must include posts_pkey, posts_uri_key or idx_posts_uri", indexNames)
				assert.Falsef(t, hasBitmapOr(node),
					"hydrate's bitmap scan of posts ORs indexes %q; another index may only be ANDed with a key index", indexNames)
			default:
				assert.Failf(t, "hydrate must read posts by key",
					"plan node %q reads posts; only index scans on posts_pkey, posts_uri_key or idx_posts_uri are allowed", nodeType)
			}
			rows, _ := node["Actual Rows"].(float64)
			loops, _ := node["Actual Loops"].(float64)
			assert.LessOrEqualf(t, rows*loops, float64(requested),
				"hydrate's %s of posts returned %.0f rows for %d requested URIs", nodeType, rows*loops, requested)
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if childNode, ok := child.(map[string]any); ok {
				walk(childNode)
			}
		}
	}
	walk(root)
	assert.Positive(t, found, "the hydrate plan must read the posts table")
}

// hasBitmapOr reports whether a BitmapOr node is under node.
func hasBitmapOr(node map[string]any) bool {
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		childNode, ok := child.(map[string]any)
		if !ok {
			continue
		}
		if childNode["Node Type"] == "BitmapOr" || hasBitmapOr(childNode) {
			return true
		}
	}
	return false
}
