//go:build integration

package moderation_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Recovery of community post removals against real Postgres: lost PDS
// responses settled by a same-key retry or by the firehose, a removal that
// already stands, and an intent that never committed. The community repo is
// faked; the index, intents and actions are real.

// assertRemovedByModerator: the post's admission is removed with code spam
// and postDBRemovalReason, one action names M, and the version advanced once.
func assertRemovedByModerator(t *testing.T, f *communityPostDBFixture, post seededCommunityPost, msg string) {
	t.Helper()
	admission := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status, "%s: the admission is removed", msg)
	if assert.NotNil(t, admission.DecisionCode, "%s: the removal's code is stored", msg) {
		assert.Equal(t, "spam", *admission.DecisionCode, msg)
	}
	if assert.NotNil(t, admission.RemovalReason, "%s: the removal's reason is stored", msg) {
		assert.Equal(t, postDBRemovalReason, *admission.RemovalReason, msg)
	}
	assert.Equal(t, []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
	}, f.postActions(t), "%s: one action names the moderator", msg)
	assert.EqualValues(t, 2, f.postVersion(t, post.URI), "%s: the version advanced once", msg)
}

// I7 with Round 1 #5 and R2-1: M's removal commits at head one and the
// response is lost. Whichever of the same-key retry and the firehose copies
// settles it first, there is one action naming M and one version advance;
// the other then changes nothing.
func TestCommunityPostRecoveryDB_LostRemovalResponse(t *testing.T) {
	t.Parallel()
	removalBody := func(post seededCommunityPost) map[string]any {
		return removalRecordBody(post, "spam", postDBRemovalReason, postDBIntentCreatedAt)
	}
	tests := []struct {
		name string
		// beforeRetry runs after the lost commit and before the retry.
		beforeRetry func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost)
		// copiesAfterRetry: the commit's firehose copies arrive after the retry.
		copiesAfterRetry bool
		// externalRewriteAfter: a removal rewrite from outside Coves follows.
		externalRewriteAfter bool
	}{
		{
			name:             "retry before any firehose copy",
			beforeRetry:      func(*testing.T, *communityPostDBFixture, seededCommunityPost) {},
			copiesAfterRetry: true,
		},
		{
			name: "firehose copies before the retry",
			beforeRetry: func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				f.deliverRemovalCommit(t, post, rosterHeadOneRev, postDBAlphaRemovalBodyCID, removalBody(post))
				assertRemovedByModerator(t, f, post, "after the firehose copies")
				assert.Equal(t, 0, f.openIntents(t), "the copies complete the open intent")
			},
		},
		{
			name: "an unrelated commit moves the head before the retry",
			beforeRetry: func(_ *testing.T, f *communityPostDBFixture, _ seededCommunityPost) {
				f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
			},
			copiesAfterRetry:     true,
			externalRewriteAfter: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			f.setRemovalWriteCID(post, postDBAlphaRemovalBodyCID)
			f.repo.applyThenErr = errors.New("pds: connection reset by peer")

			_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

			assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
			assert.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "the intent stays open")
			assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "the admission is unchanged")
			assert.EqualValues(t, postDBSeededVersion, f.postVersion(t, post.URI), "the version is unchanged")
			assert.Empty(t, f.postActions(t), "no action yet")

			tt.beforeRetry(t, f, post)

			result, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

			require.NoError(t, err, "the same-key retry settles the lost commit")
			require.NotNil(t, result, "the retry returns its result")
			assertRemovedByModerator(t, f, post, "after the retry")
			ids := f.actionIDs(t)
			if assert.Len(t, ids, 1) {
				assert.Equal(t, ids[0], result.ActionID, "the retry's result names the one action")
			}
			if tt.copiesAfterRetry {
				assert.False(t, result.AlreadyApplied, "the retry completed this intent's own change")
			}
			assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
			assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no second batch")
			settled := f.admission(t, post.URI)

			if tt.copiesAfterRetry {
				// The real commit's events, at head one's rev.
				f.deliverRemovalCommit(t, post, rosterHeadOneRev, postDBAlphaRemovalBodyCID, removalBody(post))

				assertRemovedByModerator(t, f, post, "after the delayed firehose copies")
				assert.Equal(t, ids, f.actionIDs(t), "the copies add no action")
				assert.Equal(t, settled, f.admission(t, post.URI), "the copies leave the admission row")
			}

			if tt.externalRewriteAfter {
				// A later rewrite from outside Coves is a new change.
				rewrite := removalRecordBody(post, "off-topic", postDBRemovalReason, "2026-11-13T13:00:00Z")
				require.NoError(t, f.deliverPost(postStandEvent("update", posts.RemovalCollection, post.SubjectRkey, "3krosterrev03",
					"bafyreiaremovalexternalrewriteaaaaaaaaaaaaaaaaaaaaaaaa", rewrite)))

				assert.Equal(t, []storedPostAction{
					localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
					externalPostAction("remove", post.URI, postDBReasonRuleViolation),
				}, f.postActions(t), "the external rewrite records its own actorless action")
				assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the external rewrite advances the version")
				if code := f.admission(t, post.URI).DecisionCode; assert.NotNil(t, code) {
					assert.Equal(t, "off-topic", *code, "the admission carries the rewritten code")
				}
			}
		})
	}
}

// I7d with Round 1 #2: the first attempt fails before anything commits; the
// same-key retry sends a second batch, which commits, and completes with one
// action naming M. Recording the retry's attempt does not wait on its own
// intent row.
func TestCommunityPostRecoveryDB_RetryAfterUncommittedAttempt(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the failed write reads as unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := f.service.RemovePost(ctx, moderation.RemovePostRequest{
		ActorDID: rosterModerator, PostURI: post.URI, Code: "spam", Reason: postDBRemovalReason,
		ExpectedVersion: postDBSeededVersion, IdempotencyKey: "remove-post-1",
	})

	require.NoError(t, err, "the retry must finish, not wait on its own intent")
	require.NoError(t, ctx.Err(), "the retry finished before the deadline")
	require.NotNil(t, result, "the retry returns its result")
	assert.False(t, result.AlreadyApplied)
	assert.Len(t, f.repo.batchesSnapshot(), 2, "the retry sends a second batch")
	assertRemovedByModerator(t, f, post, "after the retry")
	assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
}

// I8: an identical removal (code spam, same reason) already stands, written
// outside Coves or by M2. M's removal is already applied: no batch, no new
// action, the existing action keeps its actor, version and admission
// unchanged, no intent left open.
func TestCommunityPostRecoveryDB_IdenticalRemovalAlreadyStanding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		stand       func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost)
		wantActions func(post seededCommunityPost) []storedPostAction
	}{
		{
			name: "written outside Coves",
			stand: func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				f.standExternalRemoval(t, post, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
					"bafyreiaremovalexternalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					removalRecordBody(post, "spam", postDBRemovalReason, "2026-11-12T09:00:00Z"))
			},
			wantActions: func(post seededCommunityPost) []storedPostAction {
				return []storedPostAction{externalPostAction("remove", post.URI, postDBReasonSpam)}
			},
		},
		{
			name: "written by M2 through RemovePost",
			stand: func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				_, err := f.removePost(rosterSecondModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-m2")
				require.NoError(t, err)
			},
			wantActions: func(post seededCommunityPost) []storedPostAction {
				return []storedPostAction{localPostAction("remove", rosterSecondModerator, false, post.URI, postDBReasonSpam)}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			tt.stand(t, f, post)
			assert.Equal(t, tt.wantActions(post), f.postActions(t), "fixture: the standing removal's action")
			before := f.admission(t, post.URI)
			versionBefore := f.postVersion(t, post.URI)
			batchesBefore := len(f.repo.batchesSnapshot())
			f.clock.Set(rosterClockStart.Add(time.Minute))
			f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})

			result, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, versionBefore, "remove-post-1")

			require.NoError(t, err)
			require.NotNil(t, result, "RemovePost returns its result")
			assert.True(t, result.AlreadyApplied, "the removal is already in place")
			assert.Equal(t, tt.wantActions(post), f.postActions(t), "no new action; the existing one keeps its actor")
			assert.Equal(t, versionBefore, f.postVersion(t, post.URI), "the version is unchanged")
			assert.Equal(t, versionBefore, result.Version, "the result carries the unchanged version")
			assert.Equal(t, before, f.admission(t, post.URI), "the admission is unchanged")
			assert.Len(t, f.repo.batchesSnapshot(), batchesBefore, "no batch is sent")
			assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
		})
	}
}

// I9: M rewrites a standing spam removal to off-topic; the commit lands and
// the response is lost. The same-key retry recognizes its own commit (the
// removal's CID moved from the recorded pre-write CID) and records one new
// action naming M.
func TestCommunityPostRecoveryDB_LostRewriteResponse(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	f.setRemovalWriteCID(post, "bafyreiaremovalrewrittenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	const offTopicReason = "Not about this community"

	_, err = f.removePost(rosterModerator, post.URI, "off-topic", offTopicReason, 2, "rewrite-post-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	result, err := f.removePost(rosterModerator, post.URI, "off-topic", offTopicReason, 2, "rewrite-post-1")

	require.NoError(t, err, "the same-key retry settles the lost rewrite")
	require.NotNil(t, result, "the retry returns its result")
	assert.False(t, result.AlreadyApplied, "the rewrite is this intent's own change")
	assert.Equal(t, []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonRuleViolation),
	}, f.postActions(t), "one new action names M with the off-topic mapping")
	admission := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status)
	if assert.NotNil(t, admission.DecisionCode) {
		assert.Equal(t, "off-topic", *admission.DecisionCode, "the admission carries the rewritten code")
	}
	if assert.NotNil(t, admission.RemovalReason) {
		assert.Equal(t, offTopicReason, *admission.RemovalReason)
	}
	assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the version advanced once for the rewrite")
	assert.EqualValues(t, 3, result.Version)
	assert.Len(t, f.repo.batchesSnapshot(), 2, "the retry sends no third batch")
	assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
}

// I10: M's removal never commits; once the intent expires the sweep closes it
// with nothing applied.
func TestCommunityPostRecoveryDB_SweepExpiresAnUncommittedRemoval(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the failed write reads as unavailable")
	f.pastIntentExpiry()

	swept, err := f.service.SweepExpiredIntents(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, swept, "the one expired intent is reconciled")
	assert.Equal(t, []storedIntent{{State: "expired"}}, f.intents(t), "the intent is expired, not open")
	admission := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusAccepted, admission.Status, "the admission is unchanged")
	if assert.NotNil(t, admission.AcceptedCID) {
		assert.Equal(t, postAlphaCID, *admission.AcceptedCID)
	}
	assert.Empty(t, f.postActions(t), "no action")
	assert.EqualValues(t, postDBSeededVersion, f.postVersion(t, post.URI), "the version is unchanged")
}

// Review r1 #1 (scenarios A and B): M's removal commits at head one and the
// response is lost; the commit's acceptance-delete copy is indexed before its
// removal create. With a same-key retry in between (it completes at the
// intent's guard rev, older than the indexed delete) or without one, once the
// removal create arrives the post is removed with M's code and reason, one
// action names M, and the commit advanced the version once.
func TestCommunityPostRecoveryDB_LostRemovalAcceptanceDeleteFirst(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		retry bool
	}{
		{name: "same-key retry before the removal create", retry: true},
		{name: "no retry"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			f.setRemovalWriteCID(post, postDBAlphaRemovalBodyCID)
			f.repo.applyThenErr = errors.New("pds: connection reset by peer")
			_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

			require.NoError(t, f.deliverPost(postStandEvent("delete", posts.AcceptanceCollection, post.SubjectRkey, rosterHeadOneRev, "", nil)))
			if tt.retry {
				result, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
				require.NoError(t, err, "the same-key retry settles the lost commit")
				require.NotNil(t, result)
			}
			require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, post.SubjectRkey, rosterHeadOneRev,
				postDBAlphaRemovalBodyCID, removalRecordBody(post, "spam", postDBRemovalReason, postDBIntentCreatedAt))))

			assertRemovedByModerator(t, f, post, "after the removal create")
			assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
		})
	}
}

// Review r1 #1 (scenario C): M's removal commits at head one and the response
// is lost; a rewrite from outside Coves at head two is indexed first, then the
// lost commit's copies arrive late. The late copy completes M's intent but
// leaves the newer rewrite standing in the index, so M2 can still restore the
// post against the rewrite the community repo holds.
func TestCommunityPostRecoveryDB_LateCopyAfterExternalRewrite(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.setRemovalWriteCID(post, postDBAlphaRemovalBodyCID)
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	const rewriteCID = "bafyreiaremovalexternalrewriteaaaaaaaaaaaaaaaaaaaaaaaa"
	rewrite := removalRecordBody(post, "off-topic", postDBRemovalReason, "2026-11-13T13:00:00Z")
	f.repo.setPostRecord(posts.RemovalCollection, post.SubjectRkey, rewriteCID, rewrite)
	f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	require.NoError(t, f.deliverPost(postStandEvent("update", posts.RemovalCollection, post.SubjectRkey, rosterHeadTwoRev, rewriteCID, rewrite)))
	f.deliverRemovalCommit(t, post, rosterHeadOneRev, postDBAlphaRemovalBodyCID,
		removalRecordBody(post, "spam", postDBRemovalReason, postDBIntentCreatedAt))

	assert.Equal(t, rewriteCID, f.indexedRemovalCID(t, post.URI), "the index keeps the newer rewrite")
	if code := f.admission(t, post.URI).DecisionCode; assert.NotNil(t, code) {
		assert.Equal(t, "off-topic", *code, "the admission keeps the rewrite's code")
	}
	assert.Equal(t, 0, f.openIntents(t), "the late copy completes M's intent")
	f.setCommitHead(pds.LatestCommit{CID: postFirehoseHeadThreeCID, Rev: rosterHeadThreeRev})

	_, err = f.restorePost(rosterSecondModerator, post.URI, f.postVersion(t, post.URI), "restore-post-m2")

	assert.NoError(t, err, "M2's restore is judged against the rewrite the repo holds")
}

// Review r1 #5: M's removal commits and the PDS then answers 500. The writer
// re-reads, finds the record its own batch wrote and reports a skip; the
// standing CID differs from the CID the recorded attempt found before the
// batch, so the intent completes as M's change at the attempt's guard rev,
// not at the skip's head rev. The commit's firehose copies then change nothing.
func TestCommunityPostRecoveryDB_CommitThenServerError(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.setRemovalWriteCID(post, postDBAlphaRemovalBodyCID)
	f.repo.applyThenErr = fmt.Errorf("applyWrites: %w", pds.ErrServerError)

	result, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

	require.NoError(t, err, "the writer's re-read finds the batch's own record")
	require.NotNil(t, result)
	assert.False(t, result.AlreadyApplied, "the skip is this intent's own commit, not an earlier removal")
	assertRemovedByModerator(t, f, post, "after the commit-then-500")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadZeroRev}}, f.intents(t),
		"the intent completes at the attempt's guard rev, not the skip's head rev")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "the re-read sends no second batch")
	ids := f.actionIDs(t)
	settled := f.admission(t, post.URI)

	f.deliverRemovalCommit(t, post, rosterHeadOneRev, postDBAlphaRemovalBodyCID,
		removalRecordBody(post, "spam", postDBRemovalReason, postDBIntentCreatedAt))

	assertRemovedByModerator(t, f, post, "after the firehose copies")
	assert.Equal(t, ids, f.actionIDs(t), "the copies add no action")
	assert.Equal(t, settled, f.admission(t, post.URI), "the copies leave the admission row")
}

// postDBExternalOffTopicCID is the CID of a removal of postDBAlpha written
// outside Coves with code off-topic.
const postDBExternalOffTopicCID = "bafyreiaremovalexternaloffaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// postDBPastUnknownOutcomeBound is past the first intent's expiry (10 minutes)
// plus the firehose-lag bound (30 minutes) an unknown-outcome post intent
// waits for its firehose copy.
var postDBPastUnknownOutcomeBound = rosterClockStart.Add(40*time.Minute + time.Second)

// standForeignRemovalAfterFailedAttempt: M's removal of postDBAlpha fails
// before committing (its attempt is recorded), then a removal written outside
// Coves under code off-topic stands at head one and the firehose indexes it.
func standForeignRemovalAfterFailedAttempt(t *testing.T, f *communityPostDBFixture) seededCommunityPost {
	t.Helper()
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the failed write reads as unavailable")
	f.standExternalRemoval(t, post, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev}, postDBExternalOffTopicCID,
		removalRecordBody(post, "off-topic", postDBRemovalReason, "2026-11-13T12:01:00Z"))
	require.Equal(t, []storedPostAction{externalPostAction("remove", post.URI, postDBReasonRuleViolation)}, f.postActions(t),
		"fixture: the external removal is logged without an actor")
	require.Equal(t, 1, f.openIntents(t), "fixture: the external removal does not complete M's intent")
	return post
}

// Review r1 #7(b): M's first attempt failed before committing and the head
// then moved. A same-key retry judges the stand record against the body M's
// batch sent: a different removal standing there is not M's commit, so no
// action names M; a stand record as the attempt found it means M's write did
// not happen.
func TestCommunityPostRecoveryDB_MovedHeadAfterUncommittedAttempt(t *testing.T) {
	t.Parallel()
	t.Run("a different removal stands: the outcome is unknown", func(t *testing.T) {
		t.Parallel()
		f := newCommunityPostDBFixture(t)
		post := standForeignRemovalAfterFailedAttempt(t, f)

		_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

		assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the record changed after the write")
		assert.Equal(t, []storedPostAction{externalPostAction("remove", post.URI, postDBReasonRuleViolation)}, f.postActions(t),
			"only the external removal's actorless action; none names M")
		assert.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "the intent waits for a firehose copy")
		assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no second batch")
		if code := f.admission(t, post.URI).DecisionCode; assert.NotNil(t, code) {
			assert.Equal(t, "off-topic", *code, "the admission keeps the external removal's code")
		}
	})
	t.Run("an unrelated commit moved the head: the write did not happen", func(t *testing.T) {
		t.Parallel()
		f := newCommunityPostDBFixture(t)
		post := f.seedAcceptedPost(t, postDBAlpha)
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the failed write reads as unavailable")
		f.repo.landCommit(pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev})

		_, err = f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")

		assert.ErrorIs(t, err, moderation.ErrStateConflict, "the stand records were not written")
		assert.Equal(t, []storedIntent{{State: "failed"}}, f.intents(t), "the intent fails")
		assert.Empty(t, f.postActions(t), "no action")
		assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "the admission is unchanged")
		assert.EqualValues(t, postDBSeededVersion, f.postVersion(t, post.URI), "the version is unchanged")
		assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no second batch")
	})
}

// Review r1 #2: an unknown-outcome post intent (its stand record changed to a
// body its batch did not send) does not block the post forever. Within the
// firehose-lag bound past its expiry the sweep leaves it open; once past the
// bound, whichever of a same-key retry, another moderator's request or the
// sweep reaches it first closes it as failed, and the other moderator's
// request goes ahead. No action names M.
func TestCommunityPostRecoveryDB_UnknownOutcomeIntentCloses(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		settle func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost)
		// m2Restores: M2's restore settled the intent and restored the post.
		m2Restores bool
	}{
		{
			name: "same-key retry",
			settle: func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
				assert.ErrorIs(t, err, moderation.ErrStateConflict, "the retry learns its write did not land")
			},
		},
		{
			name: "another moderator's restore",
			settle: func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
				_, err := f.restorePost(rosterSecondModerator, post.URI, f.postVersion(t, post.URI), "restore-post-m2")
				assert.NoError(t, err, "M2's restore is no longer blocked")
			},
			m2Restores: true,
		},
		{
			name: "sweep",
			settle: func(t *testing.T, f *communityPostDBFixture, _ seededCommunityPost) {
				swept, err := f.service.SweepExpiredIntents(context.Background())
				assert.NoError(t, err)
				assert.Equal(t, 1, swept, "the sweep closes the intent")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := standForeignRemovalAfterFailedAttempt(t, f)
			f.pastIntentExpiry()
			swept, err := f.service.SweepExpiredIntents(context.Background())
			assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "within the lag bound the outcome is still unknown")
			assert.Equal(t, 0, swept)
			assert.Equal(t, 1, f.openIntents(t), "within the lag bound the intent stays open")
			f.clock.Set(postDBPastUnknownOutcomeBound)

			tt.settle(t, f, post)

			intents := f.intents(t)
			if assert.NotEmpty(t, intents) {
				assert.Equal(t, storedIntent{State: "failed"}, intents[0], "M's intent closes as failed")
			}
			assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
			want := []storedPostAction{externalPostAction("remove", post.URI, postDBReasonRuleViolation)}
			if tt.m2Restores {
				want = append(want, localPostAction("restore", rosterSecondModerator, false, post.URI, postDBReasonRuleViolation))
				assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "M2's restore applies")
			}
			// The firehose logs the inherited action at the wall clock, so the
			// two actions' order is not the fake clock's.
			assert.ElementsMatch(t, want, f.postActions(t), "no action names M")
		})
	}
}

// Review r1 #2: stuck unknown-outcome intents filling a sweep page do not keep
// a newer expired intent from being swept. With a page of one, M's stuck
// intent on postDBAlpha (older expiry) and an uncommitted removal of a second
// post: the sweep reads past the stuck one and expires the second.
func TestCommunityPostRecoveryDB_SweepReadsPastStuckIntents(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := standForeignRemovalAfterFailedAttempt(t, f)
	config := f.config
	config.ExpiredIntentSweepLimit = 1
	sweeper := moderation.NewCommunityActionService(config)
	f.clock.Set(rosterClockStart.Add(time.Minute))
	beta := f.seedAcceptedPost(t, postDBSeedFor("beta"))
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	_, err := f.removePost(rosterModerator, beta.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-2")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the failed write reads as unavailable")
	f.clock.Set(rosterClockStart.Add(11*time.Minute + time.Second))

	swept, err := sweeper.SweepExpiredIntents(context.Background())

	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the stuck intent's outcome is still unknown")
	assert.Equal(t, 1, swept, "the second intent is swept")
	assert.Equal(t, []storedIntent{{State: "open"}, {State: "expired"}}, f.intents(t),
		"the stuck intent stays open within the lag bound; the second expires")
	assert.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, post.URI).Status)
}
