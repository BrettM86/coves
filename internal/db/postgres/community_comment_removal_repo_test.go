//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ccrRepoCommunity  = "did:plc:ccrrepocommunity"
	ccrRepoSubjectURI = "at://did:plc:ccrrepocommenter/social.coves.community.comment/ccrreposubject"
)

func ccrRepoRevGateURI(recordKey string) string {
	return "at://" + ccrRepoCommunity + "/social.coves.community.removal/" + recordKey
}

func ccrRepoRemovalCount(t *testing.T, db *sql.DB, recordKey string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM community_comment_removals WHERE repository_did = $1 AND record_key = $2`,
		ccrRepoCommunity, recordKey).Scan(&count))
	return count
}

// Update and delete deliveries for one removal record can be processed at the
// same time (overlapping feeds). Both RecordRemoval and DeleteRemoval must take
// the per-record rev gate (jetstream_record_revs) before the
// community_comment_removals row, or the two deadlock.
//
// The test transaction does RecordRemoval's two steps by hand: it first holds
// the rev-gate row lock, then, once DeleteRemoval is waiting behind it, takes
// the removal row. DeleteRemoval must be waiting for the rev gate while holding
// nothing on the removal row.
func TestCommunityCommentRemovalRepository_DeleteTakesTheRevGateBeforeTheRemovalRow(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := testkit.DB(t)
	repo := NewCommunityCommentRemovalRepository(db)

	recordKey := "ccrrepo" + testkit.UniqueID(t)
	createRev, updateRev, deleteRev := testkit.TID(), testkit.TID(), testkit.TID()
	require.Less(t, createRev, updateRev)
	require.Less(t, updateRev, deleteRev)

	held, err := repo.RecordRemoval(ctx, moderation.CommunityCommentRemoval{
		RepositoryDID: ccrRepoCommunity, RecordKey: recordKey, RecordCID: "bafyreiccrrepocreate",
		Rev: createRev, SubjectURI: ccrRepoSubjectURI, SubjectCID: "bafyreiccrreposubject",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.CommunityCommentRemovalHeld, held, "fixture: the subject comment is not indexed")

	// The update's first step: advance the rev gate, keeping its row lock.
	update, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { _ = update.Rollback() })
	var updatePID int
	require.NoError(t, update.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&updatePID))
	_, err = update.ExecContext(ctx, `
		INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)
		ON CONFLICT (record_uri) DO UPDATE SET rev = EXCLUDED.rev, updated_at = NOW()
		WHERE jetstream_record_revs.rev < EXCLUDED.rev`, ccrRepoRevGateURI(recordKey), updateRev)
	require.NoError(t, err)

	type deleteResult struct {
		outcome moderation.CommunityCommentRemovalOutcome
		err     error
	}
	deleted := make(chan deleteResult, 1)
	go func() {
		outcome, err := repo.DeleteRemoval(ctx, ccrRepoCommunity, recordKey, deleteRev)
		deleted <- deleteResult{outcome, err}
	}()

	observer, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		if len(deleted) > 0 {
			r := <-deleted
			t.Fatalf("DeleteRemoval finished while the rev gate was locked: outcome %q, err %v", r.outcome, r.err)
		}
		var waiting bool
		err := observer.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			               WHERE datname = current_database() AND $1 = ANY (pg_blocking_pids(pid)))`,
			updatePID).Scan(&waiting)
		return waiting, err
	}, testkit.WithDescription("DeleteRemoval waiting behind the locked rev gate"))

	// While it waits for the rev gate, DeleteRemoval must hold no lock on the
	// removal row.
	probe, err := observer.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, probeErr := probe.ExecContext(ctx, `
		SELECT 1 FROM community_comment_removals
		WHERE repository_did = $1 AND record_key = $2 FOR UPDATE NOWAIT`, ccrRepoCommunity, recordKey)
	require.NoError(t, probe.Rollback())
	assert.NoError(t, probeErr,
		"DeleteRemoval must not lock the removal row before it has passed the rev gate")

	// The update's second step: upsert the removal row, as RecordRemoval does.
	_, err = update.ExecContext(ctx, `
		INSERT INTO community_comment_removals
		    (repository_did, record_key, record_cid, subject_uri, subject_cid, state)
		VALUES ($1, $2, $3, $4, $5, 'held')
		ON CONFLICT (repository_did, record_key) DO UPDATE SET
		    record_cid = EXCLUDED.record_cid, subject_cid = EXCLUDED.subject_cid, updated_at = NOW()
		WHERE community_comment_removals.subject_uri = EXCLUDED.subject_uri`,
		ccrRepoCommunity, recordKey, "bafyreiccrrepoupdate", ccrRepoSubjectURI, "bafyreiccrreposubject")
	require.NoError(t, err, "the update must reach the removal row without a deadlock against the delete")
	require.NoError(t, update.Commit())

	var result deleteResult
	select {
	case result = <-deleted:
	case <-ctx.Done():
		t.Fatal("DeleteRemoval did not finish after the update committed")
	}
	require.NoError(t, result.err, "the delete must complete without a deadlock against the update")
	assert.Equal(t, moderation.CommunityCommentRemovalDiscarded, result.outcome,
		"the newer delete discards the held removal the update left")
	assert.Equal(t, 0, ccrRepoRemovalCount(t, db, recordKey), "the removal row must be gone")

	var gateRev string
	require.NoError(t, db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`,
		ccrRepoRevGateURI(recordKey)).Scan(&gateRev))
	assert.Equal(t, deleteRev, gateRev, "the rev gate must end at the delete's rev")
}

// A delete for a key with no removal row must answer NotFound whether or not
// its rev advances the gate, so the consumer falls back to the post-removal
// path; and it must record its rev so an older create is refused as stale.
func TestCommunityCommentRemovalRepository_DeleteWithoutMappingIsNotFoundAndRecordsTheRev(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	repo := NewCommunityCommentRemovalRepository(db)

	for _, row := range []struct {
		name   string
		replay func(older, newer string) string // the rev of the second delete
	}{
		{name: "equal rev replay", replay: func(_, newer string) string { return newer }},
		{name: "older rev replay", replay: func(older, _ string) string { return older }},
	} {
		t.Run(row.name, func(t *testing.T) {
			recordKey := "ccrnomap" + testkit.UniqueID(t)
			createRev, deleteRev := testkit.TID(), testkit.TID()
			require.Less(t, createRev, deleteRev)

			first, err := repo.DeleteRemoval(ctx, ccrRepoCommunity, recordKey, deleteRev)
			require.NoError(t, err)
			assert.Equal(t, moderation.CommunityCommentRemovalNotFound, first, "first delivery of the delete")

			replayed, err := repo.DeleteRemoval(ctx, ccrRepoCommunity, recordKey, row.replay(createRev, deleteRev))
			require.NoError(t, err)
			assert.Equal(t, moderation.CommunityCommentRemovalNotFound, replayed,
				"a delete whose rev does not advance still finds no comment removal under the key")

			created, err := repo.RecordRemoval(ctx, moderation.CommunityCommentRemoval{
				RepositoryDID: ccrRepoCommunity, RecordKey: recordKey, RecordCID: "bafyreiccrnomapcreate",
				Rev: createRev, SubjectURI: ccrRepoSubjectURI, SubjectCID: "bafyreiccrreposubject",
			})
			require.NoError(t, err)
			assert.Equal(t, moderation.CommunityCommentRemovalStale, created,
				"a create older than the recorded delete is stale")
			assert.Equal(t, 0, ccrRepoRemovalCount(t, db, recordKey), "a stale create indexes nothing")
		})
	}
}

// ccrRepoThread is a comment (ccrRepoSubjectURI) replying to community C's
// post, with community C2's post indexed beside it.
type ccrRepoThread struct {
	communityDID, otherCommunityDID string
	rootURI, otherRootURI           string
}

func seedCCRRepoThread(t *testing.T, db *sql.DB) ccrRepoThread {
	t.Helper()
	communityDID, err := fixtures.Community(t.Context(), db, "ccrrepoc", "ccrrepoowner")
	require.NoError(t, err)
	otherCommunityDID, err := fixtures.Community(t.Context(), db, "ccrrepoc2", "ccrrepoowner")
	require.NoError(t, err)
	thread := ccrRepoThread{
		communityDID:      communityDID,
		otherCommunityDID: otherCommunityDID,
		rootURI:           fixtures.Post(t, db, communityDID, "did:plc:ccrrepoauthor", "C root", 0, time.Now()),
		otherRootURI:      fixtures.Post(t, db, otherCommunityDID, "did:plc:ccrrepoauthor", "C2 root", 0, time.Now()),
	}
	fixtures.User(t, db, "ccrrepocommenter.test", "did:plc:ccrrepocommenter")
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, 'bafyreiccrreposubject', 'ccrreposubject', 'did:plc:ccrrepocommenter', $2, 'bafytest', $2, 'bafytest', 'the comment', NOW())
	`, ccrRepoSubjectURI, thread.rootURI)
	require.NoError(t, err)
	return thread
}

// ccrRepoReparentComment re-indexes the comment as re-created at the same URI
// replying to rootURI, as the comment consumer does for a delete and re-create.
func ccrRepoReparentComment(t *testing.T, db *sql.DB, rootURI, cid string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		UPDATE comments SET cid = $3, root_uri = $2, root_cid = 'bafytest', parent_uri = $2, parent_cid = 'bafytest'
		WHERE uri = $1
	`, ccrRepoSubjectURI, rootURI, cid)
	require.NoError(t, err)
}

func ccrRepoRemovalFrom(communityDID, recordKey, rev string) moderation.CommunityCommentRemoval {
	return moderation.CommunityCommentRemoval{
		RepositoryDID: communityDID, RecordKey: recordKey, RecordCID: "bafyreiccrreporecord",
		Rev: rev, SubjectURI: ccrRepoSubjectURI, SubjectCID: "bafyreiccrreposubject",
	}
}

func ccrRepoMappingState(t *testing.T, db *sql.DB, communityDID, recordKey string) string {
	t.Helper()
	var state string
	require.NoError(t, db.QueryRow(`
		SELECT state FROM community_comment_removals WHERE repository_did = $1 AND record_key = $2`,
		communityDID, recordKey).Scan(&state))
	return state
}

// ccrRepoActiveDecisions counts active community-scope removal decisions of
// communityDID on the comment.
func ccrRepoActiveDecisions(t *testing.T, db *sql.DB, communityDID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM moderation_decisions
		WHERE subject_uri = $1 AND authority_did = $2 AND scope_kind = 'community'
		  AND scope_community_did = $2 AND kind = 'removal' AND active`,
		ccrRepoSubjectURI, communityDID).Scan(&count))
	return count
}

type ccrRepoActionRow struct {
	id, actor, action, origin, reason, reverses string
}

// ccrRepoActions lists the comment's moderation actions oldest first.
func ccrRepoActions(t *testing.T, db *sql.DB) []ccrRepoActionRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT id, actor_did, action, origin, COALESCE(reason, ''), COALESCE(reverses_action_id, '')
		FROM moderation_actions WHERE subject_uri = $1 ORDER BY created_at, id`, ccrRepoSubjectURI)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []ccrRepoActionRow
	for rows.Next() {
		var row ccrRepoActionRow
		require.NoError(t, rows.Scan(&row.id, &row.actor, &row.action, &row.origin, &row.reason, &row.reverses))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	return got
}

// ccrRepoBlockedBy returns a backend waiting on a lock blockerPID holds, or 0.
// It runs through the given transaction, since the test database allows only
// three connections.
func ccrRepoBlockedBy(ctx context.Context, tx *sql.Tx, blockerPID int) (int, error) {
	var pid int
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT pid FROM pg_locks
		                 WHERE NOT granted AND $1 = ANY (pg_blocking_pids(pid)) LIMIT 1), 0)`,
		blockerPID).Scan(&pid)
	return pid, err
}

// A removal's application that resolved the thread to C and is still writing
// the decision when the comment is re-created under C2's post must not leave
// C's removal active: the ApplyHeldForComment run for the re-created comment,
// which waits for the application's mapping row lock, must demote it.
//
// The gate transaction holds moderation_decisions in SHARE mode, so
// RecordRemoval's application blocks at its decision write after it has
// resolved the thread to C.
func TestCommunityCommentRemovalRepository_ApplicationRacingAMoveIsDemoted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := testkit.DB(t)
	repo := NewCommunityCommentRemovalRepository(db)
	thread := seedCCRRepoThread(t, db)
	const recordKey = "ccrreporace"

	gate, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { _ = gate.Rollback() })
	var gatePID int
	require.NoError(t, gate.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&gatePID))
	_, err = gate.ExecContext(ctx, `LOCK TABLE moderation_decisions IN SHARE MODE`)
	require.NoError(t, err)

	type result struct {
		outcome moderation.CommunityCommentRemovalOutcome
		err     error
	}
	recorded := make(chan result, 1)
	go func() {
		outcome, err := repo.RecordRemoval(ctx, ccrRepoRemovalFrom(thread.communityDID, recordKey, testkit.TID()))
		recorded <- result{outcome, err}
	}()

	var recordPID int
	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		if len(recorded) > 0 {
			r := <-recorded
			t.Fatalf("RecordRemoval finished while moderation_decisions was locked: outcome %q, err %v", r.outcome, r.err)
		}
		pid, err := ccrRepoBlockedBy(ctx, gate, gatePID)
		recordPID = pid
		return pid != 0, err
	}, testkit.WithDescription("RecordRemoval's application waiting behind the gate"))

	// The commenter re-creates the comment under C2's post while the
	// application, which resolved the thread to C, is still writing.
	ccrRepoReparentComment(t, db, thread.otherRootURI, "bafyreiccrrepomoved")
	reconciled := make(chan error, 1)
	go func() { reconciled <- repo.ApplyHeldForComment(ctx, ccrRepoSubjectURI) }()

	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		if len(reconciled) > 0 {
			t.Fatalf("ApplyHeldForComment finished while the application was in flight: err %v", <-reconciled)
		}
		pid, err := ccrRepoBlockedBy(ctx, gate, recordPID)
		return pid != 0, err
	}, testkit.WithDescription("ApplyHeldForComment waiting behind RecordRemoval's application"))

	require.NoError(t, gate.Rollback())

	select {
	case r := <-recorded:
		require.NoError(t, r.err, "RecordRemoval")
	case <-ctx.Done():
		t.Fatal("RecordRemoval did not finish after the gate was released")
	}
	select {
	case err := <-reconciled:
		require.NoError(t, err, "ApplyHeldForComment")
	case <-ctx.Done():
		t.Fatal("ApplyHeldForComment did not finish after the gate was released")
	}

	assert.Equal(t, 0, ccrRepoActiveDecisions(t, db, "did:plc:community-ccrrepoc"),
		"C's removal must not stay active on a comment now shown under C2's post")
	assert.Equal(t, "held", ccrRepoMappingState(t, db, "did:plc:community-ccrrepoc", recordKey),
		"the removal must return to held, so it re-applies if the comment comes back")
}

// Moving a removed comment out of the removing community's thread and back is
// the commenter's doing, not the community's: the demotion and the
// re-application are system actions with no actor and reason
// reasonSubjectMoved. Only the first application and the community's own
// delete are attributed to the community.
func TestCommunityCommentRemovalRepository_MoveDemotionAndReapplicationAreSystemActions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	repo := NewCommunityCommentRemovalRepository(db)
	thread := seedCCRRepoThread(t, db)
	const recordKey = "ccrrepomoved"
	createRev, deleteRev := testkit.TID(), testkit.TID()
	require.Less(t, createRev, deleteRev)

	applied, err := repo.RecordRemoval(ctx, ccrRepoRemovalFrom(thread.communityDID, recordKey, createRev))
	require.NoError(t, err)
	require.Equal(t, moderation.CommunityCommentRemovalApplied, applied, "fixture: the first application")

	ccrRepoReparentComment(t, db, thread.otherRootURI, "bafyreiccrrepoinc2")
	require.NoError(t, repo.ApplyHeldForComment(ctx, ccrRepoSubjectURI))
	ccrRepoReparentComment(t, db, thread.rootURI, "bafyreiccrrepoback")
	require.NoError(t, repo.ApplyHeldForComment(ctx, ccrRepoSubjectURI))

	actions := ccrRepoActions(t, db)
	require.Len(t, actions, 3, "the apply, the demotion and the re-application")
	assert.Equal(t, ccrRepoActionRow{
		id: actions[0].id, actor: "did:plc:community-ccrrepoc", action: "apply-removal", origin: "inherited",
	}, actions[0], "the first application is the community's")
	assert.Equal(t, ccrRepoActionRow{
		id: actions[1].id, actor: "", action: "retract-removal", origin: "inherited",
		reason: "social.coves.moderation.defs#reasonSubjectMoved", reverses: actions[0].id,
	}, actions[1], "the demotion is a system retract-removal reversing the first application")
	assert.Equal(t, ccrRepoActionRow{
		id: actions[2].id, actor: "", action: "apply-removal", origin: "inherited",
		reason: "social.coves.moderation.defs#reasonSubjectMoved", reverses: actions[1].id,
	}, actions[2], "the re-application is a system apply-removal reversing the demotion")
	communityActions := 0
	for _, action := range actions {
		if action.actor == "did:plc:community-ccrrepoc" {
			communityActions++
		}
	}
	assert.Equal(t, 1, communityActions, "only the first application names the community as actor")
	assert.Equal(t, 1, ccrRepoActiveDecisions(t, db, "did:plc:community-ccrrepoc"),
		"the removal is active again once the comment is back under C's post")

	// The public log names no actor for the system rows.
	listed, err := NewModerationRepository(db).ListActions(ctx, moderation.ActionListQuery{
		Limit: 10, SubjectURI: ccrRepoSubjectURI,
	})
	require.NoError(t, err)
	systemViews := 0
	for _, action := range listed {
		if action.ID != actions[1].id && action.ID != actions[2].id {
			continue
		}
		systemViews++
		encoded, err := json.Marshal(moderation.NewActionView(action))
		require.NoError(t, err)
		var view map[string]any
		require.NoError(t, json.Unmarshal(encoded, &view))
		assert.NotContains(t, view, "actor", "a system action's public view has no actor")
		assert.Equal(t, "social.coves.moderation.defs#reasonSubjectMoved", view["reason"])
	}
	assert.Equal(t, 2, systemViews, "both system actions are in the public log")

	// Re-running with no move writes nothing.
	require.NoError(t, repo.ApplyHeldForComment(ctx, ccrRepoSubjectURI))
	assert.Len(t, ccrRepoActions(t, db), 3, "a reconcile with no move must not log an action")

	deleted, err := repo.DeleteRemoval(ctx, thread.communityDID, recordKey, deleteRev)
	require.NoError(t, err)
	assert.Equal(t, moderation.CommunityCommentRemovalRetracted, deleted)
	actions = ccrRepoActions(t, db)
	require.Len(t, actions, 4, "the community's delete logs one retract-removal")
	assert.Equal(t, "did:plc:community-ccrrepoc", actions[3].actor, "the community deleting its record is the community's act")
	assert.Equal(t, "retract-removal", actions[3].action)
	assert.Equal(t, actions[2].id, actions[3].reverses, "the delete reverses the active (system) apply-removal")
	assert.Equal(t, 0, ccrRepoActiveDecisions(t, db, "did:plc:community-ccrrepoc"),
		"the community's delete ends the removal")
}

// A crash after the comment was re-created under C2's post but before its
// ApplyHeldForComment ran leaves C's removal applied; redelivery of the same
// removal must reconcile it.
func TestCommunityCommentRemovalRepository_RedeliveryDemotesAMovedAppliedRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	repo := NewCommunityCommentRemovalRepository(db)
	thread := seedCCRRepoThread(t, db)
	const recordKey = "ccrreporedeliver"
	removal := ccrRepoRemovalFrom(thread.communityDID, recordKey, testkit.TID())

	applied, err := repo.RecordRemoval(ctx, removal)
	require.NoError(t, err)
	require.Equal(t, moderation.CommunityCommentRemovalApplied, applied, "fixture: the first application")

	ccrRepoReparentComment(t, db, thread.otherRootURI, "bafyreiccrrepoinc2")
	redelivered, err := repo.RecordRemoval(ctx, removal)
	require.NoError(t, err)

	assert.Equal(t, moderation.CommunityCommentRemovalOutcome("demoted"), redelivered,
		"demoting an applied removal of a moved comment must report demoted, not rejected")
	assert.Equal(t, "held", ccrRepoMappingState(t, db, "did:plc:community-ccrrepoc", recordKey),
		"the redelivered removal must return to held")
	assert.Equal(t, 0, ccrRepoActiveDecisions(t, db, "did:plc:community-ccrrepoc"),
		"C's removal must not stay active on a comment shown under C2's post")
}
