//go:build integration

package postgres

import (
	"database/sql"
	"net/url"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"
	"Coves/internal/core/feedgen"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getFeed with the built-in new and top generators must serve exactly what the
// old feeds served for sort new and top: getTimeline for scope home and
// getCommunity for scope community, read through the oracle
// (feed_oracle_test.go); the old endpoints, now aliases, must serve the same.
// The harness, paging and comparison are the hot equivalence test's
// (getfeed_hot_equivalence_test.go). Every expected
// order is written out by hand: new is created_at DESC, uri DESC; top is score
// DESC, created_at DESC, uri DESC inside the timeframe window.

// newFeedURI and topFeedURI are the registry's at-uris for the built-in new and
// top feeds.
func (h *getFeedHarness) newFeedURI() string { return h.registry.URI("new") }
func (h *getFeedHarness) topFeedURI() string { return h.registry.URI("top") }

// readSkeletonURIs pages a generator's skeleton directly at limit 2 with its own
// inner cursors and returns every URI it ranked.
func readSkeletonURIs(t *testing.T, generator feedgen.PostGenerator, req feedgen.SkeletonRequest) []string {
	t.Helper()

	const maxPages = 20
	var uris []string
	req.Limit = 2
	req.Cursor = ""
	for range maxPages {
		page, err := generator.Skeleton(t.Context(), req)
		require.NoErrorf(t, err, "skeleton page after cursor %q", req.Cursor)
		for _, item := range page.Items {
			uris = append(uris, item.URI)
		}
		if page.Cursor == "" {
			return uris
		}
		req.Cursor = page.Cursor
	}
	require.FailNowf(t, "skeleton did not end", "still had a cursor after %d pages", maxPages)
	return nil
}

// equivalenceRow is one old read and every path that must serve it: the
// oracle (the old read's SQL and cursors), the getFeed service, getFeed over
// HTTP and the old endpoint, now a getFeed alias.
type equivalenceRow struct {
	name string
	// surface is the old read: its oracle, its alias endpoint and the getFeed
	// scope it maps to (timeline: home, community: community, discover: all).
	surface oracleFeedSurface
	// sort is the alias's sort parameter and the built-in feed's rkey: hot,
	// new or top.
	sort string
	// timeframe goes as given to the alias and getFeed ("" = omitted); the
	// oracle receives it as the old services passed it on (top: "" -> day).
	timeframe string
	// community is the community DID for surface community.
	community string
	viewer    string
	want      []string
	// hidden are planted posts no path may serve.
	hidden []string
}

// scope is the getFeed scope the row's alias maps to.
func (row equivalenceRow) scope() feedgen.Scope {
	switch row.surface {
	case oracleTimeline:
		return feedgen.Scope{Name: feedgen.ScopeHome}
	case oracleCommunity:
		return feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: row.community}
	default:
		return feedgen.Scope{Name: feedgen.ScopeAll}
	}
}

// aliasPath is the row's old endpoint.
func (row equivalenceRow) aliasPath() string {
	switch row.surface {
	case oracleTimeline:
		return getTimelinePath
	case oracleCommunity:
		return getCommunityPath
	default:
		return getDiscoverPath
	}
}

// aliasParams is the old endpoint's query.
func (row equivalenceRow) aliasParams() url.Values {
	params := withTimeframe(url.Values{"sort": {row.sort}}, row.timeframe)
	if row.surface == oracleCommunity {
		params.Set("community", row.community)
	}
	return params
}

// oracleRequest is what the old repository received for the row at limit 2.
func (row equivalenceRow) oracleRequest() oracleFeedRequest {
	timeframe := row.timeframe
	if row.sort == "top" && timeframe == "" {
		timeframe = "day"
	}
	return oracleFeedRequest{
		ViewerDID: row.viewer, Community: row.community,
		Sort: row.sort, Timeframe: timeframe, Limit: 2,
	}
}

// feedParams is the getFeed query the row's alias maps to.
func (h *getFeedHarness) feedParams(row equivalenceRow) url.Values {
	scope := row.scope()
	params := withTimeframe(url.Values{"scope": {scope.Name}, "feed": {h.registry.URI(row.sort)}}, row.timeframe)
	if scope.CommunityDID != "" {
		params.Set("community", scope.CommunityDID)
	}
	return params
}

// serviceRequest is the getFeed service request the row's alias maps to.
func (h *getFeedHarness) serviceRequest(row equivalenceRow) feedgen.GetFeedRequest {
	return feedgen.GetFeedRequest{
		ViewerDID: row.viewer, Scope: row.scope(),
		Feed: h.registry.URI(row.sort), Timeframe: row.timeframe,
	}
}

// equivalenceReads is a row's oracle pages and getFeed (HTTP) pages, for
// further literal checks.
type equivalenceReads struct {
	oracle []oracleFeedPageResult
	feed   []feedPage
}

// assertNoneServed requires that served holds none of hidden.
func assertNoneServed(t *testing.T, served, hidden []string, label string) {
	t.Helper()

	for _, uri := range hidden {
		assert.NotContainsf(t, served, uri, "%s: served a hidden post", label)
	}
}

// assertGetFeedMatchesOld pages the oracle and requires its literal order
// (proving the fixture), then, each paged to the end with its own cursors:
// getFeed (service and HTTP) against the oracle page for page, and the alias
// against both (the oracle's URIs, getFeed's enriched items).
func assertGetFeedMatchesOld(t *testing.T, h *getFeedHarness, row equivalenceRow) equivalenceReads {
	t.Helper()

	oracle := readOracleFeedPages(t, h.oracle, row.surface, row.oracleRequest())
	require.Equalf(t, row.want, oraclePagedURIs(oracle), "%s: oracle fixture order", row.name)
	assertNoneServed(t, oraclePagedURIs(oracle), row.hidden, row.name+": oracle")

	feed := h.readFeedPages(t, getFeedPath, h.feedParams(row), row.viewer)

	t.Run("oracle vs getFeed", func(t *testing.T) {
		assertOracleMatchesFeed(t, oracle, feed, h.readServicePages(t, h.serviceRequest(row)), row.name+": getFeed")
		assert.Equalf(t, row.want, pagedURIs(t, feed), "%s: getFeed order", row.name)
		assertNoneServed(t, pagedURIs(t, feed), row.hidden, row.name+": getFeed")
	})

	t.Run("alias", func(t *testing.T) {
		alias := h.readFeedPages(t, row.aliasPath(), row.aliasParams(), row.viewer)
		assertAliasMatchesFeed(t, oraclePagesURIs(oracle), feed, alias, row.name+": alias "+row.aliasPath())
		assertNoneServed(t, pagedURIs(t, alias), row.hidden, row.name+": alias")
	})

	return equivalenceReads{oracle: oracle, feed: feed}
}

// withTimeframe adds timeframe to params unless it is "".
func withTimeframe(params url.Values, timeframe string) url.Values {
	if timeframe != "" {
		params.Set("timeframe", timeframe)
	}
	return params
}

// homeNewTopFixture is the hot home fixture plus posts spread across every top
// timeframe window, one more below-threshold level-1 post, and a second
// subscriber whose subscriptions must not reach the viewer's home feed.
type homeNewTopFixture struct {
	homeHotFixture
	// n20m is the only visible post inside the hour window; w3d, m20d, y200d
	// and all2y first enter the week, month, year and all windows.
	n20m, w3d, m20d, y200d, all2y string
	// belowThreshold are level-1/2 posts under their community's score line.
	belowThreshold []string
	// otherOnly is a visible post in a community only the second subscriber
	// follows; it would top every home read if it leaked.
	otherOnly string
}

// seedHomeNewTopFixture seeds seedHomeHotFixture and adds to it. The community
// and author DIDs are that fixture's.
func seedHomeNewTopFixture(t *testing.T, db *sql.DB) homeNewTopFixture {
	t.Helper()

	const (
		authorA    = "did:plc:gfeqauthora"
		levelOne   = "did:plc:gfeqlevelone"
		levelThree = "did:plc:gfeqlevelthree"
		levelFour  = "did:plc:gfeqlevelfour"
		levelFive  = "did:plc:gfeqlevelfive"
	)
	f := homeNewTopFixture{homeHotFixture: seedHomeHotFixture(t, db)}

	base := time.Now().UTC().Truncate(time.Microsecond)
	at := func(age time.Duration) time.Time { return base.Add(-age) }
	days := func(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

	f.n20m = seedAcceptedHotPost(t, db, levelThree, authorA, "n20m", at(20*time.Minute), 7)
	f.w3d = seedAcceptedHotPost(t, db, levelFour, authorA, "w3d", at(days(3)), 30)
	f.m20d = seedAcceptedHotPost(t, db, levelFive, authorA, "m20d", at(days(20)), 60)
	f.y200d = seedAcceptedHotPost(t, db, levelThree, authorA, "y200d", at(days(200)), 45)
	f.all2y = seedAcceptedHotPost(t, db, levelThree, authorA, "all2y", at(days(730)), 100)
	// Inside the hour window and below levelOne's line: its window is now a50,
	// a1 and a30m, ceil(3×0.10) = 1, so the line stays a50's 50.
	a30m := seedAcceptedHotPost(t, db, levelOne, authorA, "a30m", at(30*time.Minute), 2)

	// A second subscriber: to levelThree at level 1 (the viewer is at 3), and
	// alone to otherOnlyCommunity. Without the viewer's subscription filter,
	// levelThree's posts would repeat and otherOnly would rank first.
	const (
		otherSubscriber    = "did:plc:gfntothersub"
		otherOnlyCommunity = "did:plc:gfntotheronly"
	)
	createTestUser(t, db, otherSubscriber[len("did:plc:"):]+".test", otherSubscriber)
	subscribeAtLevel(t, db, otherSubscriber, levelThree, 1)
	createTestCommunity(t, db, otherOnlyCommunity, "c-gfntotheronly.coves.social", authorA)
	subscribeAtLevel(t, db, otherSubscriber, otherOnlyCommunity, 3)
	f.otherOnly = seedAcceptedHotPost(t, db, otherOnlyCommunity, authorA, "oo", at(10*time.Minute), 900)

	f.belowThreshold = []string{
		a30m,
		"at://did:plc:gfeqlevelone/social.coves.community.post/a1",
		"at://did:plc:gfeqleveltwo/social.coves.community.post/b2",
	}
	return f
}

// B3 (acceptance): getFeed scope=home with the registry's new and top uris
// serves the old getTimeline sort=new and sort=top (every timeframe, and none)
// page for page, as does the getTimeline alias; the generators' own skeletons rank only what the old feeds serve; and a
// lowered hot candidate bound changes neither.
func TestGetFeedNewTop_HomeMatchesTimeline(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := seedHomeNewTopFixture(t, db)

	wantNew := []string{f.n20m, f.d1, f.d2, f.d4, f.v1, f.f2, f.f1, f.a50, f.b40, f.d3, f.w3d, f.m20d, f.y200d, f.all2y}
	wantTopDay := []string{f.a50, f.f1, f.d1, f.d4, f.n20m, f.d2, f.v1, f.f2}
	wantTopAll := []string{f.all2y, f.m20d, f.a50, f.y200d, f.b40, f.w3d, f.f1, f.d1, f.d4, f.n20m, f.d2, f.v1, f.d3, f.f2}

	type sortRow struct {
		name      string
		sort      string
		timeframe string
		want      []string
	}
	sortRows := []sortRow{
		{name: "new", sort: "new", want: wantNew},
		{name: "top timeframe omitted (day)", sort: "top", want: wantTopDay},
		{name: "top hour", sort: "top", timeframe: "hour", want: []string{f.n20m}},
		{name: "top day", sort: "top", timeframe: "day", want: wantTopDay},
		{
			name: "top week", sort: "top", timeframe: "week",
			want: []string{f.a50, f.b40, f.w3d, f.f1, f.d1, f.d4, f.n20m, f.d2, f.v1, f.d3, f.f2},
		},
		{
			name: "top month", sort: "top", timeframe: "month",
			want: []string{f.m20d, f.a50, f.b40, f.w3d, f.f1, f.d1, f.d4, f.n20m, f.d2, f.v1, f.d3, f.f2},
		},
		{
			name: "top year", sort: "top", timeframe: "year",
			want: []string{f.m20d, f.a50, f.y200d, f.b40, f.w3d, f.f1, f.d1, f.d4, f.n20m, f.d2, f.v1, f.d3, f.f2},
		},
		{name: "top all", sort: "top", timeframe: "all", want: wantTopAll},
	}

	row := func(r sortRow) equivalenceRow {
		return equivalenceRow{
			name:      r.name,
			surface:   oracleTimeline,
			sort:      r.sort,
			timeframe: r.timeframe,
			viewer:    f.viewer,
			want:      r.want,
			hidden:    []string{f.otherOnly},
		}
	}

	h := newGetFeedHarness(t, db, 0, f.viewer)

	for _, r := range sortRows {
		t.Run(r.name, func(t *testing.T) {
			assertGetFeedMatchesOld(t, h, row(r))
		})
	}

	t.Run("new excludes the below-threshold level-1/2 posts", func(t *testing.T) {
		pages := h.readFeedPages(t, getFeedPath, url.Values{"scope": {"home"}, "feed": {h.newFeedURI()}}, f.viewer)
		served := pagedURIs(t, pages)
		require.NotEmpty(t, served, "getFeed home feed=new served nothing")
		for _, uri := range f.belowThreshold {
			assert.NotContains(t, served, uri)
		}
	})

	t.Run("new skeleton ranks only visible posts", func(t *testing.T) {
		got := readSkeletonURIs(t, h.newGenerator, feedgen.SkeletonRequest{
			ViewerDID: f.viewer, Scope: feedgen.Scope{Name: feedgen.ScopeHome}, Timeframe: "day",
		})
		assert.Equal(t, wantNew, got)
		assertNoneServed(t, got, []string{f.otherOnly}, "new home skeleton")
	})

	t.Run("top all skeleton ranks only visible posts", func(t *testing.T) {
		got := readSkeletonURIs(t, h.topGenerator, feedgen.SkeletonRequest{
			ViewerDID: f.viewer, Scope: feedgen.Scope{Name: feedgen.ScopeHome}, Timeframe: "all",
		})
		assert.Equal(t, wantTopAll, got)
		assertNoneServed(t, got, []string{f.otherOnly}, "top home skeleton")
	})

	t.Run("hot candidate bound lowered to 2", func(t *testing.T) {
		bounded := newGetFeedHarness(t, db, 2, f.viewer)
		for _, r := range []sortRow{sortRows[0], sortRows[len(sortRows)-1]} {
			t.Run(r.name, func(t *testing.T) {
				assertGetFeedMatchesOld(t, bounded, row(r))
			})
		}
	})
}

// B4: getFeed scope=community with the new and top uris serves getCommunity
// sort=new and sort=top page for page, for a community the signed-in viewer
// blocks (the block does not apply to the community's own feed; an author
// block still does), one they do not, and an anonymous reader. Soft-deleted,
// moderation-removed and taken-down-author posts are served to neither. The blocked
// community's equal-score posts break ties on created_at and then on uri
// across limit-2 page boundaries.
func TestGetFeedNewTop_CommunityMatchesGetCommunity(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const (
		viewer           = "did:plc:gfntcviewer"
		author           = "did:plc:gfntcauthor"
		blocked          = "did:plc:gfntcblocked"
		restricted       = "did:plc:gfntcrestrict"
		blockedCommunity = "did:plc:gfntcblockedc"
		plainCommunity   = "did:plc:gfntcplainc"
	)
	for _, did := range []string{viewer, author, blocked, restricted} {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	accountstatusfixture.Set(t, db, restricted, accountstatus.Takendown)
	createTestCommunity(t, db, blockedCommunity, "c-gfntcblockedc.coves.social", author)
	createTestCommunity(t, db, plainCommunity, "c-gfntcplainc.coves.social", author)
	insertCommunityBlock(t, db, viewer, blockedCommunity)
	insertUserBlock(t, db, viewer, blocked)

	base := time.Now().UTC().Truncate(time.Microsecond)
	at := func(age time.Duration) time.Time { return base.Add(-age) }

	c1 := seedAcceptedHotPost(t, db, blockedCommunity, author, "c1", at(hours(2)), 10)
	c2 := seedAcceptedHotPost(t, db, blockedCommunity, author, "c2", at(hours(5)), 10)
	// c3 and c4 tie on score and created_at: uri DESC puts c4 first.
	c3 := seedAcceptedHotPost(t, db, blockedCommunity, author, "c3", at(hours(8)), 10)
	c4 := seedAcceptedHotPost(t, db, blockedCommunity, author, "c4", at(hours(8)), 10)
	c5 := seedAcceptedHotPost(t, db, blockedCommunity, author, "c5", at(hours(3)), -2)
	c6 := seedAcceptedHotPost(t, db, blockedCommunity, author, "c6", at(hours(72)), 40)
	// The blocked author's post: hidden from the viewer, served anonymously.
	cblk := seedAcceptedHotPost(t, db, blockedCommunity, blocked, "cblk", at(hours(1)), 1000)
	// Never served.
	pending := seedHotBoundPost(t, db, blockedCommunity, author, "cpend", at(hours(1)), 1000)
	seedHotBoundPendingAdmission(t, db, blockedCommunity, pending)
	// Never served to anyone; each is the newest and highest-scored post in
	// every window, so it would lead every read if it leaked.
	deleted := seedAcceptedHotPost(t, db, blockedCommunity, author, "cdel", at(40*time.Minute), 900)
	_, err := db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, deleted)
	require.NoError(t, err)
	removed := seedAcceptedHotPost(t, db, blockedCommunity, author, "crem", at(45*time.Minute), 900)
	insertDiscoverHotRemoval(t, db, removed)
	takenDown := seedAcceptedHotPost(t, db, blockedCommunity, restricted, "crestr", at(50*time.Minute), 900)
	hidden := []string{pending, deleted, removed, takenDown}

	p1 := seedAcceptedHotPost(t, db, plainCommunity, author, "p1", at(hours(1)), 3)
	p2 := seedAcceptedHotPost(t, db, plainCommunity, author, "p2", at(hours(48)), 7)
	seedAcceptedHotPost(t, db, plainCommunity, blocked, "p3", at(30*time.Minute), 9)

	h := newGetFeedHarness(t, db, 0, viewer)

	wantBlockedTopAll := []string{c6, c1, c2, c4, c3, c5}
	rows := []struct {
		name      string
		community string
		sort      string
		timeframe string
		viewer    string
		want      []string
	}{
		{"blocked community new", blockedCommunity, "new", "", viewer, []string{c1, c5, c2, c4, c3, c6}},
		{"blocked community top day", blockedCommunity, "top", "day", viewer, []string{c1, c2, c4, c3, c5}},
		{"blocked community top all", blockedCommunity, "top", "all", viewer, wantBlockedTopAll},
		{"plain community new", plainCommunity, "new", "", viewer, []string{p1, p2}},
		{"plain community top day", plainCommunity, "top", "day", viewer, []string{p1}},
		{"plain community top all", plainCommunity, "top", "all", viewer, []string{p2, p1}},
		{"anonymous new", blockedCommunity, "new", "", "", []string{cblk, c1, c5, c2, c4, c3, c6}},
		{"anonymous top all", blockedCommunity, "top", "all", "", []string{cblk, c6, c1, c2, c4, c3, c5}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			assertGetFeedMatchesOld(t, h, equivalenceRow{
				name:      r.name,
				surface:   oracleCommunity,
				sort:      r.sort,
				timeframe: r.timeframe,
				community: r.community,
				viewer:    r.viewer,
				want:      r.want,
				hidden:    hidden,
			})
		})
	}

	for _, r := range []struct {
		name      string
		generator *SortGenerator
		timeframe string
		viewer    string
		want      []string
	}{
		{"new skeleton signed in", h.newGenerator, "", viewer, []string{c1, c5, c2, c4, c3, c6}},
		{"new skeleton anonymous", h.newGenerator, "", "", []string{cblk, c1, c5, c2, c4, c3, c6}},
		{"top all skeleton signed in", h.topGenerator, "all", viewer, wantBlockedTopAll},
		{"top all skeleton anonymous", h.topGenerator, "all", "", []string{cblk, c6, c1, c2, c4, c3, c5}},
	} {
		t.Run(r.name+" ranks only visible posts", func(t *testing.T) {
			got := readSkeletonURIs(t, r.generator, feedgen.SkeletonRequest{
				ViewerDID: r.viewer,
				Scope:     feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: blockedCommunity},
				Timeframe: r.timeframe,
			})
			assert.Equal(t, r.want, got)
			assertNoneServed(t, got, hidden, r.name)
		})
	}
}

// B5: the top timeframe selects the window: a 20-day-old post, the highest
// scored, is outside week, first in month, and outside the default day.
func TestGetFeedTop_CommunityTimeframes(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const (
		author    = "did:plc:gfnttauthor"
		community = "did:plc:gfnttcommunity"
	)
	createTestUser(t, db, author[len("did:plc:"):]+".test", author)
	createTestCommunity(t, db, community, "c-gfnttcommunity.coves.social", author)

	base := time.Now().UTC().Truncate(time.Microsecond)
	q2h := seedAcceptedHotPost(t, db, community, author, "q2h", base.Add(-hours(2)), 5)
	q3d := seedAcceptedHotPost(t, db, community, author, "q3d", base.Add(-hours(72)), 10)
	q20d := seedAcceptedHotPost(t, db, community, author, "q20d", base.Add(-hours(480)), 100)

	h := newGetFeedHarness(t, db, 0)

	for _, r := range []struct {
		name      string
		timeframe string
		want      []string
	}{
		{"week leaves out the 20-day post", "week", []string{q3d, q2h}},
		{"month serves the 20-day post first", "month", []string{q20d, q3d, q2h}},
		{"omitted is day: only the 2-hour post", "", []string{q2h}},
	} {
		t.Run(r.name, func(t *testing.T) {
			assertGetFeedMatchesOld(t, h, equivalenceRow{
				name:      r.name,
				surface:   oracleCommunity,
				sort:      "top",
				timeframe: r.timeframe,
				community: community,
				want:      r.want,
			})
		})
	}
}
