//go:build integration

package postgres

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The old feed endpoints (getTimeline, communityFeed.getCommunity,
// getDiscover) are aliases of getFeed, so their cursors are getFeed cursors:
// bound to the built-in feed, scope, community and viewer the alias mapped
// to, and the pre-change cursor formats are rejected once with 400
// InvalidCursor (no compatibility shim).

// withFeedParams copies params and sets each key/value pair.
func withFeedParams(params url.Values, pairs ...string) url.Values {
	copied := url.Values{}
	for key, values := range params {
		copied[key] = append([]string(nil), values...)
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		copied.Set(pairs[i], pairs[i+1])
	}
	return copied
}

// B4: a page-2 cursor from each alias, sent to getFeed with the feed, scope
// (and community) the alias maps to, returns the same page 2; an alias cursor
// sent to an alias of another scope is rejected.
func TestFeedAliases_CursorsAreGetFeedCursors(t *testing.T) {
	t.Parallel()

	homeDB := testkit.DB(t)
	home := seedHomeHotFixture(t, homeDB)
	homeHarness := newGetFeedHarness(t, homeDB, 0, home.viewer)

	discoverDB := testkit.DB(t)
	all := seedDiscoverAllFixture(t, discoverDB)
	discoverHarness := newGetFeedHarness(t, discoverDB, 0, all.viewer)

	const levelThree = "did:plc:gfeqlevelthree"

	rows := []struct {
		name        string
		h           *getFeedHarness
		viewer      string
		aliasPath   string
		aliasParams url.Values
		feedParams  url.Values
		// wantPage2 is the literal page 2 at limit 2; nil for Discover Hot,
		// whose order is the snapshot's: getFeed must then serve the alias's
		// own page 2.
		wantPage2 []string
	}{
		{
			name: "getTimeline hot", h: homeHarness, viewer: home.viewer,
			aliasPath:   getTimelinePath,
			aliasParams: url.Values{"sort": {"hot"}},
			feedParams:  url.Values{"scope": {"home"}, "feed": {homeHarness.registry.URI("hot")}},
			wantPage2:   []string{home.f1, home.d2},
		},
		{
			name: "getCommunity top all", h: homeHarness, viewer: home.viewer,
			aliasPath:   getCommunityPath,
			aliasParams: url.Values{"community": {levelThree}, "sort": {"top"}, "timeframe": {"all"}},
			feedParams: url.Values{
				"scope": {"community"}, "community": {levelThree},
				"feed": {homeHarness.registry.URI("top")}, "timeframe": {"all"},
			},
			wantPage2: []string{home.d2, home.d3},
		},
		{
			name: "getDiscover new", h: discoverHarness, viewer: all.viewer,
			aliasPath:   getDiscoverPath,
			aliasParams: url.Values{"sort": {"new"}},
			feedParams:  url.Values{"scope": {"all"}, "feed": {discoverHarness.registry.URI("new")}},
			wantPage2:   []string{all.c1, all.h1},
		},
		{
			name: "getDiscover no sort", h: discoverHarness, viewer: all.viewer,
			aliasPath:   getDiscoverPath,
			aliasParams: url.Values{},
			feedParams:  url.Values{"scope": {"all"}, "feed": {discoverHarness.registry.URI("discover-hot")}},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			page1, cursor := row.h.getFeedPage(t, row.aliasPath, withFeedParams(row.aliasParams, "limit", "2"), row.viewer)
			require.NotEmptyf(t, cursor, "%s page 1 must return a cursor", row.name)

			aliasPage2, _ := row.h.getFeedPage(t, row.aliasPath,
				withFeedParams(row.aliasParams, "limit", "2", "cursor", cursor), row.viewer)
			aliasURIs := feedPageURIs(t, aliasPage2.Feed)
			if row.wantPage2 != nil {
				require.Equal(t, row.wantPage2, aliasURIs, "%s page 2 through the alias", row.name)
			} else {
				require.Len(t, aliasURIs, 2, "%s page 2 through the alias", row.name)
				for _, uri := range aliasURIs {
					assert.NotContains(t, feedPageURIs(t, page1.Feed), uri, "%s page 2 repeats page 1", row.name)
				}
			}

			feedPage2, _ := row.h.getFeedPage(t, getFeedPath,
				withFeedParams(row.feedParams, "limit", "2", "cursor", cursor), row.viewer)
			assert.Equal(t, aliasURIs, feedPageURIs(t, feedPage2.Feed),
				"%s cursor sent to getFeed must serve the alias's page 2", row.name)
		})
	}

	t.Run("getTimeline hot cursor sent to getCommunity hot", func(t *testing.T) {
		_, cursor := homeHarness.getFeedPage(t, getTimelinePath, url.Values{"sort": {"hot"}, "limit": {"2"}}, home.viewer)
		require.NotEmpty(t, cursor)
		// The viewer subscribes to levelThree, so the cursor's post is in scope.
		recorder := homeHarness.serve(homeHarness.newRequest(t, getCommunityPath, url.Values{
			"community": {levelThree}, "sort": {"hot"}, "limit": {"2"}, "cursor": {cursor},
		}, home.viewer))
		requireFeedXRPCError(t, recorder, http.StatusBadRequest, "InvalidCursor", "getTimeline hot cursor on getCommunity hot")
	})

	t.Run("getDiscover new cursor sent to getTimeline new", func(t *testing.T) {
		_, cursor := discoverHarness.getFeedPage(t, getDiscoverPath, url.Values{"sort": {"new"}, "limit": {"2"}}, all.viewer)
		require.NotEmpty(t, cursor)
		recorder := discoverHarness.serve(discoverHarness.newRequest(t, getTimelinePath, url.Values{
			"sort": {"new"}, "limit": {"2"}, "cursor": {cursor},
		}, all.viewer))
		requireFeedXRPCError(t, recorder, http.StatusBadRequest, "InvalidCursor", "getDiscover new cursor on getTimeline new")
	})
}

// B5: a cursor in the pre-change format, which today's endpoint accepts, is
// rejected by the alias with 400 InvalidCursor. Each cursor is first proven
// acceptable to the old read (the oracle, which parses exactly as the old
// repositories did; Discover Hot's generator for its raw inner cursor) on the
// same database.
func TestFeedAliases_PreChangeCursorsAreRejected(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := seedHomeHotFixture(t, db)
	h := newGetFeedHarness(t, db, 0, f.viewer)
	const levelThree = "did:plc:gfeqlevelthree"

	// Old-format payloads for d1, signed as the old repositories signed them
	// (feedRepoBase.signCursorPayload, through NewCursorSigner) with the
	// harness's cursor secret, which the old repositories and getFeed share.
	var createdAt time.Time
	var score int
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT created_at, score FROM posts WHERE uri = $1`, f.d1).Scan(&createdAt, &score))
	created := createdAt.Format(time.RFC3339Nano)
	signer := NewCursorSigner(getFeedEquivalenceCursorSecret)
	oldHot := signer.Sign(fmt.Sprintf("%s::%s::%s", created, f.d1, time.Now().Format(time.RFC3339Nano)))
	oldNew := signer.Sign(fmt.Sprintf("%s::%s", created, f.d1))
	oldTop := signer.Sign(fmt.Sprintf("%d::%s::%s", score, created, f.d1))

	// Discover Hot's pre-change cursor is the raw inner discover-hot cursor
	// its generator returns (getDiscover served it unwrapped).
	discoverHotPage, err := h.discoverRepo.HydratedPage(t.Context(), feedgen.SkeletonRequest{
		ViewerDID: f.viewer, Scope: feedgen.Scope{Name: feedgen.ScopeAll}, Limit: 2,
	})
	require.NoError(t, err)
	oldDiscoverHot := discoverHotPage.Cursor
	require.NotEmpty(t, oldDiscoverHot, "Discover Hot page 1 must return a cursor")

	oracle := newFeedOracle(db, getFeedEquivalenceCursorSecret, WithAccountRestrictionAuthority(fixtures.InstanceDID()))

	rows := []struct {
		name    string
		path    string
		params  url.Values
		cursor  string
		surface oracleFeedSurface
		// oracleReq is the old read the cursor belongs to; zero for Discover Hot.
		oracleReq oracleFeedRequest
	}{
		{
			"getTimeline hot", getTimelinePath,
			url.Values{"sort": {"hot"}},
			oldHot,
			oracleTimeline,
			oracleFeedRequest{ViewerDID: f.viewer, Sort: "hot", Limit: 2},
		},
		{
			"getTimeline new", getTimelinePath,
			url.Values{"sort": {"new"}},
			oldNew,
			oracleTimeline,
			oracleFeedRequest{ViewerDID: f.viewer, Sort: "new", Limit: 2},
		},
		{
			"getCommunity hot", getCommunityPath,
			url.Values{"community": {levelThree}, "sort": {"hot"}},
			oldHot,
			oracleCommunity,
			oracleFeedRequest{ViewerDID: f.viewer, Community: levelThree, Sort: "hot", Limit: 2},
		},
		{
			"getCommunity top", getCommunityPath,
			url.Values{"community": {levelThree}, "sort": {"top"}},
			oldTop,
			oracleCommunity,
			oracleFeedRequest{ViewerDID: f.viewer, Community: levelThree, Sort: "top", Timeframe: "day", Limit: 2},
		},
		{
			"getDiscover new", getDiscoverPath,
			url.Values{"sort": {"new"}},
			oldNew,
			oracleDiscover,
			oracleFeedRequest{ViewerDID: f.viewer, Sort: "new", Limit: 2},
		},
		{
			"getDiscover top", getDiscoverPath,
			url.Values{"sort": {"top"}},
			oldTop,
			oracleDiscover,
			oracleFeedRequest{ViewerDID: f.viewer, Sort: "top", Timeframe: "day", Limit: 2},
		},
		{"getDiscover hot", getDiscoverPath, url.Values{"sort": {"hot"}}, oldDiscoverHot, "", oracleFeedRequest{}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			// The cursor is one today's read accepts, continuing past page 1.
			if row.surface == "" {
				page, err := h.discoverRepo.HydratedPage(t.Context(), feedgen.SkeletonRequest{
					ViewerDID: f.viewer, Scope: feedgen.Scope{Name: feedgen.ScopeAll}, Limit: 2, Cursor: row.cursor,
				})
				require.NoError(t, err, "THE CURSOR IS WRONG, not the code: Discover Hot must accept its own inner cursor")
				require.NotEmpty(t, page.Feed)
			} else {
				req := row.oracleReq
				req.Cursor = row.cursor
				views, _, err := oracleFeedPage(t.Context(), oracle, row.surface, req)
				require.NoError(t, err, "THE CURSOR IS WRONG, not the code: the old %s read must accept it", row.surface)
				require.NotEmpty(t, views, "the old %s read continues past the cursor's post", row.surface)
			}

			recorder := h.serve(h.newRequest(t, row.path,
				withFeedParams(row.params, "limit", "2", "cursor", row.cursor), f.viewer))
			requireFeedXRPCError(t, recorder, http.StatusBadRequest, "InvalidCursor", row.name+" with a pre-change cursor")
		})
	}
}
