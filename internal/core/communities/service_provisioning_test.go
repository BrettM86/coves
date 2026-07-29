//go:build integration

package communities_test

import (
	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"context"
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
// side — the provisioner, the credential storage, the write-forwards — is the
// production path.
func newCommunityService(t *testing.T) (communities.Service, communities.Repository, *testkit.PDS) {
	t.Helper()

	repo := postgres.NewCommunityRepository(testkit.DB(t))
	pdsServer := testkit.NewPDS(t)
	service := communities.NewCommunityServiceWithPDSFactory(
		repo,
		pdsServer.URL(),
		instanceDID,
		instanceDomain,
		communities.NewPDSAccountProvisioner(instanceDomain, pdsServer.URL()),
		testkit.PasswordAuthFactory(pds.NewFromAccessToken),
		nil,
	)
	return service, repo, pdsServer
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
