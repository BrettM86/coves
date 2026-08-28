//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
)

// TestCommunityRepo_OriginRoundTrip pins the column/Scan alignment of every
// read path in community_repo.go for the `origin` column: Create writes it,
// and GetByDID, GetByHandle, List and Search each read it back — or, if a
// SELECT and its Scan drifted, fail to read the row at all. Update replaces it
// wholesale, including clearing it.
func TestCommunityRepo_OriginRoundTrip(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	repo := postgres.NewCommunityRepository(db)

	suffix := testkit.UniqueID(t)
	name := fmt.Sprintf("originrt%s", suffix)
	community := &communities.Community{
		DID:          fmt.Sprintf("did:plc:originrt%s", suffix),
		Handle:       name + ".lemmy-world.tdpl.io",
		Name:         name,
		DisplayName:  "Origin round trip " + suffix,
		Description:  "origin round trip " + suffix,
		OwnerDID:     fmt.Sprintf("did:plc:originrt%s", suffix),
		CreatedByDID: "did:plc:originrtcreator",
		HostedByDID:  "did:web:tdpl.io",
		PDSURL:       "https://pds.tdpl.io",
		Visibility:   "public",
		Origin:       "lemmy.world",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	_, err := repo.Create(ctx, community)
	require.NoError(t, err)

	byDID, err := repo.GetByDID(ctx, community.DID)
	require.NoError(t, err)
	assert.Equal(t, "lemmy.world", byDID.Origin, "GetByDID")
	assert.Equal(t, "!"+name+"@lemmy.world", byDID.GetDisplayHandle())

	byHandle, err := repo.GetByHandle(ctx, community.Handle)
	require.NoError(t, err)
	assert.Equal(t, "lemmy.world", byHandle.Origin, "GetByHandle")

	listed, err := repo.List(ctx, communities.ListCommunitiesRequest{Sort: "new", Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, "lemmy.world", originOf(listed, community.DID), "List")

	found, _, err := repo.Search(ctx, communities.SearchCommunitiesRequest{Query: name, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, "lemmy.world", originOf(found, community.DID), "Search")

	// Update replaces the stored value from the record; an empty one clears it.
	byDID.Origin = ""
	_, err = repo.Update(ctx, byDID)
	require.NoError(t, err)
	cleared, err := repo.GetByDID(ctx, community.DID)
	require.NoError(t, err)
	assert.Empty(t, cleared.Origin, "Update must be able to clear origin")
}

func originOf(rows []*communities.Community, did string) string {
	for _, c := range rows {
		if c.DID == did {
			return c.Origin
		}
	}
	return "<row not returned>"
}

// TestCommunityRepo_GetByNameAndOrigin pins the resolver's remote-origin
// lookup: exact (name, origin) match, a miss for an unknown pair, and an
// explicit ambiguity error when a second row carries the same pair rather
// than an arbitrary pick between them.
func TestCommunityRepo_GetByNameAndOrigin(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	repo := postgres.NewCommunityRepository(db)

	suffix := testkit.UniqueID(t)
	name := "pair" + suffix
	bridged := func(handleLabel, origin string) *communities.Community {
		did := fmt.Sprintf("did:plc:%s%s", handleLabel, suffix)
		community := &communities.Community{
			DID:          did,
			Handle:       handleLabel + suffix + ".lemmy-world.tdpl.io",
			Name:         name,
			DisplayName:  "Pair lookup " + handleLabel,
			OwnerDID:     did,
			CreatedByDID: "did:plc:paircreator",
			HostedByDID:  "did:web:tdpl.io",
			PDSURL:       "https://pds.tdpl.io",
			Visibility:   "public",
			Origin:       origin,
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		created, err := repo.Create(ctx, community)
		require.NoError(t, err)
		return created
	}

	world := bridged("world", "lemmy.world")
	// Same name on a different origin must not collide.
	ml := bridged("ml", "lemmy.ml")

	got, err := repo.GetByNameAndOrigin(ctx, name, "lemmy.world")
	require.NoError(t, err)
	assert.Equal(t, world.DID, got.DID)
	assert.Equal(t, "lemmy.world", got.Origin, "the row must come back fully scanned")
	assert.Equal(t, world.Handle, got.Handle)

	got, err = repo.GetByNameAndOrigin(ctx, name, "lemmy.ml")
	require.NoError(t, err)
	assert.Equal(t, ml.DID, got.DID)

	_, err = repo.GetByNameAndOrigin(ctx, name, "lemmy.zip")
	assert.ErrorIs(t, err, communities.ErrCommunityNotFound)

	_, err = repo.GetByNameAndOrigin(ctx, "someoneelse"+suffix, "lemmy.world")
	assert.ErrorIs(t, err, communities.ErrCommunityNotFound)

	// A row with no origin never matches, even for the empty string.
	_, err = repo.GetByNameAndOrigin(ctx, name, "")
	assert.ErrorIs(t, err, communities.ErrCommunityNotFound)

	// The collision case: Tidepool suffixes the handle label, the origin stays.
	bridged("world-2", "lemmy.world")
	_, err = repo.GetByNameAndOrigin(ctx, name, "lemmy.world")
	assert.ErrorIs(t, err, communities.ErrAmbiguousCommunity)
	assert.NotErrorIs(t, err, communities.ErrCommunityNotFound)
}

// Records spell the name however they like (ComicStrips); identifiers are
// lower-cased before lookup. The pair lookup has to meet in the middle.
func TestCommunityRepo_GetByNameAndOriginIgnoresNameCase(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	repo := postgres.NewCommunityRepository(db)

	suffix := testkit.UniqueID(t)
	did := "did:plc:mixedcase" + suffix
	created, err := repo.Create(ctx, &communities.Community{
		DID:          did,
		Handle:       "mixedcase" + suffix + ".lemmy-world.tdpl.io",
		Name:         "MixedCase" + suffix,
		DisplayName:  "Mixed case name",
		OwnerDID:     did,
		CreatedByDID: "did:plc:mixedcasecreator",
		HostedByDID:  "did:web:tdpl.io",
		PDSURL:       "https://pds.tdpl.io",
		Visibility:   "public",
		Origin:       "lemmy.world",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err)

	got, err := repo.GetByNameAndOrigin(ctx, "mixedcase"+suffix, "lemmy.world")
	require.NoError(t, err)
	assert.Equal(t, created.DID, got.DID)
	assert.Equal(t, "MixedCase"+suffix, got.Name, "the stored spelling is preserved")
}
