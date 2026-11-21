//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The discover-hot generator's paths through getFeed scope=all: the hydration
// race refill, a removal between pages, the snapshot build budget and inner
// cursor rejection, each against the getDiscover sort=hot alias where both
// apply.
//
// The fixture is the Discover Hot hydration race fixture
// (discover_hot_hydration_race_test.go): five posts of equal age in three
// communities. Its selection orders are the ones that test and the
// moderation race test (discover_hot_moderation_race_test.go) prove:
// a1 first; with a2 gone, b1, a3, c1 after it.

// discoverHotPathsFixture is the posts by name and their communities.
type discoverHotPathsFixture struct {
	viewer                             string
	communityA, communityB, communityC string
	a1, a2, a3, b1, c1                 string
}

// seedDiscoverHotPathsFixture seeds the fixture created at createdAt. Seeded
// with the same createdAt, two databases serve identical feed items.
func seedDiscoverHotPathsFixture(t *testing.T, db *sql.DB, createdAt time.Time, viewers ...string) discoverHotPathsFixture {
	t.Helper()

	const author = "did:plc:gfdpauthor"
	f := discoverHotPathsFixture{
		viewer:     "did:plc:gfdpviewer",
		communityA: "did:plc:gfdpcommunitya",
		communityB: "did:plc:gfdpcommunityb",
		communityC: "did:plc:gfdpcommunityc",
	}
	for _, did := range append([]string{author, f.viewer}, viewers...) {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	for _, did := range []string{f.communityA, f.communityB, f.communityC} {
		createTestCommunity(t, db, did, "c-"+did[len("did:plc:"):]+".coves.social", author)
	}
	seed := func(communityDID, rkey string, score int) string {
		t.Helper()
		uri := seedFilterablePost(t, db, communityDID, author, rkey, createdAt)
		_, err := db.ExecContext(context.Background(), `
			UPDATE posts SET score = $2, upvote_count = $2, indexed_at = created_at WHERE uri = $1
		`, uri, score)
		require.NoErrorf(t, err, "scoring post %s", rkey)
		return uri
	}
	f.a1 = seed(f.communityA, "gfdpa1", 1_000_000_000)
	f.a2 = seed(f.communityA, "gfdpa2", 1_000_000)
	f.a3 = seed(f.communityA, "gfdpa3", 2_000)
	f.b1 = seed(f.communityB, "gfdpb1", 5_000)
	f.c1 = seed(f.communityC, "gfdpc1", 100)
	return f
}

// onlyDiscoverHotCheckpointRecentCommunities is the recent-community window
// of the database's one stored checkpoint.
func onlyDiscoverHotCheckpointRecentCommunities(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `SELECT recent_communities FROM discover_hot_checkpoints ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var windows [][]string
	for rows.Next() {
		var recent pq.StringArray
		require.NoError(t, rows.Scan(&recent))
		windows = append(windows, []string(recent))
	}
	require.NoError(t, rows.Err())
	require.Lenf(t, windows, 1, "one checkpoint: page 1 stores one, the final page none")
	return windows[0]
}

// requireFeedXRPCError requires an XRPC error response with the given status
// and error name.
func requireFeedXRPCError(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantError, label string) map[string]json.RawMessage {
	t.Helper()

	require.Equalf(t, wantStatus, recorder.Code, "%s: status (body: %s)", label, recorder.Body.String())
	var body map[string]json.RawMessage
	require.NoErrorf(t, json.Unmarshal(recorder.Body.Bytes(), &body), "%s: error body %q", label, recorder.Body.String())
	var name string
	require.NoErrorf(t, json.Unmarshal(body["error"], &name), "%s: error name in %s", label, recorder.Body.String())
	require.Equalf(t, wantError, name, "%s: error name (body: %s)", label, recorder.Body.String())
	return body
}

// discoverHotRaceRun is one path's pages from a hydration race run.
type discoverHotRaceRun struct {
	fixture      discoverHotPathsFixture
	first        feedPage
	firstCursor  string
	second       feedPage
	secondCursor string
	recent       []string
}

// runDiscoverHotHydrationRace seeds a fresh database, builds the viewer's
// snapshot, arms the hydration gate on a2 and requests page 1 (limit 3) on
// path. Once the request waits at the gate, a2 is deleted and the gate opens;
// page 2 follows with the path's own cursor.
func runDiscoverHotHydrationRace(t *testing.T, createdAt time.Time, path string, params func(*getFeedHarness) url.Values, lockClass int) discoverHotRaceRun {
	t.Helper()
	ctx := context.Background()

	db := testkit.DB(t)
	f := seedDiscoverHotPathsFixture(t, db, createdAt)
	h := newGetFeedHarness(t, db, 0, f.viewer)
	h.discoverRepo.discoverHotWorkDeadline = 10 * time.Second

	// Built before the gate, so the page reuses it and only the page's
	// revalidation, after selection, meets the gate.
	_, _, err := h.discoverRepo.loadOrCreateDiscoverHotSnapshot(ctx, f.viewer)
	require.NoError(t, err, "fixture: the viewer's snapshot")
	gate := installDiscoverHotHydrationGate(t, db, f.a2, lockClass, 1)

	query := params(h)
	query.Set("limit", "3")
	label := path + "?" + query.Encode()
	request := h.newRequest(t, path, query, f.viewer)
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() { responses <- h.serve(request) }()

	var early *httptest.ResponseRecorder
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		select {
		case early = <-responses:
			return true, nil
		default:
		}
		return discoverHotAdvisoryLockHasWaiter(db, gate.holderPID)
	}, testkit.WithDescription("%s page 1 waiting at the hydration gate", label))
	if early != nil {
		decodeFeedPage(t, early, label+" page 1")
		require.FailNowf(t, "page 1 never reached the hydration gate", "%s served before revalidating a2", label)
	}

	mutation, err := db.ExecContext(ctx, `UPDATE `+discoverHotHydrationStoredPosts+` SET deleted_at = clock_timestamp() WHERE uri = $1`, f.a2)
	require.NoError(t, err)
	mutated, err := mutation.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, mutated)
	gate.release(t)

	run := discoverHotRaceRun{fixture: f}
	run.first, run.firstCursor = decodeFeedPage(t, <-responses, label+" page 1")
	require.NotEmptyf(t, run.firstCursor, "%s page 1 cursor", label)
	query.Set("cursor", run.firstCursor)
	run.second, run.secondCursor = h.getFeedPage(t, path, query, f.viewer)
	run.recent = onlyDiscoverHotCheckpointRecentCommunities(t, db)
	return run
}

// B9: a selected post deleted between selection and hydration is refilled on
// page 1 by getFeed, and by the getDiscover alias exactly as by getFeed, each
// on its own database with its own snapshot and gate, and the stored window
// counts only emitted posts.
func TestGetFeedDiscoverHot_HydrationRaceRefillsLikeGetDiscover(t *testing.T) {
	t.Parallel()

	createdAt := time.Now().Add(-4 * time.Hour).Truncate(time.Second)

	assertRefilled := func(t *testing.T, name string, run discoverHotRaceRun) {
		t.Helper()
		f := run.fixture
		assert.Equalf(t, []string{f.a1, f.b1, f.a3}, feedPageURIs(t, run.first.Feed),
			"%s page 1: a2 dropped and refilled to the limit", name)
		assert.Equalf(t, []string{f.c1}, feedPageURIs(t, run.second.Feed), "%s page 2", name)
		assert.Emptyf(t, run.secondCursor, "%s page 2 is the last", name)
		assert.Equalf(t, []string{f.communityA, f.communityB, f.communityA}, run.recent,
			"%s: the checkpoint window is the emitted posts' communities", name)
	}

	viaFeed := runDiscoverHotHydrationRace(t, createdAt, getFeedPath, func(h *getFeedHarness) url.Values {
		return url.Values{"scope": {"all"}, "feed": {h.discoverHotFeedURI()}}
	}, 1_126_644_061)
	assertRefilled(t, "getFeed", viaFeed)

	t.Run("alias getDiscover sort=hot", func(t *testing.T) {
		viaAlias := runDiscoverHotHydrationRace(t, createdAt, getDiscoverPath, func(*getFeedHarness) url.Values {
			return url.Values{"sort": {"hot"}}
		}, 1_126_644_062)
		assertRefilled(t, "alias getDiscover", viaAlias)
		assert.JSONEq(t, string(viaFeed.first.Feed), string(viaAlias.first.Feed), "page 1 items")
		assert.JSONEq(t, string(viaFeed.second.Feed), string(viaAlias.second.Feed), "page 2 items")
	})
}

// B10: a post removed by a moderator after page 1 is served on neither
// getFeed's nor the getDiscover alias's page 2, and the page 2s are
// identical.
func TestGetFeedDiscoverHot_RemovalBetweenPagesMatchesGetDiscover(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := seedDiscoverHotPathsFixture(t, db, time.Now().Add(-4*time.Hour).Truncate(time.Second))
	h := newGetFeedHarness(t, db, 0, f.viewer)

	feedParams := url.Values{"scope": {"all"}, "feed": {h.discoverHotFeedURI()}, "limit": {"1"}}
	aliasParams := url.Values{"sort": {"hot"}, "limit": {"1"}}

	newFirst, newCursor := h.getFeedPage(t, getFeedPath, feedParams, f.viewer)
	require.Equal(t, []string{f.a1}, feedPageURIs(t, newFirst.Feed), "getFeed page 1")
	require.NotEmpty(t, newCursor)
	// The alias's page 1 is read before the removal too; it is decoded in its
	// subtest.
	aliasFirst := h.serve(h.newRequest(t, getDiscoverPath, aliasParams, f.viewer))

	insertDiscoverHotRemoval(t, db, f.a2)

	feedParams.Set("limit", "2")
	feedParams.Set("cursor", newCursor)
	newSecond, newNext := h.getFeedPage(t, getFeedPath, feedParams, f.viewer)
	assert.Equal(t, []string{f.b1, f.a3}, feedPageURIs(t, newSecond.Feed), "getFeed page 2 without the removed a2")
	assert.NotEmpty(t, newNext, "getFeed page 2 cursor (c1 remains)")

	t.Run("alias getDiscover sort=hot", func(t *testing.T) {
		oldFirst, oldCursor := decodeFeedPage(t, aliasFirst, "alias getDiscover page 1")
		require.Equal(t, []string{f.a1}, feedPageURIs(t, oldFirst.Feed), "alias getDiscover page 1")
		require.NotEmpty(t, oldCursor)

		aliasParams.Set("limit", "2")
		aliasParams.Set("cursor", oldCursor)
		oldSecond, oldNext := h.getFeedPage(t, getDiscoverPath, aliasParams, f.viewer)
		assert.Equal(t, []string{f.b1, f.a3}, feedPageURIs(t, oldSecond.Feed), "alias getDiscover page 2 without the removed a2")
		assert.JSONEq(t, string(newSecond.Feed), string(oldSecond.Feed), "page 2 items")
		assert.NotEmpty(t, oldNext, "alias getDiscover page 2 cursor (c1 remains)")
	})
}

// B11: with the snapshot build budget spent, getFeed discover-hot answers 503
// DiscoverUnavailable with Retry-After and no feed.
func TestGetFeedDiscoverHot_BuildBudgetExhaustedIsUnavailable(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := seedDiscoverHotPathsFixture(t, db, time.Now().Add(-4*time.Hour).Truncate(time.Second))
	h := newGetFeedHarness(t, db, 0, f.viewer)
	h.discoverRepo.discoverHotSnapshotBuildsPerMinuteLimit = 1

	spent, _ := h.getFeedPage(t, getFeedPath, url.Values{"scope": {"all"}, "feed": {h.discoverHotFeedURI()}, "limit": {"1"}}, f.viewer)
	require.Equal(t, []string{f.a1}, feedPageURIs(t, spent.Feed), "fixture: the viewer's snapshot spends the one build")

	// Anonymous reads have their own snapshot scope, so this needs a build.
	recorder := h.serve(h.newRequest(t, getFeedPath, url.Values{"scope": {"all"}, "feed": {h.discoverHotFeedURI()}}, ""))
	body := requireFeedXRPCError(t, recorder, http.StatusServiceUnavailable, "DiscoverUnavailable", "getFeed discover-hot over budget")
	assert.Equal(t, "30", recorder.Header().Get("Retry-After"))
	assert.NotContains(t, body, "feed")
}

// B12: inside a validly signed getFeed envelope, an inner cursor that is
// expired, another viewer's, or of version discover-hot-v0 is InvalidCursor;
// the unmodified inner cursor re-signed is served.
func TestGetFeedDiscoverHot_RejectsInvalidInnerCursors(t *testing.T) {
	t.Parallel()

	const (
		expiredViewer = "did:plc:gfdpexpired"
		otherViewer   = "did:plc:gfdpother"
	)
	db := testkit.DB(t)
	f := seedDiscoverHotPathsFixture(t, db, time.Now().Add(-4*time.Hour).Truncate(time.Second), expiredViewer, otherViewer)
	h := newGetFeedHarness(t, db, 0, f.viewer, expiredViewer, otherViewer)
	signer := NewCursorSigner(getFeedEquivalenceCursorSecret)

	firstPage := url.Values{"scope": {"all"}, "feed": {h.discoverHotFeedURI()}, "limit": {"1"}}
	split := func(envelope string) (binding, inner string) {
		t.Helper()
		payload, err := signer.Verify(envelope)
		require.NoError(t, err, "fixture: a getFeed envelope verifies under the harness signer")
		binding, inner, found := strings.Cut(payload, "::")
		require.True(t, found, "fixture: envelope payload is binding::inner")
		return binding, inner
	}
	resign := func(binding, inner string) string { return signer.Sign(binding + "::" + inner) }

	// The control's page 1: on the stubs this is where getFeed refuses scope
	// all (400 UnsupportedFeedScope).
	page, envelope := h.getFeedPage(t, getFeedPath, firstPage, f.viewer)
	require.Equal(t, []string{f.a1}, feedPageURIs(t, page.Feed), "getFeed page 1")
	require.NotEmpty(t, envelope)
	binding, inner := split(envelope)
	snapshotID, checkpointID, err := h.discoverRepo.parseDiscoverHotCursor(inner, f.viewer)
	require.NoError(t, err, "fixture: the envelope carries a discover-hot-v1 inner cursor")
	oldVersion := h.discoverRepo.signCursorPayload(strings.Join([]string{
		"discover-hot-v0", strconv.FormatInt(snapshotID, 10), strconv.FormatInt(checkpointID, 10), discoverHotViewerScope(f.viewer),
	}, "::"))

	otherPage, err := h.discoverRepo.HydratedPage(context.Background(), feedgen.SkeletonRequest{
		ViewerDID: otherViewer, Scope: feedgen.Scope{Name: feedgen.ScopeAll}, Limit: 1,
	})
	require.NoError(t, err)
	otherInner := otherPage.Cursor
	require.NotEmpty(t, otherInner, "fixture: another viewer's inner cursor")

	_, expiredEnvelope := h.getFeedPage(t, getFeedPath, firstPage, expiredViewer)
	require.NotEmpty(t, expiredEnvelope)
	expiredBinding, expiredInner := split(expiredEnvelope)
	result, err := db.ExecContext(context.Background(), `
		UPDATE discover_hot_snapshots SET expires_at = NOW() - INTERVAL '1 second' WHERE viewer_scope = $1
	`, discoverHotViewerScope(expiredViewer))
	require.NoError(t, err)
	expired, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, expired, "fixture: exactly the expired viewer's snapshot")

	nextPage := func(cursor string) url.Values {
		return url.Values{"scope": {"all"}, "feed": {h.discoverHotFeedURI()}, "limit": {"10"}, "cursor": {cursor}}
	}
	for _, row := range []struct {
		name   string
		viewer string
		cursor string
	}{
		{"expired snapshot", expiredViewer, resign(expiredBinding, expiredInner)},
		{"another viewer's inner cursor", f.viewer, resign(binding, otherInner)},
		{"inner cursor version discover-hot-v0", f.viewer, resign(binding, oldVersion)},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := h.serve(h.newRequest(t, getFeedPath, nextPage(row.cursor), row.viewer))
			requireFeedXRPCError(t, recorder, http.StatusBadRequest, "InvalidCursor", row.name)
		})
	}

	t.Run("control: the unmodified inner cursor re-signed", func(t *testing.T) {
		rest, next := h.getFeedPage(t, getFeedPath, nextPage(resign(binding, inner)), f.viewer)
		assert.ElementsMatch(t, []string{f.a2, f.a3, f.b1, f.c1}, feedPageURIs(t, rest.Feed))
		assert.Empty(t, next)
	})
}
