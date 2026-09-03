//go:build integration

package postgres

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HOSTING IS CREDENTIAL PRESENCE, AND IT HAS TO BE ASKED FOR DIRECTLY.
//
// The re-materialization tool can only re-materialize (and then DELETE) posts in
// a community whose repo it can sign for, so its whole scope is "the communities
// whose PDS refresh token this AppView stores". The obvious way to get that —
// walk `List` and test `community.PDSRefreshToken != ""` — is silently WRONG:
// List does not select the credential columns at all, so the field is the empty
// string on every listed row and the filter excludes EVERY community. A default
// all-communities production run then migrates nothing while reporting a clean,
// complete, exit-0 census.
//
// Widening List to carry decrypted secrets is the wrong repair: it would leak a
// refresh token into a general-purpose listing path that feeds public endpoints.
// The right one is this query — purpose-named, DIDs only, no credential material
// crossing the boundary at all.
func TestHostedCommunityDIDs_SelectsOnlyCommunitiesWithStoredCredentials(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	id := testkit.UniqueID(t)

	hostedDID := "did:plc:hosted" + id
	remoteDID := "did:plc:remote" + id

	// A community this AppView hosts: it holds the refresh token, so it can sign
	// writes and deletes in that repo.
	_, err := repo.Create(ctx, &communities.Community{
		DID:             hostedDID,
		Handle:          fmt.Sprintf("!hosted-%s@coves.local", id),
		Name:            "hosted-" + id,
		OwnerDID:        hostedDID,
		CreatedByDID:    "did:plc:user123",
		HostedByDID:     "did:web:coves.local",
		Visibility:      "public",
		PDSEmail:        "hosted@communities.coves.local",
		PDSPassword:     "cleartext",
		PDSAccessToken:  "access-token",
		PDSRefreshToken: "refresh-token-" + id,
		PDSURL:          testkit.Endpoints().PDS.BaseURL,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	})
	require.NoError(t, err)

	// A community indexed off the firehose from somewhere else: no credentials,
	// so nothing this tool does may ever touch it.
	_, err = repo.Create(ctx, &communities.Community{
		DID:          remoteDID,
		Handle:       fmt.Sprintf("!remote-%s@elsewhere.example", id),
		Name:         "remote-" + id,
		OwnerDID:     remoteDID,
		CreatedByDID: "did:plc:user456",
		HostedByDID:  "did:web:elsewhere.example",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err)

	dids, err := NewHostedCommunityQuery(db).HostedCommunityDIDs(ctx)
	require.NoError(t, err)

	assert.Containsf(t, dids, hostedDID,
		"a community whose PDS refresh token is stored was NOT returned. This is the tool's entire scope: if the query returns nothing, "+
			"a default all-communities production run migrates ZERO posts and still reports a complete, exit-0 census")
	assert.NotContainsf(t, dids, remoteDID,
		"a community with no stored credentials was returned. The tool cannot sign for it, so listing it would fail the run at the first "+
			"listRecords — or worse, invite a delete it has no right to make")
}

// The query must never carry credential material across the boundary. Returning
// DIDs is what makes it safe to call from a batch tool that logs its scope.
func TestHostedCommunityDIDs_ReturnsIdentifiersOnlyNeverSecrets(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	id := testkit.UniqueID(t)
	did := "did:plc:secret" + id
	secret := "refresh-token-that-must-not-escape-" + id

	_, err := repo.Create(ctx, &communities.Community{
		DID:             did,
		Handle:          fmt.Sprintf("!secret-%s@coves.local", id),
		Name:            "secret-" + id,
		OwnerDID:        did,
		CreatedByDID:    "did:plc:user123",
		HostedByDID:     "did:web:coves.local",
		Visibility:      "public",
		PDSEmail:        "secret@communities.coves.local",
		PDSRefreshToken: secret,
		PDSURL:          testkit.Endpoints().PDS.BaseURL,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	})
	require.NoError(t, err)

	dids, err := NewHostedCommunityQuery(db).HostedCommunityDIDs(ctx)
	require.NoError(t, err)

	for _, got := range dids {
		assert.NotContainsf(t, got, secret,
			"the hosted-community query returned credential material; it must answer with DIDs alone so a batch tool can log its scope safely")
	}
}
