//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/aggregators"
	"Coves/tests/testkit"
)

// The federation-visible half of the aggregator index: what an aggregator IS,
// which communities have let it in, and how much it has posted.
//
// Every row touched here started life as a record in somebody's repository, and
// the AppView is a cache of those records rather than their owner. That shapes
// what is worth asserting:
//
//   - The lifecycle writers (UpdateAggregator, DeleteAggregator,
//     UpdateAuthorization, DeleteAuthorizationByURI) are driven by firehose
//     events, which arrive out of order, duplicated, and occasionally for
//     records this instance never saw created. All four check RowsAffected and
//     translate a zero into a domain error — the consumer's only signal that it
//     is applying an update to something it does not have. A silent nil there
//     would make a missed create indistinguishable from an applied update, and
//     the row would stay stale forever.
//   - Authorization is the permission itself. An authorization row is what lets
//     a bot write into a community, so a delete that misses, or a list that
//     leaks another aggregator's grants, is an access-control failure and not a
//     display bug. Every authorization assertion here is also checked through
//     IsAuthorized, which is the predicate the post path actually consults.
//   - GetRecentPosts is the input to the rate limiter. Its two predicates —
//     the time window and the (aggregator, community) pair — are each the only
//     thing standing between a quota and unlimited posting, so both are
//     asserted from the excluded side as well as the included one.
//
// Ordering and windows are asserted against rows placed on both sides of every
// boundary. The database is a per-test clone, so a count is exact rather than a
// lower bound.

// aggregatorRecordPostAt tracks an aggregator post at a chosen instant.
//
// RecordAggregatorPost hardcodes NOW(), which is right for production and
// useless for testing a rolling window, so the ledger row is written directly.
// The columns and the trigger behind them are the same either way.
func aggregatorRecordPostAt(t *testing.T, db *sql.DB, aggregatorDID, communityDID, postURI string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO aggregator_posts (aggregator_did, community_did, post_uri, post_cid, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		aggregatorDID, communityDID, postURI, "bafyledger", at)
	require.NoError(t, err, "seeding the rate-limit ledger")
}

// aggregatorPostURIs flattens a ledger page to the URIs it names.
func aggregatorPostURIs(posts []*aggregators.AggregatorPost) []string {
	uris := make([]string, 0, len(posts))
	for _, post := range posts {
		uris = append(uris, post.PostURI)
	}
	return uris
}

// aggregatorDIDsOf flattens a list of aggregators to their DIDs.
func aggregatorDIDsOf(aggs []*aggregators.Aggregator) []string {
	dids := make([]string, 0, len(aggs))
	for _, agg := range aggs {
		dids = append(dids, agg.DID)
	}
	return dids
}

// aggregatorCommunityDIDsOf flattens authorizations to the communities they name.
func aggregatorCommunityDIDsOf(auths []*aggregators.Authorization) []string {
	dids := make([]string, 0, len(auths))
	for _, auth := range auths {
		dids = append(dids, auth.CommunityDID)
	}
	return dids
}

func TestAggregatorRepo_UpdateAggregator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("replaces every declared field", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Original Name")
		declaredAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
		schema := []byte(`{"type":"object","properties":{"feedUrl":{"type":"string"}}}`)

		require.NoError(t, repo.UpdateAggregator(ctx, &aggregators.Aggregator{
			DID:           did,
			DisplayName:   "Renamed Aggregator",
			Description:   "now does something else",
			AvatarURL:     "https://cdn.example.com/new-avatar.png",
			ConfigSchema:  schema,
			MaintainerDID: "did:plc:newmaintainer",
			SourceURL:     "https://github.com/example/rewritten",
			CreatedAt:     declaredAt,
			IndexedAt:     time.Now(),
			RecordURI:     "at://" + did + "/" + aggregatorServiceCollection + "/self",
			RecordCID:     "bafyservice-v2",
		}))

		indexed, err := repo.GetAggregator(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "Renamed Aggregator", indexed.DisplayName)
		assert.Equal(t, "now does something else", indexed.Description)
		assert.Equal(t, "https://cdn.example.com/new-avatar.png", indexed.AvatarURL)
		assert.Equal(t, "did:plc:newmaintainer", indexed.MaintainerDID)
		assert.Equal(t, "https://github.com/example/rewritten", indexed.SourceURL)
		assert.Equal(t, "bafyservice-v2", indexed.RecordCID,
			"record_cid is how the consumer tells a replayed event from a genuinely newer version of "+
				"the record; leaving it stale defeats every later dedupe")
		assert.JSONEq(t, string(schema), string(indexed.ConfigSchema))
		assert.True(t, indexed.CreatedAt.Equal(declaredAt),
			"createdAt = %v, want the redeclared %v", indexed.CreatedAt, declaredAt)
	})

	// The counters are maintained by the migration-012 triggers and are absent
	// from the UPDATE's column list. If they were in it, an aggregator could
	// zero its own advertised reach by republishing its service declaration.
	t.Run("does not touch the trigger-maintained stats", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Established")
		communityDID := indexAuthorizingCommunity(t, db)
		require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, did, communityDID)))
		require.NoError(t, repo.RecordAggregatorPost(ctx, did, communityDID,
			"at://"+communityDID+"/social.coves.community.post/"+testkit.UniqueID(t), "bafypost"))

		require.NoError(t, repo.UpdateAggregator(ctx, &aggregators.Aggregator{
			DID: did, DisplayName: "Established, Renamed",
			CreatedAt: time.Now(), IndexedAt: time.Now(),
			RecordURI: "at://" + did + "/" + aggregatorServiceCollection + "/self",
			RecordCID: "bafyservice-v2",
		}))

		indexed, err := repo.GetAggregator(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, 1, indexed.CommunitiesUsing)
		assert.Equal(t, 1, indexed.PostsCreated)
	})

	// Every optional column goes through nullString, so an update that omits a
	// field clears it rather than leaving the previous value in place. That is
	// the right semantic for a record replacement — the record IS the truth, and
	// a maintainer who removed their description means it to be gone.
	t.Run("an omitted optional field is cleared, not preserved", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := "did:plc:" + testkit.UniqueID(t)
		require.NoError(t, repo.CreateAggregator(ctx, &aggregators.Aggregator{
			DID: did, DisplayName: "Described", Description: "a description",
			AvatarURL: "https://cdn.example.com/a.png", MaintainerDID: "did:plc:someone",
			CreatedAt: time.Now(), IndexedAt: time.Now(),
			RecordURI: "at://" + did + "/" + aggregatorServiceCollection + "/self",
			RecordCID: "bafyservice",
		}))

		require.NoError(t, repo.UpdateAggregator(ctx, &aggregators.Aggregator{
			DID: did, DisplayName: "Described",
			CreatedAt: time.Now(), IndexedAt: time.Now(),
			RecordURI: "at://" + did + "/" + aggregatorServiceCollection + "/self",
			RecordCID: "bafyservice2",
		}))

		indexed, err := repo.GetAggregator(ctx, did)
		require.NoError(t, err)
		assert.Empty(t, indexed.Description)
		assert.Empty(t, indexed.AvatarURL)
		assert.Empty(t, indexed.MaintainerDID)
		assert.Nil(t, indexed.ConfigSchema, "a declaration with no config schema must leave the column "+
			"NULL; an empty JSONB value would make every community's config validate against nothing")
	})

	t.Run("updates one aggregator", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		subject := indexAggregator(t, repo, "Subject")
		bystander := indexAggregator(t, repo, "Bystander")

		require.NoError(t, repo.UpdateAggregator(ctx, &aggregators.Aggregator{
			DID: subject, DisplayName: "Subject Renamed",
			CreatedAt: time.Now(), IndexedAt: time.Now(),
			RecordURI: "at://" + subject + "/" + aggregatorServiceCollection + "/self",
			RecordCID: "bafyservice2",
		}))

		untouched, err := repo.GetAggregator(ctx, bystander)
		require.NoError(t, err)
		assert.Equal(t, "Bystander", untouched.DisplayName)
	})

	t.Run("reports an aggregator the AppView never indexed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := "did:plc:" + testkit.UniqueID(t)
		err := repo.UpdateAggregator(ctx, &aggregators.Aggregator{
			DID: did, DisplayName: "Never Indexed",
			CreatedAt: time.Now(), IndexedAt: time.Now(),
			RecordURI: "at://" + did + "/" + aggregatorServiceCollection + "/self",
			RecordCID: "bafyservice",
		})
		assert.ErrorIs(t, err, aggregators.ErrAggregatorNotFound,
			"an update for a declaration this instance missed must be reported, so the consumer can "+
				"backfill the create rather than believe it applied an edit")
	})

	// record_uri is UNIQUE across the table. Two aggregators cannot claim the
	// same record, and the constraint is what stops a malformed firehose event
	// from re-pointing one aggregator's row at another's declaration.
	t.Run("refuses to claim another aggregator's record URI", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		first := indexAggregator(t, repo, "First")
		second := indexAggregator(t, repo, "Second")

		err := repo.UpdateAggregator(ctx, &aggregators.Aggregator{
			DID: second, DisplayName: "Impostor",
			CreatedAt: time.Now(), IndexedAt: time.Now(),
			RecordURI: "at://" + first + "/" + aggregatorServiceCollection + "/self",
			RecordCID: "bafyservice",
		})
		require.Error(t, err)

		unharmed, err := repo.GetAggregator(ctx, first)
		require.NoError(t, err)
		assert.Equal(t, "First", unharmed.DisplayName)
	})
}

func TestAggregatorRepo_DeleteAggregator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A deleted service declaration must take its grants with it. An
	// authorization row naming an aggregator that no longer exists would keep
	// IsAuthorized answering yes for a DID nothing hosts.
	t.Run("takes the authorizations and the post ledger with it", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Withdrawn")
		communityDID := indexAuthorizingCommunity(t, db)
		require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, did, communityDID)))
		require.NoError(t, repo.RecordAggregatorPost(ctx, did, communityDID,
			"at://"+communityDID+"/social.coves.community.post/"+testkit.UniqueID(t), "bafypost"))

		require.NoError(t, repo.DeleteAggregator(ctx, did))

		_, err := repo.GetAggregator(ctx, did)
		assert.ErrorIs(t, err, aggregators.ErrAggregatorNotFound)

		declared, err := repo.IsAggregator(ctx, did)
		require.NoError(t, err)
		assert.False(t, declared, "the post path would still treat this DID as an aggregator")

		authorized, err := repo.IsAuthorized(ctx, did, communityDID)
		require.NoError(t, err)
		assert.False(t, authorized, "the grant outlived the service it granted access to")

		remaining, err := repo.ListAuthorizationsForCommunity(ctx, communityDID, false, 10, 0)
		require.NoError(t, err)
		assert.Empty(t, remaining)

		count, err := repo.CountRecentPosts(ctx, did, communityDID, time.Now().Add(-time.Hour))
		require.NoError(t, err)
		assert.Zero(t, count, "the rate-limit ledger kept rows for a deleted aggregator")
	})

	t.Run("deletes one aggregator", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		doomed := indexAggregator(t, repo, "Doomed")
		spared := indexAggregator(t, repo, "Spared")

		require.NoError(t, repo.DeleteAggregator(ctx, doomed))

		survivor, err := repo.GetAggregator(ctx, spared)
		require.NoError(t, err, "deleting one aggregator removed another")
		assert.Equal(t, "Spared", survivor.DisplayName)
	})

	// Firehose deletes are replayed. The second delivery finds nothing, and the
	// consumer needs to be told that so it can distinguish "already applied"
	// from "applied now".
	t.Run("reports a delete that matched nothing", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Once")
		require.NoError(t, repo.DeleteAggregator(ctx, did))
		assert.ErrorIs(t, repo.DeleteAggregator(ctx, did), aggregators.ErrAggregatorNotFound)
		assert.ErrorIs(t, repo.DeleteAggregator(ctx, "did:plc:"+testkit.UniqueID(t)),
			aggregators.ErrAggregatorNotFound)
	})
}

func TestAggregatorRepo_ListAggregators(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Four aggregators with a known ranking. communities_using is written by the
	// trigger rather than by any statement here, so the ordering under test is
	// the ordering a real discovery page would see.
	//
	// The names run BACKWARDS against the uptake, deliberately: if the two
	// agreed, an ORDER BY that had lost `communities_using DESC` would produce
	// the same page and every assertion below would pass against it.
	seed := func(t *testing.T) (aggregators.Repository, map[string]string) {
		t.Helper()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		byName := map[string]string{}
		for _, spec := range []struct {
			name        string
			authorizers int
		}{
			{"Zulu Aggregator", 2},
			{"Yankee Aggregator", 1},
			// Two with no uptake at all, so the display_name tiebreak is
			// observable rather than incidental — and inserted Bravo FIRST, so
			// that the tiebreak has to reverse them. Seeded Alpha-then-Bravo,
			// physical row order alone produces the expected page and an
			// ORDER BY that had lost `display_name ASC` would pass.
			{"Bravo Aggregator", 0},
			{"Alpha Aggregator", 0},
		} {
			did := indexAggregator(t, repo, spec.name)
			byName[spec.name] = did
			for i := 0; i < spec.authorizers; i++ {
				communityDID := indexAuthorizingCommunity(t, db)
				require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, did, communityDID)))
			}
		}
		return repo, byName
	}

	t.Run("ranks by uptake, then alphabetically", func(t *testing.T) {
		t.Parallel()
		repo, byName := seed(t)

		listed, err := repo.ListAggregators(ctx, 10, 0)
		require.NoError(t, err)
		require.Len(t, listed, 4)

		assert.Equal(t, []string{
			byName["Zulu Aggregator"],
			byName["Yankee Aggregator"],
			byName["Alpha Aggregator"],
			byName["Bravo Aggregator"],
		}, aggregatorDIDsOf(listed),
			"the directory is ranked by how many communities actually use the aggregator; the name "+
				"tiebreak is what keeps two unused aggregators from swapping places between page "+
				"loads, which is a pagination bug and not only a cosmetic one")
	})

	t.Run("paginates without skipping or repeating", func(t *testing.T) {
		t.Parallel()
		repo, byName := seed(t)

		// Pages of three, so the TIED pair straddles the boundary: Alpha ends
		// page one and Bravo opens page two. A tie that sits wholly inside one
		// page is reordered harmlessly; a tie split across a boundary is how an
		// unstable ORDER BY actually loses and repeats rows, because the two
		// requests sort independently.
		first, err := repo.ListAggregators(ctx, 3, 0)
		require.NoError(t, err)
		second, err := repo.ListAggregators(ctx, 3, 3)
		require.NoError(t, err)

		assert.Equal(t, []string{
			byName["Zulu Aggregator"], byName["Yankee Aggregator"], byName["Alpha Aggregator"],
		}, aggregatorDIDsOf(first))
		assert.Equal(t, []string{byName["Bravo Aggregator"]}, aggregatorDIDsOf(second))

		assert.ElementsMatch(t,
			[]string{byName["Zulu Aggregator"], byName["Yankee Aggregator"],
				byName["Alpha Aggregator"], byName["Bravo Aggregator"]},
			append(aggregatorDIDsOf(first), aggregatorDIDsOf(second)...),
			"paging the directory must visit every aggregator exactly once")

		past, err := repo.ListAggregators(ctx, 3, 4)
		require.NoError(t, err)
		assert.Empty(t, past, "reading past the end must be an empty page, not an error")
	})

	t.Run("hydrates the fields the directory renders", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := "did:plc:" + testkit.UniqueID(t)
		schema := []byte(`{"type":"object","properties":{"feedUrl":{"type":"string"}}}`)
		require.NoError(t, repo.CreateAggregator(ctx, &aggregators.Aggregator{
			DID: did, DisplayName: "Detailed", Description: "what it does",
			AvatarURL: "https://cdn.example.com/detailed.png", ConfigSchema: schema,
			MaintainerDID: "did:plc:maintainer", SourceURL: "https://example.com/src",
			CreatedAt: time.Now(), IndexedAt: time.Now(),
			RecordURI: "at://" + did + "/" + aggregatorServiceCollection + "/self",
			RecordCID: "bafyservice",
		}))

		listed, err := repo.ListAggregators(ctx, 10, 0)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "what it does", listed[0].Description)
		assert.Equal(t, "https://cdn.example.com/detailed.png", listed[0].AvatarURL)
		assert.Equal(t, "did:plc:maintainer", listed[0].MaintainerDID)
		assert.Equal(t, "https://example.com/src", listed[0].SourceURL)
		assert.JSONEq(t, string(schema), string(listed[0].ConfigSchema),
			"the config schema is what a community's settings UI builds its form from")
	})

	// Pinned, not endorsed. ListAggregators accumulates into a nil slice, so an
	// installation with no aggregators returns nil rather than an empty slice —
	// which marshals to `null` where the sibling list endpoints emit `[]`.
	t.Run("an installation with no aggregators returns nil rather than an empty slice", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		listed, err := repo.ListAggregators(ctx, 10, 0)
		require.NoError(t, err)
		assert.Nil(t, listed,
			"IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md, item 6) the defect is "+
				"FIXED — delete this pin. The right answer is an empty, "+
				"non-nil slice: a client that distinguishes `null` from `[]` reads the former as "+
				"'the field is absent' rather than 'there are none'")
	})
}

func TestAggregatorRepo_GetAggregatorsByDIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The batch read behind getServices. It exists so hydrating a page of
	// authorizations costs one query instead of one per aggregator, which means
	// the caller indexes the answer by DID and every property of that indexing
	// is load-bearing.
	t.Run("returns exactly the aggregators asked for", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		wanted := indexAggregator(t, repo, "Wanted")
		alsoWanted := indexAggregator(t, repo, "Also Wanted")
		indexAggregator(t, repo, "Not Asked For")

		fetched, err := repo.GetAggregatorsByDIDs(ctx, []string{wanted, alsoWanted})
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{wanted, alsoWanted}, aggregatorDIDsOf(fetched),
			"a batch read that returned rows nobody asked for would hydrate a page with aggregators "+
				"that are not on it")
	})

	// A DID the AppView has not indexed is a normal occurrence — an
	// authorization can name an aggregator whose service declaration has not
	// arrived yet — so it must be an absence rather than an error that fails the
	// whole page.
	t.Run("silently omits DIDs it has never seen", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		known := indexAggregator(t, repo, "Known")
		fetched, err := repo.GetAggregatorsByDIDs(ctx, []string{known, "did:plc:" + testkit.UniqueID(t)})
		require.NoError(t, err)
		assert.Equal(t, []string{known}, aggregatorDIDsOf(fetched))
	})

	// The caller builds a map from this, so a duplicated request must not
	// produce a duplicated row: the IN list collapses it.
	t.Run("a DID asked for twice comes back once", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Repeated")
		fetched, err := repo.GetAggregatorsByDIDs(ctx, []string{did, did, did})
		require.NoError(t, err)
		assert.Len(t, fetched, 1)
	})

	t.Run("an empty request is answered without a query", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		fetched, err := repo.GetAggregatorsByDIDs(ctx, nil)
		require.NoError(t, err)
		require.NotNil(t, fetched, "the early return builds an empty slice on purpose: with no DIDs "+
			"there is no IN list to build, and a query with an empty list is a syntax error")
		assert.Empty(t, fetched)
	})

	// The same emptiness reached the other way answers differently, because the
	// scan loop accumulates into a nil slice while the short-circuit above
	// constructs one.
	t.Run("a request that matches nothing returns nil rather than an empty slice", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		fetched, err := repo.GetAggregatorsByDIDs(ctx, []string{"did:plc:" + testkit.UniqueID(t)})
		require.NoError(t, err)
		assert.Nil(t, fetched,
			"IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin. One function must not answer the "+
				"same question two ways: GetAggregatorsByDIDs(nil) returns []*Aggregator{} and this "+
				"returns nil, so a caller that ranges over the result is fine and one that marshals it "+
				"emits [] or null depending on which emptiness it hit")
	})

	t.Run("hydrates nullable fields and the config schema", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		schema := []byte(`{"type":"object","required":["feedUrl"]}`)
		did := "did:plc:" + testkit.UniqueID(t)
		require.NoError(t, repo.CreateAggregator(ctx, &aggregators.Aggregator{
			DID: did, DisplayName: "Hydrated", Description: "a description",
			AvatarURL: "https://cdn.example.com/h.png", ConfigSchema: schema,
			MaintainerDID: "did:plc:maintainer", SourceURL: "https://example.com/src",
			CreatedAt: time.Now(), IndexedAt: time.Now(),
			RecordURI: "at://" + did + "/" + aggregatorServiceCollection + "/self",
			RecordCID: "bafyservice",
		}))
		bare := indexAggregator(t, repo, "Bare")

		fetched, err := repo.GetAggregatorsByDIDs(ctx, []string{did, bare})
		require.NoError(t, err)
		require.Len(t, fetched, 2)

		byDID := map[string]*aggregators.Aggregator{}
		for _, agg := range fetched {
			byDID[agg.DID] = agg
		}

		assert.Equal(t, "a description", byDID[did].Description)
		assert.Equal(t, "https://cdn.example.com/h.png", byDID[did].AvatarURL)
		assert.Equal(t, "did:plc:maintainer", byDID[did].MaintainerDID)
		assert.Equal(t, "https://example.com/src", byDID[did].SourceURL)
		assert.JSONEq(t, string(schema), string(byDID[did].ConfigSchema))

		assert.Empty(t, byDID[bare].Description, "a NULL description must flatten to the empty string "+
			"rather than fail the scan and take the whole batch down with it")
		assert.Nil(t, byDID[bare].ConfigSchema)
	})
}

func TestAggregatorRepo_GetAuthorizationByURI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Jetstream delete events carry only the record's AT-URI. This lookup is how
	// the consumer turns that URI back into the (aggregator, community) pair it
	// needs in order to know what permission is being withdrawn.
	t.Run("resolves the record a delete event names", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Authorized")
		communityDID := indexAuthorizingCommunity(t, db)
		granted := enabledAuthorization(t, aggregatorDID, communityDID)
		granted.Config = []byte(`{"feedUrl":"https://example.com/feed.xml"}`)
		require.NoError(t, repo.CreateAuthorization(ctx, granted))

		found, err := repo.GetAuthorizationByURI(ctx, granted.RecordURI)
		require.NoError(t, err)
		assert.Equal(t, aggregatorDID, found.AggregatorDID)
		assert.Equal(t, communityDID, found.CommunityDID)
		assert.Equal(t, granted.ID, found.ID)
		assert.True(t, found.Enabled)
		assert.Equal(t, communityDID, found.CreatedBy)
		assert.Equal(t, granted.RecordURI, found.RecordURI)
		assert.JSONEq(t, string(granted.Config), string(found.Config))
		assert.Nil(t, found.DisabledAt)
	})

	t.Run("carries the revocation audit trail", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Revoked")
		communityDID := indexAuthorizingCommunity(t, db)
		disabledAt := time.Now().UTC().Truncate(time.Microsecond)
		revoked := enabledAuthorization(t, aggregatorDID, communityDID)
		revoked.Enabled = false
		revoked.DisabledAt = &disabledAt
		revoked.DisabledBy = "did:plc:moderator"
		require.NoError(t, repo.CreateAuthorization(ctx, revoked))

		found, err := repo.GetAuthorizationByURI(ctx, revoked.RecordURI)
		require.NoError(t, err)
		assert.False(t, found.Enabled)
		require.NotNil(t, found.DisabledAt)
		assert.True(t, found.DisabledAt.Equal(disabledAt))
		assert.Equal(t, "did:plc:moderator", found.DisabledBy)
	})

	t.Run("finds the one record with that URI and no other", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Multi")
		first := indexAuthorizingCommunity(t, db)
		second := indexAuthorizingCommunity(t, db)
		firstGrant := enabledAuthorization(t, aggregatorDID, first)
		secondGrant := enabledAuthorization(t, aggregatorDID, second)
		require.NoError(t, repo.CreateAuthorization(ctx, firstGrant))
		require.NoError(t, repo.CreateAuthorization(ctx, secondGrant))

		found, err := repo.GetAuthorizationByURI(ctx, secondGrant.RecordURI)
		require.NoError(t, err)
		assert.Equal(t, second, found.CommunityDID,
			"resolving the wrong record would make a delete event withdraw the wrong community's grant")
	})

	t.Run("reports a URI nothing indexed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		found, err := repo.GetAuthorizationByURI(ctx,
			"at://did:plc:"+testkit.UniqueID(t)+"/"+aggregatorAuthorizationCollection+"/missing")
		require.ErrorIs(t, err, aggregators.ErrAuthorizationNotFound,
			"sql.ErrNoRows escaping here would make the consumer treat a delete for an unindexed "+
				"record as an infrastructure failure and retry it forever")
		assert.Nil(t, found)
	})

	t.Run("an empty URI matches nothing", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Present")
		communityDID := indexAuthorizingCommunity(t, db)
		require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, communityDID)))

		_, err := repo.GetAuthorizationByURI(ctx, "")
		assert.ErrorIs(t, err, aggregators.ErrAuthorizationNotFound)
	})
}

func TestAggregatorRepo_UpdateAuthorization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Disabling is the ordinary way a community withdraws access: the record
	// stays for the audit trail and `enabled` goes false. The claim that matters
	// is not the column but that the post path stops saying yes.
	t.Run("disabling stops the aggregator being authorized", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Disabled Soon")
		communityDID := indexAuthorizingCommunity(t, db)
		granted := enabledAuthorization(t, aggregatorDID, communityDID)
		require.NoError(t, repo.CreateAuthorization(ctx, granted))

		authorized, err := repo.IsAuthorized(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		require.True(t, authorized)

		disabledAt := time.Now().UTC().Truncate(time.Microsecond)
		granted.Enabled = false
		granted.DisabledAt = &disabledAt
		granted.DisabledBy = "did:plc:moderator"
		granted.RecordCID = "bafyauthorization2"
		require.NoError(t, repo.UpdateAuthorization(ctx, granted))

		authorized, err = repo.IsAuthorized(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		assert.False(t, authorized,
			"the update wrote a disabled row that the authorization check still accepts")

		stored, err := repo.GetAuthorization(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		require.NotNil(t, stored.DisabledAt)
		assert.True(t, stored.DisabledAt.Equal(disabledAt))
		assert.Equal(t, "did:plc:moderator", stored.DisabledBy)
		assert.Equal(t, "bafyauthorization2", stored.RecordCID)
	})

	t.Run("re-enabling restores access", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Reinstated")
		communityDID := indexAuthorizingCommunity(t, db)
		granted := enabledAuthorization(t, aggregatorDID, communityDID)
		granted.Enabled = false
		require.NoError(t, repo.CreateAuthorization(ctx, granted))

		granted.Enabled = true
		require.NoError(t, repo.UpdateAuthorization(ctx, granted))

		authorized, err := repo.IsAuthorized(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		assert.True(t, authorized)

		indexed, err := repo.GetAggregator(ctx, aggregatorDID)
		require.NoError(t, err)
		assert.Equal(t, 1, indexed.CommunitiesUsing,
			"communities_using is recomputed by a trigger on UPDATE as well as INSERT; without that "+
				"branch a re-enabled community would never reappear in the aggregator's reach")
	})

	t.Run("rewrites the community's configuration", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Reconfigured")
		communityDID := indexAuthorizingCommunity(t, db)
		granted := enabledAuthorization(t, aggregatorDID, communityDID)
		granted.Config = []byte(`{"feedUrl":"https://old.example.com/feed.xml"}`)
		require.NoError(t, repo.CreateAuthorization(ctx, granted))

		granted.Config = []byte(`{"feedUrl":"https://new.example.com/feed.xml","updateInterval":30}`)
		require.NoError(t, repo.UpdateAuthorization(ctx, granted))

		stored, err := repo.GetAuthorization(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		assert.JSONEq(t, string(granted.Config), string(stored.Config),
			"the config is what the aggregator polls; a stale one keeps it fetching the old feed")

		granted.Config = nil
		require.NoError(t, repo.UpdateAuthorization(ctx, granted))
		stored, err = repo.GetAuthorization(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		assert.Nil(t, stored.Config, "clearing the config must write NULL, not leave the old one")
	})

	t.Run("updates the named pair only", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Shared")
		subject := indexAuthorizingCommunity(t, db)
		bystander := indexAuthorizingCommunity(t, db)
		subjectGrant := enabledAuthorization(t, aggregatorDID, subject)
		require.NoError(t, repo.CreateAuthorization(ctx, subjectGrant))
		require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, bystander)))

		subjectGrant.Enabled = false
		require.NoError(t, repo.UpdateAuthorization(ctx, subjectGrant))

		stillAuthorized, err := repo.IsAuthorized(ctx, aggregatorDID, bystander)
		require.NoError(t, err)
		assert.True(t, stillAuthorized,
			"disabling one community's grant disabled another's; both name the same aggregator, so "+
				"only the community predicate separates them")
	})

	t.Run("reports a pair that was never authorized", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Unauthorized")
		communityDID := indexAuthorizingCommunity(t, db)

		err := repo.UpdateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, communityDID))
		assert.ErrorIs(t, err, aggregators.ErrAuthorizationNotFound,
			"an update that matched nothing must be reported: returning nil would let the consumer "+
				"record a grant it never wrote, and the community would appear to have authorized an "+
				"aggregator it did not")
	})

	// Pinned. created_by is NOT NULL in the schema and CreateAuthorization
	// writes it verbatim, but UpdateAuthorization pipes it through nullString —
	// so an update carrying no CreatedBy tries to write NULL and dies on the
	// constraint instead of on a validation check.
	t.Run("an update with no author fails on the constraint rather than on validation", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Authorless")
		communityDID := indexAuthorizingCommunity(t, db)
		granted := enabledAuthorization(t, aggregatorDID, communityDID)
		require.NoError(t, repo.CreateAuthorization(ctx, granted))

		granted.CreatedBy = ""
		err := repo.UpdateAuthorization(ctx, granted)
		require.Error(t, err)
		assert.ErrorContains(t, err, "created_by",
			"IF THIS FAILED (issue 2026-07-31-update-authorization-rejects-rows-create-accepted.md) the defect is FIXED — delete this pin. CreateAuthorization writes "+
				"created_by verbatim and UpdateAuthorization wraps it in nullString, so the same "+
				"empty value inserts happily and then cannot be updated. The right fix is to write it "+
				"verbatim here too (or to reject it before the SQL), rather than to hand the handler a "+
				"raw not-null violation it can only map to a 500")

		unharmed, err := repo.GetAuthorization(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		assert.Equal(t, communityDID, unharmed.CreatedBy, "the failed update must not have partially applied")
	})
}

func TestAggregatorRepo_DeleteAuthorizationByURI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The firehose delete path. Deleting the record is the strongest form of
	// withdrawal — stronger than disabling, because nothing is left behind — so
	// what has to be proven is that the permission is gone from the check the
	// post path runs, not merely that a row vanished.
	t.Run("withdraws the permission the record granted", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Deauthorized")
		communityDID := indexAuthorizingCommunity(t, db)
		granted := enabledAuthorization(t, aggregatorDID, communityDID)
		require.NoError(t, repo.CreateAuthorization(ctx, granted))

		authorized, err := repo.IsAuthorized(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		require.True(t, authorized)

		require.NoError(t, repo.DeleteAuthorizationByURI(ctx, granted.RecordURI))

		authorized, err = repo.IsAuthorized(ctx, aggregatorDID, communityDID)
		require.NoError(t, err)
		assert.False(t, authorized,
			"the record is gone but the aggregator can still post; a deleted authorization that does "+
				"not revoke access is the whole permission model failing open")

		_, err = repo.GetAuthorizationByURI(ctx, granted.RecordURI)
		assert.ErrorIs(t, err, aggregators.ErrAuthorizationNotFound)

		indexed, err := repo.GetAggregator(ctx, aggregatorDID)
		require.NoError(t, err)
		assert.Zero(t, indexed.CommunitiesUsing,
			"the trigger's DELETE branch must count the community back out of the aggregator's reach")
	})

	t.Run("deletes the one record named", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Partially Deauthorized")
		doomedCommunity := indexAuthorizingCommunity(t, db)
		sparedCommunity := indexAuthorizingCommunity(t, db)
		doomed := enabledAuthorization(t, aggregatorDID, doomedCommunity)
		require.NoError(t, repo.CreateAuthorization(ctx, doomed))
		require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, sparedCommunity)))

		require.NoError(t, repo.DeleteAuthorizationByURI(ctx, doomed.RecordURI))

		stillAuthorized, err := repo.IsAuthorized(ctx, aggregatorDID, sparedCommunity)
		require.NoError(t, err)
		assert.True(t, stillAuthorized,
			"deleting one community's authorization record revoked another's")
	})

	t.Run("reports a delete that matched nothing", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		aggregatorDID := indexAggregator(t, repo, "Replayed")
		communityDID := indexAuthorizingCommunity(t, db)
		granted := enabledAuthorization(t, aggregatorDID, communityDID)
		require.NoError(t, repo.CreateAuthorization(ctx, granted))

		require.NoError(t, repo.DeleteAuthorizationByURI(ctx, granted.RecordURI))
		assert.ErrorIs(t, repo.DeleteAuthorizationByURI(ctx, granted.RecordURI),
			aggregators.ErrAuthorizationNotFound,
			"a replayed delete must be distinguishable from one that did something, or the consumer "+
				"can never tell an idempotent replay from a record it never indexed")
		assert.ErrorIs(t, repo.DeleteAuthorizationByURI(ctx, ""),
			aggregators.ErrAuthorizationNotFound)
	})
}

func TestAggregatorRepo_ListAuthorizationsForAggregator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// One aggregator with three grants in a known chronological order, plus a
	// second aggregator whose grants must never appear.
	type aggregatorGrantFixture struct {
		repo      aggregators.Repository
		subject   string
		other     string
		oldest    string
		middle    string
		newest    string
		otherComm string
	}
	seed := func(t *testing.T) aggregatorGrantFixture {
		t.Helper()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		fixture := aggregatorGrantFixture{repo: repo}
		fixture.subject = indexAggregator(t, repo, "Subject")
		fixture.other = indexAggregator(t, repo, "Other")

		base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		for i, slot := range []*string{&fixture.oldest, &fixture.middle, &fixture.newest} {
			communityDID := indexAuthorizingCommunity(t, db)
			*slot = communityDID
			granted := enabledAuthorization(t, fixture.subject, communityDID)
			granted.CreatedAt = base.AddDate(0, 0, i)
			// The middle grant is the disabled one, so enabledOnly has to remove
			// something from the middle of the ordering rather than from an end.
			if i == 1 {
				granted.Enabled = false
			}
			require.NoError(t, repo.CreateAuthorization(ctx, granted))
		}

		fixture.otherComm = indexAuthorizingCommunity(t, db)
		require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, fixture.other, fixture.otherComm)))
		return fixture
	}

	t.Run("lists the aggregator's grants newest first", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		listed, err := fixture.repo.ListAuthorizationsForAggregator(ctx, fixture.subject, false, 10, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{fixture.newest, fixture.middle, fixture.oldest},
			aggregatorCommunityDIDsOf(listed),
			"the most recent grant is the interesting one, and a stable order is what keeps "+
				"pagination from repeating or skipping rows")
	})

	t.Run("scopes to the aggregator asked about", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		listed, err := fixture.repo.ListAuthorizationsForAggregator(ctx, fixture.subject, false, 10, 0)
		require.NoError(t, err)
		assert.NotContains(t, aggregatorCommunityDIDsOf(listed), fixture.otherComm,
			"one aggregator's grant list included another's; this list is what an aggregator's "+
				"operator reads to know where their bot may post")

		others, err := fixture.repo.ListAuthorizationsForAggregator(ctx, fixture.other, false, 10, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{fixture.otherComm}, aggregatorCommunityDIDsOf(others))
	})

	t.Run("enabledOnly drops the withdrawn grants", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		listed, err := fixture.repo.ListAuthorizationsForAggregator(ctx, fixture.subject, true, 10, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{fixture.newest, fixture.oldest}, aggregatorCommunityDIDsOf(listed),
			"a disabled grant is not a place the aggregator may post, and listing it as one would "+
				"send the bot at a community that has turned it off")
	})

	t.Run("paginates", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		first, err := fixture.repo.ListAuthorizationsForAggregator(ctx, fixture.subject, false, 2, 0)
		require.NoError(t, err)
		second, err := fixture.repo.ListAuthorizationsForAggregator(ctx, fixture.subject, false, 2, 2)
		require.NoError(t, err)

		assert.Equal(t, []string{fixture.newest, fixture.middle}, aggregatorCommunityDIDsOf(first))
		assert.Equal(t, []string{fixture.oldest}, aggregatorCommunityDIDsOf(second))

		past, err := fixture.repo.ListAuthorizationsForAggregator(ctx, fixture.subject, false, 2, 3)
		require.NoError(t, err)
		assert.Empty(t, past)
	})

	t.Run("an aggregator nobody has authorized lists nothing", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Unwanted")
		listed, err := repo.ListAuthorizationsForAggregator(ctx, did, false, 10, 0)
		require.NoError(t, err)
		assert.Empty(t, listed, "no grants must be an empty answer rather than an error")
	})
}

// TestAggregatorRepo_GetRecentPosts covers the read behind the aggregator rate
// limit.
//
// CountRecentPosts answers "how many"; this answers "which", and the service
// uses it to report the window contents when a limit is hit. Both share the two
// predicates that matter, and both are only as good as those predicates: a
// window that leaked another community's posts would spend a quota the
// aggregator never used there, and a window that ignored `since` would make the
// limit permanent rather than rolling.
func TestAggregatorRepo_GetRecentPosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type aggregatorLedgerFixture struct {
		repo         aggregators.Repository
		aggregatorID string
		otherAgg     string
		communityID  string
		otherComm    string
		recent       string
		older        string
		ancient      string
	}
	seed := func(t *testing.T) aggregatorLedgerFixture {
		t.Helper()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)
		now := time.Now().UTC()

		fixture := aggregatorLedgerFixture{repo: repo}
		fixture.aggregatorID = indexAggregator(t, repo, "Prolific")
		fixture.otherAgg = indexAggregator(t, repo, "Rival")
		fixture.communityID = indexAuthorizingCommunity(t, db)
		fixture.otherComm = indexAuthorizingCommunity(t, db)

		fixture.recent = "at://" + fixture.communityID + "/social.coves.community.post/recent"
		fixture.older = "at://" + fixture.communityID + "/social.coves.community.post/older"
		fixture.ancient = "at://" + fixture.communityID + "/social.coves.community.post/ancient"

		aggregatorRecordPostAt(t, db, fixture.aggregatorID, fixture.communityID, fixture.recent, now.Add(-5*time.Minute))
		aggregatorRecordPostAt(t, db, fixture.aggregatorID, fixture.communityID, fixture.older, now.Add(-45*time.Minute))
		aggregatorRecordPostAt(t, db, fixture.aggregatorID, fixture.communityID, fixture.ancient, now.Add(-25*time.Hour))

		// Same aggregator, different community: a separate quota.
		aggregatorRecordPostAt(t, db, fixture.aggregatorID, fixture.otherComm,
			"at://"+fixture.otherComm+"/social.coves.community.post/elsewhere", now.Add(-5*time.Minute))
		// Same community, different aggregator: someone else's quota.
		aggregatorRecordPostAt(t, db, fixture.otherAgg, fixture.communityID,
			"at://"+fixture.communityID+"/social.coves.community.post/rival", now.Add(-5*time.Minute))

		return fixture
	}

	t.Run("returns the window's posts newest first", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		within, err := fixture.repo.GetRecentPosts(ctx, fixture.aggregatorID, fixture.communityID,
			time.Now().Add(-time.Hour))
		require.NoError(t, err)
		assert.Equal(t, []string{fixture.recent, fixture.older}, aggregatorPostURIs(within),
			"the ledger is read newest first so an operator investigating a rate-limit rejection sees "+
				"the posts that caused it, not the ones that aged out")
	})

	// Both sides of the boundary, because only one of them fails loudly. A
	// window that ignored `since` would still return the recent posts and would
	// simply never let the limit reset.
	t.Run("excludes posts older than the window", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		hour, err := fixture.repo.GetRecentPosts(ctx, fixture.aggregatorID, fixture.communityID,
			time.Now().Add(-time.Hour))
		require.NoError(t, err)
		assert.NotContains(t, aggregatorPostURIs(hour), fixture.ancient,
			"a post from yesterday is not in the last hour; counting it keeps an aggregator in "+
				"permanent timeout after one busy day")

		day, err := fixture.repo.GetRecentPosts(ctx, fixture.aggregatorID, fixture.communityID,
			time.Now().Add(-30*time.Hour))
		require.NoError(t, err)
		assert.Equal(t, []string{fixture.recent, fixture.older, fixture.ancient}, aggregatorPostURIs(day),
			"widening the window must bring the older post back; if it does not, the filter is not "+
				"the one under test")

		none, err := fixture.repo.GetRecentPosts(ctx, fixture.aggregatorID, fixture.communityID,
			time.Now().Add(time.Minute))
		require.NoError(t, err)
		assert.Empty(t, none, "a window that starts in the future contains nothing")
	})

	t.Run("scopes to one aggregator in one community", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		mine, err := fixture.repo.GetRecentPosts(ctx, fixture.aggregatorID, fixture.communityID,
			time.Now().Add(-time.Hour))
		require.NoError(t, err)
		uris := aggregatorPostURIs(mine)
		assert.NotContains(t, uris, "at://"+fixture.otherComm+"/social.coves.community.post/elsewhere",
			"a quota is per community; folding in another community's posts spends a limit the "+
				"aggregator never used here")
		assert.NotContains(t, uris, "at://"+fixture.communityID+"/social.coves.community.post/rival",
			"one aggregator's posts must not count against another's quota — that is free posting for "+
				"whoever is quietest")

		rival, err := fixture.repo.GetRecentPosts(ctx, fixture.otherAgg, fixture.communityID,
			time.Now().Add(-time.Hour))
		require.NoError(t, err)
		assert.Equal(t, []string{"at://" + fixture.communityID + "/social.coves.community.post/rival"},
			aggregatorPostURIs(rival))
	})

	t.Run("hydrates the ledger row the caller reports on", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		within, err := fixture.repo.GetRecentPosts(ctx, fixture.aggregatorID, fixture.communityID,
			time.Now().Add(-time.Hour))
		require.NoError(t, err)
		require.NotEmpty(t, within)

		assert.NotZero(t, within[0].ID)
		assert.Equal(t, fixture.aggregatorID, within[0].AggregatorDID)
		assert.Equal(t, fixture.communityID, within[0].CommunityDID)
		assert.WithinDuration(t, time.Now().Add(-5*time.Minute), within[0].CreatedAt, time.Minute)
	})

	t.Run("an aggregator that has posted nothing here lists nothing", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		quiet, err := fixture.repo.GetRecentPosts(ctx, fixture.otherAgg, fixture.otherComm,
			time.Now().Add(-time.Hour))
		require.NoError(t, err)
		assert.Empty(t, quiet)
	})
}
