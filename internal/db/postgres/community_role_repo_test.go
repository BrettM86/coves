//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertRoleTestCommunity inserts a community this instance hosts: it stores
// the community's PDS refresh token, the hosted fact (community_hosted.go).
// Nothing decrypts the stored bytes.
func insertRoleTestCommunity(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at, pds_refresh_token_encrypted)
		VALUES ($1, $2, 'Role Test Community', $1, $1, 'did:web:test.local', NOW(), $3)`,
		did, did[len("did:plc:"):]+".test", []byte{1, 'r', 'e', 'f', 'r', 'e', 's', 'h'})
	require.NoError(t, err)
}

// insertNonHostedRoleTestCommunity inserts an indexed community this instance
// does not host: it stores no PDS refresh token, although hosted_by_did names
// this instance as every firehose-indexed community's record may.
func insertNonHostedRoleTestCommunity(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
		VALUES ($1, $2, 'Non-hosted Role Test Community', $1, $1, 'did:web:test.local', NOW())`,
		did, did[len("did:plc:"):]+".test")
	require.NoError(t, err)
}

func countIndexedRoles(t *testing.T, db *sql.DB, communityDID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM community_roles WHERE community_did = $1`, communityDID).Scan(&count))
	return count
}

func communityDIDsOf(roles []communities.CommunityRole) []string {
	dids := make([]string, 0, len(roles))
	for _, role := range roles {
		dids = append(dids, role.CommunityDID)
	}
	return dids
}

// A role applies only in a community this instance hosts. Role records in an
// indexed community this instance does not host stay indexed, but neither
// read serves them.
func TestCommunityRoleRepository_NonHostedCommunityRolesAreIndexedButNotServed(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	const (
		subject   = "did:plc:rolenonhostedsubject"
		hosted    = "did:plc:rolenonhostedhosted"
		nonHosted = "did:plc:rolenonhostedother"
	)
	insertRoleTestCommunity(t, db, hosted)
	insertNonHostedRoleTestCommunity(t, db, nonHosted)
	repo := NewCommunityRoleRepository(db)
	createdAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	for _, community := range []string{hosted, nonHosted} {
		require.NoError(t, repo.UpsertCommunityRole(ctx, communities.CommunityRole{
			CommunityDID: community, SubjectDID: subject, Role: "owner",
			RecordKey: "rolenonhostedkey", RecordCID: "bafyreirolenonhosted", RecordCreatedAt: createdAt,
		}))
		require.NoError(t, repo.UpsertCommunityRole(ctx, communities.CommunityRole{
			CommunityDID: community, SubjectDID: "did:plc:rolenonhostedmod", Role: "moderator",
			RecordKey: "rolenonhostedmodkey", RecordCID: "bafyreirolenonhostedmod", RecordCreatedAt: createdAt,
		}))
	}
	require.Equal(t, 2, countIndexedRoles(t, db, nonHosted), "role records in a non-hosted community are still indexed")

	nonHostedRoles, err := repo.ListCommunityRoles(ctx, nonHosted)
	require.NoError(t, err)
	assert.Empty(t, nonHostedRoles, "a non-hosted community serves no roles")

	roster, err := communities.ReadRoster(ctx, repo, nonHosted)
	require.NoError(t, err)
	assert.Nil(t, roster.Owner, "a non-hosted community's roster has no owner")
	assert.Empty(t, roster.Moderators, "a non-hosted community's roster has no moderators")

	hostedRoles, err := repo.ListCommunityRoles(ctx, hosted)
	require.NoError(t, err)
	assert.Len(t, hostedRoles, 2, "a hosted community serves its roles")

	bySubject, err := repo.ListRolesBySubject(ctx, subject)
	require.NoError(t, err)
	assert.Equal(t, []string{hosted}, communityDIDsOf(bySubject),
		"the subject's roles list only the hosted community")
}

// Deleting a community removes its role rows, and a role row whose community
// is not indexed is never listed.
func TestCommunityRoleRepository_DeletedCommunityRolesAreRemovedAndNotListed(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	const (
		subject   = "did:plc:roledeletesubject"
		kept      = "did:plc:roledeletekept"
		deleted   = "did:plc:roledeletegone"
		unindexed = "did:plc:roledeletenorow"
	)
	insertRoleTestCommunity(t, db, kept)
	insertRoleTestCommunity(t, db, deleted)
	repo := NewCommunityRoleRepository(db)
	createdAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	for _, community := range []string{kept, deleted, unindexed} {
		require.NoError(t, repo.UpsertCommunityRole(ctx, communities.CommunityRole{
			CommunityDID: community, SubjectDID: subject, Role: "moderator",
			RecordKey: "roledeletekey", RecordCID: "bafyreiroledelete", RecordCreatedAt: createdAt,
		}))
	}

	// An open invitation in each community: deleting one community removes its
	// invitation and keeps the other's.
	keptInvitation := invitationAt("inv-roledelete-kept", kept, subject, invitationReaderNow.Add(-time.Hour))
	deletedInvitation := invitationAt("inv-roledelete-gone", deleted, subject, invitationReaderNow.Add(-time.Hour))
	applyInvitationSteps(t, NewCommunityActionRepository(db, db), []invitationStep{
		{kept, func(tx moderation.CommunityActionTx) error { return tx.CreateInvitation(ctx, keptInvitation) }},
		{deleted, func(tx moderation.CommunityActionTx) error { return tx.CreateInvitation(ctx, deletedInvitation) }},
	})

	require.NoError(t, NewCommunityRepository(db, credentialciphertest.Fixed()).Delete(ctx, deleted))
	assert.Equal(t, 0, countIndexedRoles(t, db, deleted), "deleting a community removes its role rows")
	assert.Equal(t, 1, countIndexedRoles(t, db, kept), "deleting one community leaves another's role rows")

	var invitationIDs []string
	rows, err := db.Query(`SELECT id FROM community_role_invitations WHERE community_did IN ($1, $2) ORDER BY id`, kept, deleted)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		invitationIDs = append(invitationIDs, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Equal(t, []string{"inv-roledelete-kept"}, invitationIDs,
		"deleting a community removes its invitations and leaves another community's")

	bySubject, err := repo.ListRolesBySubject(ctx, subject)
	require.NoError(t, err)
	assert.Equal(t, []string{kept}, communityDIDsOf(bySubject),
		"neither the deleted community nor a community with no communities row is listed")

	unindexedRoles, err := repo.ListCommunityRoles(ctx, unindexed)
	require.NoError(t, err)
	assert.Empty(t, unindexedRoles, "a community with no communities row serves no roles")
}

// normalizeIndexedRole drops the index time (asserted present) and pins
// createdAt to UTC so rows compare against literals.
func normalizeIndexedRole(t *testing.T, role communities.CommunityRole) communities.CommunityRole {
	t.Helper()
	assert.False(t, role.IndexedAt.IsZero(), "an indexed role carries its index time")
	role.IndexedAt = time.Time{}
	role.RecordCreatedAt = role.RecordCreatedAt.UTC()
	return role
}

// A community's roster has no size cap: every moderator row is read back.
func TestCommunityRoleRepository_RosterHasNoCap(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	const community = "did:plc:rolenocapcommunity"
	insertRoleTestCommunity(t, db, community)
	repo := NewCommunityRoleRepository(db)
	createdAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	require.NoError(t, repo.UpsertCommunityRole(ctx, communities.CommunityRole{
		CommunityDID: community, SubjectDID: "did:plc:rolenocapowner", Role: "owner",
		RecordKey: "rolenocapownerkey", RecordCID: "bafyreirolenocapowner", RecordCreatedAt: createdAt,
	}))
	wantModerators := make([]string, 0, 150)
	for i := 0; i < 150; i++ {
		subject := fmt.Sprintf("did:plc:rolenocapmod%03d", i)
		wantModerators = append(wantModerators, subject)
		require.NoError(t, repo.UpsertCommunityRole(ctx, communities.CommunityRole{
			CommunityDID: community, SubjectDID: subject, Role: "moderator",
			RecordKey: fmt.Sprintf("rolenocapmodkey%03d", i), RecordCID: "bafyreirolenocapmod", RecordCreatedAt: createdAt,
		}))
	}

	roster, err := communities.ReadRoster(ctx, repo, community)
	require.NoError(t, err)
	require.NotNil(t, roster.Owner, "the community must have an owner")
	assert.Equal(t, "did:plc:rolenocapowner", roster.Owner.SubjectDID)
	gotModerators := make([]string, 0, len(roster.Moderators))
	for _, moderator := range roster.Moderators {
		assert.Equal(t, "moderator", moderator.Role)
		gotModerators = append(gotModerators, moderator.SubjectDID)
	}
	assert.ElementsMatch(t, wantModerators, gotModerators, "all 150 moderators must be listed")
}

// ListRolesBySubject reports the subject's effective role in each community:
// a losing owner reads as moderator and an unknown role is omitted.
func TestCommunityRoleRepository_ListRolesBySubjectIsEffective(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	const (
		subject    = "did:plc:rolebysubject"
		rival      = "did:plc:rolebysubjectrival"
		communityA = "did:plc:rolebysubjecta"
		communityB = "did:plc:rolebysubjectb"
		communityC = "did:plc:rolebysubjectc"
		communityD = "did:plc:rolebysubjectd"
	)
	for _, community := range []string{communityA, communityB, communityC, communityD} {
		insertRoleTestCommunity(t, db, community)
	}
	repo := NewCommunityRoleRepository(db)
	t1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

	rows := []communities.CommunityRole{
		{CommunityDID: communityA, SubjectDID: subject, Role: "owner", RecordKey: "rolebysubjectkey", RecordCID: "bafyreirolebysubjecta", RecordCreatedAt: t1},
		{CommunityDID: communityB, SubjectDID: subject, Role: "moderator", RecordKey: "rolebysubjectkey", RecordCID: "bafyreirolebysubjectb", RecordCreatedAt: t1},
		{CommunityDID: communityC, SubjectDID: subject, Role: "owner", RecordKey: "rolebysubjectkey", RecordCID: "bafyreirolebysubjectc", RecordCreatedAt: t1},
		{CommunityDID: communityC, SubjectDID: rival, Role: "owner", RecordKey: "rolebysubjectrivalkey", RecordCID: "bafyreirolebysubjectrival", RecordCreatedAt: t2},
		{CommunityDID: communityD, SubjectDID: subject, Role: "admin", RecordKey: "rolebysubjectkey", RecordCID: "bafyreirolebysubjectd", RecordCreatedAt: t1},
	}
	for _, row := range rows {
		require.NoError(t, repo.UpsertCommunityRole(ctx, row))
	}

	got, err := repo.ListRolesBySubject(ctx, subject)
	require.NoError(t, err)
	normalized := make([]communities.CommunityRole, 0, len(got))
	for _, role := range got {
		normalized = append(normalized, normalizeIndexedRole(t, role))
	}
	assert.ElementsMatch(t, []communities.CommunityRole{
		{CommunityDID: "did:plc:rolebysubjecta", SubjectDID: "did:plc:rolebysubject", Role: "owner", RecordKey: "rolebysubjectkey", RecordCID: "bafyreirolebysubjecta", RecordCreatedAt: t1},
		{CommunityDID: "did:plc:rolebysubjectb", SubjectDID: "did:plc:rolebysubject", Role: "moderator", RecordKey: "rolebysubjectkey", RecordCID: "bafyreirolebysubjectb", RecordCreatedAt: t1},
		{CommunityDID: "did:plc:rolebysubjectc", SubjectDID: "did:plc:rolebysubject", Role: "moderator", RecordKey: "rolebysubjectkey", RecordCID: "bafyreirolebysubjectc", RecordCreatedAt: t1},
	}, normalized)
}

// Upserting a role record identical to the indexed row (same role, record key
// and record CID) changes nothing, so the community's roster version stays put.
func TestCommunityRoleRepository_IdenticalUpsertKeepsRosterVersion(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	const community = "did:plc:roleidenticalupsert"
	insertRoleTestCommunity(t, db, community)
	repo := NewCommunityRoleRepository(db)
	reader := NewCommunitySubjectStateReader(db)
	role := communities.CommunityRole{
		CommunityDID: community, SubjectDID: "did:plc:roleidenticalmod", Role: "moderator",
		RecordKey: "roleidenticalkey", RecordCID: "bafyreiroleidentical",
		RecordCreatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	}

	require.NoError(t, repo.UpsertCommunityRole(ctx, role))
	before, err := reader.ReadCommunitySubjectState(ctx, community, community, "roster")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.EqualValues(t, 1, before.Version, "the first upsert advances the roster version to 1")

	require.NoError(t, repo.UpsertCommunityRole(ctx, role))
	after, err := reader.ReadCommunitySubjectState(ctx, community, community, "roster")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.EqualValues(t, 1, after.Version, "an identical upsert must not advance the roster version")
}
