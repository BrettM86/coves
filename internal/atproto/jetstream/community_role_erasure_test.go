//go:build integration

package jetstream

import (
	"database/sql"
	"testing"

	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B4: a role record naming an erased account, arriving through the
// communities consumer with a rev that wins the gate, indexes nothing and
// advances no roster version. In a community this instance hosts it queues a
// guarded repo delete for the event's record; in one it does not host it
// queues nothing. Once the account is reinstated, a newer record indexes.

const (
	erasedRosterSubject = "did:plc:troublemaker456"
	// erasedRosterSubjectKey is communities.RoleRecordKey("did:plc:troublemaker456").
	erasedRosterSubjectKey = "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa"
	// rosterCommunityNotHosted is indexed but holds no PDS credentials here.
	rosterCommunityNotHosted = "did:plc:rostercommunitynothosted"
)

func erasedSubjectRoleCount(t *testing.T, db *sql.DB, communityDID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM community_roles WHERE community_did = $1 AND subject_did = $2`,
		communityDID, erasedRosterSubject).Scan(&count))
	return count
}

func communityRosterVersionRow(t *testing.T, db *sql.DB, communityDID string) int64 {
	t.Helper()
	var version int64
	require.NoError(t, db.QueryRow(`SELECT version FROM community_subject_versions WHERE community_did = $1 AND subject = $1`,
		communityDID).Scan(&version))
	return version
}

type queuedRoleDelete struct{ community, rkey, cid string }

func queuedRoleDeletes(t *testing.T, db *sql.DB) []queuedRoleDelete {
	t.Helper()
	rows, err := db.Query(`SELECT community_did, record_key, record_cid FROM community_role_record_deletes ORDER BY community_did`)
	require.NoError(t, err)
	defer rows.Close()
	queued := []queuedRoleDelete{}
	for rows.Next() {
		var d queuedRoleDelete
		require.NoError(t, rows.Scan(&d.community, &d.rkey, &d.cid))
		queued = append(queued, d)
	}
	require.NoError(t, rows.Err())
	return queued
}

func TestCommunityRoleIngestion_ErasedSubjectIsNotIndexed(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	consumer, _ := newRosterConsumer(t, db)
	insertBridgedCommunity(t, db, rosterCommunityNotHosted, "rostercommunitynothosted.test", rosterFounder)
	for _, community := range []string{rosterCommunityA, rosterCommunityNotHosted} {
		_, err := db.Exec(`INSERT INTO community_subject_versions (community_did, subject, version) VALUES ($1, $1, 7)`, community)
		require.NoError(t, err)
	}
	insertBridgedUser(t, db, erasedRosterSubject, "troublemaker.test")
	userRepo := postgres.NewUserRepository(db)
	require.NoError(t, userRepo.Delete(t.Context(), erasedRosterSubject))
	for _, community := range []string{rosterCommunityA, rosterCommunityNotHosted} {
		require.Zero(t, erasedSubjectRoleCount(t, db, community), "checkpoint: no role row before the event")
		require.EqualValues(t, 7, communityRosterVersionRow(t, db, community), "checkpoint: roster version before the event")
	}

	deliverRosterSteps(t, consumer, []rosterStep{
		{
			repo: rosterCommunityA, op: "create", subject: erasedRosterSubject, rkey: erasedRosterSubjectKey,
			role: "moderator", createdAt: rosterTime1, rev: revB, cid: "bafyreierasedhostedrole",
		},
		{
			repo: rosterCommunityNotHosted, op: "create", subject: erasedRosterSubject, rkey: erasedRosterSubjectKey,
			role: "moderator", createdAt: rosterTime1, rev: revB, cid: "bafyreierasednothostedrole",
		},
	})

	for _, community := range []string{rosterCommunityA, rosterCommunityNotHosted} {
		assert.Zerof(t, erasedSubjectRoleCount(t, db, community), "%s indexes no role for an erased account", community)
		assert.EqualValuesf(t, 7, communityRosterVersionRow(t, db, community), "%s roster version unchanged", community)
	}
	assert.Equal(t, []queuedRoleDelete{
		{rosterCommunityA, erasedRosterSubjectKey, "bafyreierasedhostedrole"},
	}, queuedRoleDeletes(t, db), "one guarded delete for the hosted community's record, none for the non-hosted one")

	reinstated, err := userRepo.(users.ErasureMarkerStore).ReinstateAccount(t.Context(), erasedRosterSubject)
	require.NoError(t, err)
	require.True(t, reinstated)
	deliverRosterSteps(t, consumer, []rosterStep{{
		repo: rosterCommunityA, op: "create", subject: erasedRosterSubject, rkey: erasedRosterSubjectKey,
		role: "moderator", createdAt: rosterTime2, rev: revC, cid: "bafyreireinstatedrole",
	}})
	var cid string
	require.NoError(t, db.QueryRow(`SELECT record_cid FROM community_roles WHERE community_did = $1 AND subject_did = $2`,
		rosterCommunityA, erasedRosterSubject).Scan(&cid), "a reinstated account's newer record is indexed")
	assert.Equal(t, "bafyreireinstatedrole", cid)
}
