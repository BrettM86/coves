//go:build integration

package postgres

import (
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the four community sorts actually order by, against real SQL.
//
// tests/integration/community_e2e_test.go asserted this through the XRPC
// endpoint — a real PDS, a real Jetstream and a provisioned community per sort —
// and could only check that the endpoint answered 200, plus a pairwise
// alphabetical comparison over whatever communities happened to be in the shared
// database. The ordering is a property of one ORDER BY clause per sort, so it is
// tested here against a database seeded to make each sort produce a DIFFERENT
// answer: a test where every ordering agrees would pass with the switch
// statement deleted.
//
// The handler's half — which sort names are accepted, what an unknown one
// answers — is internal/api/handlers/community's list_test.go.

// seedListableCommunity inserts one community with the relationships and creation time
// a sort case needs.
//
// It writes through the repository rather than raw SQL so the row goes in the
// same way the consumer puts it there, and so a schema change breaks this in the
// same place it breaks production.
//
// `subscribers` seeds real subscription rows through the same atomic operation
// the consumer uses. A community profile cannot supply that derived count.
//
// `posts` seeds that many REAL, publicly visible posts — accepted postv2 rows —
// rather than a number in the community's post_count column. The served
// postCount, and the `sort=active` key with it, is a live count over the
// read-path visibility predicate; the stored column is vestigial and is
// deliberately left at 0 here, so a fixture that only wrote the column would
// rank every community equal and this file's sort assertions would stop meaning
// anything.
func seedListableCommunity(t *testing.T, db *sql.DB, repo communities.Repository, name, visibility string, subscribers, posts int, createdAt time.Time) *communities.Community {
	t.Helper()

	community := &communities.Community{
		DID:                    "did:plc:list" + name,
		Handle:                 "c-" + name + ".coves.social",
		Name:                   name,
		DisplayName:            name,
		OwnerDID:               "did:plc:list" + name,
		CreatedByDID:           "did:plc:lister",
		HostedByDID:            "did:web:coves.social",
		Visibility:             visibility,
		AllowExternalDiscovery: true,
		// Deliberately NOT `posts`: nothing serves this column any more, and
		// leaving it at zero is what proves the sort reads the live count.
		PostCount: 0,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
		RecordURI: "at://did:plc:list" + name + "/social.coves.community.profile/self",
	}

	stored, err := repo.Create(context.Background(), community)
	require.NoErrorf(t, err, "seeding community %s", name)
	for i := 0; i < subscribers; i++ {
		_, err = repo.SubscribeWithCount(context.Background(), &communities.Subscription{
			UserDID:           fmt.Sprintf("did:plc:list%s%03d", name, i),
			CommunityDID:      stored.DID,
			SubscribedAt:      createdAt,
			ContentVisibility: 3,
		})
		require.NoErrorf(t, err, "seeding subscriber %d for community %s", i, name)
	}
	seedVisibleCommunityPosts(t, db, stored.DID, name, posts, createdAt)
	stored.PostCount = posts
	return stored
}

// seedVisibleCommunityPosts inserts `count` accepted, publicly visible postv2
// rows for a community: a content row plus the acceptance that pins its exact
// CID, which is what the read-path predicate requires before it will render (or
// count) anything.
func seedVisibleCommunityPosts(t *testing.T, db *sql.DB, communityDID, label string, count int, createdAt time.Time) {
	t.Helper()
	if count == 0 {
		return
	}

	authorDID := "did:plc:lister" + label
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, score, upvote_count, downvote_count)
		SELECT 'at://' || $1 || '/' || $2 || '/' || $3 || i,
		       'bafy' || $3 || i, $3 || i, $1, $4, 'seeded post ' || i, $5, 0, 0, 0
		FROM generate_series(1, $6) AS i
	`, authorDID, posts.PostV2Collection, label, communityDID, createdAt, count)
	require.NoErrorf(t, err, "seeding %d visible posts for %s", count, label)

	_, err = db.ExecContext(ctx, `
		INSERT INTO community_post_admissions (community_did, post_uri, status, accepted_cid, evaluated_cid, created_at, updated_at)
		SELECT $1, p.uri, 'accepted', p.cid, p.cid, NOW(), NOW()
		FROM posts p WHERE p.community_did = $1
		ON CONFLICT (community_did, post_uri) DO NOTHING
	`, communityDID)
	require.NoErrorf(t, err, "accepting the seeded posts for %s", label)
}

// namesOf renders a listing's names in order, which is the whole assertion for
// a sort test and the only readable form for its failure message.
func namesOf(listed []*communities.Community) []string {
	names := make([]string, len(listed))
	for i, c := range listed {
		names[i] = c.Name
	}
	return names
}

// seedSortFixture inserts three communities whose orderings differ under every
// sort, and returns the repository over them.
//
//	name    subscribers  posts  created
//	alpha        1        30    oldest
//	bravo       30         1    middle
//	charlie     10        10    newest
//
// So: popular → bravo, charlie, alpha · active → alpha, charlie, bravo ·
// new → charlie, bravo, alpha · alphabetical → alpha, bravo, charlie. No two
// sorts share an expected answer, which is what makes each assertion mean
// something.
func seedSortFixture(t *testing.T, db *sql.DB) communities.Repository {
	t.Helper()

	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedListableCommunity(t, db, repo, "alpha", "public", 1, 30, base)
	seedListableCommunity(t, db, repo, "bravo", "public", 30, 1, base.Add(time.Hour))
	seedListableCommunity(t, db, repo, "charlie", "public", 10, 10, base.Add(2*time.Hour))
	return repo
}

func TestCommunityRepo_ListSortOrdering(t *testing.T) {
	t.Parallel()

	repo := seedSortFixture(t, testkit.DB(t))

	for _, tc := range []struct {
		sort     string
		expected []string
	}{
		{sort: "popular", expected: []string{"bravo", "charlie", "alpha"}},
		{sort: "active", expected: []string{"alpha", "charlie", "bravo"}},
		{sort: "new", expected: []string{"charlie", "bravo", "alpha"}},
		{sort: "alphabetical", expected: []string{"alpha", "bravo", "charlie"}},
		// An unrecognised sort falls back to popular rather than to whatever
		// order the planner returns. The handler rejects these before they get
		// here, so this is the repository's own belt: a second caller (a feed
		// job, a backfill) must not get an arbitrary order.
		{sort: "", expected: []string{"bravo", "charlie", "alpha"}},
		{sort: "trending", expected: []string{"bravo", "charlie", "alpha"}},
	} {
		t.Run("sort="+tc.sort, func(t *testing.T) {
			listed, err := repo.List(context.Background(), communities.ListCommunitiesRequest{
				Limit: 10,
				Sort:  tc.sort,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.expected, namesOf(listed))
		})
	}
}

func TestCommunityRepo_ListPaginationFollowsTheSort(t *testing.T) {
	t.Parallel()

	// Offset pagination over a sorted listing: page two must continue where page
	// one stopped, in the same order. A sort applied after the LIMIT — or an
	// offset applied to an unsorted query — would still return three distinct
	// communities across two pages, so the assertion is on the ORDER, not the
	// set.
	repo := seedSortFixture(t, testkit.DB(t))
	ctx := context.Background()

	first, err := repo.List(ctx, communities.ListCommunitiesRequest{Limit: 2, Sort: "alphabetical"})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "bravo"}, namesOf(first))

	second, err := repo.List(ctx, communities.ListCommunitiesRequest{Limit: 2, Offset: 2, Sort: "alphabetical"})
	require.NoError(t, err)
	assert.Equal(t, []string{"charlie"}, namesOf(second))

	past, err := repo.List(ctx, communities.ListCommunitiesRequest{Limit: 2, Offset: 10, Sort: "alphabetical"})
	require.NoError(t, err)
	assert.Empty(t, past, "an offset past the end must be an empty page, not an error")
}

func TestCommunityRepo_ListVisibilityFilter(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	public := seedListableCommunity(t, db, repo, "openone", "public", 5, 5, base)
	unlisted := seedListableCommunity(t, db, repo, "quietone", "unlisted", 5, 5, base)

	ctx := context.Background()

	listed, err := repo.List(ctx, communities.ListCommunitiesRequest{Limit: 10, Visibility: "public"})
	require.NoError(t, err)
	assert.Equal(t, []string{public.Name}, namesOf(listed))

	listed, err = repo.List(ctx, communities.ListCommunitiesRequest{Limit: 10, Visibility: "unlisted"})
	require.NoError(t, err)
	assert.Equal(t, []string{unlisted.Name}, namesOf(listed))

	// No filter means no filter: an unfiltered listing includes the unlisted
	// community. Hiding it here instead of at the caller is how an "unlisted"
	// community becomes an invisible one.
	listed, err = repo.List(ctx, communities.ListCommunitiesRequest{Limit: 10, Sort: "alphabetical"})
	require.NoError(t, err)
	assert.Equal(t, []string{public.Name, unlisted.Name}, namesOf(listed))
}
