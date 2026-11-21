package moderation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	commentAlphaURI    = "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentalpha1"
	commentBetaURI     = "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentbeta1"
	commentRemoteURI   = "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentremote1"
	commentRemovedURI  = "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentremoved1"
	commentDanglingURI = "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentdangling1"

	// SubjectRkey(commentAlphaURI), computed once by hand.
	commentAlphaRkey       = "ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya"
	commentAlphaRemovalURI = "at://did:plc:rosteralpha/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya"

	commentAlphaCID          = "bafyreiacommentalphaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commentRemovalCID        = "bafyreiacommentremovalstandingaaaaaaaaaaaaaaaaaaaaaaaaa"
	commentRemovalWrittenCID = "bafyreiacommentremovalwrittenaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commentAcceptanceCID     = "bafyreiacommentacceptancestandingaaaaaaaaaaaaaaaaaaaaaa"

	commentStartVersion   = 5
	commentRemovalReason  = "Posting phishing links"
	commentStandingCreate = "2026-11-10T09:00:00Z"
)

// fakeCommunityCommentResolver is the comment-to-community index (the parent
// chain's root post community); it records every comment it was asked about.
type fakeCommunityCommentResolver struct {
	mu          sync.Mutex
	communities map[string]string
	lookups     []string
}

func (r *fakeCommunityCommentResolver) CommentCommunity(_ context.Context, commentURI string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, commentURI)
	community, ok := r.communities[commentURI]
	if !ok {
		return "", moderation.ErrCommentNotFound
	}
	return community, nil
}

func (r *fakeCommunityCommentResolver) lookupsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lookups...)
}

// recordingRoleAuthorizer records every Authorize call it passes through.
type recordingRoleAuthorizer struct {
	communities.RoleAuthorizer
	mu    sync.Mutex
	calls []string // community DIDs
}

func (a *recordingRoleAuthorizer) Authorize(ctx context.Context, communityDID, callerDID, required string) (communities.RoleAuthorization, error) {
	a.mu.Lock()
	a.calls = append(a.calls, communityDID)
	a.mu.Unlock()
	return a.RoleAuthorizer.Authorize(ctx, communityDID, callerDID, required)
}

func (a *recordingRoleAuthorizer) callsSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (s *fakeCommunityActionStore) seedComment(commentURI string, state moderation.CommunityCommentState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.comments[commentURI] = state
	s.commentRemovals["at://"+state.CommunityDID+"/"+posts.RemovalCollection+"/"+posts.SubjectRkey(commentURI)] = commentURI
}

func (s *fakeCommunityActionStore) commentReadsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commentReads...)
}

// completeCommentIntentLocked applies a comment intent's local effect on
// Commit: the comment's removal state follows the written record, its version
// advances once, one action is logged, the intent completes and the result is
// stored. The caller holds the store's lock.
func (tx *fakeCommunityActionTx) completeCommentIntentLocked(index int, commentURI string, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	stored := tx.store.intents[index]
	state, ok := tx.store.comments[commentURI]
	if !ok {
		return moderation.CommunityActionResult{}, fmt.Errorf("fake store: no indexed comment %s", commentURI)
	}
	next := state
	next.Version++
	if stored.Op != "delete" {
		var record struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(stored.Record, &record); err != nil {
			return moderation.CommunityActionResult{}, fmt.Errorf("fake store: decoding the comment intent's record: %w", err)
		}
		next.Removed = true
		next.RemovalCID, next.RemovalCode, next.RemovalReason = commit.RecordCID, record.Code, record.Reason
	} else {
		next.Removed = false
		next.RemovalCID, next.RemovalCode, next.RemovalReason = "", "", ""
	}
	actionID := fmt.Sprintf("community-action-%d", len(tx.store.actions)+1)
	result := moderation.CommunityActionResult{
		CommunityDID: stored.CommunityDID,
		RecordURI:    stored.TargetURI,
		ActionID:     actionID,
		Version:      next.Version,
	}
	tx.pending = append(tx.pending, func() {
		tx.store.comments[commentURI] = next
		tx.store.actions = append(tx.store.actions, communityActionRecorded{
			ActorDID: stored.ActorDID, AdminActing: stored.AdminActing, CommunityDID: stored.CommunityDID,
		})
		tx.store.actionLog = append(tx.store.actionLog, communityActionLogged{
			Action: stored.Action, ActorDID: stored.ActorDID, AdminActing: stored.AdminActing, CommunityDID: stored.CommunityDID,
		})
		completed := tx.store.intents[index]
		completed.State = moderation.CommunityIntentCompleted
		completed.CommitRev = commit.Rev
		completed.RecordCID = commit.RecordCID
		completed.ActionID = actionID
		tx.store.intents[index] = completed
		tx.store.idempotency[communityActionIdempotencyKey{stored.ActorDID, stored.CommunityDID, stored.IdempotencyKey}] = communityActionStoredRecord{
			record:    moderation.CommunityIdempotencyRecord{Fingerprint: stored.Fingerprint, Result: result},
			expiresAt: tx.store.now().Add(idempotencyTTL),
		}
	})
	return result, nil
}

type communityCommentFixture struct {
	*communityPostFixture
	comments   *fakeCommunityCommentResolver
	authorizes *recordingRoleAuthorizer
}

// newCommunityCommentFixture extends the post fixture for comment actions:
// commentAlphaURI's parent chain reaches a post in rosteralpha,
// commentBetaURI's one in rosterbeta, commentRemoteURI's one in the bridged
// rosterremote and commentRemovedURI's one in the instance-removed
// rosterremoved; commentDanglingURI's chain does not resolve. Every comment
// but commentAlphaURI and commentDanglingURI is indexed visible at
// commentStartVersion. Limits are 60 per moderator and 300 per community per
// hour.
func newCommunityCommentFixture(t *testing.T) *communityCommentFixture {
	t.Helper()
	f := newCommunityPostFixture(t)
	resolver := &fakeCommunityCommentResolver{communities: map[string]string{
		commentAlphaURI:   rosterCommunity,
		commentBetaURI:    postBetaCommunity,
		commentRemoteURI:  rosterRemote,
		commentRemovedURI: rosterRemoved,
	}}
	authorizes := &recordingRoleAuthorizer{RoleAuthorizer: f.authorizer}
	cfg := f.config
	cfg.Authorizer = authorizes
	cfg.Comments = resolver
	f.config = cfg
	f.service = moderation.NewCommunityActionService(cfg)
	f.repo.writeCIDs[posts.RemovalCollection+"/"+commentAlphaRkey] = commentRemovalWrittenCID
	for uri, community := range map[string]string{
		commentBetaURI: postBetaCommunity, commentRemoteURI: rosterRemote, commentRemovedURI: rosterRemoved,
	} {
		f.store.seedComment(uri, moderation.CommunityCommentState{
			CommunityDID: community, CID: commentAlphaCID, Version: commentStartVersion,
		})
	}
	return &communityCommentFixture{communityPostFixture: f, comments: resolver, authorizes: authorizes}
}

// seedVisibleComment: commentAlphaURI is indexed in rosteralpha with no
// community removal standing.
func (f *communityCommentFixture) seedVisibleComment() {
	f.store.seedComment(commentAlphaURI, moderation.CommunityCommentState{
		CommunityDID: rosterCommunity, CID: commentAlphaCID, Version: commentStartVersion,
	})
}

// seedRemovedComment: commentAlphaURI is removed in rosteralpha by a removal
// record with code and reason, standing in the repo and applied in the index.
func (f *communityCommentFixture) seedRemovedComment(code, reason string) {
	f.store.seedComment(commentAlphaURI, moderation.CommunityCommentState{
		CommunityDID: rosterCommunity, CID: commentAlphaCID,
		RemovalCID: commentRemovalCID, RemovalCode: code, RemovalReason: reason,
		Removed: true, Version: commentStartVersion,
	})
	body := map[string]any{
		"$type":     posts.RemovalCollection,
		"subject":   map[string]any{"uri": commentAlphaURI, "cid": commentAlphaCID},
		"code":      code,
		"createdAt": commentStandingCreate,
	}
	if reason != "" {
		body["reason"] = reason
	}
	f.repo.setPostRecord(posts.RemovalCollection, commentAlphaRkey, commentRemovalCID, body)
}

func (f *communityCommentFixture) removeComment(actorDID, commentURI, code, reason string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.RemoveComment(context.Background(), moderation.RemoveCommentRequest{
		ActorDID: actorDID, CommentURI: commentURI, Code: code, Reason: reason,
		ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

func (f *communityCommentFixture) restoreComment(actorDID, commentURI string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.RestoreComment(context.Background(), moderation.RestoreCommentRequest{
		ActorDID: actorDID, CommentURI: commentURI, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

// assertNothingReadBeyondCommunity: the comment's community was the only read
// — no action transaction, no comment state read, no community repo opened,
// no batch, no intent.
func (f *communityCommentFixture) assertNothingReadBeyondCommunity(t *testing.T) {
	t.Helper()
	assert.Empty(t, f.store.locksSnapshot(), "no action transaction may open")
	assert.Empty(t, f.store.commentReadsSnapshot(), "the comment state may not be read")
	assert.Empty(t, f.factory.openedSnapshot(), "the community repo may not be opened")
	f.assertPostNothingWritten(t)
}

// U1: a remove is ONE batch with ONE op, a create of the removal record at the
// comment's derived rkey carrying the intent's createdAt; a standing spam
// removal changed to off-topic is ONE update op; a restore is ONE batch with
// ONE op, a delete of the removal record. Each arrives with its open intent
// stored, and no acceptance op is ever sent — an acceptance standing at the
// comment's rkey is left alone.
func TestCommunityCommentActions_CommitShape(t *testing.T) {
	// The writer's clock moves on after the intent is stored: the record must
	// carry the intent's time, not the time of the write.
	advanceAtWrite := func(f *communityCommentFixture) {
		f.withHooks(moderation.CommunityActionHooks{BeforeWrite: func(context.Context) {
			f.clock.set(rosterClockStart.Add(5 * time.Minute))
		}})
	}
	standAcceptance := func(f *communityCommentFixture) {
		f.repo.setPostRecord(posts.AcceptanceCollection, commentAlphaRkey, commentAcceptanceCID, map[string]any{
			"$type":     posts.AcceptanceCollection,
			"subject":   map[string]any{"uri": commentAlphaURI, "cid": commentAlphaCID},
			"createdAt": commentStandingCreate,
		})
	}

	cases := []struct {
		name            string
		acceptanceStood bool
	}{
		{name: "remove"},
		{name: "remove with an acceptance standing at the comment's rkey", acceptanceStood: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityCommentFixture(t)
			f.seedVisibleComment()
			if tc.acceptanceStood {
				standAcceptance(f)
			}
			advanceAtWrite(f)

			result, err := f.removeComment(rosterModerator, commentAlphaURI, "spam", commentRemovalReason, commentStartVersion, "remove-comment-1")

			batches := f.repo.batchesSnapshot()
			require.Len(t, batches, 1, "a comment remove is exactly one ApplyWrites batch")
			assert.Equal(t, rosterHeadZeroCID, batches[0].swapCommit)
			assert.Equal(t, []postBatchWrite{
				{Op: "create", Collection: "social.coves.community.removal", RKey: "ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya", Record: literalRecordJSON(t, `{
					"$type": "social.coves.community.removal",
					"subject": {"uri": "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentalpha1", "cid": "bafyreiacommentalphaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
					"code": "spam",
					"reason": "Posting phishing links",
					"createdAt": "2026-11-13T12:00:00Z"
				}`)},
			}, postBatchWrites(t, batches[0]), "one op on the removal record, nothing on an acceptance")
			assert.Empty(t, f.repo.violationsSnapshot(), "the open intent must exist when the batch arrives")
			if tc.acceptanceStood {
				assert.True(t, f.repo.hasRecord(posts.AcceptanceCollection, commentAlphaRkey), "the acceptance at the comment's rkey is untouched")
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, "at://did:plc:rosteralpha/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya", result.RecordURI)
			assert.False(t, result.AlreadyApplied)
			assert.EqualValues(t, 6, result.Version)
			assert.NotEmpty(t, result.ActionID)
			assert.Equal(t, []communityActionLogged{
				{Action: "apply-removal", ActorDID: rosterModerator, CommunityDID: rosterCommunity},
			}, f.store.actionLogSnapshot())
		})
	}

	t.Run("spam removal changed to off-topic", func(t *testing.T) {
		f := newCommunityCommentFixture(t)
		f.seedRemovedComment("spam", "")

		result, err := f.removeComment(rosterModerator, commentAlphaURI, "off-topic", "Not about gardening", commentStartVersion, "remove-comment-1")

		batches := f.repo.batchesSnapshot()
		require.Len(t, batches, 1, "a rewrite is exactly one ApplyWrites batch")
		assert.Equal(t, rosterHeadZeroCID, batches[0].swapCommit)
		require.Len(t, batches[0].writes, 1, "a rewrite is one op")
		write := batches[0].writes[0]
		assert.Equal(t, "update", string(write.Op))
		assert.Equal(t, "social.coves.community.removal", write.Collection)
		assert.Equal(t, "ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya", write.RKey)
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(canonicalRecordJSON(t, write.Record)), &body))
		assert.NotEmpty(t, body["createdAt"], "the rewritten record carries a createdAt")
		delete(body, "createdAt")
		assert.Equal(t, map[string]any{
			"$type":   "social.coves.community.removal",
			"subject": map[string]any{"uri": "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentalpha1", "cid": "bafyreiacommentalphaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			"code":    "off-topic",
			"reason":  "Not about gardening",
		}, body)
		assert.Empty(t, f.repo.violationsSnapshot(), "the open intent must exist when the batch arrives")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.False(t, result.AlreadyApplied)
		assert.EqualValues(t, 6, result.Version)
	})

	t.Run("restore", func(t *testing.T) {
		f := newCommunityCommentFixture(t)
		f.seedRemovedComment("spam", commentRemovalReason)
		standAcceptance(f)
		advanceAtWrite(f)

		result, err := f.restoreComment(rosterModerator, commentAlphaURI, commentStartVersion, "restore-comment-1")

		batches := f.repo.batchesSnapshot()
		require.Len(t, batches, 1, "a comment restore is exactly one ApplyWrites batch")
		assert.Equal(t, rosterHeadZeroCID, batches[0].swapCommit)
		assert.Equal(t, []postBatchWrite{
			{Op: "delete", Collection: "social.coves.community.removal", RKey: "ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya"},
		}, postBatchWrites(t, batches[0]), "one delete of the removal record, nothing on an acceptance")
		assert.Empty(t, f.repo.violationsSnapshot(), "the open intent must exist when the batch arrives")
		assert.True(t, f.repo.hasRecord(posts.AcceptanceCollection, commentAlphaRkey), "the acceptance at the comment's rkey is untouched")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, "at://did:plc:rosteralpha/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya", result.RecordURI)
		assert.False(t, result.AlreadyApplied)
		assert.EqualValues(t, 6, result.Version)
		assert.Equal(t, []communityActionLogged{
			{Action: "retract-removal", ActorDID: rosterModerator, CommunityDID: rosterCommunity},
		}, f.store.actionLogSnapshot())
	})
}

// U2: a comment whose parent chain does not resolve is CommentNotFound, and a
// caller the authorizer refuses is Forbidden, each with the comment's
// community the only read: no authorization (for not found), no action
// transaction, no comment state read, no community repo, no intent. A role
// revoked after the first authorization is Forbidden under the lock, with
// nothing written.
func TestCommunityCommentActions_RefusedBeforeAnyRead(t *testing.T) {
	cases := []struct {
		name, actor, comment string
		want                 error
		wantAuthorized       []string
	}{
		{"parent chain does not resolve", rosterModerator, commentDanglingURI, moderation.ErrCommentNotFound, nil},
		{"caller holds no role", rosterOutsider, commentAlphaURI, moderation.ErrForbidden, []string{rosterCommunity}},
		{"moderator of another community than the chain reaches", rosterModerator, commentBetaURI, moderation.ErrForbidden, []string{postBetaCommunity}},
		{"moderator of a bridged community", rosterModerator, commentRemoteURI, moderation.ErrForbidden, []string{rosterRemote}},
		{"comment in an instance-removed community", rosterModerator, commentRemovedURI, moderation.ErrForbidden, []string{rosterRemoved}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityCommentFixture(t)
			f.seedVisibleComment()

			_, err := f.removeComment(tc.actor, tc.comment, "spam", "", commentStartVersion, "remove-comment-1")

			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, []string{tc.comment}, f.comments.lookupsSnapshot(), "the comment's community is the one read before authorization")
			assert.Equal(t, tc.wantAuthorized, f.authorizes.callsSnapshot(), "authorization runs only for a resolved community")
			assert.Empty(t, f.resolver.lookupsSnapshot(), "the post resolver is not the comment path")
			f.assertNothingReadBeyondCommunity(t)
		})
	}

	t.Run("role revoked after the first authorization", func(t *testing.T) {
		f := newCommunityCommentFixture(t)
		f.seedVisibleComment()
		f.withHooks(moderation.CommunityActionHooks{AfterAuthorize: func(context.Context) {
			f.store.dropRole(rosterCommunity, rosterModeratorKey)
		}})

		_, err := f.removeComment(rosterModerator, commentAlphaURI, "spam", "", commentStartVersion, "remove-comment-1")

		require.ErrorIs(t, err, moderation.ErrForbidden)
		assert.Empty(t, f.store.commentReadsSnapshot(), "the comment state may not be read")
		assert.Empty(t, f.factory.openedSnapshot(), "the community repo may not be opened")
		f.assertPostNothingWritten(t)
	})
}

// U3: expectedVersion is the comment's community record version; a stale one
// is StateConflict with nothing written. A comment whose chain resolves to
// another community under the lock than before it is StateConflict too.
func TestCommunityCommentActions_StateConflicts(t *testing.T) {
	t.Run("stale version", func(t *testing.T) {
		f := newCommunityCommentFixture(t)
		f.seedVisibleComment()

		_, err := f.removeComment(rosterModerator, commentAlphaURI, "spam", "", commentStartVersion-1, "remove-comment-1")

		require.ErrorIs(t, err, moderation.ErrStateConflict)
		f.assertPostNothingWritten(t)
	})

	t.Run("community re-resolved under the lock differs", func(t *testing.T) {
		f := newCommunityCommentFixture(t)
		// The comment was reparented under a rosterbeta post after the first
		// resolution: the state read under the lock names rosterbeta.
		f.store.seedComment(commentAlphaURI, moderation.CommunityCommentState{
			CommunityDID: postBetaCommunity, CID: commentAlphaCID, Version: commentStartVersion,
		})

		_, err := f.removeComment(rosterModerator, commentAlphaURI, "spam", "", commentStartVersion, "remove-comment-1")

		require.ErrorIs(t, err, moderation.ErrStateConflict)
		assert.Equal(t, []string{commentAlphaURI}, f.store.commentReadsSnapshot(), "the conflict is found by the read under the lock")
		f.assertPostNothingWritten(t)
	})
}

// U4: comment removals share the post removal limits: a moderator at 60 in
// the window, or a community at 300, is RateLimited with no intent; an
// instance admin acting is exempt, and a restore is not limited.
func TestCommunityCommentActions_RemovalRateLimits(t *testing.T) {
	cases := []struct {
		name                 string
		actor                string
		byActor, byCommunity int
		restore              bool
		limited              bool
	}{
		{name: "moderator at the per-moderator limit", actor: rosterModerator, byActor: 60, limited: true},
		{name: "community at the per-community limit", actor: rosterModerator, byCommunity: 300, limited: true},
		{name: "instance admin acting at both limits", actor: rosterAdmin, byActor: 60, byCommunity: 300},
		{name: "restore at both limits", actor: rosterModerator, byActor: 60, byCommunity: 300, restore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityCommentFixture(t)
			f.store.setRecentRemovals(rosterCommunity, tc.actor, tc.byActor, tc.byCommunity)
			var err error
			if tc.restore {
				f.seedRemovedComment("spam", "")
				_, err = f.restoreComment(tc.actor, commentAlphaURI, commentStartVersion, "restore-comment-1")
			} else {
				f.seedVisibleComment()
				_, err = f.removeComment(tc.actor, commentAlphaURI, "spam", "", commentStartVersion, "remove-comment-1")
			}

			if tc.limited {
				require.ErrorIs(t, err, moderation.ErrRateLimited)
				f.assertPostNothingWritten(t)
				return
			}
			require.NoError(t, err)
			assert.Len(t, f.repo.batchesSnapshot(), 1, "the action is written")
		})
	}
}

// U5: a post URI as the subject, an unknown code or an over-long reason is
// InvalidRequest before anything is read.
func TestCommunityCommentActions_RequestValidation(t *testing.T) {
	cases := []struct {
		name    string
		restore bool
		subject string
		code    string
		reason  string
	}{
		{name: "remove with a post URI as the subject", subject: postAlphaURI, code: "spam"},
		{name: "restore with a post URI as the subject", restore: true, subject: postAlphaURI},
		{name: "unknown code", subject: commentAlphaURI, code: "foo"},
		{name: "reason over 1000 graphemes", subject: commentAlphaURI, code: "spam", reason: strings.Repeat("a", 1001)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityCommentFixture(t)
			f.seedVisibleComment()
			f.seedAcceptedPost("accepted", postAlphaCID, postAlphaCID)
			// A post subject must not be resolved as a comment either.
			f.comments.communities[postAlphaURI] = rosterCommunity

			var err error
			if tc.restore {
				_, err = f.restoreComment(rosterModerator, tc.subject, commentStartVersion, "restore-comment-1")
			} else {
				_, err = f.removeComment(rosterModerator, tc.subject, tc.code, tc.reason, commentStartVersion, "remove-comment-1")
			}

			require.ErrorIs(t, err, moderation.ErrInvalidRequest)
			assert.Empty(t, f.comments.lookupsSnapshot(), "nothing may be read for an invalid request")
			assert.Empty(t, f.resolver.lookupsSnapshot(), "nothing may be read for an invalid request")
			assert.Empty(t, f.authorizes.callsSnapshot(), "no authorization for an invalid request")
			f.assertNothingReadBeyondCommunity(t)
		})
	}
}

// U6: a remove with the identical code and reason already standing, or a
// restore with no community removal standing, is AlreadyApplied: nothing is
// written and no action is logged.
func TestCommunityCommentActions_AlreadyApplied(t *testing.T) {
	t.Run("identical removal standing", func(t *testing.T) {
		f := newCommunityCommentFixture(t)
		f.seedRemovedComment("spam", commentRemovalReason)

		result, err := f.removeComment(rosterModerator, commentAlphaURI, "spam", commentRemovalReason, commentStartVersion, "remove-comment-1")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.AlreadyApplied)
		assert.Equal(t, "at://did:plc:rosteralpha/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya", result.RecordURI)
		assert.EqualValues(t, 5, result.Version)
		assert.Empty(t, f.repo.batchesSnapshot(), "nothing is written")
		assert.Empty(t, f.store.actionLogSnapshot(), "no action is logged")
	})

	t.Run("restore with no removal standing", func(t *testing.T) {
		f := newCommunityCommentFixture(t)
		f.seedVisibleComment()

		result, err := f.restoreComment(rosterModerator, commentAlphaURI, commentStartVersion, "restore-comment-1")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.AlreadyApplied)
		assert.Equal(t, "at://did:plc:rosteralpha/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya", result.RecordURI)
		assert.EqualValues(t, 5, result.Version)
		assert.Empty(t, f.repo.batchesSnapshot(), "nothing is written")
		assert.Empty(t, f.store.actionLogSnapshot(), "no action is logged")
	})
}

// U7: the idempotency fingerprint covers the code and the reason, against a
// stored result and against the first request's still-open intent.
func TestCommunityCommentActions_IdempotencyFingerprint(t *testing.T) {
	variants := []struct {
		name, code, reason string
	}{
		{"different code", "off-topic", commentRemovalReason},
		{"different reason", "spam", "Posting malware links"},
	}
	for _, first := range []string{"completed", "intent open"} {
		for _, variant := range variants {
			t.Run(first+"/"+variant.name, func(t *testing.T) {
				f := newCommunityCommentFixture(t)
				f.seedVisibleComment()
				if first == "intent open" {
					f.repo.applyErr = errors.New("connection reset by peer")
				}
				_, firstErr := f.removeComment(rosterModerator, commentAlphaURI, "spam", commentRemovalReason, commentStartVersion, "remove-comment-1")

				_, err := f.removeComment(rosterModerator, commentAlphaURI, variant.code, variant.reason, commentStartVersion, "remove-comment-1")

				require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
				assert.Len(t, f.repo.batchesSnapshot(), 1, "the conflicting request sends nothing")
				if first == "completed" {
					require.NoError(t, firstErr)
					return
				}
				require.ErrorIs(t, firstErr, moderation.ErrModerationUnavailable)
				open := 0
				for _, intent := range f.store.intentsSnapshot() {
					if intent.State == moderation.CommunityIntentOpen {
						open++
					}
				}
				assert.Equal(t, 1, open, "the first request's intent stays open")
			})
		}
	}
}

// A batch that failed before it committed leaves the intent open with the
// repo head unmoved: a same-key retry writes again and completes, sending one
// more batch and logging one action.
func TestCommunityCommentActions_SameKeyRetryAfterUncommittedBatch(t *testing.T) {
	cases := []struct {
		name   string
		seed   func(f *communityCommentFixture)
		send   func(f *communityCommentFixture) (*moderation.CommunityActionResult, error)
		action string
		stands bool
	}{
		{
			name: "remove",
			seed: func(f *communityCommentFixture) { f.seedVisibleComment() },
			send: func(f *communityCommentFixture) (*moderation.CommunityActionResult, error) {
				return f.removeComment(rosterModerator, commentAlphaURI, "spam", commentRemovalReason, commentStartVersion, "remove-comment-1")
			},
			action: "apply-removal",
			stands: true,
		},
		{
			name: "restore",
			seed: func(f *communityCommentFixture) { f.seedRemovedComment("spam", commentRemovalReason) },
			send: func(f *communityCommentFixture) (*moderation.CommunityActionResult, error) {
				return f.restoreComment(rosterModerator, commentAlphaURI, commentStartVersion, "restore-comment-1")
			},
			action: "retract-removal",
			stands: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityCommentFixture(t)
			tc.seed(f)
			f.repo.applyErr = errors.New("connection reset by peer")

			_, firstErr := tc.send(f)
			require.ErrorIs(t, firstErr, moderation.ErrModerationUnavailable)
			assert.Empty(t, f.store.actionLogSnapshot(), "the failed batch logs nothing")

			result, err := tc.send(f)

			require.NoError(t, err)
			require.NotNil(t, result)
			assert.False(t, result.AlreadyApplied)
			assert.EqualValues(t, 6, result.Version)
			assert.Equal(t, "at://did:plc:rosteralpha/social.coves.community.removal/ucpsa3lxx6xehv5sgjv4gxmqipvkj2qju6fier3hf6wbxx4hncya", result.RecordURI)
			assert.Len(t, f.repo.batchesSnapshot(), 2, "the retry sends one more batch")
			assert.Equal(t, tc.stands, f.repo.hasRecord(posts.RemovalCollection, commentAlphaRkey))
			assert.Equal(t, []communityActionLogged{
				{Action: tc.action, ActorDID: rosterModerator, CommunityDID: rosterCommunity},
			}, f.store.actionLogSnapshot(), "exactly one action is logged")
			for _, intent := range f.store.intentsSnapshot() {
				assert.NotEqual(t, moderation.CommunityIntentOpen, intent.State, "no intent stays open")
			}
		})
	}
}
