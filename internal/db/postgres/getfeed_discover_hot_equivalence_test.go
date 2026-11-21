//go:build integration

package postgres

import (
	"database/sql"
	"net/url"
	"testing"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getFeed at scope all must serve exactly what getDiscover served: the
// registry's new and top feeds against the old getDiscover sort=new and
// sort=top (the oracle, feed_oracle_test.go), and getDiscover, now an alias,
// must serve getFeed's pages: discover-hot for sort=hot and for no sort, new
// and top for sort=new and sort=top. The harness, paging and comparison are
// the hot equivalence test's (getfeed_hot_equivalence_test.go); getDiscover
// and getFeed items are both {post} objects, so whole feed items are
// compared.
//
// Discover Hot stays in production as the discover-hot generator, so it has
// no oracle: its order (hosted bonus keyed by the cursor secret, community
// diversity) is taken from getFeed; the served SET is written out by hand.
// New and top orders are written out by hand: new is created_at DESC, top is
// score DESC inside the timeframe window.

// discoverHotFeedURI is the registry's at-uri for the built-in discover-hot
// feed.
func (h *getFeedHarness) discoverHotFeedURI() string { return h.registry.URI("discover-hot") }

// discoverAllFixture is the scope-all fixture: a signed-in viewer who blocks
// one author and one community, a hosted community, an instance-removed
// community with posts, and posts no reader may be served.
type discoverAllFixture struct {
	viewer string
	// Visible to everyone, by name.
	b1, a1, c1, h1, a2, b2, h2 string
	// Visible anonymously, hidden from the viewer: a blocked author's post and
	// a post in a blocked community (aggregate surface).
	blockedAuthor, blockedCommunity string
	// Served to nobody: another author's pending post, a moderator-removed
	// post and the instance-removed community's two posts. Each outscores
	// every visible post, so it would lead every top read if it leaked.
	pending, removed, removedCommunityRecent, removedCommunityOld string
	// voteURI is the viewer's upvote on a1.
	voteURI string
}

func (f discoverAllFixture) hiddenFromEveryone() []string {
	return []string{f.pending, f.removed, f.removedCommunityRecent, f.removedCommunityOld}
}

func (f discoverAllFixture) hiddenFromViewer() []string {
	return append([]string{f.blockedAuthor, f.blockedCommunity}, f.hiddenFromEveryone()...)
}

func seedDiscoverAllFixture(t *testing.T, db *sql.DB) discoverAllFixture {
	t.Helper()

	const (
		viewer  = "did:plc:gfdaviewer"
		author  = "did:plc:gfdaauthor"
		other   = "did:plc:gfdaother"
		blocked = "did:plc:gfdablocked"

		communityA       = "did:plc:gfdacommunitya"
		communityB       = "did:plc:gfdacommunityb"
		communityC       = "did:plc:gfdacommunityc"
		hosted           = "did:plc:gfdahosted"
		blockedCommunity = "did:plc:gfdablockedc"
		removedCommunity = "did:plc:gfdaremovedc"
	)
	for _, did := range []string{viewer, author, other, blocked} {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	for _, did := range []string{communityA, communityB, communityC, hosted, blockedCommunity, removedCommunity} {
		createTestCommunity(t, db, did, "c-"+did[len("did:plc:"):]+".coves.social", author)
	}
	// Hosted is credential presence (admission_queue_repo_test hostedCommunity).
	_, err := db.ExecContext(t.Context(),
		`UPDATE communities SET pds_refresh_token_encrypted = $2 WHERE did = $1`, hosted, []byte("a stored refresh token"))
	require.NoError(t, err)
	insertUserBlock(t, db, viewer, blocked)
	insertCommunityBlock(t, db, viewer, blockedCommunity)

	base := time.Now().UTC().Truncate(time.Microsecond)
	at := func(age time.Duration) time.Time { return base.Add(-age) }

	f := discoverAllFixture{viewer: viewer}
	f.b1 = seedAcceptedHotPost(t, db, communityB, author, "b1", at(hours(1)), 0)
	f.a1 = seedAcceptedHotPost(t, db, communityA, author, "a1", at(hours(2)), 10)
	f.c1 = seedAcceptedHotPost(t, db, communityC, author, "c1", at(hours(3)), 7)
	f.h1 = seedAcceptedHotPost(t, db, hosted, author, "h1", at(hours(4)), 5)
	f.a2 = seedAcceptedHotPost(t, db, communityA, author, "a2", at(hours(5)), 3)
	f.b2 = seedAcceptedHotPost(t, db, communityB, author, "b2", at(hours(30)), 40)
	f.h2 = seedAcceptedHotPost(t, db, hosted, author, "h2", at(hours(50)), 20)

	f.blockedAuthor = seedAcceptedHotPost(t, db, communityA, blocked, "xa", at(hours(6)), 15)
	f.blockedCommunity = seedAcceptedHotPost(t, db, blockedCommunity, author, "xc", at(hours(7)), 12)

	f.pending = seedHotBoundPost(t, db, communityA, other, "pending", at(90*time.Minute), 1000)
	seedHotBoundPendingAdmission(t, db, communityA, f.pending)
	f.removed = seedAcceptedHotPost(t, db, communityB, author, "removed", at(105*time.Minute), 1000)
	insertDiscoverHotRemoval(t, db, f.removed)
	f.removedCommunityRecent = seedAcceptedHotPost(t, db, removedCommunity, author, "r1", at(150*time.Minute), 1000)
	f.removedCommunityOld = seedAcceptedHotPost(t, db, removedCommunity, author, "r2", at(hours(40)), 500)
	removeCommunityAtInstance(t, db, removedCommunity)

	f.voteURI = insertViewerUpvote(t, db, viewer, f.a1, "gfdaa1")
	return f
}

// B7 (acceptance): getFeed scope=all with the registry's discover-hot uri
// serves the fixture's set, signed in and anonymous, and never a hidden post;
// with feed omitted it serves the same pages; and the getDiscover alias with
// sort=hot and with no sort serves getFeed's pages.
func TestGetFeedDiscoverHot_AllMatchesGetDiscoverHot(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := seedDiscoverAllFixture(t, db)
	h := newGetFeedHarness(t, db, 0, f.viewer)

	rows := []struct {
		name   string
		viewer string
		want   []string
		hidden []string
	}{
		{
			name:   "signed-in viewer blocking an author and a community",
			viewer: f.viewer,
			want:   []string{f.b1, f.a1, f.c1, f.h1, f.a2, f.b2, f.h2},
			hidden: f.hiddenFromViewer(),
		},
		{
			name:   "anonymous",
			viewer: "",
			want:   []string{f.b1, f.a1, f.c1, f.h1, f.a2, f.blockedAuthor, f.blockedCommunity, f.b2, f.h2},
			hidden: f.hiddenFromEveryone(),
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			explicit := h.readFeedPages(t, getFeedPath, url.Values{"scope": {"all"}, "feed": {h.discoverHotFeedURI()}}, row.viewer)
			served := pagedURIs(t, explicit)
			require.ElementsMatch(t, row.want, served, "getFeed all feed=discover-hot: fixture set")
			require.GreaterOrEqual(t, len(explicit), 4, "getFeed all feed=discover-hot: at least four pages at limit 2")
			assertNoneServed(t, served, row.hidden, "getFeed all feed=discover-hot")
			if row.viewer != "" {
				require.Contains(t, string(joinedFeeds(explicit)), f.voteURI, "getFeed all feed=discover-hot: the viewer's vote on a1")
			}

			omitted := h.readFeedPages(t, getFeedPath, url.Values{"scope": {"all"}}, row.viewer)
			assertSameFeedPages(t, explicit, omitted, "getFeed all feed omitted")
			assertNoneServed(t, pagedURIs(t, omitted), row.hidden, "getFeed all feed omitted")

			for _, alias := range []struct {
				name   string
				params url.Values
			}{
				{"alias getDiscover sort=hot", url.Values{"sort": {"hot"}}},
				{"alias getDiscover no sort", url.Values{}},
			} {
				t.Run(alias.name, func(t *testing.T) {
					pages := h.readFeedPages(t, getDiscoverPath, alias.params, row.viewer)
					assertAliasMatchesFeed(t, feedPagesURIs(t, explicit), explicit, pages, alias.name)
					assertNoneServed(t, pagedURIs(t, pages), row.hidden, alias.name)
				})
			}
		})
	}
}

// joinedFeeds concatenates every page's raw feed JSON, for presence checks.
func joinedFeeds(pages []feedPage) []byte {
	var joined []byte
	for _, page := range pages {
		joined = append(joined, page.Feed...)
	}
	return joined
}

// B8: getFeed scope=all with the new and top uris serves the old getDiscover
// sort=new and sort=top (timeframes day and all) page for page, as does the
// getDiscover alias, signed in and anonymous; the instance-removed community's
// posts are served by none of them.
func TestGetFeedNewTop_AllMatchesGetDiscover(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := seedDiscoverAllFixture(t, db)
	h := newGetFeedHarness(t, db, 0, f.viewer)

	rows := []struct {
		name      string
		sort      string
		timeframe string
		viewer    string
		want      []string
	}{
		{"new signed in", "new", "", f.viewer, []string{f.b1, f.a1, f.c1, f.h1, f.a2, f.b2, f.h2}},
		{"top day signed in", "top", "day", f.viewer, []string{f.a1, f.c1, f.h1, f.a2, f.b1}},
		{"top all signed in", "top", "all", f.viewer, []string{f.b2, f.h2, f.a1, f.c1, f.h1, f.a2, f.b1}},
		{
			"new anonymous", "new", "", "",
			[]string{f.b1, f.a1, f.c1, f.h1, f.a2, f.blockedAuthor, f.blockedCommunity, f.b2, f.h2},
		},
		{
			"top day anonymous", "top", "day", "",
			[]string{f.blockedAuthor, f.blockedCommunity, f.a1, f.c1, f.h1, f.a2, f.b1},
		},
		{
			"top all anonymous", "top", "all", "",
			[]string{f.b2, f.h2, f.blockedAuthor, f.blockedCommunity, f.a1, f.c1, f.h1, f.a2, f.b1},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			hidden := f.hiddenFromEveryone()
			if r.viewer != "" {
				hidden = f.hiddenFromViewer()
			}
			assertGetFeedMatchesOld(t, h, equivalenceRow{
				name:      r.name,
				surface:   oracleDiscover,
				sort:      r.sort,
				timeframe: r.timeframe,
				viewer:    r.viewer,
				want:      r.want,
				hidden:    hidden,
			})
		})
	}
}

// B8b: the hot candidate bound does not apply to new and top at scope all.
// With it lowered to 3 on the generators and five eligible posts, the oldest
// of them the highest scored, both serve every post, through the generator
// skeleton and through getFeed.
func TestGetFeedNewTop_AllHasNoCandidateBound(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const (
		author     = "did:plc:gfdbauthor"
		communityP = "did:plc:gfdbcommunityp"
		communityQ = "did:plc:gfdbcommunityq"
	)
	createTestUser(t, db, author[len("did:plc:"):]+".test", author)
	createTestCommunity(t, db, communityP, "c-gfdbcommunityp.coves.social", author)
	createTestCommunity(t, db, communityQ, "c-gfdbcommunityq.coves.social", author)

	base := time.Now().UTC().Truncate(time.Microsecond)
	n1 := seedAcceptedHotPost(t, db, communityQ, author, "n1", base.Add(-10*time.Minute), 1)
	n2 := seedAcceptedHotPost(t, db, communityP, author, "n2", base.Add(-hours(1)), 2)
	n3 := seedAcceptedHotPost(t, db, communityQ, author, "n3", base.Add(-hours(2)), 0)
	n4 := seedAcceptedHotPost(t, db, communityP, author, "n4", base.Add(-hours(3)), 5)
	// Past any bound of 3 newest, and the highest scored.
	n5 := seedAcceptedHotPost(t, db, communityP, author, "n5", base.Add(-hours(24*30)), 100)

	h := newGetFeedHarness(t, db, 3)
	require.Equal(t, 3, h.newGenerator.hotCandidateLimit, "fixture: new generator bound lowered")
	require.Equal(t, 3, h.topGenerator.hotCandidateLimit, "fixture: top generator bound lowered")

	wantNew := []string{n1, n2, n3, n4, n5}
	wantTopAll := []string{n5, n4, n2, n1, n3}
	allScope := feedgen.Scope{Name: feedgen.ScopeAll}

	t.Run("new skeleton", func(t *testing.T) {
		assert.Equal(t, wantNew, readSkeletonURIs(t, h.newGenerator, feedgen.SkeletonRequest{Scope: allScope}))
	})
	t.Run("top all skeleton", func(t *testing.T) {
		assert.Equal(t, wantTopAll, readSkeletonURIs(t, h.topGenerator, feedgen.SkeletonRequest{Scope: allScope, Timeframe: "all"}))
	})
	t.Run("getFeed new", func(t *testing.T) {
		pages := h.readFeedPages(t, getFeedPath, url.Values{"scope": {"all"}, "feed": {h.newFeedURI()}}, "")
		assert.Equal(t, wantNew, pagedURIs(t, pages))
	})
	t.Run("getFeed top all", func(t *testing.T) {
		pages := h.readFeedPages(t, getFeedPath, url.Values{"scope": {"all"}, "feed": {h.topFeedURI()}, "timeframe": {"all"}}, "")
		assert.Equal(t, wantTopAll, pagedURIs(t, pages))
	})
}
