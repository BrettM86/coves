//go:build integration

package postgres_test

import (
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/discover"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/communityremovalfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// removalSurfaceCommunities is a community the test removes and a live one,
// each with one post old enough for every Discover sort.
type removalSurfaceCommunities struct {
	removedDID, liveDID         string
	removedPostURI, livePostURI string
}

// seedRemovalSurfaceCommunities seeds the two communities into db under the
// given names. Each test runs on its own database clone, so plain names
// cannot collide.
func seedRemovalSurfaceCommunities(t *testing.T, db *sql.DB, removedName, liveName string) removalSurfaceCommunities {
	t.Helper()
	removedDID, err := fixtures.Community(t.Context(), db, removedName, "owner"+removedName)
	require.NoError(t, err)
	liveDID, err := fixtures.Community(t.Context(), db, liveName, "owner"+liveName)
	require.NoError(t, err)
	createdAt := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	return removalSurfaceCommunities{
		removedDID:     removedDID,
		liveDID:        liveDID,
		removedPostURI: fixtures.Post(t, db, removedDID, fixtures.DID("removalsurfaceauthor"), "a post in the removed community", 500, createdAt),
		livePostURI:    fixtures.Post(t, db, liveDID, fixtures.DID("removalsurfaceauthor"), "a post in the live community", 400, createdAt),
	}
}

func communityDIDs(list []*communities.Community) []string {
	dids := make([]string, 0, len(list))
	for _, community := range list {
		dids = append(dids, community.DID)
	}
	return dids
}

func discoverPostURIs(feed []*discover.FeedViewPost) []string {
	uris := make([]string, 0, len(feed))
	for _, item := range feed {
		if item.Post != nil {
			uris = append(uris, item.Post.URI)
		}
	}
	return uris
}

// TestCommunityRemovalHidesCommunityFromList: community.list leaves a removed
// community out and lists it again after restore.
func TestCommunityRemovalHidesCommunityFromList(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	seeded := seedRemovalSurfaceCommunities(t, db, "listremoved", "listlive")
	repository := postgres.NewCommunityRepository(db, credentialciphertest.Fixed(), postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	list := func(t *testing.T) []string {
		t.Helper()
		listed, err := repository.List(t.Context(), communities.ListCommunitiesRequest{Sort: "alphabetical", Limit: 100})
		require.NoError(t, err)
		return communityDIDs(listed)
	}
	require.ElementsMatch(t, []string{seeded.removedDID, seeded.liveDID}, list(t), "both communities are listed before the removal")

	moderator := communityremovalfixture.New(db)
	moderator.Remove(t, seeded.removedDID)
	assert.Equal(t, []string{seeded.liveDID}, list(t), "a removed community is not listed")

	moderator.Restore(t, seeded.removedDID)
	assert.ElementsMatch(t, []string{seeded.removedDID, seeded.liveDID}, list(t), "a restored community is listed again")
}

// TestCommunityRemovalHidesCommunityFromSearch: community.search leaves a
// removed community out of its rows and its total, and finds it again after
// restore. Both community names match the query.
func TestCommunityRemovalHidesCommunityFromSearch(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	seeded := seedRemovalSurfaceCommunities(t, db, "birdwatching-removed", "birdwatching-live")
	repository := postgres.NewCommunityRepository(db, credentialciphertest.Fixed(), postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	search := func(t *testing.T) ([]string, int) {
		t.Helper()
		results, total, err := repository.Search(t.Context(), communities.SearchCommunitiesRequest{Query: "birdwatching", Limit: 10})
		require.NoError(t, err)
		return communityDIDs(results), total
	}
	found, total := search(t)
	require.ElementsMatch(t, []string{seeded.removedDID, seeded.liveDID}, found, "both communities match before the removal")
	require.Equal(t, 2, total)

	moderator := communityremovalfixture.New(db)
	moderator.Remove(t, seeded.removedDID)
	found, total = search(t)
	assert.Equal(t, []string{seeded.liveDID}, found, "a removed community is not a search result")
	assert.Equal(t, 1, total, "a removed community is not counted in the search total")

	moderator.Restore(t, seeded.removedDID)
	found, total = search(t)
	assert.ElementsMatch(t, []string{seeded.removedDID, seeded.liveDID}, found, "a restored community is found again")
	assert.Equal(t, 2, total)
}

// TestCommunityRemovalHidesPostsFromDiscoverNew: Discover new leaves out the
// posts of a removed community and serves them again after restore.
func TestCommunityRemovalHidesPostsFromDiscoverNew(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	seeded := seedRemovalSurfaceCommunities(t, db, "discovernewremoved", "discovernewlive")
	repository := postgres.NewDiscoverRepository(db, "community-removal-discover-new", postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	read := func(t *testing.T) []string {
		t.Helper()
		feed, _, err := repository.GetDiscover(t.Context(), discover.GetDiscoverRequest{Sort: "new", Limit: 50})
		require.NoError(t, err)
		return discoverPostURIs(feed)
	}
	require.ElementsMatch(t, []string{seeded.removedPostURI, seeded.livePostURI}, read(t), "both posts are served before the removal")

	moderator := communityremovalfixture.New(db)
	moderator.Remove(t, seeded.removedDID)
	assert.Equal(t, []string{seeded.livePostURI}, read(t), "a removed community's post is not served by Discover new")

	moderator.Restore(t, seeded.removedDID)
	assert.ElementsMatch(t, []string{seeded.removedPostURI, seeded.livePostURI}, read(t), "a restored community's post is served again")
}

// TestCommunityRemovalHidesPostsFromDiscoverHot: an anonymous Discover hot
// snapshot built before the removal is revalidated per page, so its next
// anonymous page one leaves the removed community's post out. After restore a
// distinct authenticated viewer, whose snapshot is built fresh, is served the
// post again. A snapshot built during the removal may lag a restore by its
// reuse window, which is existing snapshot semantics and not asserted here.
func TestCommunityRemovalHidesPostsFromDiscoverHot(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	seeded := seedRemovalSurfaceCommunities(t, db, "discoverhotremoved", "discoverhotlive")
	const viewerDID = "did:plc:removalhotviewer"
	fixtures.User(t, db, "removalhotviewer.test", viewerDID)
	repository := postgres.NewDiscoverRepository(db, "community-removal-discover-hot", postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	readPageOne := func(t *testing.T, viewer string) []string {
		t.Helper()
		feed, _, err := repository.GetDiscover(t.Context(), discover.GetDiscoverRequest{ViewerDID: viewer, Sort: "hot", Limit: 10})
		require.NoError(t, err)
		return discoverPostURIs(feed)
	}
	beforeRemoval := readPageOne(t, "")
	require.Contains(t, beforeRemoval, seeded.removedPostURI, "the anonymous snapshot holds the post before the removal")
	require.Contains(t, beforeRemoval, seeded.livePostURI)

	moderator := communityremovalfixture.New(db)
	moderator.Remove(t, seeded.removedDID)
	duringRemoval := readPageOne(t, "")
	assert.NotContains(t, duringRemoval, seeded.removedPostURI,
		"the reused anonymous snapshot must not serve a post whose community was removed after it was built")
	assert.Contains(t, duringRemoval, seeded.livePostURI)

	moderator.Restore(t, seeded.removedDID)
	afterRestore := readPageOne(t, viewerDID)
	assert.Contains(t, afterRestore, seeded.removedPostURI, "a fresh snapshot after restore serves the post again")
	assert.Contains(t, afterRestore, seeded.livePostURI)
}
