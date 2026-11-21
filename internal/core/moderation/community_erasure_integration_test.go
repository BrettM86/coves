//go:build integration

package moderation_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Account erasure against the community roster: an erased owner leaves the
// community ownerless without promoting anyone, and an erased moderator's
// record is deleted from the community repo by the role record delete queue,
// after which the firehose copy of that delete changes nothing.

func (r *fakeRosterRepo) guardedDeletesSnapshot() []rosterGuardedDelete {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rosterGuardedDelete(nil), r.guardedDeletes...)
}

// eraseAccount indexes did as a user and erases it through the production
// user repository.
func (f *communityActionDBFixture) eraseAccount(t *testing.T, did, handle string) {
	t.Helper()
	f.indexUser(t, did, handle)
	require.NoError(t, postgres.NewUserRepository(f.db).Delete(context.Background(), did), "erase %s", did)
}

// roleRecordDeleteQueue is the production queue over the fixture's database,
// deleting through the fixture's community repo. Its clock runs a minute past
// the wall clock, so rows the erasure queued at the database's now are due.
func (f *communityActionDBFixture) roleRecordDeleteQueue() *moderation.RoleRecordDeleteQueue {
	deleters := func(ctx context.Context, communityDID string) (pds.GuardedDeleter, error) {
		repo, err := f.factory.open(ctx, communityDID)
		if err != nil {
			return nil, err
		}
		deleter, ok := repo.(pds.GuardedDeleter)
		if !ok {
			return nil, fmt.Errorf("the community repo for %s has no guarded delete", communityDID)
		}
		return deleter, nil
	}
	return moderation.NewRoleRecordDeleteQueue(postgres.NewRoleRecordDeleteRepository(f.db), deleters,
		moderation.RoleRecordDeleteQueueConfig{
			Now:            func() time.Time { return time.Now().UTC().Add(time.Minute) },
			AttemptTimeout: 5 * time.Second,
		})
}

// B5: the owner of hosted rosteralpha is erased. The roster has no owner and
// M is still a moderator; M gains no owner authority (the owner-level
// settings update and InviteModerator are refused); an instance admin can
// offer ownership to M.
func TestCommunityErasureDB_ErasedOwnerLeavesTheCommunityOwnerless(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	f.indexUser(t, rosterModerator, "rostermod.test")

	f.eraseAccount(t, rosterOwner, "rosterowner.test")

	roster, err := communities.ReadRoster(ctx, postgres.NewCommunityRoleRepository(f.db), rosterCommunity)
	require.NoError(t, err)
	require.Nil(t, roster.Owner, "the erased owner no longer owns the community")
	require.Len(t, roster.Moderators, 1)
	assert.Equal(t, rosterModerator, roster.Moderators[0].SubjectDID, "M is still a moderator")
	assert.Equal(t, communities.RoleModerator, roster.Moderators[0].Role)

	// UpdateCommunity (communities/service.go) requires the owner role.
	_, err = f.authorizer.Authorize(ctx, rosterCommunity, rosterModerator, communities.RoleOwner)
	assert.ErrorIs(t, err, communities.ErrUnauthorized, "M is not promoted to owner-level authority")

	_, err = f.invitations.InviteModerator(ctx, moderation.InviteRequest{
		ActorDID: rosterModerator, CommunityDID: rosterCommunity, Invitee: "rosteroutsider.test",
		ExpectedVersion: 8, IdempotencyKey: "invite-1",
	})
	assert.ErrorIs(t, err, moderation.ErrForbidden, "M cannot invite moderators in an ownerless community")

	result, err := f.invitations.OfferOwnership(ctx, moderation.InviteRequest{
		ActorDID: rosterAdmin, CommunityDID: rosterCommunity, Invitee: "rostermod.test",
		ExpectedVersion: 8, IdempotencyKey: "offer-1",
	})
	require.NoError(t, err, "an instance admin offers ownership of the ownerless community")
	require.NotNil(t, result)
	listed, err := f.invitations.ListCommunityInvitations(ctx, rosterAdmin, rosterCommunity)
	require.NoError(t, err)
	assert.Equal(t, []communities.Invitation{{
		ID: result.InvitationID, CommunityDID: rosterCommunity, InviteeDID: rosterModerator,
		Role: communities.RoleOwner, InviterDID: rosterAdmin, InviterAdmin: true, State: communities.InvitationOpen,
		CreatedAt: rosterClockStart, ExpiresAt: rosterInvitationExpiresFromClockStart,
	}}, utcInvitations(listed), "one open ownership offer to M")
}

// B7: moderator M is erased (roster version 7 to 8). The queue deletes M's
// record from the community repo under a swap guard on the indexed CID. The
// firehose copy of that delete then changes no row, no version and logs no
// action.
func TestCommunityErasureDB_DeleteCopyAfterTheSweepChangesNothing(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()

	f.eraseAccount(t, rosterModerator, "rostermod.test")

	require.False(t, f.moderatorRowIndexed(t), "checkpoint: the erasure removed M's role row")
	require.EqualValues(t, 8, f.rosterVersion(t), "checkpoint: the erasure advanced the roster version once")

	queue := f.roleRecordDeleteQueue()
	require.NoError(t, queue.Sweep(ctx))

	assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey),
		"the sweep deleted M's record from the community repo")
	assert.Equal(t, []rosterGuardedDelete{{
		Collection: "social.coves.community.moderator", RKey: rosterModeratorKey, SwapRecord: rosterModeratorCID,
	}}, f.repo.guardedDeletesSnapshot(), "one delete guarded on the erased index row's CID")
	assert.True(t, f.repo.hasRecord(communities.RoleCollection, rosterOwnerKey), "the owner's record stays")
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM community_role_record_deletes`).Scan(&pending))
	assert.Zero(t, pending, "the finished delete leaves no queue row")

	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadOneRev)))

	assert.Equal(t, map[string]indexedRole{
		rosterOwner: {Role: communities.RoleOwner, CID: rosterOwnerCID},
	}, f.indexedRoles(t), "the delete copy changes no role row")
	assert.EqualValues(t, 8, f.rosterVersion(t), "the delete copy does not advance the roster version")
	assert.Empty(t, f.actions(t), "the delete copy logs no moderation action")
}

// pendingRoleRecordDelete is one community_role_record_deletes row's target.
type pendingRoleRecordDelete struct {
	CommunityDID, RecordKey, RecordCID string
}

// pendingRoleRecordDeletes is every queued role record delete in the test's
// database, by community and record key.
func (f *communityActionDBFixture) pendingRoleRecordDeletes(t *testing.T) []pendingRoleRecordDelete {
	t.Helper()
	rows, err := f.db.Query(`SELECT community_did, record_key, record_cid FROM community_role_record_deletes
		ORDER BY community_did, record_key`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []pendingRoleRecordDelete{}
	for rows.Next() {
		var d pendingRoleRecordDelete
		require.NoError(t, rows.Scan(&d.CommunityDID, &d.RecordKey, &d.RecordCID))
		out = append(out, d)
	}
	require.NoError(t, rows.Err())
	return out
}

// reinstate removes did's erasure marker, as a re-signup does.
func (f *communityActionDBFixture) reinstate(t *testing.T, did string) {
	t.Helper()
	store, ok := postgres.NewUserRepository(f.db).(users.ErasureMarkerStore)
	require.True(t, ok, "the postgres user repository clears erasure markers")
	reinstated, err := store.ReinstateAccount(context.Background(), did)
	require.NoError(t, err)
	require.True(t, reinstated, "fixture: %s carried an erasure marker", did)
}

// B6 (DoD: re-signup before the sweep): M is erased, leaving its record (CID
// A, rosterModeratorCID) in the community repo, unindexed, with a delete of
// CID A queued. M signs up again and accepts a new moderator invitation before
// the sweep: the accept updates the record at M's key (CID B,
// rosterModeratorWrittenCID) and indexes it. The sweep's delete, guarded on
// CID A, then conflicts: the record keeps CID B, M stays indexed and the
// queue row is gone.
func TestCommunityErasureDB_ReSignupAcceptsBeforeTheSweep(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()

	f.eraseAccount(t, rosterModerator, "rostermod.test")

	require.False(t, f.moderatorRowIndexed(t), "checkpoint: the erasure removed M's index row")
	require.True(t, f.repo.hasRecord(communities.RoleCollection, rosterModeratorKey),
		"checkpoint: M's record is still in the community repo")
	require.EqualValues(t, 8, f.rosterVersion(t), "checkpoint: the erasure advanced the roster version once")
	require.Equal(t, []pendingRoleRecordDelete{{rosterCommunity, rosterModeratorKey, rosterModeratorCID}},
		f.pendingRoleRecordDeletes(t), "checkpoint: a delete of CID A is queued")

	f.reinstate(t, rosterModerator)
	f.indexUser(t, rosterModerator, "rostermod.test")
	f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, rosterModerator, communities.RoleModerator, rosterOwner, false))

	result, err := f.acceptInvitation(rosterModerator, rosterModeratorInvitationID, 8, "accept-1")

	require.NoError(t, err, "the re-signed-up account accepts over its own erased, delete-pending record")
	if assert.NotNil(t, result) {
		assert.EqualValues(t, 9, result.Version)
	}
	batches := f.repo.batchesSnapshot()
	if assert.Len(t, batches, 1, "one PDS commit: the accept") && assert.Len(t, batches[0].writes, 1) {
		assert.Equal(t, pds.WriteOpUpdate, batches[0].writes[0].Op, "the accept replaces the record at M's key")
		assert.Equal(t, rosterModeratorKey, batches[0].writes[0].RKey)
		assert.Equal(t, rosterHeadZeroCID, batches[0].swapCommit, "the write is guarded on the head it read")
	}
	record, err := f.repo.GetRecord(ctx, communities.RoleCollection, rosterModeratorKey)
	require.NoError(t, err)
	assert.Equal(t, rosterModeratorWrittenCID, record.CID, "the community repo holds CID B")
	wantRoles := map[string]indexedRole{
		rosterOwner:     {Role: communities.RoleOwner, CID: rosterOwnerCID},
		rosterModerator: {Role: communities.RoleModerator, CID: rosterModeratorWrittenCID},
	}
	assert.Equal(t, wantRoles, f.indexedRoles(t), "M is indexed with CID B")
	assert.EqualValues(t, 9, f.rosterVersion(t))
	assert.Equal(t, "accepted", f.invitationState(t, rosterModeratorInvitationID))

	require.NoError(t, f.roleRecordDeleteQueue().Sweep(ctx))

	assert.Equal(t, []rosterGuardedDelete{{
		Collection: "social.coves.community.moderator", RKey: rosterModeratorKey, SwapRecord: rosterModeratorCID,
	}}, f.repo.guardedDeletesSnapshot(), "the sweep's delete is guarded on CID A")
	record, err = f.repo.GetRecord(ctx, communities.RoleCollection, rosterModeratorKey)
	require.NoError(t, err, "the swap conflict leaves M's record in the community repo")
	assert.Equal(t, rosterModeratorWrittenCID, record.CID, "the record keeps CID B")
	assert.Empty(t, f.pendingRoleRecordDeletes(t), "the conflicted delete leaves no queue row")
	assert.Equal(t, wantRoles, f.indexedRoles(t), "M stays indexed with CID B")
	assert.EqualValues(t, 9, f.rosterVersion(t), "the sweep changes no roster version")
}

// Guard for the re-signup rule: an accept over an unindexed record at the
// invitee's key is refused with StateConflict, writing nothing, unless a
// queued delete carries exactly that record's CID.
func TestCommunityErasureDB_AcceptRefusesAnUnindexedRecordWithoutItsQueuedDelete(t *testing.T) {
	t.Parallel()
	const (
		strayCID  = "bafyreiaoutsiderstrayaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		queuedCID = "bafyreiaoutsiderqueuedaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	tests := []struct {
		name  string
		setup func(t *testing.T, f *communityActionDBFixture)
	}{
		{name: "no queued delete", setup: func(*testing.T, *communityActionDBFixture) {}},
		{
			name: "a queued delete for another CID",
			setup: func(t *testing.T, f *communityActionDBFixture) {
				_, err := f.db.Exec(`INSERT INTO community_role_record_deletes (community_did, record_key, record_cid)
					VALUES ($1, $2, $3)`, rosterCommunity, rosterOutsiderKey, queuedCID)
				require.NoError(t, err)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			f.repo.mu.Lock()
			f.repo.setRecord(communities.RoleCollection, rosterOutsiderKey, strayCID)
			f.repo.mu.Unlock()
			tc.setup(t, f)
			f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

			result, err := f.acceptInvitation(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

			assert.ErrorIs(t, err, moderation.ErrStateConflict, "the unindexed record refuses the accept")
			assert.Nil(t, result)
			assert.Empty(t, f.repo.batchesSnapshot(), "nothing is written to the community repo")
			record, err := f.repo.GetRecord(context.Background(), communities.RoleCollection, rosterOutsiderKey)
			require.NoError(t, err)
			assert.Equal(t, strayCID, record.CID, "the unindexed record is untouched")
			assert.Equal(t, seededRoles(), f.indexedRoles(t), "the role index is unchanged")
			assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
			assert.Equal(t, "open", f.invitationState(t, rosterModeratorInvitationID), "the invitation stays open")
		})
	}
}

// B11 (R2): a role write whose PDS commit landed but whose local completion
// had not run when its subject was erased is completed without indexing the
// erased subject, and the record it wrote is queued for deletion. The accept's
// response is lost (its intent stays open), the subject is erased, and the
// sweep or the firehose copy then completes the write. An intent whose target
// subject was erased ends failed, logging no action naming that subject and
// saving no idempotency record for it. The same holds when the erased account
// signs up again (its erasure marker cleared) before the completion: a
// returning account starts with no roles.
func TestCommunityErasureDB_RoleWriteCompletedAfterTheSubjectsErasure(t *testing.T) {
	t.Parallel()
	sweep := func(t *testing.T, f *communityActionDBFixture) {
		f.pastIntentExpiry()
		swept, err := f.service.SweepExpiredIntents(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 1, swept, "the sweep reconciles the one expired intent")
	}
	tests := []struct {
		name               string
		invitation         communities.Invitation
		erased, handle     string
		reinstate          bool
		complete           func(t *testing.T, f *communityActionDBFixture)
		wantRoles          map[string]indexedRole
		wantVersion        int64
		wantState          string
		wantPendingDeletes []pendingRoleRecordDelete
	}{
		{
			name:       "(a) moderator accept completed by the sweep",
			invitation: rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			erased:     rosterOutsider, handle: "rosteroutsider.test",
			complete:           sweep,
			wantRoles:          seededRoles(),
			wantVersion:        7,
			wantState:          "failed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOutsiderKey, rosterOutsiderModeratorBodyCID}},
		},
		{
			name:       "(b) moderator accept completed by its firehose copy",
			invitation: rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			erased:     rosterOutsider, handle: "rosteroutsider.test",
			complete: func(t *testing.T, f *communityActionDBFixture) {
				require.NoError(t, f.deliver(outsiderGrantCopy(rosterHeadOneRev)))
			},
			wantRoles:          seededRoles(),
			wantVersion:        7,
			wantState:          "failed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOutsiderKey, rosterOutsiderModeratorBodyCID}},
		},
		{
			// The erasure removes P's owner row (version 8) and queues CID
			// rosterOwnerCID; the transfer's demotion write replaced it.
			name:       "(c) ownership transfer whose paired write demotes the erased owner, completed by the sweep",
			invitation: rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false),
			erased:     rosterOwner, handle: "rosterowner.test",
			complete: sweep,
			wantRoles: map[string]indexedRole{
				rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorOwnerBodyCID},
			},
			wantVersion:        9,
			wantState:          "completed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOwnerKey, rosterOwnerModeratorBodyCID}},
		},
		{
			// The firehose copy of M's promotion completes the transfer, so
			// the erased owner's paired demotion is gated and not indexed.
			name:       "(c2) ownership transfer whose paired write demotes the erased owner, completed by the promotion's firehose copy",
			invitation: rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false),
			erased:     rosterOwner, handle: "rosterowner.test",
			complete: func(t *testing.T, f *communityActionDBFixture) {
				require.NoError(t, f.deliver(moderatorPromotionCopy(rosterHeadOneRev)))
			},
			wantRoles: map[string]indexedRole{
				rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorOwnerBodyCID},
			},
			wantVersion:        9,
			wantState:          "completed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOwnerKey, rosterOwnerModeratorBodyCID}},
		},
		{
			name:       "(e) moderator accept, the invitee signs up again, then the sweep",
			invitation: rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			erased:     rosterOutsider, handle: "rosteroutsider.test",
			reinstate:          true,
			complete:           sweep,
			wantRoles:          seededRoles(),
			wantVersion:        7,
			wantState:          "failed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOutsiderKey, rosterOutsiderModeratorBodyCID}},
		},
		{
			// The sweep stamps the record's gate at the intent's guard rev,
			// before the write's rev, so the delayed firehose copy of the
			// create still claims the gate after the intent settled.
			name:       "(e2) moderator accept, the invitee signs up again, the sweep, then the create's delayed firehose copy",
			invitation: rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			erased:     rosterOutsider, handle: "rosteroutsider.test",
			reinstate: true,
			complete: func(t *testing.T, f *communityActionDBFixture) {
				sweep(t, f)
				require.NoError(t, f.deliver(outsiderGrantCopy(rosterHeadOneRev)))
			},
			wantRoles:          seededRoles(),
			wantVersion:        7,
			wantState:          "failed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOutsiderKey, rosterOutsiderModeratorBodyCID}},
		},
		{
			name:       "(c3) ownership transfer, the erased previous owner signs up again, the sweep, then the demotion's delayed firehose copy",
			invitation: rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false),
			erased:     rosterOwner, handle: "rosterowner.test",
			reinstate: true,
			complete: func(t *testing.T, f *communityActionDBFixture) {
				sweep(t, f)
				require.NoError(t, f.deliver(ownerDemotionCopy(rosterHeadOneRev)))
			},
			wantRoles: map[string]indexedRole{
				rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorOwnerBodyCID},
			},
			wantVersion:        9,
			wantState:          "completed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOwnerKey, rosterOwnerModeratorBodyCID}},
		},
		{
			name:       "(f) moderator accept, the invitee signs up again, then its firehose copy",
			invitation: rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			erased:     rosterOutsider, handle: "rosteroutsider.test",
			reinstate: true,
			complete: func(t *testing.T, f *communityActionDBFixture) {
				require.NoError(t, f.deliver(outsiderGrantCopy(rosterHeadOneRev)))
			},
			wantRoles:          seededRoles(),
			wantVersion:        7,
			wantState:          "failed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOutsiderKey, rosterOutsiderModeratorBodyCID}},
		},
		{
			name:       "(g) ownership transfer, the erased previous owner signs up again, then the demotion's firehose copy",
			invitation: rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false),
			erased:     rosterOwner, handle: "rosterowner.test",
			reinstate: true,
			complete: func(t *testing.T, f *communityActionDBFixture) {
				require.NoError(t, f.deliver(ownerDemotionCopy(rosterHeadOneRev)))
			},
			wantRoles: map[string]indexedRole{
				rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorOwnerBodyCID},
			},
			wantVersion:        9,
			wantState:          "completed",
			wantPendingDeletes: []pendingRoleRecordDelete{{rosterCommunity, rosterOwnerKey, rosterOwnerModeratorBodyCID}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			f.writeBodyCIDs()
			f.loseAcceptResponse(t, tc.invitation, "accept-1")
			require.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "fixture: the accept's intent is open")

			f.eraseAccount(t, tc.erased, tc.handle)
			if tc.reinstate {
				f.reinstate(t, tc.erased)
				f.indexUser(t, tc.erased, tc.handle)
			}
			tc.complete(t, f)

			assert.Equal(t, tc.wantRoles, f.indexedRoles(t), "the erased subject is not indexed")
			intents := f.intents(t)
			if assert.Len(t, intents, 1) {
				assert.Equal(t, tc.wantState, intents[0].State, "the write's intent is settled")
			}
			for _, action := range f.actions(t) {
				assert.NotEqual(t, moderation.AccountSubjectURI(tc.erased), action.SubjectURI,
					"no action names the erased subject: %+v", action)
			}
			var keys int
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM moderation_idempotency_keys WHERE actor_did = $1`,
				tc.erased).Scan(&keys))
			assert.Zero(t, keys, "no idempotency record is saved for the erased account")
			assert.EqualValues(t, tc.wantVersion, f.rosterVersion(t), "no roster version for the erased subject")
			assert.Equal(t, tc.wantPendingDeletes, f.pendingRoleRecordDeletes(t),
				"the record written for the erased subject is queued for deletion at its written CID")
		})
	}
}

// B11 (e): the erasure mark on a swept accept's record holds only for that
// record at its written CID. After the sweep settles the intent and the
// returning invitee is not indexed, a later write of a different record body
// at the same key (CID B) is indexed as usual.
func TestCommunityErasureDB_ErasureMarkSparesALaterRecordAtANewCID(t *testing.T) {
	t.Parallel()
	// The outsider as moderator, createdAt "2026-11-13T12:30:00.000Z",
	// computed as rosterOutsiderModeratorBodyCID is.
	const laterOutsiderModeratorBodyCID = "bafyreibvtr5ox4p3bsub6xyomwf7wgadbzpnntuktt2uodrqnzqonvt66e"
	f := newCommunityActionDBFixture(t)
	f.writeBodyCIDs()
	f.loseAcceptResponse(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false), "accept-1")
	f.eraseAccount(t, rosterOutsider, "rosteroutsider.test")
	f.reinstate(t, rosterOutsider)
	f.indexUser(t, rosterOutsider, "rosteroutsider.test")
	f.pastIntentExpiry()
	swept, err := f.service.SweepExpiredIntents(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, swept, "fixture: the sweep reconciles the one expired intent")
	require.NoError(t, f.deliver(outsiderGrantCopy(rosterHeadOneRev)))
	require.Equal(t, seededRoles(), f.indexedRoles(t), "fixture: the marked record is not indexed")

	require.NoError(t, f.deliver(roleWriteEvent(rosterCommunity, "update", rosterOutsiderKey, rosterHeadTwoRev,
		laterOutsiderModeratorBodyCID, roleBody(rosterOutsider, communities.RoleModerator, "2026-11-13T12:30:00.000Z"))))

	want := seededRoles()
	want[rosterOutsider] = indexedRole{Role: communities.RoleModerator, CID: laterOutsiderModeratorBodyCID}
	assert.Equal(t, want, f.indexedRoles(t), "the record at the new CID is indexed")
	assert.EqualValues(t, 8, f.rosterVersion(t), "the new role advances the roster version once")
}

// B11 (d), R9: provisioning's local owner index for a creator erased before
// it runs writes no owner row and no version, and queues the written owner
// record of the hosted community for deletion.
func TestCommunityErasureDB_ProvisioningIndexAfterTheCreatorsErasure(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	insertRosterCommunity(t, f.db, rosterBeta, "rosterbeta.test", true)
	_, err := f.db.Exec(`INSERT INTO community_subject_versions (community_did, subject, version) VALUES ($1, $1, 3)`, rosterBeta)
	require.NoError(t, err)
	f.eraseAccount(t, rosterOutsider, "rosteroutsider.test")

	applied, err := postgres.NewCommunityRoleRepository(f.db).UpsertCommunityRoleAtRev(ctx, communities.CommunityRole{
		CommunityDID: rosterBeta, SubjectDID: rosterOutsider, Role: communities.RoleOwner,
		RecordKey: rosterOutsiderKey, RecordCID: rosterBetaOwnerCID,
		RecordCreatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
	}, rosterHeadOneRev)

	require.NoError(t, err)
	assert.False(t, applied, "the erased creator's owner role is not applied")
	assert.False(t, f.roleIndexed(t, rosterBeta, rosterOutsider), "no owner row for the erased creator")
	state, err := postgres.NewCommunitySubjectStateReader(f.db).ReadCommunitySubjectState(ctx, rosterBeta, rosterBeta,
		moderation.CommunitySubjectRoster)
	require.NoError(t, err)
	assert.EqualValues(t, 3, state.Version, "roster version unchanged")
	assert.Equal(t, []pendingRoleRecordDelete{{rosterBeta, rosterOutsiderKey, rosterBetaOwnerCID}},
		f.pendingRoleRecordDeletes(t), "the written owner record is queued for deletion")
}
