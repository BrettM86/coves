//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func indexedLabelPost(t *testing.T, db *sql.DB) moderation.StrongRef {
	t.Helper()
	communityDID, authorDID := moderationPostActors(t, db)
	return indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "label persistence", "")
}

func labelPost(t *testing.T, service moderation.Service, actor string, subject moderation.StrongRef, version, key string) *moderation.MutationResult {
	t.Helper()
	result, err := service.LabelContent(t.Context(), actor, moderation.LabelContentRequest{
		Subject: subject, LabelValue: moderation.LabelNSFW, ExpectedVersion: version, IdempotencyKey: key,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	return result
}

func requirePersistedLabelState(t *testing.T, state moderation.SubjectState, id, version, status string) {
	t.Helper()
	assert.Equal(t, version, state.Version)
	assert.Equal(t, status, state.Moderation.State)
	require.Equal(t, []moderation.LocalLabel{{Value: moderation.LabelNSFW, Action: moderation.ActionRef{
		ServiceDID: fixtures.InstanceDID(), ActionID: id,
	}}}, state.LocalLabels)
	require.Equal(t, []moderation.ContentLabel{{Value: moderation.LabelNSFW, Sources: []moderation.DecisionSource{{
		AuthorityDID: fixtures.InstanceDID(), ScopeKind: moderation.ScopeInstance,
	}}}}, state.Moderation.ContentLabels)
}

func TestModerationLabelPostgresApplyPersistsOverlayAndReplay(t *testing.T) {
	db := testkit.DB(t)
	subject := indexedLabelPost(t, db)
	service := newPostgresModerationService(db)
	actor := fixtures.DID("labelpersistadmin")
	var beforeRow, afterRow string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(p)::text FROM posts p WHERE uri = $1`, subject.URI).Scan(&beforeRow))
	views, err := postgres.NewPostRepository(db).GetViewsByURIs(t.Context(), []string{subject.URI}, "")
	require.NoError(t, err)
	require.Contains(t, views, subject.URI)
	beforeRecord := views[subject.URI].Record
	request := moderation.LabelContentRequest{Subject: subject, LabelValue: moderation.LabelNSFW, ExpectedVersion: "v0", IdempotencyKey: "label-postgres-apply"}
	first, err := service.LabelContent(t.Context(), actor, request)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, moderation.OutcomeApplied, first.Outcome)
	require.NotNil(t, first.Action)
	id := first.Action.ID
	requirePersistedLabelState(t, first.State, id, "v1", moderation.ModerationStateClear)
	var actionCount int
	var kind, value, observedCID, origin string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT action, label_value, observed_cid, origin FROM moderation_actions WHERE id = $1 AND subject_uri = $2`, id, subject.URI).Scan(&kind, &value, &observedCID, &origin))
	assert.Equal(t, moderation.ActionLabel, kind)
	assert.Equal(t, moderation.LabelNSFW, value)
	assert.Equal(t, subject.CID, observedCID)
	assert.Equal(t, moderation.OriginLocal, origin)
	assert.Equal(t, 1, countModerationActions(t, db, subject.URI, moderation.ActionLabel))
	var decisionKind, decisionValue, activeAction string
	var active bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT kind, value, active, active_action_id FROM moderation_decisions WHERE subject_uri = $1`, subject.URI).Scan(&decisionKind, &decisionValue, &active, &activeAction))
	assert.Equal(t, "label", decisionKind)
	assert.Equal(t, moderation.LabelNSFW, decisionValue)
	assert.True(t, active)
	assert.Equal(t, id, activeAction)
	var version int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT version FROM moderation_subjects WHERE subject_uri = $1`, subject.URI).Scan(&version))
	assert.EqualValues(t, 1, version)
	state, err := service.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	require.NotNil(t, state)
	requirePersistedLabelState(t, *state, id, "v1", moderation.ModerationStateClear)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(p)::text FROM posts p WHERE uri = $1`, subject.URI).Scan(&afterRow))
	assert.Equal(t, beforeRow, afterRow, "the entire indexed post row, including content, labels and CID, must be unchanged")
	views, err = postgres.NewPostRepository(db).GetViewsByURIs(t.Context(), []string{subject.URI}, "")
	require.NoError(t, err)
	require.Contains(t, views, subject.URI)
	assert.Equal(t, beforeRecord, views[subject.URI].Record, "the served record must also be unchanged")

	replayed, err := service.LabelContent(t.Context(), actor, request)
	require.NoError(t, err)
	assert.Equal(t, first, replayed, "Postgres idempotency must round-trip the full result and labels")
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1`, subject.URI).Scan(&actionCount))
	assert.Equal(t, 1, actionCount)
}

func TestModerationLabelPostgresRetractAndReactivateSameDecision(t *testing.T) {
	db := testkit.DB(t)
	subject := indexedLabelPost(t, db)
	service := newPostgresModerationService(db)
	actor := fixtures.DID("labelcycleadmin")
	first := labelPost(t, service, actor, subject, "v0", "cycle-first")
	retracted, err := service.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
		ActionID: first.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v1", IdempotencyKey: "cycle-retract",
	})
	require.NoError(t, err)
	require.NotNil(t, retracted)
	require.Equal(t, moderation.OutcomeApplied, retracted.Outcome)
	require.NotNil(t, retracted.Action)
	assert.Equal(t, "v2", retracted.State.Version)
	assert.Empty(t, retracted.State.LocalLabels)
	assert.Empty(t, retracted.State.Moderation.ContentLabels)
	var value, reverses string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT label_value, reverses_action_id FROM moderation_actions WHERE id = $1 AND action = 'retract-label'`, retracted.Action.ID).Scan(&value, &reverses))
	assert.Equal(t, moderation.LabelNSFW, value)
	assert.Equal(t, first.Action.ID, reverses)
	var active bool
	var activeAction string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT active, active_action_id FROM moderation_decisions WHERE subject_uri = $1 AND kind = 'label' AND value = 'nsfw'`, subject.URI).Scan(&active, &activeAction))
	assert.False(t, active)
	assert.Equal(t, first.Action.ID, activeAction)
	state, err := service.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, "v2", state.Version)
	assert.Empty(t, state.LocalLabels)
	assert.Empty(t, state.Moderation.ContentLabels)
	second := labelPost(t, service, actor, subject, "v2", "cycle-second")
	requirePersistedLabelState(t, second.State, second.Action.ID, "v3", moderation.ModerationStateClear)
	var decisions int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_decisions WHERE subject_uri = $1 AND kind = 'label' AND value = 'nsfw'`, subject.URI).Scan(&decisions))
	assert.Equal(t, 1, decisions, "re-apply must reuse the existing decision row")
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT active, active_action_id FROM moderation_decisions WHERE subject_uri = $1 AND kind = 'label' AND value = 'nsfw'`, subject.URI).Scan(&active, &activeAction))
	assert.True(t, active)
	assert.Equal(t, second.Action.ID, activeAction)
	state, err = service.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	require.NotNil(t, state)
	requirePersistedLabelState(t, *state, second.Action.ID, "v3", moderation.ModerationStateClear)
	retry, err := service.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
		ActionID: first.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v3", IdempotencyKey: "cycle-superseded",
	})
	assert.ErrorIs(t, err, moderation.ErrInvalidDecision)
	assert.Nil(t, retry)
	assert.Equal(t, 1, countModerationActions(t, db, subject.URI, moderation.ActionRetractLabel))
}

func TestModerationLabelPostgresCoexistsWithRemoval(t *testing.T) {
	db := testkit.DB(t)
	subject := indexedLabelPost(t, db)
	service := newPostgresModerationService(db)
	actor := fixtures.DID("labeloverlayadmin")
	label := labelPost(t, service, actor, subject, "v0", "overlay-label")
	removed, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v1", IdempotencyKey: "overlay-remove", Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, removed.Action)
	state, err := service.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	require.NotNil(t, state)
	requirePersistedLabelState(t, *state, label.Action.ID, "v2", moderation.ModerationStateRemoved)
	require.NotNil(t, state.LocalRemoval)
	assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)
	restored, err := service.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v2",
		IdempotencyKey: "overlay-restore", Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, restored)
	requirePersistedLabelState(t, restored.State, label.Action.ID, "v3", moderation.ModerationStateClear)
	state, err = service.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	require.NotNil(t, state)
	requirePersistedLabelState(t, *state, label.Action.ID, "v3", moderation.ModerationStateClear)
	assert.Nil(t, state.LocalRemoval)
}

func TestModerationLabelPostgresConcurrentAppliesRejectStaleVersion(t *testing.T) {
	db := testkit.DB(t)
	subject := indexedLabelPost(t, db)
	service := newPostgresModerationService(db)
	// Establish the subject row before either request so the second waiter is
	// demonstrably blocked on LockSubject, not on a concurrent INSERT.
	_, err := db.ExecContext(t.Context(), `INSERT INTO moderation_subjects (subject_uri, version) VALUES ($1, 0)`, subject.URI)
	require.NoError(t, err)
	connection, release := holdModerationActionInsert(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	firstAdmin, secondAdmin := fixtures.DID("labelracefirst"), fixtures.DID("labelracesecond")
	require.NotEqual(t, firstAdmin, secondAdmin)
	results := startConcurrentModerationCalls(
		func() (*moderation.MutationResult, error) {
			return service.LabelContent(ctx, firstAdmin, moderation.LabelContentRequest{Subject: subject, LabelValue: moderation.LabelNSFW, ExpectedVersion: "v0", IdempotencyKey: "race-first"})
		},
		func() (*moderation.MutationResult, error) {
			return service.LabelContent(ctx, secondAdmin, moderation.LabelContentRequest{Subject: subject, LabelValue: moderation.LabelNSFW, ExpectedVersion: "v0", IdempotencyKey: "race-second"})
		},
	)
	waitForSubjectLockContention(t, connection)
	release()
	var applied, conflicts int
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			assert.ErrorIs(t, outcome.err, moderation.ErrStateConflict)
			assert.Nil(t, outcome.result)
			if errors.Is(outcome.err, moderation.ErrStateConflict) {
				conflicts++
			}
			continue
		}
		require.NotNil(t, outcome.result)
		assert.Equal(t, moderation.OutcomeApplied, outcome.result.Outcome)
		if outcome.result.Outcome == moderation.OutcomeApplied {
			applied++
		}
	}
	assert.Equal(t, 1, applied)
	assert.Equal(t, 1, conflicts)
	assert.Equal(t, 1, countModerationActions(t, db, subject.URI, moderation.ActionLabel), "exactly one racing apply")
}

// Activating a label decision that is already active must fail closed and
// leave the active decision pointing at its original action.
func TestModerationLabelPostgresActivateRejectsAlreadyActiveDecision(t *testing.T) {
	db := testkit.DB(t)
	subject := indexedLabelPost(t, db)
	service := newPostgresModerationService(db)
	actor := fixtures.DID("labeldoubleadmin")
	first := labelPost(t, service, actor, subject, "v0", "double-activate")
	var activateErr error
	var secondID string
	require.NoError(t, postgres.NewModerationRepository(db).InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
		second, err := tx.InsertAction(ctx, moderation.Action{
			ActorDID: actor, AuthorityDID: fixtures.InstanceDID(), ScopeKind: moderation.ScopeInstance,
			SubjectURI: subject.URI, SubjectCollection: first.Action.SubjectCollection,
			SubjectCommunityDID: first.Action.SubjectCommunityDID, ObservedCID: subject.CID,
			Action: moderation.ActionLabel, LabelValue: moderation.LabelNSFW,
			Origin: moderation.OriginLocal, CreatedAt: time.Now(),
		})
		if err != nil {
			return err
		}
		secondID = second.ID
		// Commit whatever the failed activation wrote, so a write it made
		// before reporting the error would be visible below.
		activateErr = tx.SetLabelDecision(ctx, fixtures.InstanceDID(), subject.URI, moderation.LabelNSFW, second.ID, true)
		return nil
	}))
	require.Error(t, activateErr)
	require.NotEmpty(t, secondID)
	var active bool
	var activeAction string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT active, active_action_id FROM moderation_decisions WHERE subject_uri = $1 AND kind = 'label' AND value = 'nsfw'`, subject.URI).Scan(&active, &activeAction))
	assert.True(t, active)
	assert.Equal(t, first.Action.ID, activeAction, "the already-active decision must keep its original action")
}
