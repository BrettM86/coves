package moderation_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rosterCommunity = "did:plc:rosteralpha"
	rosterRemote    = "did:plc:rosterremote"
	rosterRemoved   = "did:plc:rosterremoved"

	rosterOwner           = "did:plc:rosterowner"
	rosterModerator       = "did:plc:troublemaker456"
	rosterSecondModerator = "did:plc:rostermodtwo"
	rosterOutsider        = "did:plc:rosteroutsider"
	rosterAdmin           = "did:plc:rosteradmin"

	// RoleRecordKey of each account above.
	rosterOwnerKey           = "5jbtoxiw4eatw5k4ht7uq6zz37en4i73i5b6yu6ctid6v23oimya"
	rosterModeratorKey       = "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa"
	rosterSecondModeratorKey = "4djvwvjcr5ob27rrjd4qfzotxnrze76wgblxyo4xkjjjrwjqoonq"
	rosterOutsiderKey        = "udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq"

	rosterModeratorURI       = "at://did:plc:rosteralpha/social.coves.community.moderator/a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa"
	rosterSecondModeratorURI = "at://did:plc:rosteralpha/social.coves.community.moderator/4djvwvjcr5ob27rrjd4qfzotxnrze76wgblxyo4xkjjjrwjqoonq"

	rosterOwnerCID           = "bafyreiaowneraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterModeratorCID       = "bafyreiamoderatoraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterModeratorStaleCID  = "bafyreiamoderatorstaleaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterSecondModeratorCID = "bafyreiasecondmoderatoraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// CIDs the fake community repo assigns to a create or update of each
	// account's role record.
	rosterOutsiderWrittenCID  = "bafyreiaoutsiderwrittenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterModeratorWrittenCID = "bafyreiamoderatorwrittenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterOwnerWrittenCID     = "bafyreiaownerwrittenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	rosterHeadZeroCID = "bafyreiaheadzeroaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterHeadZeroRev = "3krosterrev00"
	rosterHeadOneCID  = "bafyreiaheadoneaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterHeadOneRev  = "3krosterrev01"
	rosterHeadTwoCID  = "bafyreiaheadtwoaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterHeadTwoRev  = "3krosterrev02"

	rosterStartVersion = 7
)

var rosterClockStart = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

type communityActionFixture struct {
	service     *moderation.CommunityActionService
	invitations *moderation.CommunityInvitationService
	store       *fakeCommunityActionStore
	repo        *fakeRosterRepo
	factory     *rosterRepoFactory
	authorizer  communities.RoleAuthorizer
	clock       *rosterClock
	removals    communityStateRemovals
	config      moderation.CommunityActionConfig
}

// rosterClock is the fixture's injected clock; it starts at rosterClockStart.
type rosterClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *rosterClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *rosterClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// rosterHandles resolves the fixture's handles; any other handle is unknown.
type rosterHandles map[string]string

func (h rosterHandles) ResolveHandleToDID(_ context.Context, handle string) (string, error) {
	did, ok := h[handle]
	if !ok {
		return "", fmt.Errorf("%w: %s", users.ErrUserNotFound, handle)
	}
	return did, nil
}

// withHooks rebuilds the action service over the fixture's state with hooks.
func (f *communityActionFixture) withHooks(hooks moderation.CommunityActionHooks) {
	cfg := f.config
	cfg.Hooks = hooks
	f.service = moderation.NewCommunityActionService(cfg)
}

func rosterRole(communityDID, subjectDID, role, rkey, cid string, created time.Time) communities.CommunityRole {
	return communities.CommunityRole{
		CommunityDID: communityDID, SubjectDID: subjectDID, Role: role,
		RecordKey: rkey, RecordCID: cid, RecordCreatedAt: created,
	}
}

// newCommunityActionFixture: rosteralpha is hosted at roster version 7 with
// owner O, moderators M and N, each indexed and present in the community repo
// at head zero. rosterremote (not hosted) and rosterremoved (hosted, removed by
// the instance) each index M as a moderator. rosteradmin is an instance admin.
func newCommunityActionFixture(t *testing.T) *communityActionFixture {
	t.Helper()
	clock := &rosterClock{now: rosterClockStart}
	now := clock.Now
	store := newFakeCommunityActionStore(now)
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	store.seedRole(rosterRole(rosterCommunity, rosterOwner, communities.RoleOwner, rosterOwnerKey, rosterOwnerCID, created))
	store.seedRole(rosterRole(rosterCommunity, rosterModerator, communities.RoleModerator, rosterModeratorKey, rosterModeratorCID, created.Add(time.Hour)))
	store.seedRole(rosterRole(rosterCommunity, rosterSecondModerator, communities.RoleModerator, rosterSecondModeratorKey, rosterSecondModeratorCID, created.Add(2*time.Hour)))
	store.seedRole(rosterRole(rosterRemote, rosterModerator, communities.RoleModerator, rosterModeratorKey, rosterModeratorCID, created))
	store.seedRole(rosterRole(rosterRemoved, rosterModerator, communities.RoleModerator, rosterModeratorKey, rosterModeratorCID, created))
	store.versions[rosterCommunity] = rosterStartVersion

	repo := &fakeRosterRepo{
		did:        rosterCommunity,
		records:    map[string]*pds.RecordResponse{},
		head:       pds.LatestCommit{CID: rosterHeadZeroCID, Rev: rosterHeadZeroRev},
		commitHead: pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		checkWrite: store.checkIntentForWrite,
		writeCIDs: map[string]string{
			communities.RoleCollection + "/" + rosterOutsiderKey:  rosterOutsiderWrittenCID,
			communities.RoleCollection + "/" + rosterModeratorKey: rosterModeratorWrittenCID,
			communities.RoleCollection + "/" + rosterOwnerKey:     rosterOwnerWrittenCID,
		},
	}
	repo.setRecord(communities.RoleCollection, rosterOwnerKey, rosterOwnerCID)
	repo.setRecord(communities.RoleCollection, rosterModeratorKey, rosterModeratorCID)
	repo.setRecord(communities.RoleCollection, rosterSecondModeratorKey, rosterSecondModeratorCID)
	factory := &rosterRepoFactory{repo: repo}
	removals := communityStateRemovals{rosterRemoved: true}

	authorizer := communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted: &communityStateHosted{hosted: map[string]bool{
			rosterCommunity: true,
			rosterRemoved:   true,
		}},
		Removals:      removals,
		Roles:         store,
		AccountBans:   communityStateAccountBans{},
		AccountStatus: communityStateAccountStatus{},
		Admins:        communityStateAdmins{rosterAdmin: true},
		Now:           now,
	})
	config := moderation.CommunityActionConfig{
		Authorizer:     authorizer,
		Store:          store,
		Repos:          factory.open,
		Now:            now,
		IntentTTL:      10 * time.Minute,
		IdempotencyTTL: 24 * time.Hour,
	}
	service := moderation.NewCommunityActionService(config)
	invitations := moderation.NewCommunityInvitationService(moderation.CommunityInvitationConfig{
		Authorizer:  authorizer,
		Store:       store,
		Invitations: store,
		Resolver: rosterHandles{
			"rosterowner.test":    rosterOwner,
			"rostermod.test":      rosterModerator,
			"rosteroutsider.test": rosterOutsider,
		},
		Now:            now,
		IdempotencyTTL: 24 * time.Hour,
	})
	return &communityActionFixture{
		service: service, invitations: invitations, store: store, repo: repo, factory: factory,
		authorizer: authorizer, clock: clock, removals: removals, config: config,
	}
}

func (f *communityActionFixture) removeModerator(subjectDID string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.RemoveModerator(context.Background(), moderation.RemoveModeratorRequest{
		ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: subjectDID,
		ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

// assertNothingWritten: no PDS write, no intent, no action, index and version
// as seeded.
func (f *communityActionFixture) assertNothingWritten(t *testing.T) {
	t.Helper()
	assert.Empty(t, f.repo.batchesSnapshot(), "no PDS write")
	assert.Empty(t, f.store.intentsSnapshot(), "no intent saved")
	assert.Empty(t, f.store.actionsSnapshot(), "no action written")
	assert.EqualValues(t, rosterStartVersion, f.store.version(rosterCommunity), "roster version unchanged")
	for _, rkey := range []string{rosterOwnerKey, rosterModeratorKey, rosterSecondModeratorKey} {
		assert.True(t, f.store.hasRole(rosterCommunity, rkey), "index row %s kept", rkey)
		assert.True(t, f.repo.hasRecord(communities.RoleCollection, rkey), "repo record %s kept", rkey)
	}
}

// intentSummary is the stored intent fields the tests pin.
type intentSummary struct {
	ActorDID, CommunityDID, SubjectDID, TargetURI, Op string
	AdminActing                                       bool
	ReplacedCID, GuardCommitCID, GuardRev             string
	IdempotencyKey                                    string
	State                                             moderation.CommunityIntentState
	CommitRev                                         string
	ExpiresAt                                         time.Time
}

func summarizeIntents(intents []moderation.CommunityIntent) []intentSummary {
	out := make([]intentSummary, 0, len(intents))
	for _, intent := range intents {
		out = append(out, intentSummary{
			ActorDID: intent.ActorDID, CommunityDID: intent.CommunityDID, SubjectDID: intent.SubjectDID,
			TargetURI: intent.TargetURI, Op: intent.Op, AdminActing: intent.AdminActing,
			ReplacedCID: intent.ReplacedCID, GuardCommitCID: intent.GuardCommitCID, GuardRev: intent.GuardRev,
			IdempotencyKey: intent.IdempotencyKey, State: intent.State, CommitRev: intent.CommitRev,
			ExpiresAt: intent.ExpiresAt,
		})
	}
	return out
}

var rosterModeratorDelete = []pds.Write{{Op: pds.WriteOpDelete, Collection: "social.coves.community.moderator", RKey: rosterModeratorKey}}

func TestCommunityActionResignDeletesTheModeratorRecord(t *testing.T) {
	f := newCommunityActionFixture(t)

	result, err := f.service.ResignRole(context.Background(), moderation.ResignRoleRequest{
		ActorDID: rosterModerator, CommunityDID: rosterCommunity, ExpectedVersion: 7, IdempotencyKey: "resign-1",
	})

	require.NoError(t, err)
	assert.Equal(t, &moderation.CommunityActionResult{
		CommunityDID: rosterCommunity, SubjectDID: rosterModerator, RecordURI: rosterModeratorURI,
		ActionID: "community-action-1", Version: 8,
	}, result)
	assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey), "M's role record is gone from the community repo")
	assert.Equal(t, []rosterBatch{{writes: rosterModeratorDelete, swapCommit: rosterHeadZeroCID}}, f.repo.batchesSnapshot(),
		"one delete of M's record guarded by the head read before the write")
	assert.Empty(t, f.repo.violations, "the intent was stored, guarded by the swapCommit, when the write arrived")
	opened := f.factory.openedSnapshot()
	assert.NotEmpty(t, opened, "the community repo was opened")
	for _, did := range opened {
		assert.Equal(t, rosterCommunity, did, "the repo opened is the community's")
	}
	assert.Equal(t, []intentSummary{{
		ActorDID: rosterModerator, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
		TargetURI: rosterModeratorURI, Op: "delete",
		ReplacedCID: rosterModeratorCID, GuardCommitCID: rosterHeadZeroCID, GuardRev: rosterHeadZeroRev,
		IdempotencyKey: "resign-1", State: moderation.CommunityIntentCompleted, CommitRev: rosterHeadOneRev,
		ExpiresAt: time.Date(2026, 11, 13, 12, 10, 0, 0, time.UTC),
	}}, summarizeIntents(f.store.intentsSnapshot()))
	assert.Equal(t, []communityActionRecorded{{ActorDID: rosterModerator, CommunityDID: rosterCommunity, SubjectDID: rosterModerator}},
		f.store.actionsSnapshot())
	_, authErr := f.authorizer.Authorize(context.Background(), rosterCommunity, rosterModerator, communities.RoleModerator)
	assert.ErrorIs(t, authErr, communities.ErrUnauthorized, "M no longer holds the moderator role")
}

func TestCommunityActionRefusalsWriteNothing(t *testing.T) {
	resign := func(actor, community string, version int64, key string) func(*moderation.CommunityActionService) error {
		return func(s *moderation.CommunityActionService) error {
			_, err := s.ResignRole(context.Background(), moderation.ResignRoleRequest{
				ActorDID: actor, CommunityDID: community, ExpectedVersion: version, IdempotencyKey: key,
			})
			return err
		}
	}
	remove := func(actor, community, subject string, version int64, key string) func(*moderation.CommunityActionService) error {
		return func(s *moderation.CommunityActionService) error {
			_, err := s.RemoveModerator(context.Background(), moderation.RemoveModeratorRequest{
				ActorDID: actor, CommunityDID: community, SubjectDID: subject, ExpectedVersion: version, IdempotencyKey: key,
			})
			return err
		}
	}
	tests := []struct {
		name string
		call func(*moderation.CommunityActionService) error
		want error
	}{
		{"moderator removes another moderator", remove(rosterModerator, rosterCommunity, rosterSecondModerator, 7, "k"), moderation.ErrForbidden},
		{"owner resigns", resign(rosterOwner, rosterCommunity, 7, "k"), moderation.ErrOwnerMustTransfer},
		{"owner removes its own owner record", remove(rosterOwner, rosterCommunity, rosterOwner, 7, "k"), moderation.ErrCannotRemoveOwner},
		{"non-member resigns", resign(rosterOutsider, rosterCommunity, 7, "k"), moderation.ErrForbidden},
		{"moderator resigns in a community this instance does not host", resign(rosterModerator, rosterRemote, 0, "k"), moderation.ErrForbidden},
		{"moderator resigns in an instance-removed community", resign(rosterModerator, rosterRemoved, 0, "k"), moderation.ErrForbidden},
		{"instance admin with no role resigns", resign(rosterAdmin, rosterCommunity, 7, "k"), moderation.ErrRoleNotFound},
		{"owner removes an account with no role", remove(rosterOwner, rosterCommunity, rosterOutsider, 7, "k"), moderation.ErrRoleNotFound},
		{"empty actor", resign("", rosterCommunity, 7, "k"), moderation.ErrAuthRequired},
		{"malformed community DID", resign(rosterModerator, "not-a-did", 7, "k"), moderation.ErrInvalidRequest},
		{"malformed subject DID", remove(rosterOwner, rosterCommunity, "not-a-did", 7, "k"), moderation.ErrInvalidRequest},
		{"empty idempotency key", resign(rosterModerator, rosterCommunity, 7, ""), moderation.ErrInvalidRequest},
		{"idempotency key over 128 bytes", resign(rosterModerator, rosterCommunity, 7, strings.Repeat("k", 129)), moderation.ErrInvalidRequest},
		{"negative expected version", resign(rosterModerator, rosterCommunity, -1, "k"), moderation.ErrInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommunityActionFixture(t)

			err := tt.call(f.service)

			assert.ErrorIs(t, err, tt.want)
			f.assertNothingWritten(t)
			assert.Empty(t, f.factory.openedSnapshot(), "a refused request never opens the community repo")
		})
	}
}

func TestCommunityActionVersionAndIdempotency(t *testing.T) {
	ctx := context.Background()

	t.Run("stale expected version is a state conflict", func(t *testing.T) {
		f := newCommunityActionFixture(t)

		_, err := f.removeModerator(rosterModerator, 6, "stale-1")

		assert.ErrorIs(t, err, moderation.ErrStateConflict)
		f.assertNothingWritten(t)
	})

	t.Run("same key after success replays the result", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		first, err := f.removeModerator(rosterModerator, 7, "remove-1")
		assert.NoError(t, err)

		second, err := f.removeModerator(rosterModerator, 7, "remove-1")

		require.NoError(t, err)
		assert.Equal(t, &moderation.CommunityActionResult{
			CommunityDID: rosterCommunity, SubjectDID: rosterModerator, RecordURI: rosterModeratorURI,
			ActionID: "community-action-1", Version: 8,
		}, second)
		assert.Equal(t, first, second)
		assert.Len(t, f.repo.batchesSnapshot(), 1, "the replay writes nothing to the PDS")
		assert.Len(t, f.store.intentsSnapshot(), 1, "the replay saves no intent")
		assert.Len(t, f.store.actionsSnapshot(), 1, "the replay writes no action")
		assert.EqualValues(t, 8, f.store.version(rosterCommunity))
	})

	t.Run("same key replays to a caller authorization now refuses", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		request := moderation.ResignRoleRequest{ActorDID: rosterModerator, CommunityDID: rosterCommunity, ExpectedVersion: 7, IdempotencyKey: "resign-1"}
		_, err := f.service.ResignRole(ctx, request)
		assert.NoError(t, err)

		replay, err := f.service.ResignRole(ctx, request)

		require.NoError(t, err, "M's own stored result, not Forbidden")
		assert.Equal(t, &moderation.CommunityActionResult{
			CommunityDID: rosterCommunity, SubjectDID: rosterModerator, RecordURI: rosterModeratorURI,
			ActionID: "community-action-1", Version: 8,
		}, replay)
		assert.Len(t, f.repo.batchesSnapshot(), 1)
		assert.Len(t, f.store.actionsSnapshot(), 1)
	})

	t.Run("same key with a different subject after success conflicts", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		assert.NoError(t, err)

		_, err = f.removeModerator(rosterSecondModerator, 8, "remove-1")

		assert.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
		assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterSecondModeratorKey), "N's record kept")
		assert.True(t, f.store.hasRole(rosterCommunity, rosterSecondModeratorKey), "N's index row kept")
		assert.Len(t, f.repo.batchesSnapshot(), 1)
		assert.Len(t, f.store.actionsSnapshot(), 1)
	})

	t.Run("same key with a different subject while the intent is open conflicts before any reconcile", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		// A reconcile would now find the head moved and M's record unchanged,
		// and fail the intent.
		f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})

		_, err = f.removeModerator(rosterSecondModerator, 7, "remove-1")

		assert.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
		intents := f.store.intentsSnapshot()
		if assert.Len(t, intents, 1, "no second intent") {
			assert.Equal(t, moderation.CommunityIntentOpen, intents[0].State, "the first call's intent was not reconciled")
		}
		assert.Len(t, f.repo.batchesSnapshot(), 1, "no second write")
		assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterSecondModeratorKey))
		assert.Empty(t, f.store.actionsSnapshot())
	})
}

func TestCommunityActionPDSFailureLeavesTheIntentOpen(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.repo.applyErr = errors.New("pds: 502 bad gateway")

	_, err := f.removeModerator(rosterModerator, 7, "remove-1")

	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	assert.True(t, f.store.hasRole(rosterCommunity, rosterModeratorKey), "M's index row kept")
	assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey), "M's record kept")
	assert.EqualValues(t, 7, f.store.version(rosterCommunity))
	assert.Empty(t, f.store.actionsSnapshot())
	assert.Equal(t, []intentSummary{{
		ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
		TargetURI: rosterModeratorURI, Op: "delete",
		ReplacedCID: rosterModeratorCID, GuardCommitCID: rosterHeadZeroCID, GuardRev: rosterHeadZeroRev,
		IdempotencyKey: "remove-1", State: moderation.CommunityIntentOpen,
		ExpiresAt: time.Date(2026, 11, 13, 12, 10, 0, 0, time.UTC),
	}}, summarizeIntents(f.store.intentsSnapshot()))
}

func TestCommunityActionIndexedCIDBehindTheRepoConflicts(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedRole(rosterRole(rosterCommunity, rosterModerator, communities.RoleModerator, rosterModeratorKey,
		rosterModeratorStaleCID, time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)))

	_, err := f.removeModerator(rosterModerator, 7, "remove-1")

	assert.ErrorIs(t, err, moderation.ErrStateConflict)
	assert.Empty(t, f.repo.batchesSnapshot(), "no PDS write")
	assert.Empty(t, f.store.intentsSnapshot(), "no intent saved")
	assert.Empty(t, f.store.actionsSnapshot(), "no action written")
	assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey))
	assert.True(t, f.store.hasRole(rosterCommunity, rosterModeratorKey))
}

func TestCommunityActionReconcilesAnOpenIntent(t *testing.T) {
	t.Run("retry with the head unchanged re-attempts under the original guard", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)

		result, err := f.removeModerator(rosterModerator, 7, "remove-1")

		require.NoError(t, err)
		assert.Equal(t, &moderation.CommunityActionResult{
			CommunityDID: rosterCommunity, SubjectDID: rosterModerator, RecordURI: rosterModeratorURI,
			ActionID: "community-action-1", Version: 8,
		}, result)
		batches := f.repo.batchesSnapshot()
		if assert.Len(t, batches, 2) {
			assert.Equal(t, rosterHeadZeroCID, batches[1].swapCommit, "the retry passes the stored guard commit CID")
		}
		intents := f.store.intentsSnapshot()
		if assert.Len(t, intents, 1, "the retry reuses the original intent") {
			assert.Equal(t, moderation.CommunityIntentCompleted, intents[0].State)
			assert.Equal(t, rosterHeadOneRev, intents[0].CommitRev)
		}
		assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey))
		assert.Empty(t, f.repo.violations)
	})

	t.Run("head moved with the replaced record still present fails the intent", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})

		_, err = f.removeModerator(rosterModerator, 7, "remove-1")

		assert.ErrorIs(t, err, moderation.ErrStateConflict)
		intents := f.store.intentsSnapshot()
		if assert.Len(t, intents, 1) {
			assert.Equal(t, moderation.CommunityIntentFailed, intents[0].State)
		}
		assert.Len(t, f.repo.batchesSnapshot(), 1, "no second delete")
		assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey))
		assert.True(t, f.store.hasRole(rosterCommunity, rosterModeratorKey))
		assert.EqualValues(t, 7, f.store.version(rosterCommunity))
		assert.Empty(t, f.store.actionsSnapshot())
	})

	t.Run("record read failure is unavailable, not role not found", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.getRecordErr = errors.New("pds: 500 internal server error")

		_, err := f.removeModerator(rosterModerator, 7, "remove-1")

		assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		assert.NotErrorIs(t, err, moderation.ErrRoleNotFound)
		f.assertNothingWritten(t)
	})

	t.Run("retry refused by a late commit of the original write completes the intent", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		// The original delete commits after the retry read the unchanged head
		// and before the retry's write reaches the PDS.
		f.repo.afterHeadRead = func(r *fakeRosterRepo) {
			delete(r.records, communities.RoleCollection+"/"+rosterModeratorKey)
			r.head = r.commitHead
		}

		result, err := f.removeModerator(rosterModerator, 7, "remove-1")

		require.NoError(t, err, "the original write landed")
		assert.Equal(t, &moderation.CommunityActionResult{
			CommunityDID: rosterCommunity, SubjectDID: rosterModerator, RecordURI: rosterModeratorURI,
			ActionID: "community-action-1", Version: 8,
		}, result)
		intents := f.store.intentsSnapshot()
		if assert.Len(t, intents, 1) {
			assert.Equal(t, moderation.CommunityIntentCompleted, intents[0].State)
			assert.Equal(t, rosterHeadOneRev, intents[0].CommitRev)
		}
		assert.Equal(t, []communityActionRecorded{{ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator}},
			f.store.actionsSnapshot())
		assert.False(t, f.store.hasRole(rosterCommunity, rosterModeratorKey), "M's index row dropped")
	})

	t.Run("retry refused by another writer with the record kept fails the intent", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		f.repo.afterHeadRead = func(r *fakeRosterRepo) {
			r.head = pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev}
		}

		_, err = f.removeModerator(rosterModerator, 7, "remove-1")

		assert.ErrorIs(t, err, moderation.ErrStateConflict)
		intents := f.store.intentsSnapshot()
		if assert.Len(t, intents, 1) {
			assert.Equal(t, moderation.CommunityIntentFailed, intents[0].State)
		}
		assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey))
		assert.True(t, f.store.hasRole(rosterCommunity, rosterModeratorKey))
		assert.Empty(t, f.store.actionsSnapshot())
	})

	t.Run("lost response completes at the head rev read before the record", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.applyThenErr = errors.New("pds: connection reset by peer")
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		// On the retry, another commit lands between the head read and the
		// record read.
		f.repo.afterHeadRead = func(r *fakeRosterRepo) {
			r.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
		}

		result, err := f.removeModerator(rosterModerator, 7, "remove-1")

		require.NoError(t, err)
		assert.Equal(t, &moderation.CommunityActionResult{
			CommunityDID: rosterCommunity, SubjectDID: rosterModerator, RecordURI: rosterModeratorURI,
			ActionID: "community-action-1", Version: 8,
		}, result)
		intents := f.store.intentsSnapshot()
		if assert.Len(t, intents, 1) {
			assert.Equal(t, moderation.CommunityIntentCompleted, intents[0].State)
			assert.Equal(t, rosterHeadOneRev, intents[0].CommitRev, "the head rev read before the record read")
		}
		assert.Equal(t, []communityActionRecorded{{ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator}},
			f.store.actionsSnapshot())
		assert.Len(t, f.repo.batchesSnapshot(), 1, "no second delete")
		assert.False(t, f.store.hasRole(rosterCommunity, rosterModeratorKey), "M's index row dropped")
	})
}

func TestNewCommunityActionServiceRequiresDependencies(t *testing.T) {
	f := newCommunityActionFixture(t)
	valid := moderation.CommunityActionConfig{
		Authorizer:     f.authorizer,
		Store:          f.store,
		Repos:          f.factory.open,
		Now:            func() time.Time { return rosterClockStart },
		IntentTTL:      10 * time.Minute,
		IdempotencyTTL: 24 * time.Hour,
	}
	tests := []struct {
		name   string
		mutate func(*moderation.CommunityActionConfig)
	}{
		{"nil authorizer", func(c *moderation.CommunityActionConfig) { c.Authorizer = nil }},
		{"nil store", func(c *moderation.CommunityActionConfig) { c.Store = nil }},
		{"nil repos", func(c *moderation.CommunityActionConfig) { c.Repos = nil }},
		{"nil clock", func(c *moderation.CommunityActionConfig) { c.Now = nil }},
		{"zero intent TTL", func(c *moderation.CommunityActionConfig) { c.IntentTTL = 0 }},
		{"negative idempotency TTL", func(c *moderation.CommunityActionConfig) { c.IdempotencyTTL = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			assert.Panics(t, func() { moderation.NewCommunityActionService(cfg) })
		})
	}
}

// An open intent of another key on the target record is settled under the
// locks before the target is read, so it neither blocks the request nor
// leaves a committed removal unapplied.
func TestCommunityActionOpenIntentOfAnotherKey(t *testing.T) {
	ctx := context.Background()

	t.Run("an expired uncommitted write no longer blocks another key", func(t *testing.T) {
		for _, test := range []struct {
			name      string
			call      func(f *communityActionFixture) (*moderation.CommunityActionResult, error)
			wantActor string
			wantAdmin bool
		}{
			{"the owner with a fresh key", func(f *communityActionFixture) (*moderation.CommunityActionResult, error) {
				return f.removeModerator(rosterModerator, 7, "remove-2")
			}, rosterOwner, false},
			{"an instance admin", func(f *communityActionFixture) (*moderation.CommunityActionResult, error) {
				return f.service.RemoveModerator(ctx, moderation.RemoveModeratorRequest{
					ActorDID: rosterAdmin, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
					ExpectedVersion: 7, IdempotencyKey: "admin-1",
				})
			}, rosterAdmin, true},
			{"the moderator's own resign", func(f *communityActionFixture) (*moderation.CommunityActionResult, error) {
				return f.service.ResignRole(ctx, moderation.ResignRoleRequest{
					ActorDID: rosterModerator, CommunityDID: rosterCommunity, ExpectedVersion: 7, IdempotencyKey: "resign-1",
				})
			}, rosterModerator, false},
		} {
			t.Run(test.name, func(t *testing.T) {
				f := newCommunityActionFixture(t)
				f.repo.applyErr = errors.New("pds: 502 bad gateway")
				_, err := f.removeModerator(rosterModerator, 7, "remove-1")
				require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
				f.store.expireIntents(rosterClockStart)

				result, err := test.call(f)

				require.NoError(t, err, "the open intent of another key does not block the request")
				if assert.NotNil(t, result) {
					assert.EqualValues(t, 8, result.Version)
				}
				intents := f.store.intentsSnapshot()
				if assert.Len(t, intents, 2) {
					assert.Equal(t, moderation.CommunityIntentExpired, intents[0].State, "the uncommitted intent is settled as expired")
					assert.Equal(t, moderation.CommunityIntentCompleted, intents[1].State)
				}
				assert.Equal(t, []communityActionRecorded{{
					ActorDID: test.wantActor, AdminActing: test.wantAdmin,
					CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
				}}, f.store.actionsSnapshot())
				assert.False(t, f.store.hasRole(rosterCommunity, rosterModeratorKey))
				assert.Empty(t, f.repo.violations)
			})
		}
	})

	t.Run("an unexpired write with an unknown outcome refuses another key", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.applyErr = errors.New("pds: 502 bad gateway")
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
		// O's original delete commits on the PDS only after the admin's request
		// has read the unchanged head.
		f.repo.afterHeadRead = func(r *fakeRosterRepo) {
			delete(r.records, communities.RoleCollection+"/"+rosterModeratorKey)
			r.head = r.commitHead
		}
		adminRemoval := moderation.RemoveModeratorRequest{
			ActorDID: rosterAdmin, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
			ExpectedVersion: 7, IdempotencyKey: "admin-1",
		}

		_, err = f.service.RemoveModerator(ctx, adminRemoval)

		assert.ErrorIs(t, err, moderation.ErrStateConflict, "O's write may still commit")
		intents := f.store.intentsSnapshot()
		if assert.Len(t, intents, 1, "the admin's request saves no intent") {
			assert.Equal(t, moderation.CommunityIntentOpen, intents[0].State, "O's intent stays open until its outcome is known")
		}
		assert.Len(t, f.repo.batchesSnapshot(), 1, "no second delete")
		assert.Empty(t, f.store.actionsSnapshot())

		_, err = f.service.RemoveModerator(ctx, adminRemoval)

		assert.ErrorIs(t, err, moderation.ErrRoleNotFound, "O's late write removed the role")
		assert.Equal(t, []communityActionRecorded{{ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator}},
			f.store.actionsSnapshot(), "one action naming O, whose write landed late")
		intents = f.store.intentsSnapshot()
		if assert.Len(t, intents, 1) {
			assert.Equal(t, moderation.CommunityIntentCompleted, intents[0].State)
		}
		assert.False(t, f.store.hasRole(rosterCommunity, rosterModeratorKey))
		assert.Empty(t, f.repo.violations)
	})

	t.Run("a committed write with a lost response completes for its own actor", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.repo.applyThenErr = errors.New("pds: connection reset by peer")
		_, err := f.removeModerator(rosterModerator, 7, "remove-1")
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable)

		_, err = f.service.RemoveModerator(ctx, moderation.RemoveModeratorRequest{
			ActorDID: rosterAdmin, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
			ExpectedVersion: 7, IdempotencyKey: "admin-1",
		})

		assert.ErrorIs(t, err, moderation.ErrRoleNotFound, "the role is already removed")
		assert.False(t, f.store.hasRole(rosterCommunity, rosterModeratorKey), "the removal that landed is applied to the index")
		assert.EqualValues(t, 8, f.store.version(rosterCommunity))
		assert.Equal(t, []communityActionRecorded{{ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator}},
			f.store.actionsSnapshot(), "one action naming O, whose write landed")
		intents := f.store.intentsSnapshot()
		if assert.Len(t, intents, 1, "the admin's request saves no intent") {
			assert.Equal(t, moderation.CommunityIntentCompleted, intents[0].State)
		}
		assert.Len(t, f.repo.batchesSnapshot(), 1, "no second delete")
		_, authErr := f.authorizer.Authorize(ctx, rosterCommunity, rosterModerator, communities.RoleModerator)
		assert.ErrorIs(t, authErr, communities.ErrUnauthorized, "M no longer holds the moderator role")
	})

	t.Run("an indexed role whose record is gone with no intent is a conflict", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		delete(f.repo.records, communities.RoleCollection+"/"+rosterModeratorKey)

		_, err := f.removeModerator(rosterModerator, 7, "remove-1")

		assert.ErrorIs(t, err, moderation.ErrStateConflict)
		assert.NotErrorIs(t, err, moderation.ErrRoleNotFound)
		assert.Empty(t, f.repo.batchesSnapshot(), "no PDS write")
		assert.Empty(t, f.store.intentsSnapshot(), "no intent saved")
		assert.Empty(t, f.store.actionsSnapshot(), "no action written")
	})
}

// A delete refused for a moved head (swapCommit) committed nothing: the
// intent fails and the caller gets a conflict, and a new request proceeds.
func TestCommunityActionLostSwapIsAConflict(t *testing.T) {
	f := newCommunityActionFixture(t)
	// Another writer moves the head between the head read and the delete.
	f.repo.afterHeadRead = func(r *fakeRosterRepo) {
		r.head = pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev}
	}

	_, err := f.removeModerator(rosterModerator, 7, "remove-1")

	assert.ErrorIs(t, err, moderation.ErrStateConflict)
	assert.NotErrorIs(t, err, moderation.ErrModerationUnavailable)
	intents := f.store.intentsSnapshot()
	if assert.Len(t, intents, 1) {
		assert.Equal(t, moderation.CommunityIntentFailed, intents[0].State, "the refused write's intent is failed")
	}
	assert.True(t, f.store.hasRole(rosterCommunity, rosterModeratorKey))
	assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey))
	assert.Empty(t, f.store.actionsSnapshot())

	result, err := f.removeModerator(rosterModerator, 7, "remove-2")

	require.NoError(t, err, "nothing blocks the next request")
	if assert.NotNil(t, result) {
		assert.EqualValues(t, 8, result.Version)
	}
	assert.False(t, f.store.hasRole(rosterCommunity, rosterModeratorKey))
}
