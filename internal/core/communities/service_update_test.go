//go:build integration

package communities_test

import (
	"Coves/internal/core/communities"
	"Coves/tests/testkit"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Editing a community: whose credentials the write goes out on, and who is
// allowed to ask for it.
//
// UpdateCommunity is the only write path in this package that authenticates as
// the COMMUNITY rather than as a user or as the instance. It reads the row,
// checks the caller, refreshes the community's own PDS token, and puts a new
// profile record into the community's repo at rkey "self". Three things in that
// sentence can silently go wrong, and each has a test here:
//
//   - the write could target the wrong repo (the instance's, as V1 did), which
//     produces a record no consumer attributes to the community;
//   - it could accept a caller who is not the creator, which is a takeover;
//   - it could be attempted for a community whose credentials this AppView does
//     not hold — every federated community — which must fail cleanly rather
//     than half-writing.
//
// The avatar and banner half of the same path is asserted in
// service_provisioning_test.go's
// TestService_CreateAndUpdateWriteBlobRefsIntoTheProfileRecord; what is here is
// the text fields, the authorization gate, and the credential precondition.

// newUpdatableCommunity provisions a community and returns everything a caller
// needs to check both sides of an update: the service that performs it, the
// repository the AppView reads, the PDS that holds the record, and the creator
// DID that is allowed to ask.
func newUpdatableCommunity(t *testing.T, prefix string) (
	communities.Service, communities.Repository, *testkit.PDS, *communities.Community, string,
) {
	t.Helper()

	service, repo, pdsServer := newCommunityService(t)

	const creatorDID = "did:plc:communityupdater"
	name := testkit.UniqueIDWithPrefix(t, prefix)
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := service.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "Original Display Name",
		Description:            "Original description",
		Visibility:             "public",
		CreatedByDID:           creatorDID,
		AllowExternalDiscovery: true,
	})
	require.NoError(t, err)

	return service, repo, pdsServer, community, creatorDID
}

func TestService_UpdateWritesTheChangedProfileToTheCommunityRepo(t *testing.T) {
	t.Parallel()

	service, repo, pdsServer, community, creatorDID := newUpdatableCommunity(t, "upd")
	ctx := context.Background()

	displayName := "Updated Display Name"
	description := "Updated description, written with the community's own credentials"
	visibility := "unlisted"

	updated, err := service.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
		CommunityDID: community.DID,
		UpdatedByDID: creatorDID,
		DisplayName:  &displayName,
		Description:  &description,
		Visibility:   &visibility,
		// AllowExternalDiscovery is left nil on purpose: an unset field must be
		// carried over from the existing record, not reset to the zero value.
	})
	require.NoError(t, err)

	assert.Equal(t, displayName, updated.DisplayName)
	assert.Equal(t, description, updated.Description)
	assert.Equal(t, visibility, updated.Visibility)
	assert.True(t, updated.AllowExternalDiscovery,
		"a field the request did not mention must survive the update")

	// V2: the record stays in the COMMUNITY's repo, at the canonical key. An
	// update that moved it — to the instance's repo, or to a fresh TID — would
	// leave the original record in place and index as a second community.
	assert.Equal(t, "at://"+community.DID+"/social.coves.community.profile/self", updated.RecordURI)
	assert.NotEqual(t, community.RecordCID, updated.RecordCID,
		"a new version of the record must have a new CID; an unchanged one means nothing was written")

	// THE WRITE ITSELF, asked of the PDS rather than of the return value. The
	// service builds its answer from the request, so every assertion above holds
	// even if the record never left the process.
	record := pdsServer.Login(t, community.Handle, community.PDSPassword).
		GetRecord(t, "social.coves.community.profile", "self")
	assert.Equal(t, displayName, record.Value["displayName"])
	assert.Equal(t, description, record.Value["description"])
	assert.Equal(t, visibility, record.Value["visibility"])
	assert.Equal(t, updated.RecordCID, record.CID,
		"the CID the service reported must be the one the repo now holds")
	assert.Equal(t, community.Name, record.Value["name"],
		"the name is immutable: it is baked into the handle and cannot be edited")
	assert.Equal(t, instanceDID, record.Value["hostedBy"],
		"hostedBy is re-stamped from the instance on every write, never taken from the request")

	// THE INDEXING ASYMMETRY. CreateCommunity writes the AppView row itself;
	// UpdateCommunity does not, and leaves it to the firehose consumer. So the
	// row still holds the pre-update values here, and that is correct.
	//
	// It is asserted rather than merely noted because the two behaviours are
	// easy to "fix" into each other: if this ever starts matching the new
	// display name, the update path has begun writing Postgres synchronously,
	// and every pipeline test that watches an update arrive through the consumer
	// becomes a test that would pass with the consumer switched off.
	indexed, err := repo.GetByDID(ctx, community.DID)
	require.NoError(t, err)
	assert.Equal(t, "Original Display Name", indexed.DisplayName,
		"UpdateCommunity does not index; the row is the consumer's job")
}

func TestService_UpdateRejectsACallerWhoIsNotTheCreator(t *testing.T) {
	t.Parallel()

	service, _, pdsServer, community, _ := newUpdatableCommunity(t, "aut")
	ctx := context.Background()

	displayName := "Taken Over"
	_, err := service.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
		CommunityDID: community.DID,
		UpdatedByDID: "did:plc:nottheowner",
		DisplayName:  &displayName,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, communities.ErrUnauthorized),
		"a non-creator must be refused with the typed error the handler maps to 403, got %v", err)

	// The check runs BEFORE the token refresh and before any blob upload, which
	// is what keeps an unauthorized request from costing a PDS round trip — but
	// the assertion that matters to a reader is simpler: nothing was written.
	record := pdsServer.Login(t, community.Handle, community.PDSPassword).
		GetRecord(t, "social.coves.community.profile", "self")
	assert.Equal(t, "Original Display Name", record.Value["displayName"],
		"the rejected update reached the community's repo anyway")
}

// TestService_UpdateRefusesACommunityWhoseCredentialsThisInstanceLacks covers
// every FEDERATED community: rows indexed from the firehose carry no PDS
// password and no tokens, because their accounts live on somebody else's
// server. An update aimed at one of them cannot be performed and must not be
// half-performed.
//
// # A DEAD BRANCH THIS PINS
//
// service.go has an explicit "community %s missing PDS credentials - cannot
// update" guard immediately before the record write, and it is unreachable: the
// EnsureFreshToken call earlier in the same function parses the access token,
// and an empty token fails there first with "access token is empty". The error
// a caller actually receives is therefore the token-refresh one, which this
// test asserts, rather than the message the code appears to promise. Asserting
// the real text is the only way the day someone reorders those two checks shows
// up as a test change rather than as a silently different error string.
func TestService_UpdateRefusesACommunityWhoseCredentialsThisInstanceLacks(t *testing.T) {
	t.Parallel()

	service, repo, _ := newCommunityService(t)
	ctx := context.Background()

	// Seeded straight into the index, the way the community consumer would
	// index a community hosted elsewhere: a DID, a handle, and no credentials.
	name := testkit.UniqueIDWithPrefix(t, "fed")
	federatedDID := "did:plc:" + name
	_, err := repo.Create(ctx, &communities.Community{
		DID:          federatedDID,
		Handle:       "c-" + name + ".external.social",
		Name:         name,
		OwnerDID:     federatedDID,
		CreatedByDID: "did:plc:externaluser",
		HostedByDID:  "did:web:external.social",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err)

	displayName := "Cannot Update This"
	_, err = service.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
		CommunityDID: federatedDID,
		UpdatedByDID: "did:plc:externaluser",
		DisplayName:  &displayName,
	})
	require.Error(t, err, "an update must not be attempted for a community this instance cannot authenticate as")
	assert.ErrorContains(t, err, "failed to ensure fresh credentials")
	assert.ErrorContains(t, err, "access token is empty")

	// And the row is untouched: the failure happened before any write, so a
	// caller retrying against the right instance still sees the indexed values.
	indexed, err := repo.GetByDID(ctx, federatedDID)
	require.NoError(t, err)
	assert.Empty(t, indexed.DisplayName)
}
