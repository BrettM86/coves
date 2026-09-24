//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModerationRepositoryReplaysOriginalRemovalAfterRestore(t *testing.T) {
	db := testkit.DB(t)
	subject, _, _ := indexedModerationComment(t, db, true, "")
	service := newPostgresModerationService(db)
	actor := fixtures.DID("persistreplayadmin")
	request := moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "k-remove",
		Reason: moderationConcurrencyReason,
	}

	original, err := service.RemoveContent(t.Context(), actor, request)
	require.NoError(t, err)
	require.NotNil(t, original)
	require.NotNil(t, original.Action)
	require.Equal(t, moderation.OutcomeApplied, original.Outcome)
	require.Equal(t, "v1", original.State.Version)
	restored, err := service.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
		ActionID: original.Action.ID, ReviewedSubject: &subject,
		ExpectedVersion: "v1", IdempotencyKey: "k-restore", Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, restored)
	require.Equal(t, "v2", restored.State.Version)

	// A persisted replay must return the old result even though the subject is clear now.
	replayed, err := newPostgresModerationService(db).RemoveContent(t.Context(), actor, request)
	require.NoError(t, err)
	assert.Equal(t, original, replayed)
	assert.Equal(t, 1, countModerationActions(t, db, subject.URI, moderation.ActionRemove))
	assert.Equal(t, 1, countModerationActions(t, db, subject.URI, moderation.ActionRestore))
	state, err := service.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	assert.Equal(t, restored.State.Version, state.Version)
	assert.Equal(t, moderation.ModerationStateClear, state.Moderation.State)
	assert.Nil(t, state.LocalRemoval)
}

func TestModerationRepositorySweepsExpiredIdempotencyKeyWithoutResettingVersion(t *testing.T) {
	db := testkit.DB(t)
	subject, _, _ := indexedModerationComment(t, db, true, "")
	repository := postgres.NewModerationRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		repository,
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: time.Hour,
			MaxLiveIdempotencyKeys: 1000, Now: func() time.Time { return now }},
	)
	actor := fixtures.DID("expiryadmin")
	request := moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "k1", Reason: moderationConcurrencyReason,
	}
	removed, err := service.RemoveContent(t.Context(), actor, request)
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	require.Equal(t, "v1", removed.State.Version)
	var expiresAt time.Time
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT expires_at FROM moderation_idempotency_keys
		WHERE actor_did = $1 AND authority_did = $2 AND key = $3
	`, actor, fixtures.InstanceDID(), request.IdempotencyKey).Scan(&expiresAt))
	assert.WithinDuration(t, now.Add(time.Hour), expiresAt, time.Second)

	// A key written later is still live at sweep time and must survive it.
	liveSubject, _, _ := indexedModerationComment(t, db, true, "")
	liveActor := fixtures.DID("liveexpiryadmin")
	started := now
	now = started.Add(30 * time.Minute)
	live, err := service.RemoveContent(t.Context(), liveActor, moderation.RemoveContentRequest{
		Subject: liveSubject, ExpectedVersion: "v0", IdempotencyKey: "k-live", Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, live)
	require.Equal(t, moderation.OutcomeApplied, live.Outcome)

	now = started.Add(time.Hour + time.Second)
	deleted, err := repository.DeleteExpiredIdempotencyKeys(t.Context(), now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, deleted)
	var liveKeyCount int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1 AND key = $2`, liveActor, "k-live").Scan(&liveKeyCount))
	assert.Equal(t, 1, liveKeyCount, "the sweep must keep keys that have not expired")
	var keyCount int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1 AND authority_did = $2 AND key = $3`, actor, fixtures.InstanceDID(), request.IdempotencyKey).Scan(&keyCount))
	assert.Zero(t, keyCount)

	result, err := service.RemoveContent(t.Context(), actor, request)
	require.ErrorIs(t, err, moderation.ErrStateConflict)
	assert.Nil(t, result)
	assert.Equal(t, 1, countModerationActions(t, db, subject.URI, moderation.ActionRemove))
	state, err := service.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	assert.Equal(t, "v1", state.Version)
	assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1`, actor).Scan(&keyCount))
	assert.Zero(t, keyCount, "failed retries must not be saved")
}

func TestModerationRepositoryEnforcesLiveKeyCapWithoutChangingSecondSubject(t *testing.T) {
	db := testkit.DB(t)
	firstSubject, _, _ := indexedModerationComment(t, db, true, "")
	secondSubject, _, _ := indexedModerationComment(t, db, true, "")
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: time.Hour, MaxLiveIdempotencyKeys: 1},
	)
	actor := fixtures.DID("singlekeyadmin")
	firstRequest := moderation.RemoveContentRequest{
		Subject: firstSubject, ExpectedVersion: "v0", IdempotencyKey: "first-key", Reason: moderationConcurrencyReason,
	}
	first, err := service.RemoveContent(t.Context(), actor, firstRequest)
	require.NoError(t, err)
	require.NotNil(t, first)
	result, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
		Subject: secondSubject, ExpectedVersion: "v0", IdempotencyKey: "second-key", Reason: moderationConcurrencyReason,
	})
	require.ErrorIs(t, err, moderation.ErrInvalidRequest)
	assert.ErrorContains(t, err, "1")
	assert.Nil(t, result)
	assert.Zero(t, countModerationActions(t, db, secondSubject.URI, moderation.ActionRemove))
	state, err := service.GetSubjectState(t.Context(), secondSubject.URI)
	require.NoError(t, err)
	assert.Equal(t, "v0", state.Version)
	assert.Equal(t, moderation.ModerationStateClear, state.Moderation.State)
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1`, actor).Scan(&count))
	assert.Equal(t, 1, count)
	replayed, err := service.RemoveContent(t.Context(), actor, firstRequest)
	require.NoError(t, err)
	assert.Equal(t, first, replayed, "the cap must still allow an existing key to replay")
}

func TestModerationRepositoryReplacesOnlyExpiredUnsweptIdempotencyKey(t *testing.T) {
	db := testkit.DB(t)
	firstSubject, _, _ := indexedModerationComment(t, db, true, "")
	secondSubject, _, _ := indexedModerationComment(t, db, true, "")
	repository := postgres.NewModerationRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		repository,
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: time.Hour,
			MaxLiveIdempotencyKeys: 1000, Now: func() time.Time { return now }},
	)
	actor := fixtures.DID("reusedkeyadmin")
	const key = "reused-key"
	storedKey := func() (fingerprint string, createdAt, expiresAt time.Time) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(t.Context(), `
			SELECT fingerprint, created_at, expires_at FROM moderation_idempotency_keys
			WHERE actor_did = $1 AND authority_did = $2 AND key = $3
		`, actor, fixtures.InstanceDID(), key).Scan(&fingerprint, &createdAt, &expiresAt))
		return fingerprint, createdAt, expiresAt
	}
	first, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
		Subject: firstSubject, ExpectedVersion: "v0", IdempotencyKey: key, Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, moderation.OutcomeApplied, first.Outcome)
	firstFingerprint, _, firstExpiry := storedKey()

	// While the key is live, a direct save of a different record must not
	// overwrite it.
	now = now.Add(time.Minute)
	require.NoError(t, repository.InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
		return tx.SaveIdempotencyRecord(ctx, moderation.IdempotencyRecord{
			ActorDID: actor, AuthorityDID: fixtures.InstanceDID(), Key: key, Fingerprint: "live-overwrite-attempt",
			Result: moderation.MutationResult{Outcome: moderation.OutcomeUnchanged}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		})
	}))
	fingerprint, _, expiresAt := storedKey()
	assert.Equal(t, firstFingerprint, fingerprint)
	assert.True(t, firstExpiry.Equal(expiresAt), "a live key keeps its expiry")

	// After expiry and before any sweep, the same key with a different request
	// is admitted and replaces the stored row.
	now = firstExpiry.Add(time.Second)
	secondRequest := moderation.RemoveContentRequest{
		Subject: secondSubject, ExpectedVersion: "v0", IdempotencyKey: key, Reason: moderationConcurrencyReason,
	}
	second, err := service.RemoveContent(t.Context(), actor, secondRequest)
	require.NoError(t, err, "an expired key must not conflict with a new request")
	require.NotNil(t, second)
	require.Equal(t, moderation.OutcomeApplied, second.Outcome)
	fingerprint, createdAt, expiresAt := storedKey()
	assert.NotEqual(t, firstFingerprint, fingerprint)
	assert.WithinDuration(t, now, createdAt, time.Second)
	assert.WithinDuration(t, now.Add(time.Hour), expiresAt, time.Second)
	var rows int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1`, actor).Scan(&rows))
	assert.Equal(t, 1, rows)

	replayed, err := service.RemoveContent(t.Context(), actor, secondRequest)
	require.NoError(t, err)
	assert.Equal(t, second, replayed, "the replaced row must replay the new result")
	assert.Equal(t, 1, countModerationActions(t, db, firstSubject.URI, moderation.ActionRemove))
	assert.Equal(t, 1, countModerationActions(t, db, secondSubject.URI, moderation.ActionRemove))
}
