//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"
)

// The community repository's write-after-create paths and its search, all of
// which were at zero coverage: Update, UpdateCredentials, Delete and Search.
//
// Update is the one that matters most. It is what the firehose consumer runs
// for every social.coves.community.profile commit after the first, and it is
// the only place in the repository with a COALESCE in it — pds_url is preserved
// when the caller passes an empty string, because most callers do not know the
// community's PDS host and the consumer that does must not have its value
// erased by the next handler that does not. That one clause is what BridgeTrust
// reads to decide whether a bridged vote is trustworthy, so losing it is a
// federation bug rather than a missing field.
//
// Search's coverage gap is the more interesting one for a different reason: it
// runs two queries with different WHERE clauses and returns the count from the
// first alongside the page from the second. See
// TestCommunityRepo_SearchTotalCountsRowsTheResultsExclude.

// seededUpdatedAt is deliberately in the past. Create writes created_at and
// updated_at from the struct, so seeding a fixed old timestamp is what lets the
// updated_at assertion below be STRICTLY after rather than after-or-equal —
// which in turn is what makes deleting `updated_at = NOW()` from the UPDATE
// fail a test instead of passing one.
var seededUpdatedAt = time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)

// updatableCommunity seeds a community with every optional field populated, so
// an update can be shown to change what it means to and nothing else.
func updatableCommunity(t *testing.T) (communities.Repository, *communities.Community) {
	t.Helper()
	repo := NewCommunityRepository(testkit.DB(t))
	id := testkit.UniqueID(t)

	community, err := repo.Create(context.Background(), &communities.Community{
		DID:                    "did:plc:upd" + id,
		Handle:                 "c-updatable-" + id + ".coves.social",
		Name:                   "updatable-" + id,
		DisplayName:            "Before",
		Description:            "the original description",
		AvatarCID:              "bafyreioriginalavatar",
		BannerCID:              "bafyreioriginalbanner",
		OwnerDID:               "did:web:coves.social",
		CreatedByDID:           "did:plc:updcreator",
		HostedByDID:            "did:web:coves.social",
		Visibility:             "public",
		ModerationType:         "moderator",
		ContentWarnings:        []string{"none"},
		AllowExternalDiscovery: true,
		PDSURL:                 "https://pds.coves.social",
		RecordURI:              "at://did:plc:upd" + id + "/social.coves.community.profile/self",
		RecordCID:              "bafyreioriginalrecord",
		CreatedAt:              seededUpdatedAt,
		UpdatedAt:              seededUpdatedAt,
	})
	require.NoError(t, err)
	return repo, community
}

func TestCommunityRepo_Update(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("replaces the profile fields", func(t *testing.T) {
		t.Parallel()
		repo, community := updatableCommunity(t)

		community.DisplayName = "After"
		community.Description = "the new description"
		community.AvatarCID = "bafyreinewavatar"
		community.BannerCID = "bafyreinewbanner"
		community.Visibility = "unlisted"
		community.ModerationType = "sortition"
		community.ContentWarnings = []string{"spoilers", "politics"}
		community.AllowExternalDiscovery = false
		community.RecordCID = "bafyreinewrecord"

		_, err := repo.Update(ctx, community)
		require.NoError(t, err)

		got, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		assert.Equal(t, "After", got.DisplayName)
		assert.Equal(t, "the new description", got.Description)
		assert.Equal(t, "bafyreinewavatar", got.AvatarCID)
		assert.Equal(t, "bafyreinewbanner", got.BannerCID)
		assert.Equal(t, "unlisted", got.Visibility)
		assert.Equal(t, "sortition", got.ModerationType)
		assert.Equal(t, []string{"spoilers", "politics"}, got.ContentWarnings)
		assert.False(t, got.AllowExternalDiscovery,
			"a community opting out of external discovery is a privacy decision; an update that "+
				"cannot turn the flag off cannot honour it")
		assert.Equal(t, "bafyreinewrecord", got.RecordCID)
	})

	t.Run("leaves the identity columns alone", func(t *testing.T) {
		t.Parallel()
		repo, community := updatableCommunity(t)
		originalHandle := community.Handle
		originalName := community.Name

		// The UPDATE does not list handle, name, did, owner_did, created_by_did
		// or hosted_by_did, so setting them on the struct must have no effect. A
		// profile record that renamed a community out from under its own handle
		// would leave every existing link pointing at nothing.
		community.Handle = "c-hijacked.coves.social"
		community.Name = "hijacked"
		community.CreatedByDID = "did:plc:someoneelse00000000"
		community.HostedByDID = "did:web:elsewhere.invalid"
		_, err := repo.Update(ctx, community)
		require.NoError(t, err)

		got, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		assert.Equal(t, originalHandle, got.Handle, "an update renamed the community's handle")
		assert.Equal(t, originalName, got.Name)
		assert.Equal(t, "did:plc:updcreator", got.CreatedByDID,
			"createdBy is set once at creation; an update that rewrote it would let a later profile "+
				"commit reassign authorship")
		assert.Equal(t, "did:web:coves.social", got.HostedByDID)
	})

	t.Run("moves the updated_at clock", func(t *testing.T) {
		t.Parallel()
		repo, community := updatableCommunity(t)
		before, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		require.WithinDuration(t, seededUpdatedAt, before.UpdatedAt, time.Second,
			"the fixture must start in the past for the comparison below to mean anything")

		community.DisplayName = "Touched"
		updated, err := repo.Update(ctx, community)
		require.NoError(t, err)

		// Strictly after, not after-or-equal: `updated_at = NOW()` is what makes
		// a community's freshness usable for cache validation and for ordering
		// "recently active" listings, and an UPDATE that forgot the clause would
		// satisfy any weaker comparison.
		assert.True(t, updated.UpdatedAt.After(before.UpdatedAt),
			"the update did not move updated_at: returned %s, row started at %s",
			updated.UpdatedAt, before.UpdatedAt)
		assert.WithinDuration(t, time.Now(), updated.UpdatedAt, time.Minute,
			"the RETURNING clause must hand back the new server-side timestamp, so a caller can serve "+
				"it without a re-read")

		stored, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		assert.WithinDuration(t, updated.UpdatedAt, stored.UpdatedAt, time.Second,
			"the returned timestamp must be the one that was stored, not one computed in Go")
		assert.WithinDuration(t, seededUpdatedAt, stored.CreatedAt, time.Second,
			"created_at is not in the UPDATE and must not move")
	})

	// The COALESCE(NULLIF($13, ''), pds_url) clause, stated as the two cases it
	// distinguishes.
	t.Run("keeps the stored PDS host when the caller does not carry one", func(t *testing.T) {
		t.Parallel()
		repo, community := updatableCommunity(t)

		community.PDSURL = ""
		community.DisplayName = "No PDS carried"
		_, err := repo.Update(ctx, community)
		require.NoError(t, err)

		got, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		assert.Equal(t, "https://pds.coves.social", got.PDSURL,
			"most Update callers do not know the community's PDS host and pass the zero value. "+
				"Overwriting with it would blank a column BridgeTrust reads to decide whether a "+
				"bridged vote has provenance — and nothing would report an error")
	})

	t.Run("sets the PDS host when the caller does carry one", func(t *testing.T) {
		t.Parallel()
		repo, community := updatableCommunity(t)

		community.PDSURL = "https://pds.elsewhere.invalid"
		_, err := repo.Update(ctx, community)
		require.NoError(t, err)

		got, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		assert.Equal(t, "https://pds.elsewhere.invalid", got.PDSURL,
			"the consumer that DID resolve a host has to be able to record it, or the column never "+
				"gets populated at all")
	})

	t.Run("reports a community that does not exist", func(t *testing.T) {
		t.Parallel()
		repo, _ := updatableCommunity(t)

		_, err := repo.Update(ctx, &communities.Community{
			DID: "did:plc:nosuchcommunity0000", Visibility: "public",
		})
		require.ErrorIs(t, err, communities.ErrCommunityNotFound,
			"an update that matched nothing must not read as success: the firehose consumer would "+
				"acknowledge a profile commit it never applied")
	})

	t.Run("clears the description facets when there are none", func(t *testing.T) {
		t.Parallel()
		repo, community := updatableCommunity(t)

		community.DescriptionFacets = []byte(`[{"index":{"byteStart":0,"byteEnd":3}}]`)
		_, err := repo.Update(ctx, community)
		require.NoError(t, err)
		withFacets, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		require.NotEmpty(t, withFacets.DescriptionFacets)

		// A profile record whose description no longer has links must not keep
		// serving the old facet offsets: they index into a string that changed,
		// so the client would render a link over whatever text now sits there.
		community.DescriptionFacets = nil
		_, err = repo.Update(ctx, community)
		require.NoError(t, err)
		got, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		assert.Empty(t, got.DescriptionFacets)
	})
}

func TestCommunityRepo_UpdateCredentialsReportsAnAbsentCommunity(t *testing.T) {
	t.Parallel()

	// The happy path — that both tokens are encrypted and round-trip — is
	// community_repo_credentials_test.go's. What was uncovered is the branch
	// that matters to the token-refresh loop: a refresh for a community that is
	// no longer indexed must fail loudly, because the refresh token it just
	// spent is single-use and the old one is already revoked.
	repo := NewCommunityRepository(testkit.DB(t))

	err := repo.UpdateCredentials(context.Background(), "did:plc:nosuchcommunity0000", "access", "refresh")
	require.ErrorIs(t, err, communities.ErrCommunityNotFound,
		"a silent no-op here would strand the community: the PDS has rotated the refresh token, we "+
			"stored neither half, and every later write fails with an expired session nobody can explain")
}

func TestCommunityRepo_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("removes the community", func(t *testing.T) {
		t.Parallel()
		repo, community := updatableCommunity(t)

		require.NoError(t, repo.Delete(ctx, community.DID))

		_, err := repo.GetByDID(ctx, community.DID)
		assert.ErrorIs(t, err, communities.ErrCommunityNotFound)
		_, err = repo.GetByHandle(ctx, community.Handle)
		assert.ErrorIs(t, err, communities.ErrCommunityNotFound,
			"the handle index must go with the row, or the handle stays taken and the community can "+
				"never be recreated")
	})

	// Deleting is NOT idempotent here, which is the opposite of the user
	// repository's Delete and worth knowing before writing a consumer against
	// it: a duplicate community-profile delete on the firehose surfaces as an
	// error rather than as a no-op.
	t.Run("reports a second delete as not found", func(t *testing.T) {
		t.Parallel()
		repo, community := updatableCommunity(t)
		require.NoError(t, repo.Delete(ctx, community.DID))

		err := repo.Delete(ctx, community.DID)
		require.ErrorIs(t, err, communities.ErrCommunityNotFound,
			"IF THIS FAILED, Delete became idempotent. That is a defensible change for an "+
				"at-least-once firehose; assert the new behaviour and check the consumer's "+
				"dead-letter handling still distinguishes a real failure")
	})

	t.Run("leaves other communities alone", func(t *testing.T) {
		t.Parallel()
		repo, doomed := updatableCommunity(t)
		id := testkit.UniqueID(t)
		survivor, err := repo.Create(ctx, &communities.Community{
			DID: "did:plc:survivor" + id, Handle: "c-survivor-" + id + ".coves.social",
			Name: "survivor-" + id, OwnerDID: "did:web:coves.social",
			CreatedByDID: "did:plc:updcreator", HostedByDID: "did:web:coves.social",
			Visibility: "public", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
		require.NoError(t, err)

		require.NoError(t, repo.Delete(ctx, doomed.DID))

		_, err = repo.GetByDID(ctx, survivor.DID)
		assert.NoError(t, err)
	})
}

func TestCommunityRepo_Search(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Search ranks by pg_trgm similarity against name and description, so the
	// fixtures have to be words a trigram index can actually distinguish. A
	// unique suffix on each name would destroy the similarity being measured,
	// which is why these are seeded into a per-test clone and named plainly.
	seed := func(t *testing.T) communities.Repository {
		t.Helper()
		repo := NewCommunityRepository(testkit.DB(t))
		for _, fixture := range []struct {
			name, description, visibility string
			members                       int
		}{
			{"gardening", "growing vegetables and flowers", "public", 10},
			{"gardeners", "a place for gardening talk", "public", 5},
			{"woodworking", "sawdust and joinery", "public", 50},
			{"gardening-private", "growing things quietly", "private", 1},
		} {
			created, err := repo.Create(ctx, &communities.Community{
				DID:          "did:plc:search-" + fixture.name,
				Handle:       "c-" + fixture.name + ".coves.social",
				Name:         fixture.name,
				DisplayName:  fixture.name,
				Description:  fixture.description,
				OwnerDID:     "did:web:coves.social",
				CreatedByDID: "did:plc:searchcreator",
				HostedByDID:  "did:web:coves.social",
				Visibility:   fixture.visibility,
				CreatedAt:    time.Now(),
				UpdatedAt:    time.Now(),
			})
			require.NoError(t, err)
			for i := 0; i < fixture.members; i++ {
				require.NoError(t, repo.IncrementMemberCount(ctx, created.DID))
			}
		}
		return repo
	}

	t.Run("finds communities by name and by description", func(t *testing.T) {
		t.Parallel()
		repo := seed(t)

		results, _, err := repo.Search(ctx, communities.SearchCommunitiesRequest{Query: "gardening", Limit: 10})
		require.NoError(t, err)
		names := communityNames(results)

		assert.Contains(t, names, "gardening", "an exact name match must be found")
		assert.Contains(t, names, "gardeners",
			"the search is fuzzy on purpose: a user typing the community they half-remember is the "+
				"case this exists for")
		assert.NotContains(t, names, "woodworking",
			"a similarity threshold that lets everything through makes the ranking meaningless")
	})

	t.Run("ranks the closer match first", func(t *testing.T) {
		t.Parallel()
		repo := seed(t)

		results, _, err := repo.Search(ctx, communities.SearchCommunitiesRequest{Query: "gardening", Limit: 10})
		require.NoError(t, err)
		require.NotEmpty(t, results)
		assert.Equal(t, "gardening", results[0].Name,
			"relevance is the point of the ORDER BY; a member-count-first ordering would bury the "+
				"exact match under whichever big community happens to mention the word")
	})

	t.Run("filters by visibility", func(t *testing.T) {
		t.Parallel()
		repo := seed(t)

		results, total, err := repo.Search(ctx,
			communities.SearchCommunitiesRequest{Query: "gardening", Visibility: "public", Limit: 10})
		require.NoError(t, err)
		names := communityNames(results)
		assert.NotContains(t, names, "gardening-private",
			"a private community must not be discoverable through search; the filter is the only "+
				"thing stopping it")
		assert.Positive(t, total)
	})

	t.Run("paginates", func(t *testing.T) {
		t.Parallel()
		repo := seed(t)

		firstPage, _, err := repo.Search(ctx,
			communities.SearchCommunitiesRequest{Query: "gardening", Limit: 1, Offset: 0})
		require.NoError(t, err)
		require.Len(t, firstPage, 1)

		secondPage, _, err := repo.Search(ctx,
			communities.SearchCommunitiesRequest{Query: "gardening", Limit: 1, Offset: 1})
		require.NoError(t, err)
		require.Len(t, secondPage, 1)
		assert.NotEqual(t, firstPage[0].DID, secondPage[0].DID,
			"the second page repeated the first; an unstable ORDER BY makes every page after the "+
				"first arbitrary")
	})

	t.Run("answers an unmatched query with an empty page and a zero total", func(t *testing.T) {
		t.Parallel()
		repo := seed(t)

		results, total, err := repo.Search(ctx,
			communities.SearchCommunitiesRequest{Query: "zzzzzznothingmatchesthis", Limit: 10})
		require.NoError(t, err)
		require.NotNil(t, results, "an empty result must marshal as [] rather than null")
		assert.Empty(t, results)
		assert.Zero(t, total)
	})

	t.Run("returns the fields a client needs to render a result", func(t *testing.T) {
		t.Parallel()
		repo := seed(t)

		results, _, err := repo.Search(ctx, communities.SearchCommunitiesRequest{Query: "woodworking", Limit: 10})
		require.NoError(t, err)
		require.NotEmpty(t, results)

		got := results[0]
		assert.Equal(t, "woodworking", got.Name)
		assert.Equal(t, "c-woodworking.coves.social", got.Handle)
		assert.Equal(t, "sawdust and joinery", got.Description)
		assert.Equal(t, 50, got.MemberCount)
		assert.NotZero(t, got.ID)
		assert.False(t, got.CreatedAt.IsZero(),
			"the scan lists twenty-seven columns positionally; a shifted one shows up as a zero value "+
				"in whichever field lost its place")
	})
}

// TestCommunityRepo_SearchTotalCountsRowsTheResultsExclude pins a defect.
//
// Search runs two queries. The count uses only the ILIKE clause; the page adds
// `AND similarity(...) > 0.2`. So the total is the number of rows whose name or
// description CONTAINS the query, while the page holds only those that are also
// similar enough — and the first number can exceed everything the second query
// will ever return.
//
// The consequence is a pagination lie: a client told "42 results" walks offsets
// until it runs out and finds empty pages long before 42, with no way to tell
// that from a page boundary. It is the same shape as a filtered row eating a
// page slot, one query up.
//
// A long query string containing a short community name is the reproduction: it
// matches nothing by ILIKE, so instead the fixture inverts it — a short query
// contained in a long description scores low similarity while still matching
// ILIKE.
//
// Filed: ~/Code/claude-skills/issues/2026-07-30-community-search-total-counts-excluded-rows.md
//
// IF THIS TEST FAILED, the defect is FIXED — the count query now carries the
// same similarity threshold as the page. Delete the pin and assert that the
// total equals the number of rows the page can reach.
func TestCommunityRepo_SearchTotalCountsRowsTheResultsExclude(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewCommunityRepository(testkit.DB(t))

	// "art" appears inside this description, so ILIKE matches. Against a
	// description this long the trigram similarity of a three-character query
	// is far below the 0.2 threshold, so the row cannot appear in any page.
	_, err := repo.Create(ctx, &communities.Community{
		DID:    "did:plc:searchtotalmismatch",
		Handle: "c-longwinded.coves.social",
		Name:   "longwinded",
		Description: "a community for people who enjoy discussing the state of the art in " +
			"distributed systems, consensus protocols, replicated logs and the general " +
			"business of keeping many computers in agreement with one another over time",
		OwnerDID:     "did:web:coves.social",
		CreatedByDID: "did:plc:searchcreator",
		HostedByDID:  "did:web:coves.social",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err)

	results, total, err := repo.Search(ctx, communities.SearchCommunitiesRequest{Query: "art", Limit: 100})
	require.NoError(t, err)

	// One comparison, not three: a leading require on the total would abort
	// before the assertion carrying the issue ID, so the whole known-bad
	// outcome — counted once, returnable never — is asserted as a single tuple.
	require.Equal(t,
		[]int{1, 0},
		[]int{total, len(results)},
		"expected a total of 1 alongside an empty page (pinning known defect "+
			"2026-07-30-community-search-total-counts-excluded-rows: the count query filters on "+
			"ILIKE alone while the page query also requires pg_trgm similarity > 0.2, so the total "+
			"counts rows no page can ever return and a client paginating against it walks into "+
			"empty pages. IF THIS FAILED, the defect is FIXED — invert this step to expect "+
			"{0, 0} and close the issue)")
}

func communityNames(list []*communities.Community) []string {
	names := make([]string, 0, len(list))
	for _, community := range list {
		names = append(names, community.Name)
	}
	return names
}

// TestCommunityRepo_SearchIsNotSQLInjectable is cheap insurance on the one
// query in this file that interpolates anything into its SQL text.
//
// The interpolation is only of placeholder NUMBERS ($2, $3) — every value is a
// parameter — but the query is assembled with fmt.Sprintf, which is the pattern
// CLAUDE.md names as a red flag, so the property is worth asserting rather than
// re-reading the code for.
func TestCommunityRepo_SearchIsNotSQLInjectable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewCommunityRepository(testkit.DB(t))

	id := testkit.UniqueID(t)
	_, err := repo.Create(ctx, &communities.Community{
		DID: "did:plc:inject" + id, Handle: "c-inject-" + id + ".coves.social", Name: "inject-" + id,
		Description: "a community that must survive", OwnerDID: "did:web:coves.social",
		CreatedByDID: "did:plc:searchcreator", HostedByDID: "did:web:coves.social",
		Visibility: "public", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	require.NoError(t, err)

	for _, query := range []string{
		"'; DROP TABLE communities; --",
		"' OR '1'='1",
		"%",
		strings.Repeat("'", 20),
	} {
		_, _, searchErr := repo.Search(ctx, communities.SearchCommunitiesRequest{Query: query, Limit: 10})
		assert.NoErrorf(t, searchErr, "the query %q was not parameterised cleanly", query)
	}

	_, err = repo.GetByDID(ctx, "did:plc:inject"+id)
	require.NoError(t, err, "the communities table is still there")
}
