//go:build integration

package postgres

import (
	"database/sql"
	"fmt"
	"testing"

	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B1: erasing an account removes its community roles and every invitation
// naming it, advances each affected roster version once, and queues a guarded
// repo delete for each role record in a community this instance hosts.

const (
	erasedRoleAccount = "did:plc:troublemaker456"
	// erasedRoleRecordKey is communities.RoleRecordKey("did:plc:troublemaker456").
	erasedRoleRecordKey = "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa"
	erasedRoleInvitee   = "did:plc:erasureroleinvitee"
	erasedRoleBystander = "did:plc:erasurerolebystander"

	erasedRoleC1 = "did:plc:erasurerolec1" // hosted: X moderates; X invited Y; Z moderates
	erasedRoleC2 = "did:plc:erasurerolec2" // hosted: X moderates; Z invited
	erasedRoleC3 = "did:plc:erasurerolec3" // hosted: X owns
	erasedRoleC4 = "did:plc:erasurerolec4" // hosted: X invited only
	erasedRoleC5 = "did:plc:erasurerolec5" // not hosted: X moderates
)

// seedErasedRoleAccount seeds account X's roles, invitations and the
// bystander Z's role and invitation, every roster version at 7, written
// directly so nothing advances a version on the way in.
func seedErasedRoleAccount(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()
	createTestUser(t, db, "troublemaker"+testkit.UniqueID(t)+".test", erasedRoleAccount)
	createTestUser(t, db, "roleinvitee"+testkit.UniqueID(t)+".test", erasedRoleInvitee)
	createTestUser(t, db, "rolebystander"+testkit.UniqueID(t)+".test", erasedRoleBystander)
	for _, community := range []string{erasedRoleC1, erasedRoleC2, erasedRoleC3, erasedRoleC4} {
		insertRoleTestCommunity(t, db, community)
	}
	insertNonHostedRoleTestCommunity(t, db, erasedRoleC5)
	for _, community := range []string{erasedRoleC1, erasedRoleC2, erasedRoleC3, erasedRoleC4, erasedRoleC5} {
		_, err := db.ExecContext(ctx, `INSERT INTO community_subject_versions (community_did, subject, version)
			VALUES ($1, $1, 7)`, community)
		require.NoError(t, err)
	}
	for _, role := range []struct{ community, subject, role, rkey, cid string }{
		{erasedRoleC1, erasedRoleAccount, "moderator", erasedRoleRecordKey, "bafyreierasedrolec1"},
		{erasedRoleC2, erasedRoleAccount, "moderator", erasedRoleRecordKey, "bafyreierasedrolec2"},
		{erasedRoleC3, erasedRoleAccount, "owner", erasedRoleRecordKey, "bafyreierasedrolec3"},
		{erasedRoleC5, erasedRoleAccount, "moderator", erasedRoleRecordKey, "bafyreierasedrolec5"},
		{erasedRoleC1, erasedRoleBystander, "moderator", "bystanderrolekey", "bafyreibystanderrole"},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO community_roles
			(community_did, subject_did, role, record_key, record_cid, record_created_at)
			VALUES ($1, $2, $3, $4, $5, '2026-11-01T10:00:00Z')`,
			role.community, role.subject, role.role, role.rkey, role.cid)
		require.NoError(t, err)
	}
	for _, inv := range []struct{ id, community, invitee, role, inviter, state string }{
		{"inv-x-invitee-open", erasedRoleC4, erasedRoleAccount, "moderator", "did:plc:erasurerolec4owner", "open"},
		{"inv-x-inviter-open", erasedRoleC1, erasedRoleInvitee, "moderator", erasedRoleAccount, "open"},
		{"inv-x-accepted", erasedRoleC2, erasedRoleAccount, "moderator", "did:plc:erasurerolec2owner", "accepted"},
		{"inv-x-declined", erasedRoleC3, erasedRoleAccount, "owner", "did:plc:erasurerolec3admin", "declined"},
		{"inv-x-superseded", erasedRoleC1, erasedRoleAccount, "moderator", "did:plc:erasurerolec1owner", "superseded"},
		{"inv-z-open", erasedRoleC2, erasedRoleBystander, "moderator", "did:plc:erasurerolec2owner", "open"},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO community_role_invitations
			(id, community_did, invitee_did, role, inviter_did, inviter_admin, state, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, false, $6, '2026-11-10T12:00:00Z', '2026-11-24T12:00:00Z')`,
			inv.id, inv.community, inv.invitee, inv.role, inv.inviter, inv.state)
		require.NoError(t, err)
	}
}

// erasureRoleRows returns the whole rows of a table matching where, as
// canonical JSON, so an untouched row cannot pass on counts alone.
func erasureRoleRows(t *testing.T, db *sql.DB, table, where string, args ...any) string {
	t.Helper()
	var rows string
	require.NoError(t, db.QueryRowContext(t.Context(), fmt.Sprintf(`
		SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text), '[]'::jsonb)::text
		FROM %s r WHERE %s`, table, where), args...).Scan(&rows))
	return rows
}

func erasureRosterVersions(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT community_did, version FROM community_subject_versions
		WHERE subject = community_did`)
	require.NoError(t, err)
	defer rows.Close()
	versions := map[string]int64{}
	for rows.Next() {
		var community string
		var version int64
		require.NoError(t, rows.Scan(&community, &version))
		versions[community] = version
	}
	require.NoError(t, rows.Err())
	return versions
}

func TestUserRepo_Delete_RemovesCommunityRolesAndInvitations(t *testing.T) {
	db := testkit.DB(t)
	seedErasedRoleAccount(t, db)
	bystanderRolesBefore := erasureRoleRows(t, db, "community_roles", "subject_did = $1", erasedRoleBystander)
	bystanderInvitationsBefore := erasureRoleRows(t, db, "community_role_invitations", "invitee_did = $1", erasedRoleBystander)

	require.NoError(t, NewUserRepository(db).Delete(t.Context(), erasedRoleAccount))

	require.Equal(t, "[]", erasureRoleRows(t, db, "community_roles", "subject_did = $1", erasedRoleAccount),
		"the erased account holds no role in any community, hosted or not")
	require.Equal(t, "[]", erasureRoleRows(t, db, "community_role_invitations",
		"invitee_did = $1 OR inviter_did = $1", erasedRoleAccount),
		"no invitation in any state names the erased account as invitee or inviter")
	assert.Equal(t, bystanderRolesBefore, erasureRoleRows(t, db, "community_roles", "subject_did = $1", erasedRoleBystander),
		"another account's role is untouched")
	assert.Equal(t, bystanderInvitationsBefore, erasureRoleRows(t, db, "community_role_invitations", "invitee_did = $1", erasedRoleBystander),
		"another account's invitation is untouched")
	assert.Equal(t, map[string]int64{
		erasedRoleC1: 8, erasedRoleC2: 8, erasedRoleC3: 8, erasedRoleC4: 7, erasedRoleC5: 8,
	}, erasureRosterVersions(t, db), "each roster that lost a role advances once; an invitation alone advances nothing")

	rows, err := db.QueryContext(t.Context(), `SELECT community_did, record_key, record_cid
		FROM community_role_record_deletes ORDER BY community_did`)
	require.NoError(t, err)
	defer rows.Close()
	type pendingDelete struct{ community, rkey, cid string }
	var pending []pendingDelete
	for rows.Next() {
		var d pendingDelete
		require.NoError(t, rows.Scan(&d.community, &d.rkey, &d.cid))
		pending = append(pending, d)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []pendingDelete{
		{erasedRoleC1, erasedRoleRecordKey, "bafyreierasedrolec1"},
		{erasedRoleC2, erasedRoleRecordKey, "bafyreierasedrolec2"},
		{erasedRoleC3, erasedRoleRecordKey, "bafyreierasedrolec3"},
	}, pending, "one guarded repo delete per role record in a hosted community, none for the non-hosted one")
}

func TestUserRepo_Delete_RoleCleanupRollsBackWithLateUserDeleteFailure(t *testing.T) {
	db := testkit.DB(t)
	seedErasedRoleAccount(t, db)
	// A delete already pending for another record, which a rollback must keep.
	_, err := db.ExecContext(t.Context(), `INSERT INTO community_role_record_deletes (community_did, record_key, record_cid)
		VALUES ($1, 'otherpendingrolekey', 'bafyreiotherpending')`, erasedRoleC2)
	require.NoError(t, err)
	snapshot := func() map[string]string {
		return map[string]string{
			"users":                         erasureRoleRows(t, db, "users", "did = $1", erasedRoleAccount),
			"deleted_accounts":              erasureRoleRows(t, db, "deleted_accounts", "did = $1", erasedRoleAccount),
			"community_roles":               erasureRoleRows(t, db, "community_roles", "true"),
			"community_role_invitations":    erasureRoleRows(t, db, "community_role_invitations", "true"),
			"community_subject_versions":    erasureRoleRows(t, db, "community_subject_versions", "true"),
			"community_role_record_deletes": erasureRoleRows(t, db, "community_role_record_deletes", "true"),
		}
	}
	before := snapshot()
	_, err = db.ExecContext(t.Context(), `CREATE FUNCTION reject_erasure_user_delete() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'reject erasure user delete' USING ERRCODE = 'P4343'; END; $$`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER reject_erasure_user_delete BEFORE DELETE ON users
		FOR EACH ROW EXECUTE FUNCTION reject_erasure_user_delete()`)
	require.NoError(t, err)

	err = NewUserRepository(db).Delete(t.Context(), erasedRoleAccount)
	var postgresError *pq.Error
	require.ErrorAs(t, err, &postgresError)
	require.Equal(t, pqerror.Code("P4343"), postgresError.Code, "reach the late users DELETE failure")
	require.Equal(t, before, snapshot(),
		"rollback leaves the marker absent and the user, roles, invitations, versions and pending deletes unchanged")
}
