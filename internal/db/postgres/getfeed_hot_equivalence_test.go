//go:build integration

package postgres

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getFeed with the built-in hot generator must serve exactly what the old hot
// feeds served: getTimeline sort=hot for scope home and getCommunity sort=hot
// for scope community. The old reads are the `_test.go` oracle
// (feed_oracle_test.go, today's SQL and cursors verbatim); the old endpoints
// are now aliases of getFeed served by RegisterFeedRoutes. One database, one
// router, the same account-restriction authority and cursor secret on every
// path; each path (oracle, getFeed service, getFeed, alias) is paged with its
// own cursors.
//
// Hot ranks quoted in comments are (sign(s)·ln(|s|+1)+1)/(ageHours+2)^0.8,
// weighted ×1.5 at level 4 and ×2.0 at level 5 for a non-negative rank (÷ for
// a negative one). Every expected order is written out by hand.

const getFeedEquivalenceCursorSecret = "getfeed-equivalence-cursor-secret"

// getFeedEquivalenceImageCID is the blob CID of the fixture's image embed.
const getFeedEquivalenceImageCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"

const (
	getTimelinePath  = "/xrpc/social.coves.feed.getTimeline"
	getCommunityPath = "/xrpc/social.coves.communityFeed.getCommunity"
	getFeedPath      = "/xrpc/social.coves.feed.getFeed"
	getDiscoverPath  = "/xrpc/social.coves.feed.getDiscover"
)

// getFeedHarness is one router over one database with getFeed and its three
// aliases (getTimeline, getCommunity, getDiscover) registered by
// RegisterFeedRoutes, plus the old reads' oracle and the getFeed service
// itself.
type getFeedHarness struct {
	router   http.Handler
	registry *feedgen.Registry
	tokens   map[string]string
	// feedService is the getFeed service the router serves, for its
	// un-enriched items.
	feedService feedgen.Service
	// oracle reads the old feeds (feed_oracle_test.go) with the same cursor
	// secret, restriction authority and hot candidate bound.
	oracle *feedRepoBase
	// newGenerator and topGenerator are the registry's new and top generators,
	// for reading their skeletons directly.
	newGenerator, topGenerator *SortGenerator
	// discoverRepo backs the registry's discover-hot generator; tests may
	// lower its tunables before the first request.
	discoverRepo *postgresDiscoverRepo
}

// newGetFeedHarness wires the real services. candidateLimit > 0 lowers the hot
// candidate bound identically on the oracle and every generator (hot, new,
// top). Each viewer gets a bearer token.
func newGetFeedHarness(t *testing.T, db *sql.DB, candidateLimit int, viewers ...string) *getFeedHarness {
	t.Helper()

	instanceDID := fixtures.InstanceDID()
	restriction := WithAccountRestrictionAuthority(instanceDID)

	oracle := newFeedOracle(db, getFeedEquivalenceCursorSecret, restriction)
	feeds := newBuiltinFeeds(db, getFeedEquivalenceCursorSecret, restriction)
	if candidateLimit > 0 {
		oracle.hotCandidateLimit = candidateLimit
		feeds.setHotCandidateLimit(candidateLimit)
	}

	registry, feedService := feeds.registry, feeds.service

	auth := fixtures.NewOAuthMiddleware()
	tokens := make(map[string]string, len(viewers))
	for _, viewer := range viewers {
		tokens[viewer] = auth.AddUser(viewer)
	}

	router := newFeedRouter(db, feeds, newFeedVoteService(db), auth, restriction)

	return &getFeedHarness{
		router: router, registry: registry, tokens: tokens,
		feedService: feedService, oracle: oracle,
		newGenerator: feeds.newGenerator, topGenerator: feeds.topGenerator,
		discoverRepo: feeds.discoverRepo,
	}
}

// hotFeedURI is the registry's at-uri for the built-in hot feed.
func (h *getFeedHarness) hotFeedURI() string { return h.registry.URI("hot") }

// feedPage is one served page: the raw feed array and whether a cursor came
// with it.
type feedPage struct {
	Feed      json.RawMessage
	HasCursor bool
}

// getFeedPage issues one GET as viewer ("" = anonymous) and decodes the page.
// It returns the response's cursor.
func (h *getFeedHarness) getFeedPage(t *testing.T, path string, params url.Values, viewer string) (feedPage, string) {
	t.Helper()

	return decodeFeedPage(t, h.serve(h.newRequest(t, path, params, viewer)), path+"?"+params.Encode())
}

// newRequest builds one GET as viewer ("" = anonymous).
func (h *getFeedHarness) newRequest(t *testing.T, path string, params url.Values, viewer string) *http.Request {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path+"?"+params.Encode(), nil)
	if viewer != "" {
		token, ok := h.tokens[viewer]
		require.Truef(t, ok, "viewer %s has no token in the harness", viewer)
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

// serve runs request through the router. It makes no assertions, so it may
// run off the test goroutine.
func (h *getFeedHarness) serve(request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	return recorder
}

// decodeFeedPage requires a 200 page and decodes it, returning its cursor.
func decodeFeedPage(t *testing.T, recorder *httptest.ResponseRecorder, label string) (feedPage, string) {
	t.Helper()

	require.Equalf(t, http.StatusOK, recorder.Code, "GET %s: %s", label, recorder.Body.String())

	var body struct {
		Feed   json.RawMessage `json:"feed"`
		Cursor string          `json:"cursor"`
	}
	require.NoErrorf(t, json.Unmarshal(recorder.Body.Bytes(), &body), "GET %s: decoding %q", label, recorder.Body.String())
	require.NotNilf(t, body.Feed, "GET %s: no feed array", label)
	return feedPage{Feed: body.Feed, HasCursor: body.Cursor != ""}, body.Cursor
}

// readFeedPages pages an endpoint at limit 2 with its own cursors until the
// cursor runs out.
func (h *getFeedHarness) readFeedPages(t *testing.T, path string, params url.Values, viewer string) []feedPage {
	t.Helper()

	const maxPages = 20
	var pages []feedPage
	cursor := ""
	for range maxPages {
		query := url.Values{}
		for key, values := range params {
			query[key] = append([]string(nil), values...)
		}
		query.Set("limit", "2")
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		page, next := h.getFeedPage(t, path, query, viewer)
		pages = append(pages, page)
		if next == "" {
			return pages
		}
		cursor = next
	}
	require.FailNowf(t, "feed did not end", "%s still had a cursor after %d pages", path, maxPages)
	return nil
}

// feedPageURIs is the post URIs of one raw feed array, in order.
func feedPageURIs(t *testing.T, feed json.RawMessage) []string {
	t.Helper()

	var items []struct {
		Post struct {
			URI string `json:"uri"`
		} `json:"post"`
	}
	require.NoError(t, json.Unmarshal(feed, &items))
	uris := make([]string, 0, len(items))
	for _, item := range items {
		uris = append(uris, item.Post.URI)
	}
	return uris
}

// pagedURIs concatenates the URIs of every page.
func pagedURIs(t *testing.T, pages []feedPage) []string {
	t.Helper()

	var uris []string
	for _, page := range pages {
		uris = append(uris, feedPageURIs(t, page.Feed)...)
	}
	return uris
}

// pageCursorPresence is each page's cursor presence, in order.
func pageCursorPresence(pages []feedPage) []bool {
	presence := make([]bool, 0, len(pages))
	for _, page := range pages {
		presence = append(presence, page.HasCursor)
	}
	return presence
}

// assertSameFeedPages compares two paged reads page by page: the same number
// of pages, the same feed item JSON on each page and the same cursor presence.
func assertSameFeedPages(t *testing.T, want, got []feedPage, label string) {
	t.Helper()

	require.Equalf(t, pagedURIs(t, want), pagedURIs(t, got), "%s: URI order", label)
	require.Equalf(t, len(want), len(got), "%s: page count", label)
	for i := range want {
		assert.JSONEqf(t, string(want[i].Feed), string(got[i].Feed), "%s: page %d feed items", label, i+1)
		assert.Equalf(t, want[i].HasCursor, got[i].HasCursor, "%s: page %d cursor presence", label, i+1)
	}
}

// readServicePages pages the getFeed service at limit 2 with its own cursors
// until the cursor runs out.
func (h *getFeedHarness) readServicePages(t *testing.T, req feedgen.GetFeedRequest) []*feedgen.GetFeedResponse {
	t.Helper()

	const maxPages = 20
	var pages []*feedgen.GetFeedResponse
	req.Limit = 2
	req.Cursor = ""
	for range maxPages {
		page, err := h.feedService.GetFeed(t.Context(), req)
		require.NoErrorf(t, err, "getFeed service page %d", len(pages)+1)
		pages = append(pages, page)
		if page.Cursor == "" {
			return pages
		}
		req.Cursor = page.Cursor
	}
	require.FailNowf(t, "getFeed service did not end", "still had a cursor after %d pages", maxPages)
	return nil
}

// servicePagePosts is one getFeed service page's post views, un-enriched.
func servicePagePosts(page *feedgen.GetFeedResponse) []*posts.PostView {
	views := make([]*posts.PostView, 0, len(page.Feed))
	for _, item := range page.Feed {
		views = append(views, item.Post)
	}
	return views
}

// feedPagesURIs is each served page's post URIs.
func feedPagesURIs(t *testing.T, pages []feedPage) [][]string {
	t.Helper()

	uris := make([][]string, 0, len(pages))
	for _, page := range pages {
		uris = append(uris, feedPageURIs(t, page.Feed))
	}
	return uris
}

// oraclePagesURIs is each oracle page's post URIs.
func oraclePagesURIs(pages []oracleFeedPageResult) [][]string {
	uris := make([][]string, 0, len(pages))
	for _, page := range pages {
		uris = append(uris, page.URIs())
	}
	return uris
}

// oracleCursorPresence is each oracle page's cursor presence, in order.
func oracleCursorPresence(pages []oracleFeedPageResult) []bool {
	presence := make([]bool, 0, len(pages))
	for _, page := range pages {
		presence = append(presence, page.Cursor != "")
	}
	return presence
}

// requireSamePostViews requires two pages of un-enriched post views to
// marshal to the same JSON.
func requireSamePostViews(t *testing.T, want, got []*posts.PostView, label string) {
	t.Helper()

	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	require.JSONEqf(t, string(wantJSON), string(gotJSON), "%s: un-enriched post views", label)
}

// assertOracleMatchesFeed compares the old read (the oracle) with getFeed:
// the same URIs on each page, the same page count and cursor presence, and
// on each page the oracle's post views equal the getFeed service's items
// before the handler enriches them. Each path was paged with its own cursors.
func assertOracleMatchesFeed(t *testing.T, oracle []oracleFeedPageResult, feed []feedPage, service []*feedgen.GetFeedResponse, label string) {
	t.Helper()

	require.Equalf(t, oraclePagesURIs(oracle), feedPagesURIs(t, feed), "%s: getFeed URIs per page", label)
	require.Equalf(t, oracleCursorPresence(oracle), pageCursorPresence(feed), "%s: getFeed cursor presence", label)
	require.Lenf(t, service, len(oracle), "%s: getFeed service page count", label)
	for i := range oracle {
		requireSamePostViews(t, oracle[i].Posts, servicePagePosts(service[i]), fmt.Sprintf("%s: page %d", label, i+1))
		assert.Equalf(t, oracle[i].Cursor != "", service[i].Cursor != "", "%s: service page %d cursor presence", label, i+1)
	}
}

// assertAliasMatchesFeed compares an alias's pages with getFeed's: the alias
// serves wantURIs on each page (the oracle's, where there is one) and the
// same enriched feed items as getFeed, page for page.
func assertAliasMatchesFeed(t *testing.T, wantURIs [][]string, feed, alias []feedPage, label string) {
	t.Helper()

	require.Equalf(t, wantURIs, feedPagesURIs(t, alias), "%s: URIs per page", label)
	assertSameFeedPages(t, feed, alias, label)
}

// acceptHotFixturePost records the community's acceptance of a seeded post at
// its indexed CID, as the admission pipeline would.
func acceptHotFixturePost(t *testing.T, db *sql.DB, communityDID, uri string) {
	t.Helper()

	_, err := db.ExecContext(t.Context(), `
		INSERT INTO community_post_admissions (community_did, post_uri, status, accepted_cid, evaluated_cid, created_at, updated_at)
		SELECT p.community_did, p.uri, 'accepted', p.cid, p.cid, NOW(), NOW()
		FROM posts p WHERE p.uri = $2 AND p.community_did = $1
	`, communityDID, uri)
	require.NoErrorf(t, err, "accepting post %s", uri)
}

// seedAcceptedHotPost seeds a scored post and accepts it.
func seedAcceptedHotPost(t *testing.T, db *sql.DB, communityDID, authorDID, rkey string, createdAt time.Time, score int) string {
	t.Helper()

	uri := seedHotBoundPost(t, db, communityDID, authorDID, rkey, createdAt, score)
	acceptHotFixturePost(t, db, communityDID, uri)
	return uri
}

// insertViewerUpvote indexes viewerDID's upvote on subjectURI.
func insertViewerUpvote(t *testing.T, db *sql.DB, viewerDID, subjectURI, rkey string) string {
	t.Helper()

	voteURI := "at://" + viewerDID + "/social.coves.feed.vote/" + rkey
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at)
		VALUES ($1, $2, $3, $4, $5, 'bafysubject', 'up', NOW())
	`, voteURI, "bafyvote"+rkey, rkey, viewerDID, subjectURI)
	require.NoError(t, err, "indexing the viewer's upvote")
	return voteURI
}

// homeHotFixture is the B10 acceptance fixture: a signed-in viewer subscribed
// at levels 1-5 to five communities plus a sixth, subscribed community they
// block, with every kind of post the home hot feed must and must not serve.
type homeHotFixture struct {
	viewer string
	// visible posts by name.
	d1, v1, f1, d2, d4, a50, b40, d3, f2 string
	// voteURI is the viewer's upvote on d4.
	voteURI string
}

func seedHomeHotFixture(t *testing.T, db *sql.DB) homeHotFixture {
	t.Helper()

	const (
		viewer     = "did:plc:gfeqviewer"
		authorA    = "did:plc:gfeqauthora"
		authorB    = "did:plc:gfeqauthorb"
		restricted = "did:plc:gfeqrestricted"
		blocked    = "did:plc:gfeqblocked"

		levelOne         = "did:plc:gfeqlevelone"
		levelTwo         = "did:plc:gfeqleveltwo"
		levelThree       = "did:plc:gfeqlevelthree"
		levelFour        = "did:plc:gfeqlevelfour"
		levelFive        = "did:plc:gfeqlevelfive"
		blockedCommunity = "did:plc:gfeqblockedcommunity"
	)
	for _, did := range []string{viewer, authorA, authorB, restricted, blocked} {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	for level, did := range map[int]string{1: levelOne, 2: levelTwo, 3: levelThree, 4: levelFour, 5: levelFive} {
		createTestCommunity(t, db, did, "c-"+did[len("did:plc:"):]+".coves.social", authorA)
		subscribeAtLevel(t, db, viewer, did, level)
	}
	createTestCommunity(t, db, blockedCommunity, "c-gfeqblockedcommunity.coves.social", authorA)
	subscribeAtLevel(t, db, viewer, blockedCommunity, 3)
	insertCommunityBlock(t, db, viewer, blockedCommunity)
	insertUserBlock(t, db, viewer, blocked)
	accountstatusfixture.Set(t, db, restricted, accountstatus.Takendown)
	// The image embed on d3 is a legacy community-repo post, so its blobs
	// resolve against the community's PDS.
	_, err := db.ExecContext(t.Context(), `UPDATE communities SET pds_url = 'https://pds.gfeq.test' WHERE did = $1`, levelThree)
	require.NoError(t, err)

	base := time.Now().UTC().Truncate(time.Microsecond)
	at := func(age time.Duration) time.Time { return base.Add(-age) }

	f := homeHotFixture{viewer: viewer}
	// Visible, in served order with weighted ranks:
	f.d1 = seedAcceptedHotPost(t, db, levelThree, authorA, "d1", at(hours(2)), 12) // 1.176
	f.v1 = seedAcceptedHotPost(t, db, levelFive, authorA, "v1", at(hours(6)), 3)   // 0.452×2 = 0.904
	f.f1 = seedAcceptedHotPost(t, db, levelFour, authorB, "f1", at(hours(10)), 20) // 0.554×1.5 = 0.831
	// The viewer's own pending post is visible to the viewer.
	f.d2 = seedHotBoundPost(t, db, levelThree, viewer, "d2", at(hours(3)), 5) // 0.770
	seedHotBoundPendingAdmission(t, db, levelThree, f.d2)
	f.d4 = seedAcceptedHotPost(t, db, levelThree, authorA, "d4", at(hours(5)), 8)   // 0.674
	f.a50 = seedAcceptedHotPost(t, db, levelOne, authorA, "a50", at(hours(20)), 50) // 0.416, level-1 threshold 50
	f.b40 = seedAcceptedHotPost(t, db, levelTwo, authorB, "b40", at(hours(30)), 40) // 0.295, level-2 threshold 40
	f.d3 = seedAcceptedHotPost(t, db, levelThree, authorA, "d3", at(hours(30)), 0)  // 0.0625
	f.f2 = seedAcceptedHotPost(t, db, levelFour, authorA, "f2", at(hours(7)), -4)   // -0.105÷1.5 = -0.070

	// Below their community's threshold (each community's two visible window
	// posts give ceil(2×0.10) = ceil(2×0.25) = 1: the line is the top score).
	// Unfiltered they would rank 0.703 and 0.871.
	seedAcceptedHotPost(t, db, levelOne, authorA, "a1", at(hours(1)), 1)
	seedAcceptedHotPost(t, db, levelTwo, authorB, "b2", at(hours(1)), 2)
	// Hidden posts inside levelOne's window, scored above a50. The threshold
	// counts only posts the viewer can see; if either counted, levelOne's line
	// would be 1000 and a50 would drop.
	seedAcceptedHotPost(t, db, levelOne, blocked, "blockedwindow", at(hours(2)), 1000)
	pendingWindow := seedHotBoundPost(t, db, levelOne, authorB, "pendingwindow", at(hours(2)), 1000)
	seedHotBoundPendingAdmission(t, db, levelOne, pendingWindow)

	// Never served; each would top the feed (rank ~2.7) if it leaked.
	pending := seedHotBoundPost(t, db, levelThree, authorB, "pending", at(hours(1)), 1000)
	seedHotBoundPendingAdmission(t, db, levelThree, pending)
	removed := seedAcceptedHotPost(t, db, levelThree, authorA, "removed", at(hours(1)), 1000)
	insertDiscoverHotRemoval(t, db, removed)
	deleted := seedAcceptedHotPost(t, db, levelThree, authorA, "deleted", at(hours(1)), 1000)
	_, err = db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, deleted)
	require.NoError(t, err)
	seedAcceptedHotPost(t, db, levelThree, restricted, "restricted", at(hours(1)), 1000)
	seedAcceptedHotPost(t, db, levelThree, blocked, "blockedauthor", at(hours(1)), 1000)
	seedAcceptedHotPost(t, db, blockedCommunity, authorA, "blockedcommunity", at(hours(1)), 1000)

	_, err = db.ExecContext(t.Context(), `UPDATE posts SET embed = $2::jsonb WHERE uri = $1`, f.d3,
		`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"`+getFeedEquivalenceImageCID+`"},"mimeType":"image/png","size":10},"alt":"d3 image"}]}`)
	require.NoError(t, err)
	f.voteURI = insertViewerUpvote(t, db, viewer, f.d4, "gfeqd4")
	return f
}

// B10 (acceptance): getFeed scope=home with the registry's hot uri, and with
// feed omitted, serves the old getTimeline sort=hot page for page, and so does
// the getTimeline alias.
func TestGetFeedHot_HomeMatchesTimelineHot(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := seedHomeHotFixture(t, db)
	h := newGetFeedHarness(t, db, 0, f.viewer)

	want := []string{f.d1, f.v1, f.f1, f.d2, f.d4, f.a50, f.b40, f.d3, f.f2}

	reads := assertGetFeedMatchesOld(t, h, equivalenceRow{
		name: "getTimeline hot", surface: oracleTimeline, sort: "hot", viewer: f.viewer, want: want,
	})
	require.Equal(t, []bool{true, true, true, true, false}, oracleCursorPresence(reads.oracle),
		"getTimeline hot: four full pages and a partial final page without a cursor")
	// The enrichment the alias comparison must cover is really present.
	require.Len(t, reads.feed, 5, "getFeed home feed=hot: page count")
	require.Contains(t, string(reads.feed[2].Feed), f.voteURI, "getFeed home feed=hot: viewer vote on d4")
	require.Contains(t, string(reads.feed[3].Feed), "pds.gfeq.test", "getFeed home feed=hot: d3 image view")

	t.Run("feed omitted", func(t *testing.T) {
		pages := h.readFeedPages(t, getFeedPath, url.Values{"scope": {"home"}}, f.viewer)
		assertSameFeedPages(t, reads.feed, pages, "getFeed home feed omitted")
		assert.Equal(t, want, pagedURIs(t, pages))
	})
}

// B11: with the candidate bound lowered on the oracle and the generators,
// getFeed and the aliases end where the old feeds end.
func TestGetFeedHot_CandidateBoundMatchesOldFeeds(t *testing.T) {
	t.Parallel()

	const candidateLimit = 4

	t.Run("community", func(t *testing.T) {
		t.Parallel()

		db := testkit.DB(t)
		const (
			viewer    = "did:plc:gfeqbviewer"
			author    = "did:plc:gfeqbauthor"
			blocked   = "did:plc:gfeqbblocked"
			community = "did:plc:gfeqbcommunity"
		)
		for _, did := range []string{viewer, author, blocked} {
			createTestUser(t, db, did[len("did:plc:"):]+".test", did)
		}
		createTestCommunity(t, db, community, "c-gfeqbcommunity.coves.social", author)
		insertCommunityBlock(t, db, viewer, community)
		insertUserBlock(t, db, viewer, blocked)
		// The newest post overall is in another community: it must cost this
		// community's bound no slot.
		const otherCommunity = "did:plc:gfeqbothercommunity"
		createTestCommunity(t, db, otherCommunity, "c-gfeqbothercommunity.coves.social", author)

		base := time.Now().UTC().Truncate(time.Microsecond)
		seedAcceptedHotPost(t, db, otherCommunity, author, "other", base.Add(-time.Minute), 0)
		// Newest first. Rank order runs against recency; k6, just past every
		// bound, outranks everything.
		k1 := seedAcceptedHotPost(t, db, community, blocked, "k1", base.Add(-10*time.Minute), 1)        // 0.912
		k2 := seedAcceptedHotPost(t, db, community, author, "k2", base.Add(-hours(1)), 0)               // 0.415
		k3 := seedAcceptedHotPost(t, db, community, author, "k3", base.Add(-hours(2)), 100)             // 1.852
		k4 := seedAcceptedHotPost(t, db, community, author, "k4", base.Add(-hours(3)), 10_000)          // 2.818
		k5 := seedAcceptedHotPost(t, db, community, author, "k5", base.Add(-hours(4)), 1_000_000)       // 3.533
		seedAcceptedHotPost(t, db, community, author, "k6", base.Add(-hours(5)), hotBoundExcludedScore) // 4.580

		h := newGetFeedHarness(t, db, candidateLimit, viewer)

		cases := []struct {
			name   string
			viewer string
			want   []string
		}{
			// The blocked author's k1 costs no bound slot: k2-k5. The viewer's
			// community block does not apply to the community's own feed.
			{name: "signed-in viewer blocking the community and an author", viewer: viewer, want: []string{k5, k4, k3, k2}},
			// k1-k4.
			{name: "anonymous", viewer: "", want: []string{k4, k3, k1, k2}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				reads := assertGetFeedMatchesOld(t, h, equivalenceRow{
					name: "getCommunity hot", surface: oracleCommunity, sort: "hot",
					community: community, viewer: tc.viewer, want: tc.want,
				})
				require.Equal(t, []bool{true, false}, oracleCursorPresence(reads.oracle),
					"getCommunity hot: the bound is two full pages, the last without a cursor")

				omitted := h.readFeedPages(t, getFeedPath, url.Values{"scope": {"community"}, "community": {community}}, tc.viewer)
				assertSameFeedPages(t, reads.feed, omitted, "getFeed community feed omitted")
			})
		}
	})

	t.Run("home", func(t *testing.T) {
		t.Parallel()

		db := testkit.DB(t)
		const (
			viewer       = "did:plc:gfeqhviewer"
			author       = "did:plc:gfeqhauthor"
			blocked      = "did:plc:gfeqhblocked"
			levelOne     = "did:plc:gfeqhlevelone"
			levelThree   = "did:plc:gfeqhlevelthree"
			unsubscribed = "did:plc:gfeqhunsubscribed"
		)
		for _, did := range []string{viewer, author, blocked} {
			createTestUser(t, db, did[len("did:plc:"):]+".test", did)
		}
		createTestCommunity(t, db, levelOne, "c-gfeqhlevelone.coves.social", author)
		createTestCommunity(t, db, levelThree, "c-gfeqhlevelthree.coves.social", author)
		createTestCommunity(t, db, unsubscribed, "c-gfeqhunsubscribed.coves.social", author)
		subscribeAtLevel(t, db, viewer, levelOne, 1)
		subscribeAtLevel(t, db, viewer, levelThree, 3)
		insertUserBlock(t, db, viewer, blocked)

		base := time.Now().UTC().Truncate(time.Microsecond)
		// Newer than every bound post but never served: a blocked author's
		// post in a subscribed community and a post in an unsubscribed one.
		// Neither may cost a bound slot.
		seedAcceptedHotPost(t, db, levelThree, blocked, "hblocked", base.Add(-10*time.Minute), 0)
		seedAcceptedHotPost(t, db, unsubscribed, author, "hunsub", base.Add(-20*time.Minute), 0)
		// Newest four are the bound: h1-h4. h1 is below the level-1 line (the
		// community's window is h1 and old50: ceil(2×0.10) = 1, line 50), so
		// it is filtered after taking a bound slot. Filtering first would
		// admit h5, which outranks everything.
		seedAcceptedHotPost(t, db, levelOne, author, "h1", base.Add(-30*time.Minute), 1)                        // 0.814, filtered
		h2 := seedAcceptedHotPost(t, db, levelThree, author, "h2", base.Add(-hours(1)), 0)                      // 0.415
		h3 := seedAcceptedHotPost(t, db, levelThree, author, "h3", base.Add(-hours(2)), 100)                    // 1.852
		h4 := seedAcceptedHotPost(t, db, levelThree, author, "h4", base.Add(-hours(3)), 10_000)                 // 2.818
		seedAcceptedHotPost(t, db, levelThree, author, "h5", base.Add(-190*time.Minute), hotBoundExcludedScore) // 5.839
		seedAcceptedHotPost(t, db, levelOne, author, "old50", base.Add(-hours(20)), 50)                         // 0.416

		h := newGetFeedHarness(t, db, candidateLimit, viewer)

		reads := assertGetFeedMatchesOld(t, h, equivalenceRow{
			name: "getTimeline hot", surface: oracleTimeline, sort: "hot", viewer: viewer, want: []string{h4, h3, h2},
		})
		require.Equal(t, []bool{true, false}, oracleCursorPresence(reads.oracle))
	})
}

// B12: a post created and scores changed between page 1 and page 2 do not
// make getFeed's or the getTimeline alias's page 2 differ from the old
// getTimeline's.
func TestGetFeedHot_MidPagingMutationMatchesTimeline(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const (
		viewer    = "did:plc:gfeqmviewer"
		author    = "did:plc:gfeqmauthor"
		community = "did:plc:gfeqmcommunity"
	)
	for _, did := range []string{viewer, author} {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	createTestCommunity(t, db, community, "c-gfeqmcommunity.coves.social", author)
	subscribeAtLevel(t, db, viewer, community, 3)

	base := time.Now().UTC().Truncate(time.Microsecond)
	m1 := seedAcceptedHotPost(t, db, community, author, "m1", base.Add(-hours(1)), 100) // 2.332
	m2 := seedAcceptedHotPost(t, db, community, author, "m2", base.Add(-hours(2)), 50)  // 1.627
	m3 := seedAcceptedHotPost(t, db, community, author, "m3", base.Add(-hours(3)), 20)  // 1.116
	m4 := seedAcceptedHotPost(t, db, community, author, "m4", base.Add(-hours(4)), 10)  // 0.810
	m5 := seedAcceptedHotPost(t, db, community, author, "m5", base.Add(-hours(5)), 5)   // 0.589

	h := newGetFeedHarness(t, db, 0, viewer)
	ctx := t.Context()
	oracleReq := oracleFeedRequest{ViewerDID: viewer, Sort: "hot", Limit: 2}
	serviceReq := feedgen.GetFeedRequest{
		ViewerDID: viewer, Scope: feedgen.Scope{Name: feedgen.ScopeHome}, Feed: h.hotFeedURI(), Limit: 2,
	}
	feedParams := url.Values{"scope": {"home"}, "feed": {h.hotFeedURI()}, "limit": {"2"}}
	aliasParams := url.Values{"sort": {"hot"}, "limit": {"2"}}

	oracleFirst, oracleCursor, err := oracleFeedPage(ctx, h.oracle, oracleTimeline, oracleReq)
	require.NoError(t, err)
	require.Equal(t, []string{m1, m2}, oracleFeedPageResult{Posts: oracleFirst}.URIs(), "getTimeline hot page 1")
	require.NotEmpty(t, oracleCursor)
	serviceFirst, err := h.feedService.GetFeed(ctx, serviceReq)
	require.NoError(t, err)
	require.NotEmpty(t, serviceFirst.Cursor, "getFeed service page 1 cursor")
	newFirst, newCursor := h.getFeedPage(t, getFeedPath, feedParams, viewer)
	require.NotEmpty(t, newCursor, "getFeed page 1 cursor")
	// The alias's page 1 is read before the mutation too; it is decoded in its
	// subtest.
	aliasFirst := h.serve(h.newRequest(t, getTimelinePath, aliasParams, viewer))

	// Between pages: a new post above the boundary, m5 lifted above it
	// (2.153), m3 sunk below m4 (-0.219).
	seedAcceptedHotPost(t, db, community, author, "mnew", base.Add(-10*time.Minute), 1_000) // 4.261
	_, err = db.ExecContext(ctx, `UPDATE posts SET score = 10000 WHERE uri = $1`, m5)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE posts SET score = -5 WHERE uri = $1`, m3)
	require.NoError(t, err)

	oracleReq.Cursor = oracleCursor
	oracleSecond, oracleNext, err := oracleFeedPage(ctx, h.oracle, oracleTimeline, oracleReq)
	require.NoError(t, err)
	require.Equal(t, []string{m4, m3}, oracleFeedPageResult{Posts: oracleSecond}.URIs(), "getTimeline hot page 2 after the mutation")
	require.Empty(t, oracleNext, "getTimeline hot page 2 is the last")
	oraclePages := []oracleFeedPageResult{{Posts: oracleFirst, Cursor: oracleCursor}, {Posts: oracleSecond}}

	newSecond, newNext := h.getFeedPage(t, getFeedPath, withFeedParams(feedParams, "cursor", newCursor), viewer)
	newPages := []feedPage{newFirst, newSecond}

	t.Run("oracle vs getFeed", func(t *testing.T) {
		serviceReq.Cursor = serviceFirst.Cursor
		serviceSecond, err := h.feedService.GetFeed(ctx, serviceReq)
		require.NoError(t, err)
		assertOracleMatchesFeed(t, oraclePages, newPages, []*feedgen.GetFeedResponse{serviceFirst, serviceSecond},
			"getFeed home feed=hot across the mutation")
		assert.Empty(t, newNext, "getFeed page 2 is the last")
	})

	t.Run("alias", func(t *testing.T) {
		first, cursor := decodeFeedPage(t, aliasFirst, "alias getTimeline hot page 1")
		require.NotEmpty(t, cursor, "alias page 1 cursor")
		second, next := h.getFeedPage(t, getTimelinePath, withFeedParams(aliasParams, "cursor", cursor), viewer)
		assertAliasMatchesFeed(t, oraclePagesURIs(oraclePages), newPages, []feedPage{first, second},
			"alias getTimeline hot across the mutation")
		assert.Empty(t, next, "alias page 2 is the last")
	})
}

// B13: equal-age posts scored -3 at levels 4 and 3: dividing the negative rank
// by 1.5 makes it less negative, so the level-4 post comes first even though
// the uri DESC tiebreak alone would put it second.
func TestGetFeedHot_LevelFourNegativeScoreRanksFirst(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const (
		viewer = "did:plc:gfeqnviewer"
		author = "did:plc:gfeqnauthor"
		// The level-4 community's DID sorts lower: on an unweighted tie the
		// tiebreak would serve the level-3 post first.
		levelFour  = "did:plc:gfeqnega"
		levelThree = "did:plc:gfeqnegb"
	)
	for _, did := range []string{viewer, author} {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	createTestCommunity(t, db, levelFour, "c-gfeqnega.coves.social", author)
	createTestCommunity(t, db, levelThree, "c-gfeqnegb.coves.social", author)
	subscribeAtLevel(t, db, viewer, levelFour, 4)
	subscribeAtLevel(t, db, viewer, levelThree, 3)

	createdAt := time.Now().UTC().Truncate(time.Microsecond).Add(-hours(3))
	boosted := seedAcceptedHotPost(t, db, levelFour, author, "pair", createdAt, -3)
	plain := seedAcceptedHotPost(t, db, levelThree, author, "pair", createdAt, -3)
	want := []string{boosted, plain}

	h := newGetFeedHarness(t, db, 0, viewer)
	oraclePage, oracleCursor, err := oracleFeedPage(t.Context(), h.oracle, oracleTimeline,
		oracleFeedRequest{ViewerDID: viewer, Sort: "hot", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, want, oracleFeedPageResult{Posts: oraclePage}.URIs(), "getTimeline hot: fixture order")
	require.Empty(t, oracleCursor)

	page, cursor := h.getFeedPage(t, getFeedPath, url.Values{"scope": {"home"}, "feed": {h.hotFeedURI()}, "limit": {"10"}}, viewer)
	t.Run("oracle vs getFeed", func(t *testing.T) {
		assert.Equal(t, want, feedPageURIs(t, page.Feed))
		assert.Empty(t, cursor)
	})

	t.Run("alias", func(t *testing.T) {
		aliasPage, aliasCursor := h.getFeedPage(t, getTimelinePath, url.Values{"sort": {"hot"}, "limit": {"10"}}, viewer)
		assertAliasMatchesFeed(t, [][]string{want}, []feedPage{page}, []feedPage{aliasPage}, "alias getTimeline hot")
		assert.Empty(t, aliasCursor)
	})
}
