//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B7: the Postgres community record write outbox keeps the contract the
// in-memory store in internal/core/moderation/fake_store_test.go implements:
// one row per (community, subject); a new removal action takes the row over
// and keeps what it records about the repo; a restore is honoured only for the
// owning action; claims lease rows without handing one row to two workers;
// progress lands only under the claim that holds the row; completion only
// under an unchanged generation.

var _ moderation.CommunityRecordWriteStore = (*postgres.ModerationRepository)(nil)

const (
	recordWriteLease          = 2 * time.Minute
	recordWriteOwnedRemoval   = "bafyreiownedremovalrecord"
	recordWriteAcceptedCID    = "bafyreiacceptedcontent"
	recordWriteSubjectCID     = "bafyreireviewedcontent"
	recordWriteFailureCode    = "pds_unavailable"
	recordWriteSecondFailure  = "pds_timeout"
	recordWriteForcedRollback = "forced rollback after recording"
)

// recordWritesFixture is a moderation repository over a fresh database, a
// community row, and the cutoff the claims run at. The cutoff sits a minute
// past the wall clock so a database clock a little ahead of the test host
// still finds rows "due now".
type recordWritesFixture struct {
	db           *sql.DB
	repository   *postgres.ModerationRepository
	communityDID string
	dueAt        time.Time
}

func newRecordWritesFixture(t *testing.T) recordWritesFixture {
	t.Helper()
	db := testkit.DB(t)
	name := testkit.UniqueIDWithPrefix(t, "recordw")
	communityDID, err := fixtures.Community(t.Context(), db, name, "owner"+name)
	require.NoError(t, err)
	return recordWritesFixture{
		db: db, repository: postgres.NewModerationRepository(db), communityDID: communityDID,
		dueAt: time.Now().UTC().Add(time.Minute),
	}
}

// subjectURI is a post (or comment) URI in this fixture's community.
func (fixture recordWritesFixture) subjectURI(collection, rkey string) string {
	return "at://" + fixtures.DID("recordauthor") + "/" + collection + "/" + rkey
}

// insertRemovalAction stores an instance removal action for the subject and
// returns its ID, so the outbox row can name a real action.
func (fixture recordWritesFixture) insertRemovalAction(t *testing.T, subjectURI, collection string, createdAt time.Time) string {
	t.Helper()
	var actionID string
	require.NoError(t, fixture.repository.InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
		action, err := tx.InsertAction(ctx, moderation.Action{
			ActorDID: fixtures.DID("recordadmin"), AuthorityDID: fixtures.InstanceDID(), ScopeKind: moderation.ScopeInstance,
			SubjectURI: subjectURI, SubjectCollection: collection, SubjectCommunityDID: fixture.communityDID,
			ObservedCID: recordWriteSubjectCID, Action: moderation.ActionRemove,
			Reason: "social.coves.moderation.defs#reasonSpam", Origin: moderation.OriginLocal, CreatedAt: createdAt,
		})
		if err != nil {
			return err
		}
		actionID = action.ID
		return nil
	}))
	require.NotEmpty(t, actionID)
	return actionID
}

// removal is the outbox row a removal action asks for.
func (fixture recordWritesFixture) removal(subjectURI, collection, actionID string, code posts.DecisionCode, createdAt time.Time) moderation.CommunityRecordWrite {
	return moderation.CommunityRecordWrite{
		CommunityDID: fixture.communityDID, SubjectURI: subjectURI, SubjectCollection: collection,
		SubjectCID: recordWriteSubjectCID, RemovalActionID: actionID, Code: code, RemovalCreatedAt: createdAt,
	}
}

// requireRemoval commits RequireCommunityRecordRemoval in its own transaction.
func (fixture recordWritesFixture) requireRemoval(t *testing.T, write moderation.CommunityRecordWrite) {
	t.Helper()
	require.NoError(t, fixture.repository.InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
		return tx.RequireCommunityRecordRemoval(ctx, write)
	}))
}

// requestRestore commits RequestCommunityRecordRestore and reports whether it
// changed a row.
func (fixture recordWritesFixture) requestRestore(t *testing.T, subjectURI, actionID, acceptanceCID string) bool {
	t.Helper()
	var changed bool
	require.NoError(t, fixture.repository.InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
		var err error
		changed, err = tx.RequestCommunityRecordRestore(ctx, fixture.communityDID, subjectURI, actionID, acceptanceCID)
		return err
	}))
	return changed
}

// claim claims the subject's row at the given cutoff.
func (fixture recordWritesFixture) claim(t *testing.T, subjectURI string, dueAt time.Time) []moderation.CommunityRecordWrite {
	t.Helper()
	claimed, err := fixture.repository.ClaimDueCommunityRecordWrites(t.Context(), moderation.CommunityRecordWriteClaim{
		DueAt: dueAt, Lease: recordWriteLease, Limit: 10,
		Subject: &moderation.CommunityRecordSubject{CommunityDID: fixture.communityDID, SubjectURI: subjectURI},
	})
	require.NoError(t, err)
	return claimed
}

// claimOne claims the subject's row and requires exactly one.
func (fixture recordWritesFixture) claimOne(t *testing.T, subjectURI string, dueAt time.Time) moderation.CommunityRecordWrite {
	t.Helper()
	claimed := fixture.claim(t, subjectURI, dueAt)
	require.Lenf(t, claimed, 1, "the row for %s must be claimable at %s", subjectURI, dueAt)
	return claimed[0]
}

// withProgress is a claimed row carrying what an attempt learned about the repo.
func withProgress(write moderation.CommunityRecordWrite, startedAt time.Time) moderation.CommunityRecordWrite {
	write.OwnedRemovalCID = recordWriteOwnedRemoval
	write.AcceptanceSeen = true
	write.WithdrewAcceptance = true
	write.LastWriteStartedAt = &startedAt
	return write
}

func assertRepoFacts(t *testing.T, write moderation.CommunityRecordWrite, startedAt time.Time, message string) {
	t.Helper()
	assert.Equal(t, recordWriteOwnedRemoval, write.OwnedRemovalCID, message)
	assert.True(t, write.AcceptanceSeen, message)
	assert.True(t, write.WithdrewAcceptance, message)
	if assert.NotNil(t, write.LastWriteStartedAt, message) {
		assert.True(t, startedAt.Equal(*write.LastWriteStartedAt), "%s: last write started at %s, want %s", message, *write.LastWriteStartedAt, startedAt)
	}
}

func TestModerationCommunityRecordWritesRemovalStoresTheRequestedRow(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)
	postURI := fixture.subjectURI(moderation.PostV2Collection, "3kstored")
	actionID := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, removedAt)

	fixture.requireRemoval(t, fixture.removal(postURI, moderation.PostV2Collection, actionID, posts.DecisionSpam, removedAt))

	write := fixture.claimOne(t, postURI, fixture.dueAt)
	assert.Equal(t, fixture.communityDID, write.CommunityDID)
	assert.Equal(t, postURI, write.SubjectURI)
	assert.Equal(t, moderation.PostV2Collection, write.SubjectCollection)
	assert.Equal(t, recordWriteSubjectCID, write.SubjectCID)
	assert.Equal(t, actionID, write.RemovalActionID)
	assert.Equal(t, posts.DecisionSpam, write.Code)
	assert.True(t, removedAt.Equal(write.RemovalCreatedAt), "removal created at %s, want %s", write.RemovalCreatedAt, removedAt)
	assert.Equal(t, moderation.CommunityRecordRemoved, write.Desired)
	assert.Empty(t, write.RestoreAcceptanceCID)
	assert.Empty(t, write.OwnedRemovalCID, "nothing is known about the repo before an attempt")
	assert.False(t, write.AcceptanceSeen)
	assert.False(t, write.WithdrewAcceptance)
	assert.Nil(t, write.LastWriteStartedAt)
	assert.False(t, write.Completed)
	assert.Equal(t, 0, write.Attempts)
	assert.Empty(t, write.LastFailureCode)
}

// A second removal action for the same subject takes the one row over: it is
// pending and due now again, under a new generation, owned by the new action,
// and it keeps what earlier attempts recorded about the community's repo.
func TestModerationCommunityRecordWritesNewRemovalActionTakesOverTheRow(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	firstAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	secondAt := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 11, 11, 9, 1, 0, 0, time.UTC)
	postURI := fixture.subjectURI(moderation.PostV2Collection, "3ktakeover")
	firstAction := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, firstAt)
	secondAction := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, secondAt)

	fixture.requireRemoval(t, fixture.removal(postURI, moderation.PostV2Collection, firstAction, posts.DecisionSpam, firstAt))
	claimed := fixture.claimOne(t, postURI, fixture.dueAt)
	require.NoError(t, fixture.repository.RescheduleCommunityRecordWrite(t.Context(), withProgress(claimed, startedAt),
		fixture.dueAt.Add(24*time.Hour), recordWriteFailureCode))

	fixture.requireRemoval(t, fixture.removal(postURI, moderation.PostV2Collection, secondAction, posts.DecisionRuleViolation, secondAt))

	taken := fixture.claimOne(t, postURI, fixture.dueAt)
	assert.Equal(t, secondAction, taken.RemovalActionID)
	assert.Equal(t, posts.DecisionRuleViolation, taken.Code)
	assert.True(t, secondAt.Equal(taken.RemovalCreatedAt), "removal created at %s, want the new action's %s", taken.RemovalCreatedAt, secondAt)
	assert.Equal(t, moderation.CommunityRecordRemoved, taken.Desired)
	assert.Equal(t, claimed.Generation+1, taken.Generation, "a new owning action bumps the generation")
	assert.False(t, taken.Completed)
	assert.Equal(t, 0, taken.Attempts, "a new desired state starts its attempts over")
	assert.Empty(t, taken.LastFailureCode)
	assertRepoFacts(t, taken, startedAt, "what the row knows about the repo survives a new owning action")

	other := fixture.subjectURI(moderation.PostV2Collection, "3kotherrow")
	otherAction := fixture.insertRemovalAction(t, other, moderation.PostV2Collection, secondAt)
	fixture.requireRemoval(t, fixture.removal(other, moderation.PostV2Collection, otherAction, posts.DecisionSpam, secondAt))
	all, err := fixture.repository.ClaimDueCommunityRecordWrites(t.Context(), moderation.CommunityRecordWriteClaim{
		DueAt: fixture.dueAt.Add(10 * time.Minute), Lease: recordWriteLease, Limit: 10,
	})
	require.NoError(t, err)
	var subjects []string
	for _, write := range all {
		subjects = append(subjects, write.SubjectURI)
	}
	assert.ElementsMatch(t, []string{postURI, other}, subjects, "one row per subject")
}

// A restore is honoured only for the action that owns the row. It makes a
// completed row pending and due now under a new generation and keeps what the
// row records about the repo.
func TestModerationCommunityRecordWritesRestoreOnlyForTheOwningAction(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 11, 11, 9, 1, 0, 0, time.UTC)
	postURI := fixture.subjectURI(moderation.PostV2Collection, "3krestore")
	commentURI := fixture.subjectURI(moderation.CommentCollection, "3krestorecomment")
	postAction := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, removedAt)
	commentAction := fixture.insertRemovalAction(t, commentURI, moderation.CommentCollection, removedAt)
	otherAction := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, removedAt.Add(time.Minute))

	fixture.requireRemoval(t, fixture.removal(postURI, moderation.PostV2Collection, postAction, posts.DecisionSpam, removedAt))
	fixture.requireRemoval(t, fixture.removal(commentURI, moderation.CommentCollection, commentAction, posts.DecisionSpam, removedAt))
	completedPost := fixture.claimOne(t, postURI, fixture.dueAt)
	require.NoError(t, fixture.repository.CompleteCommunityRecordWrite(t.Context(), withProgress(completedPost, startedAt)))
	require.Empty(t, fixture.claim(t, postURI, fixture.dueAt.Add(time.Hour)), "a completed row is not claimed")

	assert.False(t, fixture.requestRestore(t, postURI, otherAction, recordWriteAcceptedCID),
		"a restore of an action that does not own the row changes nothing")
	assert.Empty(t, fixture.claim(t, postURI, fixture.dueAt.Add(time.Hour)), "the refused restore left the row completed")
	assert.False(t, fixture.requestRestore(t, fixture.subjectURI(moderation.PostV2Collection, "3knorow"), postAction, recordWriteAcceptedCID),
		"a restore for a subject with no row changes nothing")

	require.True(t, fixture.requestRestore(t, postURI, postAction, recordWriteAcceptedCID))
	restored := fixture.claimOne(t, postURI, fixture.dueAt)
	assert.Equal(t, moderation.CommunityRecordRestored, restored.Desired)
	assert.Equal(t, recordWriteAcceptedCID, restored.RestoreAcceptanceCID)
	assert.Equal(t, postAction, restored.RemovalActionID)
	assert.Equal(t, completedPost.Generation+1, restored.Generation)
	assert.False(t, restored.Completed)
	assertRepoFacts(t, restored, startedAt, "a restore keeps the removal the row owns")

	require.True(t, fixture.requestRestore(t, commentURI, commentAction, ""))
	restoredComment := fixture.claimOne(t, commentURI, fixture.dueAt)
	assert.Equal(t, moderation.CommunityRecordRestored, restoredComment.Desired)
	assert.Empty(t, restoredComment.RestoreAcceptanceCID, "a comment restore pins no acceptance")
}

// Claimers racing over the same due rows each get a different row: a row is
// never handed to two workers, and a locked row is skipped, not waited on.
func TestModerationCommunityRecordWritesConcurrentClaimsNeverShareARow(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	const rows = 8
	for i := range rows {
		uri := fixture.subjectURI(moderation.PostV2Collection, fmt.Sprintf("3kconcurrent%d", i))
		action := fixture.insertRemovalAction(t, uri, moderation.PostV2Collection, removedAt)
		fixture.requireRemoval(t, fixture.removal(uri, moderation.PostV2Collection, action, posts.DecisionSpam, removedAt))
	}

	start := make(chan struct{})
	results := make([][]moderation.CommunityRecordWrite, rows)
	errs := make([]error, rows)
	var wg sync.WaitGroup
	for i := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = fixture.repository.ClaimDueCommunityRecordWrites(context.Background(), moderation.CommunityRecordWriteClaim{
				DueAt: fixture.dueAt, Lease: recordWriteLease, Limit: 1,
			})
		}()
	}
	close(start)
	wg.Wait()

	seen := map[string]int{}
	for i := range rows {
		require.NoError(t, errs[i])
		assert.Lenf(t, results[i], 1, "claimer %d must get a row: %d rows are due for %d claimers", i, rows, rows)
		for _, write := range results[i] {
			seen[write.SubjectURI]++
		}
	}
	assert.Len(t, seen, rows, "every due row is claimed exactly once")
	for uri, count := range seen {
		assert.Equalf(t, 1, count, "%s was claimed by %d workers", uri, count)
	}

	again, err := fixture.repository.ClaimDueCommunityRecordWrites(t.Context(), moderation.CommunityRecordWriteClaim{
		DueAt: fixture.dueAt, Lease: recordWriteLease, Limit: rows,
	})
	require.NoError(t, err)
	assert.Empty(t, again, "leased rows are not claimed again before the lease runs out")
}

// A lease keeps a row from a second worker until it runs out; then a new claim
// takes the row, and the first worker's late progress, completion and
// reschedule land nowhere and report moderation.ErrCommunityRecordClaimLost.
func TestModerationCommunityRecordWritesLeaseAndStaleClaim(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 11, 11, 9, 1, 0, 0, time.UTC)
	postURI := fixture.subjectURI(moderation.PostV2Collection, "3klease")
	action := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, removedAt)
	fixture.requireRemoval(t, fixture.removal(postURI, moderation.PostV2Collection, action, posts.DecisionSpam, removedAt))

	stale := fixture.claimOne(t, postURI, fixture.dueAt)
	assert.Empty(t, fixture.claim(t, postURI, fixture.dueAt.Add(recordWriteLease/2)), "a leased row is not claimed inside its lease")

	current := fixture.claimOne(t, postURI, fixture.dueAt.Add(recordWriteLease+time.Minute))
	assert.NotEqual(t, stale.Claim, current.Claim, "a new claim replaces the expired one")
	assert.Equal(t, stale.Generation, current.Generation)

	assert.ErrorIs(t, fixture.repository.SaveCommunityRecordWriteProgress(t.Context(), withProgress(stale, startedAt)),
		moderation.ErrCommunityRecordClaimLost, "a save under a lost claim is reported, so the attempt sends no write")
	assert.ErrorIs(t, fixture.repository.CompleteCommunityRecordWrite(t.Context(), withProgress(stale, startedAt)),
		moderation.ErrCommunityRecordClaimLost)
	assert.ErrorIs(t, fixture.repository.RescheduleCommunityRecordWrite(t.Context(), withProgress(stale, startedAt),
		fixture.dueAt.Add(48*time.Hour), recordWriteFailureCode), moderation.ErrCommunityRecordClaimLost)

	require.NoError(t, fixture.repository.SaveCommunityRecordWriteProgress(t.Context(), moderation.CommunityRecordWrite{
		CommunityDID: current.CommunityDID, SubjectURI: current.SubjectURI, Generation: current.Generation, Claim: current.Claim,
		OwnedRemovalCID: "bafyreicurrentclaimremoval",
	}))
	// The current claim's own lease still holds; read past it.
	after := fixture.claimOne(t, postURI, fixture.dueAt.Add(2*recordWriteLease+2*time.Minute))
	assert.False(t, after.Completed)
	assert.Equal(t, 0, after.Attempts, "a stale reschedule counts no attempt")
	assert.Empty(t, after.LastFailureCode)
	assert.Equal(t, "bafyreicurrentclaimremoval", after.OwnedRemovalCID, "progress lands only under the claim that holds the row")
	assert.False(t, after.AcceptanceSeen)
	assert.False(t, after.WithdrewAcceptance)
	assert.Nil(t, after.LastWriteStartedAt)
}

// Completion under an unchanged generation finishes the row. If a new desired
// state arrived during the attempt, the attempt's progress is still recorded
// but the row stays pending for the next attempt.
func TestModerationCommunityRecordWritesCompletionOnlyUnderTheClaimedGeneration(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 11, 11, 9, 1, 0, 0, time.UTC)

	finished := fixture.subjectURI(moderation.PostV2Collection, "3kfinished")
	finishedAction := fixture.insertRemovalAction(t, finished, moderation.PostV2Collection, removedAt)
	fixture.requireRemoval(t, fixture.removal(finished, moderation.PostV2Collection, finishedAction, posts.DecisionSpam, removedAt))
	require.NoError(t, fixture.repository.CompleteCommunityRecordWrite(t.Context(), withProgress(fixture.claimOne(t, finished, fixture.dueAt), startedAt)))
	assert.Empty(t, fixture.claim(t, finished, fixture.dueAt.Add(24*time.Hour)), "a completed row is never claimed again")

	superseded := fixture.subjectURI(moderation.PostV2Collection, "3ksuperseded")
	supersededAction := fixture.insertRemovalAction(t, superseded, moderation.PostV2Collection, removedAt)
	fixture.requireRemoval(t, fixture.removal(superseded, moderation.PostV2Collection, supersededAction, posts.DecisionSpam, removedAt))
	attempt := fixture.claimOne(t, superseded, fixture.dueAt)
	require.True(t, fixture.requestRestore(t, superseded, supersededAction, recordWriteAcceptedCID), "a restore arrives mid-attempt")
	require.NoError(t, fixture.repository.CompleteCommunityRecordWrite(t.Context(), withProgress(attempt, startedAt)))

	next := fixture.claimOne(t, superseded, fixture.dueAt)
	assert.False(t, next.Completed, "the attempt answered an older generation")
	assert.Equal(t, moderation.CommunityRecordRestored, next.Desired)
	assert.Equal(t, attempt.Generation+1, next.Generation)
	assertRepoFacts(t, next, startedAt, "the superseded attempt's write is still recorded, so the next attempt can undo it")
}

// A reschedule releases the row until nextAttemptAt; a failure code counts an
// attempt, an empty one does not.
func TestModerationCommunityRecordWritesRescheduleDefersTheRow(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 11, 11, 9, 1, 0, 0, time.UTC)
	postURI := fixture.subjectURI(moderation.PostV2Collection, "3kreschedule")
	action := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, removedAt)
	fixture.requireRemoval(t, fixture.removal(postURI, moderation.PostV2Collection, action, posts.DecisionSpam, removedAt))

	first := fixture.claimOne(t, postURI, fixture.dueAt)
	require.NoError(t, fixture.repository.RescheduleCommunityRecordWrite(t.Context(), withProgress(first, startedAt),
		fixture.dueAt.Add(time.Hour), recordWriteFailureCode))
	assert.Empty(t, fixture.claim(t, postURI, fixture.dueAt.Add(30*time.Minute)), "a rescheduled row waits for its next attempt")

	second := fixture.claimOne(t, postURI, fixture.dueAt.Add(time.Hour))
	assert.Equal(t, 1, second.Attempts)
	assert.Equal(t, recordWriteFailureCode, second.LastFailureCode)
	assert.False(t, second.Completed)
	assertRepoFacts(t, second, startedAt, "a reschedule persists the attempt's progress")

	require.NoError(t, fixture.repository.RescheduleCommunityRecordWrite(t.Context(), second, fixture.dueAt.Add(3*time.Hour), ""))
	assert.Empty(t, fixture.claim(t, postURI, fixture.dueAt.Add(2*time.Hour)))
	third := fixture.claimOne(t, postURI, fixture.dueAt.Add(3*time.Hour))
	assert.Equal(t, 1, third.Attempts, "a reschedule without a failure code counts no attempt")
	assert.Equal(t, recordWriteFailureCode, third.LastFailureCode)

	require.NoError(t, fixture.repository.RescheduleCommunityRecordWrite(t.Context(), third, fixture.dueAt.Add(4*time.Hour), recordWriteSecondFailure))
	fourth := fixture.claimOne(t, postURI, fixture.dueAt.Add(4*time.Hour))
	assert.Equal(t, 2, fourth.Attempts)
	assert.Equal(t, recordWriteSecondFailure, fourth.LastFailureCode)
}

// The outbox row is part of the moderation transaction: a transaction that
// fails after recording leaves no row, or the row as it was.
func TestModerationCommunityRecordWritesRollBackWithTheTransaction(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	forced := errors.New(recordWriteForcedRollback)

	rolledBack := fixture.subjectURI(moderation.PostV2Collection, "3krolledback")
	rolledBackAction := fixture.insertRemovalAction(t, rolledBack, moderation.PostV2Collection, removedAt)
	err := fixture.repository.InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
		if err := tx.RequireCommunityRecordRemoval(ctx, fixture.removal(rolledBack, moderation.PostV2Collection, rolledBackAction, posts.DecisionSpam, removedAt)); err != nil {
			return err
		}
		return forced
	})
	require.ErrorIs(t, err, forced)
	assert.Empty(t, fixture.claim(t, rolledBack, fixture.dueAt), "a rolled-back removal leaves no row")

	kept := fixture.subjectURI(moderation.PostV2Collection, "3kkept")
	keptAction := fixture.insertRemovalAction(t, kept, moderation.PostV2Collection, removedAt)
	fixture.requireRemoval(t, fixture.removal(kept, moderation.PostV2Collection, keptAction, posts.DecisionSpam, removedAt))
	err = fixture.repository.InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
		changed, err := tx.RequestCommunityRecordRestore(ctx, fixture.communityDID, kept, keptAction, recordWriteAcceptedCID)
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("the restore request found no row to change inside the transaction")
		}
		return forced
	})
	require.ErrorIs(t, err, forced)
	unchanged := fixture.claimOne(t, kept, fixture.dueAt)
	assert.Equal(t, moderation.CommunityRecordRemoved, unchanged.Desired, "a rolled-back restore leaves the removal standing")
	assert.Empty(t, unchanged.RestoreAcceptanceCID)
}

// IsHostedCommunity is credential presence. A community row that names this
// instance in hosted_by_did but stores no PDS credentials (every firehose-
// indexed community copies that claim from its own profile) is not hosted.
func TestModerationCommunityRecordWritesIsHostedCommunityIsCredentialPresence(t *testing.T) {
	db := testkit.DB(t)
	repository := postgres.NewModerationRepository(db)
	communityRepository := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	id := testkit.UniqueID(t)
	hostedDID := "did:plc:recordhosted" + id
	claimedDID := "did:plc:recordclaimed" + id

	_, err := communityRepository.Create(t.Context(), &communities.Community{
		DID: hostedDID, Handle: "!recordhosted-" + id + "@coves.local", Name: "recordhosted-" + id,
		OwnerDID: hostedDID, CreatedByDID: fixtures.DID("recordcreator"), HostedByDID: fixtures.InstanceDID(),
		Visibility: "public", PDSEmail: "recordhosted@communities.coves.local", PDSPassword: "cleartext",
		PDSAccessToken: "access-token", PDSRefreshToken: "refresh-token-" + id, PDSURL: testkit.Endpoints().PDS.BaseURL,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = communityRepository.Create(t.Context(), &communities.Community{
		DID: claimedDID, Handle: "!recordclaimed-" + id + "@coves.local", Name: "recordclaimed-" + id,
		OwnerDID: claimedDID, CreatedByDID: fixtures.DID("recordcreator"), HostedByDID: fixtures.InstanceDID(),
		Visibility: "public", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	require.NoError(t, err)

	for _, test := range []struct {
		name         string
		communityDID string
		want         bool
	}{
		{name: "stored refresh token", communityDID: hostedDID, want: true},
		{name: "hosted_by_did names this instance without credentials", communityDID: claimedDID, want: false},
		{name: "no community row", communityDID: "did:plc:recordabsent" + id, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hosted bool
			require.NoError(t, repository.InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
				var err error
				hosted, err = tx.IsHostedCommunity(ctx, test.communityDID)
				return err
			}))
			assert.Equal(t, test.want, hosted)
		})
	}
}

// recordedCommunityRecordWrites is a post-commit hook that only remembers what
// it was told.
type recordedCommunityRecordWrites struct {
	mu       sync.Mutex
	subjects []moderation.CommunityRecordSubject
}

func (hook *recordedCommunityRecordWrites) WriteAfterCommit(_ context.Context, subject moderation.CommunityRecordSubject) {
	hook.mu.Lock()
	defer hook.mu.Unlock()
	hook.subjects = append(hook.subjects, subject)
}

// failingCommitModerationStore runs every moderation transaction to the end
// and then fails it, so whatever the service recorded is rolled back.
type failingCommitModerationStore struct {
	*postgres.ModerationRepository
	err error
}

func (store failingCommitModerationStore) InTransaction(ctx context.Context, fn func(ctx context.Context, tx moderation.Transaction) error) error {
	return store.ModerationRepository.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return store.err
	})
}

// credentialedModerationPost is an indexed, accepted post in a community whose
// PDS credentials this AppView stores.
func credentialedModerationPost(t *testing.T, db *sql.DB) (moderation.StrongRef, string) {
	t.Helper()
	communityDID, authorDID := moderationPostActors(t, db)
	_, err := db.ExecContext(t.Context(),
		`UPDATE communities SET pds_refresh_token_encrypted = $2 WHERE did = $1`, communityDID, []byte("a stored refresh token"))
	require.NoError(t, err)
	return indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "record write post", ""), communityDID
}

func newRecordWritesModerationService(db *sql.DB, store moderation.Store, hook moderation.CommunityRecordWrites) moderation.Service {
	config := postgresModerationConfig(nil)
	config.CommunityRecordWrites = hook
	return newPostgresModerationServiceWithConfig(db, store, config)
}

// The real moderation service over Postgres records the row with removeContent
// and turns it into a restore with restoreContent; a removal whose transaction
// fails after recording leaves no row, and a failed restore leaves the row as
// it was.
func TestModerationCommunityRecordWritesThroughTheModerationService(t *testing.T) {
	db := testkit.DB(t)
	repository := postgres.NewModerationRepository(db)
	fixture := recordWritesFixture{db: db, repository: repository, dueAt: time.Now().UTC().Add(time.Minute)}
	subject, communityDID := credentialedModerationPost(t, db)
	fixture.communityDID = communityDID
	forced := errors.New(recordWriteForcedRollback)

	failingHook := &recordedCommunityRecordWrites{}
	_, err := newRecordWritesModerationService(db, failingCommitModerationStore{ModerationRepository: repository, err: forced}, failingHook).
		RemoveContent(t.Context(), fixtures.DID("recordremovaladmin"), moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "record-remove-rolled-back",
			Reason: "social.coves.moderation.defs#reasonSpam",
		})
	require.ErrorIs(t, err, forced)
	assert.Empty(t, fixture.claim(t, subject.URI, fixture.dueAt), "a removal rolled back after recording leaves no row")
	assert.Empty(t, failingHook.subjects, "a rolled-back removal is not handed to the queue")

	hook := &recordedCommunityRecordWrites{}
	service := newRecordWritesModerationService(db, repository, hook)
	removed, err := service.RemoveContent(t.Context(), fixtures.DID("recordremovaladmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "record-remove",
		Reason: "social.coves.moderation.defs#reasonHarassment",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	require.NotNil(t, removed.Action)
	assert.Equal(t, []moderation.CommunityRecordSubject{{CommunityDID: communityDID, SubjectURI: subject.URI}}, hook.subjects)

	write := fixture.claimOne(t, subject.URI, fixture.dueAt)
	assert.Equal(t, moderation.PostV2Collection, write.SubjectCollection)
	assert.Equal(t, moderationPostCID, write.SubjectCID)
	assert.Equal(t, removed.Action.ID, write.RemovalActionID)
	assert.Equal(t, posts.DecisionRuleViolation, write.Code, "harassment is published as rule-violation")
	assert.True(t, removed.Action.CreatedAt.Equal(write.RemovalCreatedAt), "removal created at %s, want the action's %s", write.RemovalCreatedAt, removed.Action.CreatedAt)
	assert.Equal(t, moderation.CommunityRecordRemoved, write.Desired)
	require.NoError(t, repository.RescheduleCommunityRecordWrite(t.Context(), write, fixture.dueAt, ""))

	restoreRequest := moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: removed.State.Version,
		Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	}
	restoreRequest.IdempotencyKey = "record-restore-rolled-back"
	_, err = newRecordWritesModerationService(db, failingCommitModerationStore{ModerationRepository: repository, err: forced}, failingHook).
		RestoreContent(t.Context(), fixtures.DID("recordrestoreadmin"), restoreRequest)
	require.ErrorIs(t, err, forced)
	unchanged := fixture.claimOne(t, subject.URI, fixture.dueAt.Add(recordWriteLease+time.Minute))
	assert.Equal(t, moderation.CommunityRecordRemoved, unchanged.Desired, "a restore rolled back after recording leaves the row as it was")
	assert.Equal(t, write.Generation, unchanged.Generation)
	require.NoError(t, repository.RescheduleCommunityRecordWrite(t.Context(), unchanged, fixture.dueAt, ""))

	restoreRequest.IdempotencyKey = "record-restore"
	restored, err := service.RestoreContent(t.Context(), fixtures.DID("recordrestoreadmin"), restoreRequest)
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	after := fixture.claimOne(t, subject.URI, fixture.dueAt.Add(2*recordWriteLease+2*time.Minute))
	assert.Equal(t, moderation.CommunityRecordRestored, after.Desired)
	assert.Equal(t, moderationPostCID, after.RestoreAcceptanceCID, "the restore pins the post's indexed CID")
	assert.Equal(t, removed.Action.ID, after.RemovalActionID)
	assert.Equal(t, write.Generation+1, after.Generation)
}

// databaseNow is the database clock, which the claim compares leases against.
func (fixture recordWritesFixture) databaseNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, fixture.db.QueryRowContext(t.Context(), `SELECT NOW()`).Scan(&now))
	return now
}

// A claim made late in a sweep carries the sweep's early cutoff. Its lease
// still runs from when the row is claimed, so a worker claiming right after
// does not take a row whose attempt has just begun.
func TestModerationCommunityRecordWritesLeaseRunsFromTheClaim(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	postURI := fixture.subjectURI(moderation.PostV2Collection, "3kleaseclaimtime")
	action := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, removedAt)
	fixture.requireRemoval(t, fixture.removal(postURI, moderation.PostV2Collection, action, posts.DecisionSpam, removedAt))
	databaseNow := fixture.databaseNow(t)
	// Due twenty minutes ago, so a cutoff ten minutes old still reaches it.
	require.NoError(t, fixture.repository.RescheduleCommunityRecordWrite(t.Context(), fixture.claimOne(t, postURI, fixture.dueAt),
		databaseNow.Add(-20*time.Minute), ""))

	lateInTheSweep := fixture.claimOne(t, postURI, databaseNow.Add(-10*time.Minute))

	assert.Empty(t, fixture.claim(t, postURI, fixture.databaseNow(t)),
		"the row claimed a moment ago is still leased: a lease measured from a ten-minute-old cutoff has already run out")
	require.NoError(t, fixture.repository.CompleteCommunityRecordWrite(t.Context(), lateInTheSweep),
		"the late claim still holds the row")
}

// A failed attempt that answered an older generation counts nothing against
// the new desired state: the row keeps its new generation's zero attempts and
// no failure code, and stays due now. The claim is checked first: the attempt
// still holds the row, so its progress is saved and nothing is reported lost.
func TestModerationCommunityRecordWritesRescheduleUnderAChangedGenerationCountsNoFailure(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 11, 11, 9, 1, 0, 0, time.UTC)
	postURI := fixture.subjectURI(moderation.PostV2Collection, "3kgenerationfailure")
	action := fixture.insertRemovalAction(t, postURI, moderation.PostV2Collection, removedAt)
	fixture.requireRemoval(t, fixture.removal(postURI, moderation.PostV2Collection, action, posts.DecisionSpam, removedAt))

	attempt := fixture.claimOne(t, postURI, fixture.dueAt)
	require.True(t, fixture.requestRestore(t, postURI, action, recordWriteAcceptedCID), "a restore arrives mid-attempt")
	require.NoError(t, fixture.repository.RescheduleCommunityRecordWrite(t.Context(), withProgress(attempt, startedAt),
		fixture.dueAt.Add(24*time.Hour), recordWriteFailureCode))

	next := fixture.claimOne(t, postURI, fixture.dueAt)
	assert.Equal(t, moderation.CommunityRecordRestored, next.Desired, "the restore is due now, not when the old removal's retry was")
	assert.Equal(t, attempt.Generation+1, next.Generation)
	assert.Equal(t, 0, next.Attempts, "the old generation's failure is not the restore's")
	assert.Empty(t, next.LastFailureCode)
	assertRepoFacts(t, next, startedAt, "the superseded attempt's progress is still saved")
}

// sweepRepo is an in-memory community repo for driving the real queue over
// Postgres: one removal slot per subject, and per-subject hooks that run
// inside the next removal write.
type sweepRepo struct {
	mu          sync.Mutex
	removals    map[string]posts.StandingRemoval
	commits     int
	writes      map[string]int
	undos       int
	beforeWrite map[string]func()
}

func newSweepRepo() *sweepRepo {
	return &sweepRepo{removals: map[string]posts.StandingRemoval{}, writes: map[string]int{}, beforeWrite: map[string]func(){}}
}

func (repo *sweepRepo) StandingAcceptance(context.Context, string, string) (*posts.StandingRecordRef, error) {
	return nil, nil
}

func (repo *sweepRepo) StandingRemoval(_ context.Context, _, subjectURI string) (*posts.StandingRemoval, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if removal, ok := repo.removals[subjectURI]; ok {
		return &removal, nil
	}
	return nil, nil
}

func (repo *sweepRepo) WriteInstanceRemoval(_ context.Context, cmd posts.InstanceRemovalCommand) (posts.InstanceRemovalResult, error) {
	repo.mu.Lock()
	repo.writes[cmd.SubjectURI]++
	hook := repo.beforeWrite[cmd.SubjectURI]
	delete(repo.beforeWrite, cmd.SubjectURI)
	repo.mu.Unlock()
	if hook != nil {
		hook()
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.commits++
	cid := fmt.Sprintf("bafyreisweepwritten%d", repo.commits)
	repo.removals[cmd.SubjectURI] = posts.StandingRemoval{CID: cid, SubjectURI: cmd.SubjectURI, SubjectCID: cmd.SubjectCID, Code: string(cmd.Code), CreatedAt: cmd.CreatedAt}
	return posts.InstanceRemovalResult{CommunityWriteResult: posts.CommunityWriteResult{CID: cid, Rev: fmt.Sprintf("3krev%04d", repo.commits)}}, nil
}

func (repo *sweepRepo) UndoInstanceRemoval(_ context.Context, cmd posts.InstanceRemovalUndoCommand) (posts.CommunityWriteResult, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.undos++
	delete(repo.removals, cmd.SubjectURI)
	repo.commits++
	return posts.CommunityWriteResult{Rev: fmt.Sprintf("3krev%04d", repo.commits)}, nil
}

// The real queue sweeping the real store one row per claim attempts every
// due row once. A row re-pended during its own attempt is not attempted again
// by the same sweep: the sweep's exclusions keep it for the next one.
func TestModerationCommunityRecordWritesSweepAttemptsEveryDueRowOnce(t *testing.T) {
	fixture := newRecordWritesFixture(t)
	removedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	first := fixture.subjectURI(moderation.CommentCollection, "3ksweepfirst")
	second := fixture.subjectURI(moderation.CommentCollection, "3ksweepsecond")
	firstAction := fixture.insertRemovalAction(t, first, moderation.CommentCollection, removedAt)
	secondAction := fixture.insertRemovalAction(t, second, moderation.CommentCollection, removedAt)
	fixture.requireRemoval(t, fixture.removal(first, moderation.CommentCollection, firstAction, posts.DecisionSpam, removedAt))
	fixture.requireRemoval(t, fixture.removal(second, moderation.CommentCollection, secondAction, posts.DecisionSpam, removedAt))

	repo := newSweepRepo()
	repo.beforeWrite[first] = func() {
		assert.True(t, fixture.requestRestore(t, first, firstAction, ""), "the first row is re-pended during its own attempt")
	}
	queue := moderation.NewCommunityRecordWriteQueue(fixture.repository, repo, postgres.NewAdmissionRepository(fixture.db),
		moderation.CommunityRecordWriteQueueConfig{Now: func() time.Time { return fixture.dueAt }})

	require.NoError(t, queue.Sweep(t.Context()))

	repo.mu.Lock()
	assert.Equal(t, map[string]int{first: 1, second: 1}, repo.writes, "each due row is attempted once")
	assert.Equal(t, 0, repo.undos, "the re-pended restore waits for the next sweep")
	repo.mu.Unlock()
	pending, err := fixture.repository.ClaimDueCommunityRecordWrites(t.Context(), moderation.CommunityRecordWriteClaim{
		DueAt: fixture.dueAt.Add(time.Hour), Lease: recordWriteLease, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, pending, 1, "the second row completed; only the re-pended first row is left")
	assert.Equal(t, first, pending[0].SubjectURI)
	assert.Equal(t, moderation.CommunityRecordRestored, pending[0].Desired)
}
