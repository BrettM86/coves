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
