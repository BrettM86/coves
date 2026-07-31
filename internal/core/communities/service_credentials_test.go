//go:build integration

package communities_test

import (
	"Coves/internal/core/communities"
	"Coves/tests/testkit"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The credentials a community is provisioned with, and the two properties they
// have to hold at once.
//
// A community's PDS account is the AppView's to drive: every profile edit, and
// every post written into a community's repo, authenticates as the community.
// That makes the stored password unlike any other secret in the system. It has
// to be RECOVERABLE — hashing it would be the ordinary answer and would be
// wrong, because com.atproto.server.createSession needs the cleartext when the
// refresh token finally expires — and it has to be UNREADABLE at rest, because
// a database dump would otherwise be a set of live logins to every community
// this instance hosts.
//
// pgcrypto's pgp_sym_encrypt is what reconciles the two, applied in the
// repository's SQL rather than in Go. Nothing in the type system says so: the
// Community struct carries a plain string on both sides of the write, and a
// repository that quietly stopped encrypting would still round-trip perfectly.
// The only way to see the difference is to read the column directly, which is
// why these tests hold the database handle as well as the service.
//
// # WHAT IS ASSERTED ELSEWHERE
//
// Encryption of the token columns and their behaviour on empty input belong to
// the repository and are covered by TestCommunityRepository_EncryptedCredentials
// and TestCommunityRepository_CredentialPersistence. What is here is the
// provisioning seam: what the service PRODUCES, that it survives the trip
// through the repository, and that what comes back still works against a real
// PDS.

// knownWeakPasswords are the strings a hardcoded test credential tends to be.
// The provisioner draws 32 bytes from crypto/rand, so hitting one of these is
// not a coincidence to investigate — it means the random path was bypassed.
var knownWeakPasswords = []string{"", "test-password", "password123", "admin", "changeme"}

func TestService_CreateStoresCredentialsEncryptedAndRecoverable(t *testing.T) {
	t.Parallel()

	service, repo, pdsServer, db := newCommunityServiceWithDatabase(t)
	ctx := context.Background()

	name := testkit.UniqueIDWithPrefix(t, "cred")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "Credential Storage",
		Description:            "credentials that must survive encryption and still log in",
		Visibility:             "public",
		CreatedByDID:           "did:plc:credentialstorage",
		AllowExternalDiscovery: true,
	})
	require.NoError(t, err)

	// ---- what the provisioner produced ---------------------------------------

	assert.GreaterOrEqual(t, len(community.PDSPassword), 32,
		"the provisioner draws a 32-character password; a shorter one means the length argument moved")
	for _, weak := range knownWeakPasswords {
		assert.NotEqual(t, weak, community.PDSPassword,
			"the password is a hardcoded literal, not generated")
	}
	assert.Equal(t, pdsServer.URL(), community.PDSURL,
		"the row has to record WHICH PDS these credentials are for, or a refresh has nowhere to go")

	// ---- what the database holds --------------------------------------------

	var encryptedPassword []byte
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT pds_password_encrypted FROM communities WHERE did = $1`, community.DID,
	).Scan(&encryptedPassword))

	require.NotEmpty(t, encryptedPassword,
		"the column is empty: the password was never stored and this community can never re-authenticate")
	assert.NotEqual(t, community.PDSPassword, string(encryptedPassword),
		"CRITICAL: the password is in the database in cleartext")
	assert.False(t, strings.HasPrefix(string(encryptedPassword), "$2"),
		"the password looks bcrypt-hashed. A hash is the right answer for a password you VERIFY and the "+
			"wrong one for a password you must REPLAY: createSession needs the cleartext, so a hashed "+
			"column locks every community out the moment its refresh token expires")

	// ---- and that it comes back usable --------------------------------------

	stored, err := repo.GetByDID(ctx, community.DID)
	require.NoError(t, err)
	assert.Equal(t, community.PDSPassword, stored.PDSPassword, "password did not survive the encryption round trip")
	assert.Equal(t, community.PDSAccessToken, stored.PDSAccessToken)
	assert.Equal(t, community.PDSRefreshToken, stored.PDSRefreshToken)

	// THE POINT OF ALL OF IT: the password read back out of the database opens a
	// real session on the community's account. This is the P0 property — token
	// renewal after the refresh token's window closes depends on exactly this
	// call — and it is the one assertion that a decryption returning
	// plausible-looking rubbish could not survive.
	session := pdsServer.Login(t, stored.Handle, stored.PDSPassword)
	assert.Equal(t, community.DID, session.DID,
		"the stored credentials opened a session on a different account than the community they belong to")
}

// TestService_ProvisionedPasswordsDifferBetweenCommunities is the entropy
// assertion, made against the generator rather than against the test's own
// inputs.
//
// Its predecessor inserted a hundred passwords it had built itself out of a
// counter and then checked that they were distinct, which is a property of the
// counter. Two real provisionings are worth more than a hundred synthetic ones:
// a generator that returned a constant, or that seeded itself per-process,
// fails here and could not fail there.
func TestService_ProvisionedPasswordsDifferBetweenCommunities(t *testing.T) {
	t.Parallel()

	service, _, _ := newCommunityService(t)
	ctx := context.Background()

	provision := func(prefix string) *communities.Community {
		name := testkit.UniqueIDWithPrefix(t, prefix)
		require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
			"the generated community name %q makes a handle label the PDS will refuse", name)
		community, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   name,
			DisplayName:            "Password Entropy",
			Visibility:             "public",
			CreatedByDID:           "did:plc:passwordentropy",
			AllowExternalDiscovery: true,
		})
		require.NoError(t, err)
		return community
	}

	first, second := provision("pw1"), provision("pw2")

	assert.NotEqual(t, first.PDSPassword, second.PDSPassword,
		"two communities were provisioned with the SAME password: whoever learns one community's "+
			"credentials holds every community's")
	assert.NotEqual(t, first.PDSEmail, second.PDSEmail,
		"the account email is derived from the name and must be as unique as the handle")
	for _, community := range []*communities.Community{first, second} {
		assert.GreaterOrEqual(t, len(community.PDSPassword), 32)
	}
}

// TestService_EnsureFreshTokenRenewsAnExpiringTokenAndPersistsIt exercises the
// refresh path end to end against a real PDS.
//
// # WHY IT IS SHAPED THIS WAY
//
// A freshly provisioned access token is valid for hours, so nothing would
// refresh during a test run. The token is therefore REPLACED with an expired
// one while the genuine refresh token is left in place — which is precisely the
// state a community reaches on its own a couple of hours after provisioning,
// reproduced in a second instead of waited for.
//
// # WHY PERSISTENCE IS THE ASSERTION THAT MATTERS
//
// atProto refresh tokens are SINGLE USE: com.atproto.server.refreshSession
// revokes the one it was given. So a refresh that succeeds and then fails to
// write the new pair back leaves the community holding a revoked refresh token
// and an access token that is about to die — locked out until someone notices.
// The service's own comment calls this "COMMUNITY LOCKED OUT". Checking only
// the returned struct would pass in exactly that scenario, so the assertions
// are made against a re-read of the row.
func TestService_EnsureFreshTokenRenewsAnExpiringTokenAndPersistsIt(t *testing.T) {
	t.Parallel()

	service, repo, _ := newCommunityService(t)
	ctx := context.Background()

	name := testkit.UniqueIDWithPrefix(t, "tkn")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "Token Refresh",
		Visibility:             "public",
		CreatedByDID:           "did:plc:tokenrefresh",
		AllowExternalDiscovery: true,
	})
	require.NoError(t, err)

	// A token that expired a minute ago, paired with the real refresh token.
	expired := jwtExpiringAt(time.Now().Add(-time.Minute))
	require.NoError(t, repo.UpdateCredentials(ctx, community.DID, expired, community.PDSRefreshToken))

	refreshed, err := service.EnsureFreshToken(ctx, community)
	require.NoError(t, err, "the community could not renew its own session")

	assert.NotEqual(t, expired, refreshed.PDSAccessToken, "the expired token was handed straight back")
	assert.NotEqual(t, community.PDSRefreshToken, refreshed.PDSRefreshToken,
		"refreshSession issues a new refresh token and revokes the old one; an unchanged value here "+
			"means the response was not read")

	stillStale, err := communities.NeedsRefresh(refreshed.PDSAccessToken)
	require.NoError(t, err, "the renewed access token is not a parseable JWT")
	assert.False(t, stillStale, "the renewed access token is already inside the five-minute refresh window")

	stored, err := repo.GetByDID(ctx, community.DID)
	require.NoError(t, err)
	assert.Equal(t, refreshed.PDSAccessToken, stored.PDSAccessToken,
		"the refreshed access token was not persisted")
	assert.Equal(t, refreshed.PDSRefreshToken, stored.PDSRefreshToken,
		"the single-use refresh token was not persisted: this community is now locked out")
	assert.Equal(t, community.PDSPassword, stored.PDSPassword,
		"a token refresh must not disturb the password — it is the fallback the whole scheme rests on "+
			"once the refresh token's own window closes")
}

// TestService_EnsureFreshTokenLeavesAValidTokenAlone is the other half: the
// common case, where nothing should happen.
//
// A refresh that fires when it does not need to is not harmless. It spends the
// single-use refresh token, so a service that refreshed on every write would
// turn one PDS round trip per post into two and would serialise every write to
// a community behind the per-community refresh mutex.
func TestService_EnsureFreshTokenLeavesAValidTokenAlone(t *testing.T) {
	t.Parallel()

	service, _, _ := newCommunityService(t)
	ctx := context.Background()

	name := testkit.UniqueIDWithPrefix(t, "nrf")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "No Refresh Needed",
		Visibility:             "public",
		CreatedByDID:           "did:plc:tokenrefresh",
		AllowExternalDiscovery: true,
	})
	require.NoError(t, err)

	unchanged, err := service.EnsureFreshToken(ctx, community)
	require.NoError(t, err)
	assert.Equal(t, community.PDSAccessToken, unchanged.PDSAccessToken,
		"a freshly issued access token is nowhere near expiry and must be left as it is")
	assert.Equal(t, community.PDSRefreshToken, unchanged.PDSRefreshToken)
}
