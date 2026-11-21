//go:build integration

package moderation_test

import (
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postRateOtherCommunity is a second community this instance hosts, in which
// M also removes posts.
const postRateOtherCommunity = "did:plc:rosterbeta"

// The removal limits count only what the approved rule names: M's local,
// non-admin removals in this community within the window, each once. With
// both limits at 3, M has two completed removals in rosteralpha (each a logged
// action and a completed intent), two logged removals and one open removal
// intent in a second hosted community, and rosteralpha holds one removal
// written outside Coves. M's next rosteralpha removal is M's third and the
// community's third, so it proceeds. Counting another community's removals
// or intents, completed intents beside their actions, or the external
// removal reaches a limit, as does refusing at one below the limit.
func TestCommunityPostActionsDB_RemovalLimitsCountOnlyThisModeratorsLocalRemovalsHere(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	limits := moderation.CommunityRemovalLimits{PerModerator: 3, PerCommunity: 3, Window: time.Hour}
	service := f.serviceWithLimits(limits)
	insertRosterCommunity(t, f.db, postRateOtherCommunity, "rosterbeta.test", true)

	external := f.seedAcceptedPost(t, postDBSeedFor("3kpostexternal"))
	prior := []seededCommunityPost{
		f.seedAcceptedPost(t, postDBSeedFor("3kpostprior1")),
		f.seedAcceptedPost(t, postDBSeedFor("3kpostprior2")),
	}
	target := f.seedAcceptedPost(t, postDBAlpha)

	f.standExternalRemoval(t, external, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		postFirehoseExternalRemovalCID,
		removalRecordBody(external, "off-topic", postDBRemovalReason, postFirehoseExternalCreatedAt))
	for i, post := range prior {
		f.clock.Set(rosterClockStart.Add(time.Duration(i) * time.Minute))
		_, err := removePostWith(service, rosterModerator, post.URI, "spam", "", postDBSeededVersion, "prior-remove-"+post.SubjectRkey)
		require.NoError(t, err, "prior removal %d", i+1)
	}
	copyRemovalsToOtherCommunity(t, f)

	attemptAt := rosterClockStart.Add(30 * time.Minute)
	requireRateSeeds(t, f, attemptAt.Add(-limits.Window))
	f.clock.Set(attemptAt)

	_, err := removePostWith(service, rosterModerator, target.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

	require.NoError(t, err, "M's third removal in this community is within both limits")
	assert.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, target.URI).Status, "the target is removed")
}

// copyRemovalsToOtherCommunity copies M's two rosteralpha removal actions into
// postRateOtherCommunity as logged removals there, and one of M's rosteralpha
// removal intents as an open removal intent there.
func copyRemovalsToOtherCommunity(t *testing.T, f *communityPostDBFixture) {
	t.Helper()
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`CREATE TEMP TABLE rate_actions ON COMMIT DROP AS
		 SELECT * FROM moderation_actions
		 WHERE scope_community_did = '` + rosterCommunity + `' AND action = 'remove' AND origin = 'local'`,
		`UPDATE rate_actions SET id = id || '-beta', authority_did = '` + postRateOtherCommunity + `',
		 scope_community_did = '` + postRateOtherCommunity + `', subject_community_did = '` + postRateOtherCommunity + `'`,
		`INSERT INTO moderation_actions SELECT * FROM rate_actions`,
		`CREATE TEMP TABLE rate_intents ON COMMIT DROP AS
		 SELECT * FROM community_write_intents
		 WHERE community_did = '` + rosterCommunity + `' AND action = 'remove' ORDER BY created_at, id LIMIT 1`,
		`UPDATE rate_intents SET id = id || '-beta', community_did = '` + postRateOtherCommunity + `',
		 target_uri = replace(target_uri, '` + rosterCommunity + `', '` + postRateOtherCommunity + `'),
		 paired_target_uri = replace(paired_target_uri, '` + rosterCommunity + `', '` + postRateOtherCommunity + `'),
		 idempotency_key = idempotency_key || '-beta',
		 state = 'open', commit_rev = NULL, record_cid = NULL, action_id = NULL, finished_at = NULL,
		 expires_at = expires_at + interval '1 day'`,
		`INSERT INTO community_write_intents SELECT * FROM rate_intents`,
	} {
		_, err := tx.Exec(statement)
		require.NoError(t, err, "fixture: %s", statement)
	}
	require.NoError(t, tx.Commit())
}

// requireRateSeeds checks each seed is the kind the test means and falls
// inside the window, so every miscount it guards against would change a count.
func requireRateSeeds(t *testing.T, f *communityPostDBFixture, since time.Time) {
	t.Helper()
	count := func(query string, args ...any) int {
		var n int
		require.NoError(t, f.db.QueryRow(query, args...).Scan(&n))
		return n
	}
	actions := `SELECT COUNT(*) FROM moderation_actions
		WHERE scope_kind = 'community' AND scope_community_did = $1 AND action = 'remove' AND origin = $2
		  AND actor_did = $3 AND NOT admin_acting AND subject_collection IS NOT NULL AND created_at > $4`
	intents := `SELECT COUNT(*) FROM community_write_intents
		WHERE community_did = $1 AND action = 'remove' AND state = $2 AND actor_did = $3 AND NOT admin_acting AND created_at > $4`
	require.Equal(t, 2, count(actions, rosterCommunity, "local", rosterModerator, since), "fixture: M's two logged removals here")
	require.Equal(t, 2, count(intents, rosterCommunity, "completed", rosterModerator, since), "fixture: M's two completed intents here")
	require.Equal(t, 0, count(intents, rosterCommunity, "open", rosterModerator, since), "fixture: no open intent here")
	require.Equal(t, 2, count(actions, postRateOtherCommunity, "local", rosterModerator, since), "fixture: M's two logged removals elsewhere")
	require.Equal(t, 1, count(intents, postRateOtherCommunity, "open", rosterModerator, since), "fixture: M's open intent elsewhere")
	require.Equal(t, 1, count(actions, rosterCommunity, "inherited", "", since), "fixture: the external removal here")
}
