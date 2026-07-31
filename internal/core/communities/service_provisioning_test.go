//go:build integration

package communities_test

import (
	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What creating a community actually produces on the PDS.
//
// tests/integration/community_service_integration_test.go already covers the
// service's own view of provisioning — the returned DID, handle, record URI and
// the credentials landing encrypted in Postgres. What it does NOT check, and
// what tests/integration/community_e2e_test.go's deleted queryPDSAccount step
// did, is the binding on the OTHER side: that the handle the service reports is
// a handle the PDS will actually resolve, to this community's DID.
//
// That is not a formality. The service derives the handle by string
// construction (pds_provisioning.go: "c-%s.%s") and reports it from its own
// request rather than from the PDS' answer, so a PDS that normalised, rejected
// or reassigned it would leave the AppView holding a handle nothing resolves —
// and every client that addresses a community by handle would 404 while the row
// looked perfectly healthy. The AppView's own community consumer depends on the
// same binding when it resolves a federated community's handle from its DID
// document.

const (
	// The instance identity these tests provision under. It matches the
	// AppView's default (internal/config: INSTANCE_DID defaults to
	// did:web:coves.social, and the domain is derived from it), and the domain
	// must be one of the PDS' PDS_SERVICE_HANDLE_DOMAINS or account creation is
	// refused.
	instanceDID    = "did:web:coves.social"
	instanceDomain = "coves.social"
)

// newCommunityService builds the service under test over a fresh database clone
// and the test PDS.
//
// It is wired the way cmd/server wires it, with one substitution: the PDS
// client factory is password auth rather than OAuth/DPoP, so a test can hold a
// user session without an authorization-code flow. Everything on the community
// side — the provisioner, the credential storage, the blob uploads, the
// write-forwards — is the production path.
//
// The blob service is REAL (blobs.NewBlobService against the test PDS, exactly
// as cmd/server/wiring.go builds it). It used to be nil here, which was
// invisible for as long as no test uploaded an image: the service's avatar path
// fails closed with "blob service not configured", so a nil one does not
// silently skip the upload, it refuses the request. Passing the real one is
// what lets a test assert on what an avatar becomes.
func newCommunityService(t *testing.T) (communities.Service, communities.Repository, *testkit.PDS) {
	t.Helper()
	service, repo, pdsServer, _ := newCommunityServiceWithDatabase(t)
	return service, repo, pdsServer
}

// newCommunityServiceWithDatabase is the same wiring, handing back the database
// as well.
//
// It exists because testkit.DB clones a fresh database on every call, so a test
// that has to look at a stored row with its own SQL — service_credentials_test.go
// reads the encrypted credential columns the repository never exposes — cannot
// simply ask for the database a second time. It would get an empty one, and the
// query would return no rows rather than the wrong ones, which is the kind of
// pass nobody investigates.
func newCommunityServiceWithDatabase(t *testing.T) (
	communities.Service, communities.Repository, *testkit.PDS, *sql.DB,
) {
	t.Helper()

	db := testkit.DB(t)
	repo := postgres.NewCommunityRepository(db)
	pdsServer := testkit.NewPDS(t)
	service := communities.NewCommunityServiceWithPDSFactory(
		repo,
		pdsServer.URL(),
		instanceDID,
		instanceDomain,
		communities.NewPDSAccountProvisioner(instanceDomain, pdsServer.URL()),
		testkit.PasswordAuthFactory(pds.NewFromAccessToken),
		blobs.NewBlobService(pdsServer.URL()),
	)
	return service, repo, pdsServer, db
}

func TestService_CreateProvisionsAResolvableAccount(t *testing.T) {
	t.Parallel()

	service, _, pdsServer := newCommunityService(t)
	ctx := context.Background()

	// The name is short enough that "c-" plus it stays inside the PDS' 18
	// character local-label cap — the same budget every generated handle lives
	// in, and the reason UniqueIDWithPrefix is the only generator allowed.
	name := testkit.UniqueIDWithPrefix(t, "p")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "Provisioning",
		Description:            "a community with a real PDS account",
		Visibility:             "public",
		CreatedByDID:           "did:plc:provisioningtest",
		AllowExternalDiscovery: true,
	})
	require.NoError(t, err)

	expectedHandle := "c-" + name + "." + instanceDomain
	require.Equal(t, expectedHandle, community.Handle,
		"the provisioner's handle convention is c-<name>.<instance domain>")
	require.True(t, strings.HasPrefix(community.DID, "did:plc:"),
		"the PDS mints a did:plc for the community, got %q", community.DID)

	// THE BINDING. Asked of the PDS, not of the service: this is the only
	// assertion in the suite that the handle the AppView stores is one the
	// network will resolve back to this community.
	var resolved struct {
		DID string `json:"did"`
	}
	require.NoError(t, pdsServer.Anon.Query(ctx, "com.atproto.identity.resolveHandle",
		url.Values{"handle": {expectedHandle}}, &resolved))
	assert.Equal(t, community.DID, resolved.DID,
		"the PDS resolves %s to a different DID than the community the service reported", expectedHandle)

	// And the community owns its own repository: the profile record is in the
	// community's repo at the canonical rkey, not in the instance's.
	assert.Equal(t, "at://"+community.DID+"/social.coves.community.profile/self", community.RecordURI)
	assert.Equal(t, community.DID, community.OwnerDID, "V2: a community owns itself")

	// Provisioning must also hand back a usable SESSION on the new account, not
	// only its password. Every later write into this repo — a post, a moderation
	// action — goes out on these credentials after EnsureFreshToken, and the
	// refresh token is the only thing that keeps that working past the access
	// token's lifetime. A provisioner that stored one and dropped the other would
	// look healthy here and fail hours later, on the first refresh.
	assert.NotEmpty(t, community.PDSAccessToken, "provisioning must return an access token for the community's repo")
	assert.NotEmpty(t, community.PDSRefreshToken, "without a refresh token the community's credentials expire unrecoverably")

	record := pdsServer.Login(t, expectedHandle, community.PDSPassword).
		GetRecord(t, "social.coves.community.profile", "self")
	assert.Equal(t, name, record.Value["name"])
	assert.Equal(t, instanceDID, record.Value["hostedBy"],
		"hostedBy is stamped from the instance configuration, never from the request")
	assert.Equal(t, "did:plc:provisioningtest", record.Value["createdBy"])
}

// TestService_CreateAndUpdateWriteBlobRefsIntoTheProfileRecord covers the
// community half of the avatar/banner write-forward: that an uploaded image
// reaches the community's PROFILE RECORD in the shape the firehose consumer
// reads back.
//
// # WHY IT IS ASSERTED HERE AND NOT WHERE IT LOOKS LIKE IT BELONGS
//
// The service uploads the blob to the community's PDS and then embeds the
// returned reference in the profile record. Those are two calls, and only the
// second one matters to anybody downstream: a correct upload followed by a
// reference embedded under the wrong key, or flattened to a bare CID string,
// leaves a record that stores fine, indexes fine, and renders no picture.
// jetstream.extractBlobCID declines anything that is not {$type:"blob",
// ref:{$link:...}} — silently, because a malformed image is not worth failing a
// whole profile event over — so there is no error anywhere on that path.
//
// Nothing covered it. tests/integration/community_avatar_e2e_test.go (deleted
// with this commit) came closest and could not see it: it read the AVATAR_CID
// COLUMN back through the repo, which the service writes synchronously itself
// on create, so its assertions held with the consumer switched off entirely —
// the §3.4 false-pass in its purest form. Its own consumer call had the error
// swallowed into a t.Logf with a comment saying the conflict was expected.
//
// The two neighbours this joins: the user-side equivalent is
// internal/api/handlers/user's
// TestUpdateProfileHandler_WritesTheRecordTheConsumerReadsBack, and the
// pipeline proof that a record of this shape becomes a working image URL is
// tests/e2e/community_contract_test.go.
func TestService_CreateAndUpdateWriteBlobRefsIntoTheProfileRecord(t *testing.T) {
	t.Parallel()

	service, _, pdsServer := newCommunityService(t)
	ctx := context.Background()

	name := testkit.UniqueIDWithPrefix(t, "b")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	// Created WITH an avatar and WITHOUT a banner, so the update below exercises
	// the nil→value transition as well as the replacement one.
	community, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:           name,
		DisplayName:    "Blob Write Forward",
		Description:    "an avatar that has to survive the trip",
		Visibility:     "public",
		CreatedByDID:   "did:plc:blobwriteforward",
		AvatarBlob:     testkit.TestPNG(64, 64),
		AvatarMimeType: "image/png",
	})
	require.NoError(t, err)

	account := pdsServer.Login(t, community.Handle, community.PDSPassword)

	// blobCIDOf reads a picture out of the profile record exactly the way
	// jetstream.extractBlobCID does, and fails with the reason it would have
	// declined rather than with a bare type-assertion panic.
	blobCIDOf := func(t *testing.T, record map[string]any, field string) string {
		t.Helper()
		raw, present := record[field]
		require.Truef(t, present, "the profile record has no %q field at all", field)
		blob, ok := raw.(map[string]any)
		require.Truef(t, ok, "the record's %q is a %T, not an object: a bare CID is not a blob "+
			"ref and the consumer ignores it", field, raw)
		require.Equalf(t, "blob", blob["$type"],
			"the %q ref must carry $type \"blob\" or extractBlobCID declines it", field)
		ref, ok := blob["ref"].(map[string]any)
		require.Truef(t, ok, "the %q ref has no `ref` object holding $link", field)
		link, ok := ref["$link"].(string)
		require.Truef(t, ok, "the %q ref's $link is a %T, not a string", field, ref["$link"])
		require.NotEmptyf(t, link, "the %q ref's $link is empty", field)
		return link
	}

	created := account.GetRecord(t, "social.coves.community.profile", "self")
	avatarCID := blobCIDOf(t, created.Value, "avatar")
	assert.Equal(t, avatarCID, community.AvatarCID,
		"the AppView row and the PDS record must name the same avatar blob; if they diverge, "+
			"the hydrated URL points at bytes the record does not reference")
	_, hasBanner := created.Value["banner"]
	assert.False(t, hasBanner,
		"a community created without a banner emitted a banner key: the consumer reads a "+
			"present-but-unparseable value differently from an absent one")

	// ---- update: replace the avatar and add a banner ------------------------
	displayName := "Blob Write Forward v2"
	updated, err := service.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
		CommunityDID:   community.DID,
		UpdatedByDID:   "did:plc:blobwriteforward",
		DisplayName:    &displayName,
		AvatarBlob:     testkit.TestPNG(48, 48),
		AvatarMimeType: "image/png",
		BannerBlob:     testkit.TestJPEG(96, 32),
		BannerMimeType: "image/jpeg",
	})
	require.NoError(t, err)

	afterUpdate := account.GetRecord(t, "social.coves.community.profile", "self")
	newAvatarCID := blobCIDOf(t, afterUpdate.Value, "avatar")
	bannerCID := blobCIDOf(t, afterUpdate.Value, "banner")

	assert.NotEqual(t, avatarCID, newAvatarCID,
		"different image bytes must produce a different CID in the record: an unchanged CID here "+
			"means the update re-embedded the old reference and the community keeps its old picture")
	assert.NotEqual(t, newAvatarCID, bannerCID,
		"the avatar and banner must reference different blobs; the same CID in both means one "+
			"upload's result was embedded twice")

	// THE CREATE/UPDATE ASYMMETRY, which this test discovered by asserting the
	// wrong thing first and is worth stating rather than quietly accommodating.
	//
	// CreateCommunity writes the AppView row SYNCHRONOUSLY (service.go's
	// repo.Create), which is why the create-side assertion above could compare
	// community.AvatarCID against the record. UpdateCommunity does NOT: it
	// uploads, writes the record to the PDS, and returns — leaving the row to
	// the firehose consumer. So the value returned here still carries the
	// PREVIOUS avatar and no banner at all, and that is correct behaviour, not
	// a stale read.
	//
	// It is also exactly the §3.4 distinction the whole test tier is built
	// around: the create path can be verified end-to-end in-process and
	// therefore proves nothing about the pipeline, while the update path can
	// only be completed by the consumer. The update's visible effect is
	// asserted where it becomes visible — tests/e2e/community_contract_test.go,
	// through social.coves.community.get, after the firehose has delivered it.
	assert.Equal(t, avatarCID, updated.AvatarCID,
		"UpdateCommunity does not index; the returned row should still show the pre-update "+
			"avatar. If this now matches the NEW CID, the update path started writing Postgres "+
			"synchronously — which would make the community contract's update step a false pass, "+
			"because it could then be satisfied with the consumer dead")
	assert.Empty(t, updated.BannerCID,
		"the banner added by this update reaches the row through the consumer, not through the "+
			"service; a non-empty value here means the same synchronous-indexing change")

	assert.Equal(t, "image/jpeg", afterUpdate.Value["banner"].(map[string]any)["mimeType"],
		"the blob's declared MIME type must survive into the record: the image proxy serves it "+
			"back as the response content type")
}
