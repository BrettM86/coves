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

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	postBetaCommunity    = "did:plc:rosterbeta"
	postBetaModerator    = "did:plc:rosterbetamod"
	postBetaModeratorKey = "rosterbetamodkey"
	postBetaModeratorCID = "bafyreiabetamoderatoraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	postAlphaURI   = "at://did:plc:rosterauthor/social.coves.community.postv2/3kpostalpha1"
	postRemoteURI  = "at://did:plc:rosterauthor/social.coves.community.postv2/3kpostremote1"
	postRemovedURI = "at://did:plc:rosterauthor/social.coves.community.postv2/3kpostremoved1"

	// SubjectRkey(postAlphaURI), computed once by hand.
	postAlphaRkey = "4lkade3qvbscv3bbotyzz4u45b42aqu65aefgkc5kkdgmm2djtxa"

	postAlphaAcceptanceURI = "at://did:plc:rosteralpha/social.coves.community.acceptance/4lkade3qvbscv3bbotyzz4u45b42aqu65aefgkc5kkdgmm2djtxa"
	postAlphaRemovalURI    = "at://did:plc:rosteralpha/social.coves.community.removal/4lkade3qvbscv3bbotyzz4u45b42aqu65aefgkc5kkdgmm2djtxa"

	// The post's CID the acceptance pins, and the CID of the post's later edit.
	postAlphaCID       = "bafyreiapostalphaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	postAlphaEditedCID = "bafyreiapostalphaeditedaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Standing stand records' CIDs, and the CIDs the fake repo gives a write.
	postAcceptanceCID        = "bafyreiaacceptancestandingaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	postRemovalCID           = "bafyreiaremovalstandingaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	postAcceptanceWrittenCID = "bafyreiaacceptancewrittenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	postRemovalWrittenCID    = "bafyreiaremovalwrittenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	postStandingCreatedAt = "2026-11-10T08:00:00Z"
	postStartVersion      = 3
	postRemovalReason     = "Selling knock-off watches"
)

// fakeCommunityPostResolver is the post-to-community index; it records every
// post it was asked about.
type fakeCommunityPostResolver struct {
	mu          sync.Mutex
	communities map[string]string
	lookups     []string
}

func (r *fakeCommunityPostResolver) PostCommunity(_ context.Context, postURI string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, postURI)
	community, ok := r.communities[postURI]
	if !ok {
		return "", moderation.ErrPostNotFound
	}
	return community, nil
}

func (r *fakeCommunityPostResolver) lookupsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lookups...)
}

// CommunityPost serves the store's indexed post stand state and records the
// read.
func (tx *fakeCommunityActionTx) CommunityPost(_ context.Context, postURI string) (moderation.CommunityPostState, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	tx.store.postReads = append(tx.store.postReads, postURI)
	state, ok := tx.store.posts[postURI]
	if !ok {
		return moderation.CommunityPostState{}, moderation.ErrPostNotFound
	}
	return state, nil
}

// RecentCommunityRemovals serves the counts the test seeded.
func (tx *fakeCommunityActionTx) RecentCommunityRemovals(_ context.Context, communityDID, actorDID string, _ time.Time) (int, int, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	return tx.store.recentRemovalsByActor[actorDID], tx.store.recentRemovalsCommunity[communityDID], nil
}

func (s *fakeCommunityActionStore) seedPost(postURI string, state moderation.CommunityPostState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts[postURI] = state
}

func (s *fakeCommunityActionStore) setRecentRemovals(communityDID, actorDID string, byActor, byCommunity int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recentRemovalsByActor[actorDID] = byActor
	s.recentRemovalsCommunity[communityDID] = byCommunity
}

func (s *fakeCommunityActionStore) postReadsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.postReads...)
}

func (s *fakeCommunityActionStore) locksSnapshot() []moderation.CommunityActionLock {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]moderation.CommunityActionLock(nil), s.locks...)
}

// isFakePostIntent reports whether the intent writes a post's stand records:
// its target is the post's removal record.
func isFakePostIntent(intent moderation.CommunityIntent) bool {
	return strings.Contains(intent.TargetURI, "/"+posts.RemovalCollection+"/")
}

// completePostIntentLocked applies a post intent's local effect on Commit: the
// post's stand state follows the written records, its version advances once,
// one action is logged, the intent completes and the result is stored. The
// caller holds the store's lock.
func (tx *fakeCommunityActionTx) completePostIntentLocked(index int, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	stored := tx.store.intents[index]
	removing := stored.Op != string(pds.WriteOpDelete)
	body, recordURI := stored.Record, stored.TargetURI
	if !removing {
		body, recordURI = stored.PairedRecord, stored.PairedTargetURI
	}
	var record struct {
		Subject struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		} `json:"subject"`
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &record); err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("fake store: decoding the post intent's record: %w", err)
	}
	state, ok := tx.store.posts[record.Subject.URI]
	if !ok {
		return moderation.CommunityActionResult{}, fmt.Errorf("fake store: no indexed post %s", record.Subject.URI)
	}
	next := state
	next.Version++
	if removing {
		next.Status, next.HasAcceptance, next.PinnedCID = "removed", false, ""
		next.RemovalCID, next.RemovalCode, next.RemovalReason = commit.RecordCID, record.Code, record.Reason
	} else {
		next.Status, next.HasAcceptance, next.PinnedCID = "accepted", true, record.Subject.CID
		next.RemovalCID, next.RemovalCode, next.RemovalReason = "", "", ""
	}
	actionID := fmt.Sprintf("community-action-%d", len(tx.store.actions)+1)
	result := moderation.CommunityActionResult{
		CommunityDID: stored.CommunityDID,
		RecordURI:    recordURI,
		ActionID:     actionID,
		Version:      next.Version,
	}
	actorDID := stored.ActorDID
	tx.pending = append(tx.pending, func() {
		tx.store.posts[record.Subject.URI] = next
		tx.store.actions = append(tx.store.actions, communityActionRecorded{
			ActorDID: actorDID, AdminActing: stored.AdminActing, CommunityDID: stored.CommunityDID,
		})
		tx.store.actionLog = append(tx.store.actionLog, communityActionLogged{
			Action: stored.Action, ActorDID: actorDID, AdminActing: stored.AdminActing, CommunityDID: stored.CommunityDID,
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

// setPostRecord puts a stand record with its body into the community repo.
func (r *fakeRosterRepo) setPostRecord(collection, rkey, cid string, value map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[collection+"/"+rkey] = &pds.RecordResponse{
		URI: "at://" + r.did + "/" + collection + "/" + rkey, CID: cid, Value: value,
	}
}

func (r *fakeRosterRepo) violationsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.violations...)
}

type communityPostFixture struct {
	*communityActionFixture
	resolver *fakeCommunityPostResolver
}

// newCommunityPostFixture extends the roster fixture for post actions:
// rosterbeta is a second hosted community whose only moderator is
// postBetaModerator; postAlphaURI is in rosteralpha, postRemoteURI in the
// bridged rosterremote and postRemovedURI in the instance-removed
// rosterremoved. Limits are 60 per moderator and 300 per community per hour.
// No post state is seeded.
func newCommunityPostFixture(t *testing.T) *communityPostFixture {
	t.Helper()
	f := newCommunityActionFixture(t)
	f.store.seedRole(rosterRole(postBetaCommunity, postBetaModerator, communities.RoleModerator,
		postBetaModeratorKey, postBetaModeratorCID, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)))
	resolver := &fakeCommunityPostResolver{communities: map[string]string{
		postAlphaURI:   rosterCommunity,
		postRemoteURI:  rosterRemote,
		postRemovedURI: rosterRemoved,
	}}
	authorizer := communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted: &communityStateHosted{hosted: map[string]bool{
			rosterCommunity:   true,
			rosterRemoved:     true,
			postBetaCommunity: true,
		}},
		Removals:      f.removals,
		Roles:         f.store,
		AccountBans:   communityStateAccountBans{},
		AccountStatus: communityStateAccountStatus{},
		Admins:        communityStateAdmins{rosterAdmin: true},
		Now:           f.clock.Now,
	})
	cfg := f.config
	cfg.Authorizer = authorizer
	cfg.Posts = resolver
	cfg.RemovalLimits = moderation.CommunityRemovalLimits{PerModerator: 60, PerCommunity: 300, Window: time.Hour}
	f.config = cfg
	f.authorizer = authorizer
	f.service = moderation.NewCommunityActionService(cfg)
	f.repo.writeCIDs[posts.RemovalCollection+"/"+postAlphaRkey] = postRemovalWrittenCID
	f.repo.writeCIDs[posts.AcceptanceCollection+"/"+postAlphaRkey] = postAcceptanceWrittenCID
	for uri, community := range map[string]string{postRemoteURI: rosterRemote, postRemovedURI: rosterRemoved} {
		f.store.seedPost(uri, moderation.CommunityPostState{
			CommunityDID: community, CID: postAlphaCID, Status: "accepted",
			PinnedCID: postAlphaCID, HasAcceptance: true, Version: postStartVersion,
		})
	}
	return &communityPostFixture{communityActionFixture: f, resolver: resolver}
}

// seedAcceptedPost: postAlphaURI is accepted in rosteralpha, its acceptance
// pinning pinnedCID standing in the repo, its current CID currentCID.
func (f *communityPostFixture) seedAcceptedPost(status, currentCID, pinnedCID string) {
	f.store.seedPost(postAlphaURI, moderation.CommunityPostState{
		CommunityDID: rosterCommunity, CID: currentCID, Status: status,
		PinnedCID: pinnedCID, HasAcceptance: true, Version: postStartVersion,
	})
	f.repo.setPostRecord(posts.AcceptanceCollection, postAlphaRkey, postAcceptanceCID, map[string]any{
		"$type":     posts.AcceptanceCollection,
		"subject":   map[string]any{"uri": postAlphaURI, "cid": pinnedCID},
		"createdAt": postStandingCreatedAt,
	})
}

// seedRemovedPost: postAlphaURI was removed (spam) while at postAlphaCID and
// edited since to postAlphaEditedCID; the removal stands and no acceptance does.
func (f *communityPostFixture) seedRemovedPost() {
	f.store.seedPost(postAlphaURI, moderation.CommunityPostState{
		CommunityDID: rosterCommunity, CID: postAlphaEditedCID, Status: "removed",
		RemovalCID: postRemovalCID, RemovalCode: "spam", Version: postStartVersion,
	})
	f.repo.setPostRecord(posts.RemovalCollection, postAlphaRkey, postRemovalCID, map[string]any{
		"$type":     posts.RemovalCollection,
		"subject":   map[string]any{"uri": postAlphaURI, "cid": postAlphaCID},
		"code":      "spam",
		"createdAt": postStandingCreatedAt,
	})
}

func (f *communityPostFixture) removePost(actorDID, postURI, code, reason string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.RemovePost(context.Background(), moderation.RemovePostRequest{
		ActorDID: actorDID, PostURI: postURI, Code: code, Reason: reason,
		ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

func (f *communityPostFixture) restorePost(actorDID, postURI string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.RestorePost(context.Background(), moderation.RestorePostRequest{
		ActorDID: actorDID, PostURI: postURI, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

// postBatchWrite is one write of a received batch, its record as canonical
// JSON ("" for a delete).
type postBatchWrite struct {
	Op, Collection, RKey, Record string
}

func canonicalRecordJSON(t *testing.T, record any) string {
	t.Helper()
	if record == nil {
		return ""
	}
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	var value any
	require.NoError(t, json.Unmarshal(encoded, &value))
	canonical, err := json.Marshal(value)
	require.NoError(t, err)
	return string(canonical)
}

func postBatchWrites(t *testing.T, batch rosterBatch) []postBatchWrite {
	t.Helper()
	out := make([]postBatchWrite, 0, len(batch.writes))
	for _, write := range batch.writes {
		out = append(out, postBatchWrite{
			Op: string(write.Op), Collection: write.Collection, RKey: write.RKey,
			Record: canonicalRecordJSON(t, write.Record),
		})
	}
	return out
}

func literalRecordJSON(t *testing.T, document string) string {
	t.Helper()
	var value any
	require.NoError(t, json.Unmarshal([]byte(document), &value))
	return canonicalRecordJSON(t, value)
}

// assertPostNothingWritten: no batch reached the repo and no intent was stored.
func (f *communityPostFixture) assertPostNothingWritten(t *testing.T) {
	t.Helper()
	assert.Empty(t, f.repo.batchesSnapshot(), "no batch may reach the community repo")
	assert.Empty(t, f.store.intentsSnapshot(), "no intent may be stored")
}

// U1: a remove of an accepted post is one batch deleting the acceptance and
// creating the removal, stamped with the intent's createdAt; a restore is one
// batch deleting the removal and creating an acceptance pinned to the post's
// current CID. Each arrives with its open intent stored.
func TestCommunityPostActions_CommitShape(t *testing.T) {
	// The writer's clock moves on after the intent is stored: the record must
	// carry the intent's time, not the time of the write.
	advanceAtWrite := func(f *communityPostFixture) {
		f.withHooks(moderation.CommunityActionHooks{BeforeWrite: func(context.Context) {
			f.clock.set(rosterClockStart.Add(5 * time.Minute))
		}})
	}

	t.Run("remove", func(t *testing.T) {
		f := newCommunityPostFixture(t)
		f.seedAcceptedPost("accepted", postAlphaCID, postAlphaCID)
		advanceAtWrite(f)

		result, err := f.removePost(rosterModerator, postAlphaURI, "spam", postRemovalReason, postStartVersion, "remove-1")

		batches := f.repo.batchesSnapshot()
		require.Len(t, batches, 1, "a remove is exactly one ApplyWrites batch")
		assert.Equal(t, rosterHeadZeroCID, batches[0].swapCommit)
		assert.ElementsMatch(t, []postBatchWrite{
			{Op: "delete", Collection: "social.coves.community.acceptance", RKey: postAlphaRkey},
			{Op: "create", Collection: "social.coves.community.removal", RKey: postAlphaRkey, Record: literalRecordJSON(t, `{
				"$type": "social.coves.community.removal",
				"subject": {"uri": "at://did:plc:rosterauthor/social.coves.community.postv2/3kpostalpha1", "cid": "bafyreiapostalphaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"code": "spam",
				"reason": "Selling knock-off watches",
				"createdAt": "2026-11-13T12:00:00Z"
			}`)},
		}, postBatchWrites(t, batches[0]))
		assert.Empty(t, f.repo.violationsSnapshot(), "the open intent must exist when the batch arrives")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, postAlphaRemovalURI, result.RecordURI)
		assert.False(t, result.AlreadyApplied)
		assert.EqualValues(t, postStartVersion+1, result.Version)
	})

	t.Run("restore", func(t *testing.T) {
		f := newCommunityPostFixture(t)
		f.seedRemovedPost()
		advanceAtWrite(f)

		result, err := f.restorePost(rosterModerator, postAlphaURI, postStartVersion, "restore-1")

		batches := f.repo.batchesSnapshot()
		require.Len(t, batches, 1, "a restore is exactly one ApplyWrites batch")
		assert.Equal(t, rosterHeadZeroCID, batches[0].swapCommit)
		assert.ElementsMatch(t, []postBatchWrite{
			{Op: "delete", Collection: "social.coves.community.removal", RKey: postAlphaRkey},
			{Op: "create", Collection: "social.coves.community.acceptance", RKey: postAlphaRkey, Record: literalRecordJSON(t, `{
				"$type": "social.coves.community.acceptance",
				"subject": {"uri": "at://did:plc:rosterauthor/social.coves.community.postv2/3kpostalpha1", "cid": "bafyreiapostalphaeditedaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"createdAt": "2026-11-13T12:00:00Z"
			}`)},
		}, postBatchWrites(t, batches[0]))
		assert.Empty(t, f.repo.violationsSnapshot(), "the open intent must exist when the batch arrives")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, postAlphaAcceptanceURI, result.RecordURI)
		assert.False(t, result.AlreadyApplied)
		assert.EqualValues(t, postStartVersion+1, result.Version)
	})
}

// U2: a caller the authorizer refuses is Forbidden before anything but the
// post's community was read: no action transaction, no post state read, no
// community repo opened, no intent.
func TestCommunityPostActions_AuthorizationRefusedBeforeAnyRead(t *testing.T) {
	cases := []struct {
		name, actor, post string
	}{
		{"caller holds no role", rosterOutsider, postAlphaURI},
		{"moderator of another community", postBetaModerator, postAlphaURI},
		{"moderator of a bridged community", rosterModerator, postRemoteURI},
		{"post in an instance-removed community", rosterModerator, postRemovedURI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityPostFixture(t)
			f.seedAcceptedPost("accepted", postAlphaCID, postAlphaCID)

			_, err := f.removePost(tc.actor, tc.post, "spam", "", postStartVersion, "remove-1")

			require.ErrorIs(t, err, moderation.ErrForbidden)
			assert.Equal(t, []string{tc.post}, f.resolver.lookupsSnapshot(), "the post's community is the one read before authorization")
			assert.Empty(t, f.store.locksSnapshot(), "no action transaction may open")
			assert.Empty(t, f.store.postReadsSnapshot(), "the post state may not be read")
			assert.Empty(t, f.factory.openedSnapshot(), "the community repo may not be opened")
			f.assertPostNothingWritten(t)
		})
	}
}

// U3: expectedVersion is the post's community record version. A stale one is
// StateConflict with nothing written; a local status move that left the
// version alone does not conflict.
func TestCommunityPostActions_ExpectedVersion(t *testing.T) {
	t.Run("stale version", func(t *testing.T) {
		f := newCommunityPostFixture(t)
		f.seedAcceptedPost("accepted", postAlphaCID, postAlphaCID)

		_, err := f.removePost(rosterModerator, postAlphaURI, "spam", "", postStartVersion-1, "remove-1")

		require.ErrorIs(t, err, moderation.ErrStateConflict)
		f.assertPostNothingWritten(t)
	})

	t.Run("local status move with the same version", func(t *testing.T) {
		f := newCommunityPostFixture(t)
		// The author edited the post: it awaits re-acceptance, and the standing
		// acceptance still pins the earlier CID.
		f.seedAcceptedPost("pending_reacceptance", postAlphaEditedCID, postAlphaCID)

		_, err := f.removePost(rosterModerator, postAlphaURI, "spam", "", postStartVersion, "remove-1")

		require.NoError(t, err)
		assert.Len(t, f.repo.batchesSnapshot(), 1, "the removal is written")
	})
}

// U4: a moderator at 60 removals in the window, or a community at 300, is
// RateLimited with nothing written; an instance admin acting is exempt, and a
// restore is not limited.
func TestCommunityPostActions_RemovalRateLimits(t *testing.T) {
	cases := []struct {
		name                 string
		actor                string
		byActor, byCommunity int
		restore              bool
		limited              bool
	}{
		{name: "moderator at the per-moderator limit", actor: rosterModerator, byActor: 60, byCommunity: 0, limited: true},
		{name: "community at the per-community limit", actor: rosterModerator, byActor: 0, byCommunity: 300, limited: true},
		{name: "instance admin acting at both limits", actor: rosterAdmin, byActor: 60, byCommunity: 300},
		{name: "restore at both limits", actor: rosterModerator, byActor: 60, byCommunity: 300, restore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityPostFixture(t)
			f.store.setRecentRemovals(rosterCommunity, tc.actor, tc.byActor, tc.byCommunity)
			var err error
			if tc.restore {
				f.seedRemovedPost()
				_, err = f.restorePost(tc.actor, postAlphaURI, postStartVersion, "restore-1")
			} else {
				f.seedAcceptedPost("accepted", postAlphaCID, postAlphaCID)
				_, err = f.removePost(tc.actor, postAlphaURI, "spam", "", postStartVersion, "remove-1")
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

// U5: the action reason a community removal code maps to.
func TestCommunityRemovalActionReason(t *testing.T) {
	cases := map[string]string{
		"spam":                 "social.coves.moderation.defs#reasonSpam",
		"off-topic":            "social.coves.moderation.defs#reasonRuleViolation",
		"rule-violation":       "social.coves.moderation.defs#reasonRuleViolation",
		"harassment":           "social.coves.moderation.defs#reasonHarassment",
		"doxing":               "social.coves.moderation.defs#reasonDoxing",
		"illegal-content":      "social.coves.moderation.defs#reasonIllegalContent",
		"author-banned":        "social.coves.moderation.defs#reasonModeratorDiscretion",
		"moderator-discretion": "social.coves.moderation.defs#reasonModeratorDiscretion",
		"foo":                  "social.coves.moderation.defs#reasonModeratorDiscretion",
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			assert.Equal(t, want, moderation.CommunityRemovalActionReason(code))
		})
	}
}

// U6: an unknown code or an over-long reason is InvalidRequest before
// anything is read; harassment and doxing are accepted request codes.
func TestCommunityPostActions_RemoveRequestValidation(t *testing.T) {
	cases := []struct {
		name, code, reason string
		invalid            bool
	}{
		{name: "unknown code", code: "foo", invalid: true},
		{name: "reason over 1000 graphemes", code: "spam", reason: strings.Repeat("a", 1001), invalid: true},
		{name: "harassment", code: "harassment"},
		{name: "doxing", code: "doxing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityPostFixture(t)
			f.seedAcceptedPost("accepted", postAlphaCID, postAlphaCID)

			_, err := f.removePost(rosterModerator, postAlphaURI, tc.code, tc.reason, postStartVersion, "remove-1")

			if tc.invalid {
				require.ErrorIs(t, err, moderation.ErrInvalidRequest)
				assert.Empty(t, f.resolver.lookupsSnapshot(), "nothing may be read for an invalid request")
				assert.Empty(t, f.store.locksSnapshot(), "no action transaction may open")
				f.assertPostNothingWritten(t)
				return
			}
			require.NoError(t, err)
			assert.Len(t, f.repo.batchesSnapshot(), 1, "the removal is written")
		})
	}
}

// U7: the idempotency fingerprint covers the code and the reason, against a
// stored result and against the first request's still-open intent.
func TestCommunityPostActions_IdempotencyFingerprint(t *testing.T) {
	variants := []struct {
		name, code, reason string
	}{
		{"different code", "off-topic", postRemovalReason},
		{"different reason", "spam", "Counterfeit goods"},
	}
	for _, first := range []string{"completed", "intent open"} {
		for _, variant := range variants {
			t.Run(first+"/"+variant.name, func(t *testing.T) {
				f := newCommunityPostFixture(t)
				f.seedAcceptedPost("accepted", postAlphaCID, postAlphaCID)
				if first == "intent open" {
					f.repo.applyErr = errors.New("connection reset by peer")
				}
				_, firstErr := f.removePost(rosterModerator, postAlphaURI, "spam", postRemovalReason, postStartVersion, "remove-1")

				_, err := f.removePost(rosterModerator, postAlphaURI, variant.code, variant.reason, postStartVersion, "remove-1")

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
