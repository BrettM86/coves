package moderation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B4/B5: an instance removal or restore of a post or comment in a community
// this AppView hosts records an outbox row in the same transaction, and the
// post-commit hook hears about it only once it is committed.

const (
	recordRulesInstanceDID  = "did:web:moderation.test"
	recordRulesAdminDID     = "did:plc:firstadmin"
	recordRulesHostedDID    = "did:plc:hostedcommunity"
	recordRulesElsewhereDID = "did:plc:bridgedcommunity"
	recordRulesPostURI      = "at://did:plc:postauthor/social.coves.community.postv2/3kpost"
	recordRulesPostCID      = "bafyreipostversion"
	recordRulesEditedCID    = "bafyreipostedited"
	recordRulesOtherPostURI = "at://did:plc:postauthor/social.coves.community.postv2/3kother"
	recordRulesOtherPostCID = "bafyreiotherversion"
	recordRulesCommentURI   = "at://did:plc:commentauthor/social.coves.community.comment/3kcomment"
	recordRulesCommentCID   = "bafyreicommentversion"
	recordRulesRootURI      = "at://did:plc:postauthor/social.coves.community.postv2/3kroot"

	recordRulesReasonSpam          = "social.coves.moderation.defs#reasonSpam"
	recordRulesReasonHarassment    = "social.coves.moderation.defs#reasonHarassment"
	recordRulesReasonRuleViolation = "social.coves.moderation.defs#reasonRuleViolation"
	recordRulesReasonDoxing        = "social.coves.moderation.defs#reasonDoxing"
	recordRulesReasonIllegal       = "social.coves.moderation.defs#reasonIllegalContent"
	recordRulesReasonDiscretion    = "social.coves.moderation.defs#reasonModeratorDiscretion"
)

// recordingCommunityRecordWrites is the post-commit hook. It records, for
// every call, whether the row was committed when the hook ran; a call made
// while the transaction still holds the store counts as not committed.
type recordingCommunityRecordWrites struct {
	store     *inMemoryModerationStore
	calls     []moderation.CommunityRecordSubject
	committed []bool
}

func (hook *recordingCommunityRecordWrites) WriteAfterCommit(_ context.Context, subject moderation.CommunityRecordSubject) {
	hook.calls = append(hook.calls, subject)
	if !hook.store.mu.TryLock() {
		hook.committed = append(hook.committed, false)
		return
	}
	_, exists := hook.store.state.communityRecordWrites[subject]
	hook.store.mu.Unlock()
	hook.committed = append(hook.committed, exists)
}

type recordRulesScenario struct {
	store   *inMemoryModerationStore
	hook    *recordingCommunityRecordWrites
	service moderation.Service
	clock   *time.Time
}

func newRecordRulesScenario() recordRulesScenario {
	clock := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	store := newInMemoryModerationStore(clock)
	store.hostedCommunities[recordRulesHostedDID] = true
	for _, post := range []moderation.IndexedPost{
		{URI: recordRulesPostURI, CID: recordRulesPostCID, OwnerDID: "did:plc:postauthor", AuthorDID: "did:plc:postauthor", CommunityDID: recordRulesHostedDID},
		{URI: recordRulesOtherPostURI, CID: recordRulesOtherPostCID, OwnerDID: "did:plc:postauthor", AuthorDID: "did:plc:postauthor", CommunityDID: recordRulesElsewhereDID},
	} {
		store.state.indexedPosts[post.URI] = post
	}
	store.state.indexedComments[recordRulesCommentURI] = moderation.IndexedComment{
		URI: recordRulesCommentURI, CID: recordRulesCommentCID, OwnerDID: "did:plc:commentauthor",
		CommunityDID: recordRulesHostedDID, RootURI: recordRulesRootURI,
	}
	hook := &recordingCommunityRecordWrites{store: store}
	scenario := recordRulesScenario{store: store, hook: hook, clock: &clock}
	scenario.service = moderation.NewService(&fakeSubjectReader{}, store, moderation.Config{
		InstanceDID: recordRulesInstanceDID, IdempotencyRetention: 24 * time.Hour,
		MaxLiveIdempotencyKeys: 1000, Now: func() time.Time { return *scenario.clock },
		CommunityRecordWrites: hook,
	})
	return scenario
}

// advance moves both the service clock and the time actions are stamped with.
func (scenario recordRulesScenario) advance(d time.Duration) {
	*scenario.clock = scenario.clock.Add(d)
	scenario.store.now = *scenario.clock
}

func (scenario recordRulesScenario) remove(t *testing.T, uri, cid, version, key, reason string) *moderation.MutationResult {
	t.Helper()
	result, err := scenario.service.RemoveContent(t.Context(), recordRulesAdminDID, moderation.RemoveContentRequest{
		Subject: moderation.StrongRef{URI: uri, CID: cid}, ExpectedVersion: version, IdempotencyKey: key, Reason: reason,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func (scenario recordRulesScenario) restore(t *testing.T, actionID, uri, cid, version, key string) *moderation.MutationResult {
	t.Helper()
	result, err := scenario.service.RestoreContent(t.Context(), recordRulesAdminDID, moderation.RestoreContentRequest{
		ActionID: actionID, ReviewedSubject: &moderation.StrongRef{URI: uri, CID: cid},
		ExpectedVersion: version, IdempotencyKey: key, Reason: recordRulesReasonDiscretion,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	return result
}

func (scenario recordRulesScenario) row(t *testing.T, communityDID, subjectURI string) moderation.CommunityRecordWrite {
	t.Helper()
	write, exists := scenario.store.communityRecordWrite(communityDID, subjectURI)
	require.Truef(t, exists, "no community record write row for %s in %s", subjectURI, communityDID)
	return write
}

// ---------------------------------------------------------------------------
// B4: removeContent
// ---------------------------------------------------------------------------

func TestRemoveContentRecordsACommunityRecordWriteInAHostedCommunity(t *testing.T) {
	for _, row := range []struct {
		reason   string
		wantCode posts.DecisionCode
	}{
		{recordRulesReasonSpam, posts.DecisionCode("spam")},
		{recordRulesReasonRuleViolation, posts.DecisionCode("rule-violation")},
		{recordRulesReasonHarassment, posts.DecisionCode("rule-violation")},
		{recordRulesReasonDoxing, posts.DecisionCode("moderator-discretion")},
		{recordRulesReasonIllegal, posts.DecisionCode("moderator-discretion")},
		{recordRulesReasonDiscretion, posts.DecisionCode("moderator-discretion")},
	} {
		t.Run("post "+row.reason, func(t *testing.T) {
			scenario := newRecordRulesScenario()
			result := scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v0", "remove-post", row.reason)
			require.Equal(t, moderation.OutcomeApplied, result.Outcome)

			write := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
			assert.Equal(t, recordRulesHostedDID, write.CommunityDID)
			assert.Equal(t, recordRulesPostURI, write.SubjectURI)
			assert.Equal(t, "social.coves.community.postv2", write.SubjectCollection)
			assert.Equal(t, recordRulesPostCID, write.SubjectCID)
			assert.Equal(t, result.Action.ID, write.RemovalActionID)
			assert.Equal(t, row.wantCode, write.Code)
			assert.Equal(t, time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC), write.RemovalCreatedAt)
			assert.Equal(t, moderation.CommunityRecordRemoved, write.Desired)
			assert.False(t, write.Completed)
			assert.Equal(t, time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC), write.NextAttemptAt, "a new row is due at once")

			assert.Equal(t, []moderation.CommunityRecordSubject{{CommunityDID: recordRulesHostedDID, SubjectURI: recordRulesPostURI}}, scenario.hook.calls)
			assert.Equal(t, []bool{true}, scenario.hook.committed, "the hook must run after the row is committed")
		})
	}

	t.Run("comment, in its thread root's community", func(t *testing.T) {
		scenario := newRecordRulesScenario()
		result := scenario.remove(t, recordRulesCommentURI, recordRulesCommentCID, "v0", "remove-comment", recordRulesReasonHarassment)

		write := scenario.row(t, recordRulesHostedDID, recordRulesCommentURI)
		assert.Equal(t, "social.coves.community.comment", write.SubjectCollection)
		assert.Equal(t, recordRulesCommentCID, write.SubjectCID)
		assert.Equal(t, result.Action.ID, write.RemovalActionID)
		assert.Equal(t, posts.DecisionCode("rule-violation"), write.Code)
		assert.Equal(t, moderation.CommunityRecordRemoved, write.Desired)
		assert.Equal(t, []moderation.CommunityRecordSubject{{CommunityDID: recordRulesHostedDID, SubjectURI: recordRulesCommentURI}}, scenario.hook.calls)
		assert.Equal(t, []bool{true}, scenario.hook.committed)
	})
}

func TestRemoveContentRecordsNothingForACommunityHostedElsewhere(t *testing.T) {
	scenario := newRecordRulesScenario()

	scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v0", "remove-hosted", recordRulesReasonSpam)
	elsewhere := scenario.remove(t, recordRulesOtherPostURI, recordRulesOtherPostCID, "v0", "remove-elsewhere", recordRulesReasonSpam)
	scenario.restore(t, elsewhere.Action.ID, recordRulesOtherPostURI, recordRulesOtherPostCID, "v1", "restore-elsewhere")

	scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
	_, exists := scenario.store.communityRecordWrite(recordRulesElsewhereDID, recordRulesOtherPostURI)
	assert.False(t, exists, "a community this AppView does not host gets the label only")
	assert.Equal(t, 1, scenario.store.committedCommunityRecordWriteCount())
	assert.Equal(t, []moderation.CommunityRecordSubject{{CommunityDID: recordRulesHostedDID, SubjectURI: recordRulesPostURI}}, scenario.hook.calls)
}

func TestRemoveContentUnchangedLeavesTheRowAlone(t *testing.T) {
	scenario := newRecordRulesScenario()
	scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v0", "first-removal", recordRulesReasonSpam)
	first := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
	scenario.store.markCommunityRecordWriteCompleted(recordRulesHostedDID, recordRulesPostURI)

	scenario.advance(time.Minute)
	again := scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v1", "second-removal", recordRulesReasonRuleViolation)
	require.Equal(t, moderation.OutcomeUnchanged, again.Outcome)

	after := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
	assert.True(t, after.Completed, "an unchanged removal must not re-pend the row")
	assert.Equal(t, first.Generation, after.Generation)
	assert.Equal(t, first.RemovalActionID, after.RemovalActionID)
	assert.Equal(t, posts.DecisionCode("spam"), after.Code)
	assert.Len(t, scenario.hook.calls, 1, "the hook runs only for the applied removal")
}

func TestRemoveContentRolledBackRecordsNoRowAndCallsNoHook(t *testing.T) {
	scenario := newRecordRulesScenario()
	scenario.store.failSaveIdempotencyRecord = errors.New("idempotency write failed")

	_, err := scenario.service.RemoveContent(t.Context(), recordRulesAdminDID, moderation.RemoveContentRequest{
		Subject:         moderation.StrongRef{URI: recordRulesPostURI, CID: recordRulesPostCID},
		ExpectedVersion: "v0", IdempotencyKey: "rolled-back", Reason: recordRulesReasonSpam,
	})
	require.Error(t, err)
	assert.Equal(t, 0, scenario.store.committedCommunityRecordWriteCount(), "a rolled-back removal leaves no row")
	assert.Empty(t, scenario.hook.calls, "a rolled-back removal must not reach the hook")

	scenario.store.failSaveIdempotencyRecord = nil
	scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v0", "committed", recordRulesReasonSpam)
	scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
	assert.Equal(t, []bool{true}, scenario.hook.committed)
}

func TestRemoveContentAfterARestoreTakesOverTheSameRow(t *testing.T) {
	scenario := newRecordRulesScenario()
	first := scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v0", "first-removal", recordRulesReasonSpam)
	scenario.advance(time.Minute)
	scenario.restore(t, first.Action.ID, recordRulesPostURI, recordRulesPostCID, "v1", "restore")
	restored := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)

	scenario.advance(time.Minute)
	second := scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v2", "second-removal", recordRulesReasonHarassment)
	require.Equal(t, moderation.OutcomeApplied, second.Outcome)

	assert.Equal(t, 1, scenario.store.committedCommunityRecordWriteCount(), "one row per community and subject")
	write := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
	assert.Equal(t, second.Action.ID, write.RemovalActionID)
	assert.NotEqual(t, first.Action.ID, write.RemovalActionID)
	assert.Equal(t, moderation.CommunityRecordRemoved, write.Desired)
	assert.Equal(t, posts.DecisionCode("rule-violation"), write.Code)
	assert.Equal(t, time.Date(2026, time.November, 11, 12, 2, 0, 0, time.UTC), write.RemovalCreatedAt)
	assert.Greater(t, write.Generation, restored.Generation)
	assert.False(t, write.Completed)
	assert.Len(t, scenario.hook.calls, 3)
}

// ---------------------------------------------------------------------------
// B5: restoreContent
// ---------------------------------------------------------------------------

func TestRestoreContentRequestsTheRestoreOfItsOwnRemoval(t *testing.T) {
	t.Run("post: the acceptance pins the CID indexed at restore", func(t *testing.T) {
		scenario := newRecordRulesScenario()
		removed := scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v0", "remove", recordRulesReasonSpam)
		before := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
		scenario.store.markCommunityRecordWriteCompleted(recordRulesHostedDID, recordRulesPostURI)

		// The author edited the removed post; the admin reviews the new version.
		post := scenario.store.state.indexedPosts[recordRulesPostURI]
		post.CID = recordRulesEditedCID
		scenario.store.state.indexedPosts[recordRulesPostURI] = post
		scenario.advance(time.Minute)
		scenario.restore(t, removed.Action.ID, recordRulesPostURI, recordRulesEditedCID, "v1", "restore")

		write := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
		assert.Equal(t, moderation.CommunityRecordRestored, write.Desired)
		assert.Equal(t, recordRulesEditedCID, write.RestoreAcceptanceCID)
		assert.Equal(t, recordRulesPostCID, write.SubjectCID, "the removal's reviewed CID is unchanged")
		assert.Equal(t, removed.Action.ID, write.RemovalActionID)
		assert.False(t, write.Completed)
		assert.Greater(t, write.Generation, before.Generation)
		assert.Equal(t, time.Date(2026, time.November, 11, 12, 1, 0, 0, time.UTC), write.NextAttemptAt)
		assert.Equal(t, []bool{true, true}, scenario.hook.committed)
	})

	t.Run("comment: no acceptance", func(t *testing.T) {
		scenario := newRecordRulesScenario()
		removed := scenario.remove(t, recordRulesCommentURI, recordRulesCommentCID, "v0", "remove", recordRulesReasonSpam)
		scenario.restore(t, removed.Action.ID, recordRulesCommentURI, recordRulesCommentCID, "v1", "restore")

		write := scenario.row(t, recordRulesHostedDID, recordRulesCommentURI)
		assert.Equal(t, moderation.CommunityRecordRestored, write.Desired)
		assert.Equal(t, "", write.RestoreAcceptanceCID)
		assert.Len(t, scenario.hook.calls, 2)
	})
}

func TestRestoreContentLeavesARowOwnedByAnotherActionAlone(t *testing.T) {
	// The second removal happens while the community is not hosted here, so
	// the row still belongs to the first; restoring the second must not touch
	// it.
	scenario := newRecordRulesScenario()
	first := scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v0", "first-removal", recordRulesReasonSpam)
	scenario.restore(t, first.Action.ID, recordRulesPostURI, recordRulesPostCID, "v1", "first-restore")
	owned := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
	require.Equal(t, moderation.CommunityRecordRestored, owned.Desired)

	scenario.store.hostedCommunities[recordRulesHostedDID] = false
	second := scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v2", "second-removal", recordRulesReasonSpam)
	scenario.store.hostedCommunities[recordRulesHostedDID] = true
	scenario.restore(t, second.Action.ID, recordRulesPostURI, recordRulesPostCID, "v3", "second-restore")

	assert.Equal(t, owned, scenario.row(t, recordRulesHostedDID, recordRulesPostURI))
	assert.Len(t, scenario.hook.calls, 2, "only the first removal and its restore reach the hook")
}

func TestRestoreContentOfAnAuthorDeletedPostPinsNoAcceptance(t *testing.T) {
	// The author deleted the removed post: there is no content left to
	// accept, so the restore deletes the instance's removal only.
	scenario := newRecordRulesScenario()
	removed := scenario.remove(t, recordRulesPostURI, recordRulesPostCID, "v0", "remove", recordRulesReasonSpam)
	post := scenario.store.state.indexedPosts[recordRulesPostURI]
	post.AuthorDeleted = true
	scenario.store.state.indexedPosts[recordRulesPostURI] = post

	result, err := scenario.service.RestoreContent(t.Context(), recordRulesAdminDID, moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ExpectedVersion: "v1", IdempotencyKey: "restore-deleted",
		Reason: recordRulesReasonDiscretion,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)

	write := scenario.row(t, recordRulesHostedDID, recordRulesPostURI)
	assert.Equal(t, moderation.CommunityRecordRestored, write.Desired)
	assert.Equal(t, "", write.RestoreAcceptanceCID, "a deleted post gets no acceptance back")
}
