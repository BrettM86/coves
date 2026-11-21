package moderation_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rosterOutsiderURI = "at://did:plc:rosteralpha/social.coves.community.moderator/udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq"

	rosterModeratorInvitationID = "inv-moderator"
	rosterOwnershipOfferID      = "inv-ownership"
)

// Invitations the tests seed are created a day before rosterClockStart and
// expire 14 days after that.
var (
	rosterInvitationCreated = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	rosterInvitationExpires = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
)

func rosterInvitation(id, inviteeDID, role, inviterDID string, inviterAdmin bool) communities.Invitation {
	return communities.Invitation{
		ID: id, CommunityDID: rosterCommunity, InviteeDID: inviteeDID, Role: role,
		InviterDID: inviterDID, InviterAdmin: inviterAdmin, State: communities.InvitationOpen,
		CreatedAt: rosterInvitationCreated, ExpiresAt: rosterInvitationExpires,
	}
}

func (f *communityActionFixture) accept(actorDID, invitationID string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.AcceptInvitation(context.Background(), moderation.AcceptInvitationRequest{
		ActorDID: actorDID, InvitationID: invitationID, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

func (f *communityActionFixture) decline(actorDID, invitationID string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.invitations.DeclineInvitation(context.Background(), moderation.DeclineInvitationRequest{
		ActorDID: actorDID, InvitationID: invitationID, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

func (f *communityActionFixture) hasIdempotencyRecord(actorDID, communityDID, key string) bool {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	_, ok := f.store.idempotency[communityActionIdempotencyKey{actorDID, communityDID, key}]
	return ok
}

// assertAcceptWroteNothing: no PDS write, no intent, no action, and the
// invitation still open.
func (f *communityActionFixture) assertAcceptWroteNothing(t *testing.T, invitationID string) {
	t.Helper()
	assert.Empty(t, f.repo.batchesSnapshot(), "no PDS write")
	assert.Empty(t, f.store.intentsSnapshot(), "no intent saved")
	assert.Empty(t, f.store.actionsSnapshot(), "no action written")
	assert.Equal(t, communities.InvitationOpen, f.store.invitationState(invitationID), "the invitation is still open")
}

// writtenRecord is one write of a batch with its record body decoded as JSON.
type writtenRecord struct {
	Op, Collection, RKey string
	Record               map[string]any
}

type writtenBatch struct {
	Writes     []writtenRecord
	SwapCommit string
}

func decodeBatches(t *testing.T, batches []rosterBatch) []writtenBatch {
	t.Helper()
	out := make([]writtenBatch, 0, len(batches))
	for _, batch := range batches {
		decoded := writtenBatch{SwapCommit: batch.swapCommit}
		for _, write := range batch.writes {
			record := writtenRecord{Op: string(write.Op), Collection: write.Collection, RKey: write.RKey}
			if write.Record != nil {
				encoded, err := json.Marshal(write.Record)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(encoded, &record.Record))
			}
			decoded.Writes = append(decoded.Writes, record)
		}
		out = append(out, decoded)
	}
	return out
}

func roleBody(subjectDID, role, createdAt string) map[string]any {
	return map[string]any{
		"$type":     "social.coves.community.moderator",
		"subject":   subjectDID,
		"role":      role,
		"createdAt": createdAt,
	}
}

func TestCommunityInvitationInviteRefusedForNonOwners(t *testing.T) {
	tests := []struct {
		name  string
		offer bool
		actor string
		who   string
		want  error
	}{
		{"moderator invites a moderator", false, rosterModerator, rosterOutsider, moderation.ErrForbidden},
		{"moderator offers ownership", true, rosterModerator, rosterSecondModerator, moderation.ErrForbidden},
		{"unrelated account invites a moderator", false, rosterOutsider, "did:plc:rosterfriend", moderation.ErrForbidden},
		{"owner invites an unresolvable handle", false, rosterOwner, "nobody.test", moderation.ErrAccountNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommunityActionFixture(t)
			req := moderation.InviteRequest{
				ActorDID: tt.actor, CommunityDID: rosterCommunity, Invitee: tt.who,
				ExpectedVersion: rosterStartVersion, IdempotencyKey: "invite-1",
			}

			var err error
			if tt.offer {
				_, err = f.invitations.OfferOwnership(context.Background(), req)
			} else {
				_, err = f.invitations.InviteModerator(context.Background(), req)
			}

			assert.ErrorIs(t, err, tt.want)
			assert.Empty(t, f.store.invitationsSnapshot(), "no invitation created")
			assert.Empty(t, f.factory.openedSnapshot(), "the community repo was never opened")
		})
	}
}

func TestCommunityInvitationInviteRefusesRoleConflicts(t *testing.T) {
	tests := []struct {
		name  string
		offer bool
		actor string
		who   string
		want  error
	}{
		{"owner invites themselves as moderator", false, rosterOwner, rosterOwner, moderation.ErrInviteeHasRole},
		{"owner invites an existing moderator by handle", false, rosterOwner, "rostermod.test", moderation.ErrInviteeHasRole},
		{"admin offers ownership to the current owner", true, rosterAdmin, rosterOwner, moderation.ErrInviteeHasRole},
		{"owner offers ownership to a non-moderator", true, rosterOwner, rosterOutsider, moderation.ErrInviteeNotModerator},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommunityActionFixture(t)
			req := moderation.InviteRequest{
				ActorDID: tt.actor, CommunityDID: rosterCommunity, Invitee: tt.who,
				ExpectedVersion: rosterStartVersion, IdempotencyKey: "invite-1",
			}

			var err error
			if tt.offer {
				_, err = f.invitations.OfferOwnership(context.Background(), req)
			} else {
				_, err = f.invitations.InviteModerator(context.Background(), req)
			}

			assert.ErrorIs(t, err, tt.want)
			assert.Empty(t, f.store.invitationsSnapshot(), "no invitation created")
			assert.Empty(t, f.factory.openedSnapshot(), "the community repo was never opened")
		})
	}
}

func TestCommunityInvitationInviteReplayReturnsTheOriginalInvitation(t *testing.T) {
	f := newCommunityActionFixture(t)
	req := moderation.InviteRequest{
		ActorDID: rosterOwner, CommunityDID: rosterCommunity, Invitee: rosterOutsider,
		ExpectedVersion: rosterStartVersion, IdempotencyKey: "invite-1",
	}

	first, err := f.invitations.InviteModerator(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, first, "the invite returns its result")
	second, err := f.invitations.InviteModerator(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, first, second, "the same key and request replays the original result")
	assert.NotEmpty(t, first.InvitationID)
	assert.Equal(t, rosterCommunity, first.CommunityDID)
	assert.Equal(t, rosterOutsider, first.SubjectDID)
	assert.EqualValues(t, rosterStartVersion, first.Version, "an invitation does not change the roster version")
	assert.Equal(t, []communities.Invitation{{
		ID: first.InvitationID, CommunityDID: rosterCommunity, InviteeDID: rosterOutsider,
		Role: communities.RoleModerator, InviterDID: rosterOwner, State: communities.InvitationOpen,
		CreatedAt: rosterClockStart, ExpiresAt: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC),
	}}, f.store.invitationsSnapshot(), "one invitation, created once")
	assert.Empty(t, f.factory.openedSnapshot(), "an invitation never opens the community repo")
	assert.Empty(t, f.store.actionsSnapshot(), "an invitation logs no action")
	assert.EqualValues(t, rosterStartVersion, f.store.version(rosterCommunity))
}

func TestCommunityInvitationAcceptRechecksUnderTheLocks(t *testing.T) {
	const externalCID = "bafyreiaoutsiderexternalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const externalOwnerCID = "bafyreiasecondmoderatorownedaaaaaaaaaaaaaaaaaaaaaaaaaa"
	externalCreated := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		invitation communities.Invitation
		actor      string
		version    int64
		setup      func(f *communityActionFixture)
		hookSetup  func(f *communityActionFixture)
		want       error
		check      func(t *testing.T, f *communityActionFixture)
	}{
		{
			name:       "moderator invitation whose invitee now holds a moderator role",
			invitation: rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			actor:      rosterOutsider,
			version:    8,
			setup: func(f *communityActionFixture) {
				f.store.seedRole(rosterRole(rosterCommunity, rosterOutsider, communities.RoleModerator, rosterOutsiderKey, externalCID, externalCreated))
				f.repo.setRecord(communities.RoleCollection, rosterOutsiderKey, externalCID)
				f.store.setVersion(rosterCommunity, 8)
			},
			want: moderation.ErrInviteeHasRole,
		},
		{
			name:       "open moderator invitation whose invitee is now the owner",
			invitation: rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			actor:      rosterOutsider,
			version:    8,
			setup: func(f *communityActionFixture) {
				f.store.seedRole(rosterRole(rosterCommunity, rosterOutsider, communities.RoleOwner, rosterOutsiderKey, externalCID, externalCreated))
				f.repo.setRecord(communities.RoleCollection, rosterOutsiderKey, externalCID)
				f.store.setVersion(rosterCommunity, 8)
			},
			want: moderation.ErrInviteeHasRole,
			check: func(t *testing.T, f *communityActionFixture) {
				row, ok := f.store.role(rosterCommunity, rosterOutsiderKey)
				require.True(t, ok, "the invitee's owner row is kept")
				assert.Equal(t, rosterRole(rosterCommunity, rosterOutsider, communities.RoleOwner, rosterOutsiderKey, externalCID, externalCreated), row,
					"the invitee's owner row is unchanged")
				record, err := f.repo.GetRecord(context.Background(), communities.RoleCollection, rosterOutsiderKey)
				require.NoError(t, err, "the invitee's owner record is kept")
				assert.Equal(t, externalCID, record.CID, "the invitee's owner record is unchanged")
			},
		},
		{
			name:       "admin ownership offer accepted by the current owner",
			invitation: rosterInvitation(rosterOwnershipOfferID, rosterOwner, communities.RoleOwner, rosterAdmin, true),
			actor:      rosterOwner,
			version:    rosterStartVersion,
			want:       moderation.ErrInviteeHasRole,
			check: func(t *testing.T, f *communityActionFixture) {
				row, ok := f.store.role(rosterCommunity, rosterOwnerKey)
				require.True(t, ok, "the owner's row is kept")
				assert.Equal(t, rosterOwnerCID, row.RecordCID, "the owner's record is not rewritten")
			},
		},
		{
			name:       "owner-made ownership offer whose invitee is no longer a moderator",
			invitation: rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false),
			actor:      rosterModerator,
			version:    8,
			setup: func(f *communityActionFixture) {
				f.store.dropRole(rosterCommunity, rosterModeratorKey)
				f.repo.mu.Lock()
				delete(f.repo.records, communities.RoleCollection+"/"+rosterModeratorKey)
				f.repo.mu.Unlock()
				f.store.setVersion(rosterCommunity, 8)
			},
			want: moderation.ErrInviteeNotModerator,
		},
		{
			name:       "owner-made ownership offer whose inviter is no longer the owner",
			invitation: rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false),
			actor:      rosterModerator,
			version:    8,
			setup: func(f *communityActionFixture) {
				// N's record is rewritten as a newer owner record from outside:
				// N is the effective owner and O reads as a moderator.
				f.store.seedRole(rosterRole(rosterCommunity, rosterSecondModerator, communities.RoleOwner, rosterSecondModeratorKey, externalOwnerCID, externalCreated))
				f.repo.setRecord(communities.RoleCollection, rosterSecondModeratorKey, externalOwnerCID)
				f.store.setVersion(rosterCommunity, 8)
			},
			want: moderation.ErrInvitationStale,
		},
		{
			name:       "community removed while the accept is paused after authorization",
			invitation: rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			actor:      rosterOutsider,
			version:    rosterStartVersion,
			hookSetup: func(f *communityActionFixture) {
				f.withHooks(moderation.CommunityActionHooks{AfterAuthorize: func(context.Context) {
					f.removals[rosterCommunity] = true
				}})
			},
			want: moderation.ErrForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommunityActionFixture(t)
			f.store.seedInvitation(tt.invitation)
			if tt.setup != nil {
				tt.setup(f)
			}
			if tt.hookSetup != nil {
				tt.hookSetup(f)
			}

			_, err := f.accept(tt.actor, tt.invitation.ID, tt.version, "accept-1")

			assert.ErrorIs(t, err, tt.want)
			f.assertAcceptWroteNothing(t, tt.invitation.ID)
			if tt.check != nil {
				tt.check(t, f)
			}
		})
	}
}

func TestCommunityInvitationAcceptModeratorWritesOneCreate(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
	// 12:00:00.123456789 UTC, read in another zone: createdAt is UTC at
	// millisecond precision.
	f.clock.set(time.Date(2026, 11, 13, 4, 0, 0, 123456789, time.FixedZone("UTC-8", -8*60*60)))

	result, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

	require.NoError(t, err)
	assert.Equal(t, &moderation.CommunityActionResult{
		CommunityDID: rosterCommunity, SubjectDID: rosterOutsider, RecordURI: rosterOutsiderURI,
		ActionID: "community-action-1", Version: 8, InvitationID: rosterModeratorInvitationID,
	}, result)
	assert.Equal(t, []writtenBatch{{
		Writes: []writtenRecord{{
			Op: "create", Collection: "social.coves.community.moderator", RKey: rosterOutsiderKey,
			Record: roleBody(rosterOutsider, "moderator", "2026-11-13T12:00:00.123Z"),
		}},
		SwapCommit: rosterHeadZeroCID,
	}}, decodeBatches(t, f.repo.batchesSnapshot()), "one create of the invitee's record guarded by the head")
	assert.Empty(t, f.repo.violations, "the intent was stored, guarded by the swapCommit, when the write arrived")
	intents := f.store.intentsSnapshot()
	require.Len(t, intents, 1)
	assert.Equal(t, rosterOutsider, intents[0].ActorDID, "the intent's requester is the invitee")
	assert.Equal(t, rosterOwner, intents[0].AuditActorDID, "the intent's audit actor is the inviter")
	assert.Equal(t, rosterModeratorInvitationID, intents[0].InvitationID)
	assert.True(t, f.hasIdempotencyRecord(rosterOutsider, rosterCommunity, "accept-1"), "the idempotency record is the invitee's")
	assert.Equal(t, []communityActionLogged{{
		Action: moderation.ActionGrantRole, ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterOutsider,
	}}, f.store.actionLogSnapshot(), "one grant-role action naming the inviter")
	assert.Equal(t, communities.InvitationAccepted, f.store.invitationState(rosterModeratorInvitationID))
}

func TestCommunityInvitationAcceptLapse(t *testing.T) {
	lapsing := communities.Invitation{
		ID: rosterModeratorInvitationID, CommunityDID: rosterCommunity, InviteeDID: rosterOutsider,
		Role: communities.RoleModerator, InviterDID: rosterOwner, State: communities.InvitationOpen,
		CreatedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}

	t.Run("at created plus 14 days the invitation has lapsed", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.store.seedInvitation(lapsing)
		f.clock.set(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))

		_, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

		assert.ErrorIs(t, err, moderation.ErrInvitationLapsed)
		f.assertAcceptWroteNothing(t, rosterModeratorInvitationID)
	})

	t.Run("one second before it lapses the accept succeeds", func(t *testing.T) {
		f := newCommunityActionFixture(t)
		f.store.seedInvitation(lapsing)
		f.clock.set(time.Date(2026, 11, 13, 11, 59, 59, 0, time.UTC))

		result, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.EqualValues(t, 8, result.Version)
		assert.Len(t, f.repo.batchesSnapshot(), 1, "the invitee's record was written")
		assert.Equal(t, communities.InvitationAccepted, f.store.invitationState(rosterModeratorInvitationID))
	})
}

func TestCommunityInvitationDeclineThenAcceptIsNotOpen(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

	declined, err := f.decline(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "decline-1")
	require.NoError(t, err)
	require.NotNil(t, declined, "the decline returns its result")
	assert.Equal(t, rosterModeratorInvitationID, declined.InvitationID)
	assert.Equal(t, communities.InvitationDeclined, f.store.invitationState(rosterModeratorInvitationID))

	_, err = f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

	assert.ErrorIs(t, err, moderation.ErrInvitationNotOpen)
	assert.Empty(t, f.repo.batchesSnapshot(), "no PDS write")
	assert.Empty(t, f.store.intentsSnapshot(), "no intent saved")
	assert.Empty(t, f.store.actionsSnapshot(), "no action written")
	assert.EqualValues(t, rosterStartVersion, f.store.version(rosterCommunity))
}

func TestCommunityInvitationNonInviteeGetsNotFound(t *testing.T) {
	tests := []struct {
		name       string
		call       func(f *communityActionFixture) error
		invitation string
	}{
		{"inviter declines", func(f *communityActionFixture) error {
			_, err := f.decline(rosterOwner, rosterModeratorInvitationID, rosterStartVersion, "k")
			return err
		}, rosterModeratorInvitationID},
		{"another moderator declines", func(f *communityActionFixture) error {
			_, err := f.decline(rosterSecondModerator, rosterModeratorInvitationID, rosterStartVersion, "k")
			return err
		}, rosterModeratorInvitationID},
		{"inviter accepts", func(f *communityActionFixture) error {
			_, err := f.accept(rosterOwner, rosterModeratorInvitationID, rosterStartVersion, "k")
			return err
		}, rosterModeratorInvitationID},
		{"instance admin accepts", func(f *communityActionFixture) error {
			_, err := f.accept(rosterAdmin, rosterModeratorInvitationID, rosterStartVersion, "k")
			return err
		}, rosterModeratorInvitationID},
		{"invitee accepts an unknown invitation", func(f *communityActionFixture) error {
			_, err := f.accept(rosterOutsider, "inv-unknown", rosterStartVersion, "k")
			return err
		}, rosterModeratorInvitationID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommunityActionFixture(t)
			f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

			err := tt.call(f)

			assert.ErrorIs(t, err, moderation.ErrInvitationNotFound)
			f.assertAcceptWroteNothing(t, tt.invitation)
		})
	}
}

func TestCommunityInvitationListCommunityInvitationsIsOwnerOrAdminOnly(t *testing.T) {
	tests := []struct {
		name    string
		actor   string
		wantErr error
		wantIDs []string
	}{
		{"owner lists", rosterOwner, nil, []string{rosterModeratorInvitationID}},
		{"instance admin lists", rosterAdmin, nil, []string{rosterModeratorInvitationID}},
		{"moderator is forbidden", rosterModerator, moderation.ErrForbidden, nil},
		{"unrelated account is forbidden", "did:plc:rosterfriend", moderation.ErrForbidden, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommunityActionFixture(t)
			f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

			got, err := f.invitations.ListCommunityInvitations(context.Background(), tt.actor, rosterCommunity)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, got, "no invitations returned")
				return
			}
			require.NoError(t, err)
			ids := make([]string, 0, len(got))
			for _, inv := range got {
				ids = append(ids, inv.ID)
			}
			assert.Equal(t, tt.wantIDs, ids)
		})
	}
}

func TestCommunityInvitationDeclineStaleVersionConflicts(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

	_, err := f.decline(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion-1, "decline-1")

	assert.ErrorIs(t, err, moderation.ErrStateConflict)
	assert.Equal(t, communities.InvitationOpen, f.store.invitationState(rosterModeratorInvitationID), "the invitation is still pending")
}

func TestCommunityInvitationDeclineReplayReturnsTheOriginalResult(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

	first, err := f.decline(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "decline-1")
	require.NoError(t, err)
	require.NotNil(t, first, "the decline returns its result")
	second, err := f.decline(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "decline-1")

	require.NoError(t, err, "a same-key replay is not refused as not open")
	assert.Equal(t, first, second)
	assert.Equal(t, rosterModeratorInvitationID, first.InvitationID)
	assert.Equal(t, communities.InvitationDeclined, f.store.invitationState(rosterModeratorInvitationID))
	assert.Empty(t, f.factory.openedSnapshot(), "a decline never opens the community repo")
	assert.Empty(t, f.store.actionsSnapshot(), "a decline logs no action")
}

func TestCommunityInvitationTransferIsOneCommit(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false))

	result, err := f.accept(rosterModerator, rosterOwnershipOfferID, rosterStartVersion, "accept-1")

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.EqualValues(t, 8, result.Version)
	assert.Equal(t, rosterOwnershipOfferID, result.InvitationID)
	batches := decodeBatches(t, f.repo.batchesSnapshot())
	require.Len(t, batches, 1, "exactly one ApplyWrites call")
	assert.Equal(t, rosterHeadZeroCID, batches[0].SwapCommit, "guarded by the head")
	assert.ElementsMatch(t, []writtenRecord{
		{
			Op: "update", Collection: "social.coves.community.moderator", RKey: rosterModeratorKey,
			Record: roleBody(rosterModerator, "owner", "2026-11-13T12:00:00.000Z"),
		},
		{
			Op: "update", Collection: "social.coves.community.moderator", RKey: rosterOwnerKey,
			Record: roleBody(rosterOwner, "moderator", "2026-11-13T12:00:00.000Z"),
		},
	}, batches[0].Writes, "M's owner record and O's record rewritten as moderator in the one call")
	assert.Empty(t, f.repo.violations, "the intent covered both writes when they arrived")
	assert.Equal(t, []communityActionLogged{{
		Action: moderation.ActionTransferOwnership, ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterModerator,
	}}, f.store.actionLogSnapshot(), "one transfer-ownership action naming O")
}

func TestCommunityInvitationAcceptStaleVersionConflicts(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

	_, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion-1, "accept-1")

	assert.ErrorIs(t, err, moderation.ErrStateConflict)
	f.assertAcceptWroteNothing(t, rosterModeratorInvitationID)
}

func TestCommunityInvitationAcceptReplayReturnsTheOriginalResult(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

	first, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")
	require.NoError(t, err)
	require.NotNil(t, first, "the accept returns its result")
	// The roster moves on after the accept.
	f.store.setVersion(rosterCommunity, 9)

	second, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

	require.NoError(t, err)
	assert.Equal(t, first, second, "the replay returns the original result unchanged")
	assert.EqualValues(t, 8, second.Version)
	assert.Len(t, f.repo.batchesSnapshot(), 1, "the replay writes nothing")
	assert.Len(t, f.store.actionsSnapshot(), 1, "the replay logs nothing")
}

func TestCommunityInvitationAcceptKeyReusedForAnotherInvitationConflicts(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
	first, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")
	require.NoError(t, err)
	require.NotNil(t, first, "the first accept returns its result")
	f.store.seedInvitation(rosterInvitation(rosterOwnershipOfferID, rosterOutsider, communities.RoleOwner, rosterAdmin, true))

	_, err = f.accept(rosterOutsider, rosterOwnershipOfferID, 8, "accept-1")

	assert.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
	assert.Len(t, f.repo.batchesSnapshot(), 1, "only the first accept wrote")
	assert.Equal(t, communities.InvitationOpen, f.store.invitationState(rosterOwnershipOfferID), "the second invitation is still open")
}

// A lost-response accept whose committed record the retry cannot compare
// with the intent's body (the record read back does not encode) is
// Unavailable: the intent stays open for a later reconciliation, and nothing
// is concluded about whether the write committed.
func TestCommunityInvitationRetryOnAnUnreadableRecordLeavesTheIntentOpen(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the accept's response is lost")
	f.repo.mu.Lock()
	f.repo.records[communities.RoleCollection+"/"+rosterOutsiderKey].Value = map[string]any{"createdAt": math.NaN()}
	f.repo.mu.Unlock()

	result, err := f.accept(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	assert.NotErrorIs(t, err, moderation.ErrStateConflict)
	assert.Nil(t, result)
	intents := f.store.intentsSnapshot()
	if assert.Len(t, intents, 1) {
		assert.Equal(t, moderation.CommunityIntentOpen, intents[0].State, "the intent stays open")
	}
	assert.Equal(t, communities.InvitationOpen, f.store.invitationState(rosterModeratorInvitationID), "the invitation stays open")
}

// R3: invite and offer require expectedVersion to be the roster version; a
// stale one is StateConflict and creates no invitation.
func TestCommunityInvitationInviteStaleVersionConflicts(t *testing.T) {
	tests := []struct {
		name  string
		offer bool
		who   string
	}{
		{"owner invites a moderator", false, rosterOutsider},
		{"owner offers ownership to a moderator", true, rosterModerator},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommunityActionFixture(t)
			req := moderation.InviteRequest{
				ActorDID: rosterOwner, CommunityDID: rosterCommunity, Invitee: tt.who,
				ExpectedVersion: rosterStartVersion - 1, IdempotencyKey: "invite-1",
			}

			var err error
			if tt.offer {
				_, err = f.invitations.OfferOwnership(context.Background(), req)
			} else {
				_, err = f.invitations.InviteModerator(context.Background(), req)
			}

			assert.ErrorIs(t, err, moderation.ErrStateConflict)
			assert.Empty(t, f.store.invitationsSnapshot(), "no invitation created")
			assert.False(t, f.hasIdempotencyRecord(rosterOwner, rosterCommunity, "invite-1"), "no result stored")
		})
	}
}

// failingHandles fails every handle resolution with err.
type failingHandles struct{ err error }

func (h failingHandles) ResolveHandleToDID(context.Context, string) (string, error) {
	return "", h.err
}

// A2: a handle that resolution reports invalid is InvalidRequest; any other
// resolution failure is ModerationUnavailable, not AccountNotFound.
func TestCommunityInvitationInviteHandleResolutionErrors(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		want    error
		notWant error
	}{
		{"invalid handle", &users.InvalidHandleError{Handle: "bad..handle", Reason: "empty label"}, moderation.ErrInvalidRequest, moderation.ErrAccountNotFound},
		{"resolver failure", errors.New("dns: i/o timeout"), moderation.ErrModerationUnavailable, moderation.ErrAccountNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommunityActionFixture(t)
			invitations := moderation.NewCommunityInvitationService(moderation.CommunityInvitationConfig{
				Authorizer: f.authorizer, Store: f.store, Invitations: f.store,
				Resolver: failingHandles{err: tt.err}, Now: f.clock.Now, IdempotencyTTL: 24 * time.Hour,
			})

			_, err := invitations.InviteModerator(context.Background(), moderation.InviteRequest{
				ActorDID: rosterOwner, CommunityDID: rosterCommunity, Invitee: "bad..handle",
				ExpectedVersion: rosterStartVersion, IdempotencyKey: "invite-1",
			})

			assert.ErrorIs(t, err, tt.want)
			assert.NotErrorIs(t, err, tt.notWant)
			assert.Empty(t, f.store.invitationsSnapshot(), "no invitation created")
		})
	}
}

// A lapsed invitation is refused, not declined: it stays open in storage and
// lapses at read time.
func TestCommunityInvitationDeclineOfALapsedInvitationIsRefused(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(communities.Invitation{
		ID: rosterModeratorInvitationID, CommunityDID: rosterCommunity, InviteeDID: rosterOutsider,
		Role: communities.RoleModerator, InviterDID: rosterOwner, State: communities.InvitationOpen,
		CreatedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	})
	f.clock.set(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))

	_, err := f.decline(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "decline-1")

	assert.ErrorIs(t, err, moderation.ErrInvitationLapsed)
	assert.Equal(t, communities.InvitationOpen, f.store.invitationState(rosterModeratorInvitationID), "the invitation is not declined")
	assert.False(t, f.hasIdempotencyRecord(rosterOutsider, rosterCommunity, "decline-1"), "no result stored")
}

// A decline under a key that already holds another request's result is
// IdempotencyConflict and leaves its invitation open.
func TestCommunityInvitationDeclineKeyReusedForAnotherInvitationConflicts(t *testing.T) {
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
	first, err := f.decline(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "decline-1")
	require.NoError(t, err)
	require.NotNil(t, first, "the first decline returns its result")
	f.store.seedInvitation(rosterInvitation(rosterOwnershipOfferID, rosterOutsider, communities.RoleOwner, rosterAdmin, true))

	_, err = f.decline(rosterOutsider, rosterOwnershipOfferID, rosterStartVersion, "decline-1")

	assert.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
	assert.Equal(t, communities.InvitationOpen, f.store.invitationState(rosterOwnershipOfferID), "the second invitation is still open")
}

// Q5/Q10: an admin's offer on a community with an owner. The first attempt
// locks only the invitee's record; the roster shows the owner's record must be
// rewritten too, so the accept restarts under both accounts and both records,
// then writes both in one call.
func TestCommunityInvitationAdminOfferOnAnOwnedCommunityRestartsUnderTheOwnersLocks(t *testing.T) {
	const rosterOwnerRecordURI = "at://did:plc:rosteralpha/social.coves.community.moderator/" + rosterOwnerKey
	f := newCommunityActionFixture(t)
	f.store.seedInvitation(rosterInvitation(rosterOwnershipOfferID, rosterOutsider, communities.RoleOwner, rosterAdmin, true))

	result, err := f.accept(rosterOutsider, rosterOwnershipOfferID, rosterStartVersion, "accept-1")

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.EqualValues(t, 8, result.Version)
	f.store.mu.Lock()
	locks := append([]moderation.CommunityActionLock(nil), f.store.locks...)
	f.store.mu.Unlock()
	require.Len(t, locks, 2, "one attempt under the first guess, one under the widened locks")
	assert.Equal(t, []string{rosterOutsider}, locks[0].AccountDIDs, "the first guess locks only the invitee")
	assert.Equal(t, rosterOutsiderURI, locks[0].TargetURI)
	assert.Empty(t, locks[0].OtherTargetURIs, "the first guess locks only the invitee's record")
	assert.True(t, locks[1].Roster)
	assert.Equal(t, []string{rosterOutsider, rosterOwner}, locks[1].AccountDIDs, "the restart locks the invitee and the owner")
	assert.Equal(t, rosterOwnerRecordURI, locks[1].TargetURI, "the restart locks the owner's record")
	assert.Equal(t, []string{rosterOutsiderURI}, locks[1].OtherTargetURIs, "and the invitee's record")
	assert.Equal(t, []string{rosterOutsider}, locks[1].BanSubjectDIDs)
	batches := decodeBatches(t, f.repo.batchesSnapshot())
	require.Len(t, batches, 1, "exactly one ApplyWrites call")
	assert.ElementsMatch(t, []writtenRecord{
		{
			Op: "create", Collection: "social.coves.community.moderator", RKey: rosterOutsiderKey,
			Record: roleBody(rosterOutsider, "owner", "2026-11-13T12:00:00.000Z"),
		},
		{
			Op: "update", Collection: "social.coves.community.moderator", RKey: rosterOwnerKey,
			Record: roleBody(rosterOwner, "moderator", "2026-11-13T12:00:00.000Z"),
		},
	}, batches[0].Writes, "the invitee's owner record and O's record rewritten as moderator in the one call")
	assert.Equal(t, []communityActionLogged{{
		Action: moderation.ActionTransferOwnership, ActorDID: rosterAdmin, AdminActing: true, CommunityDID: rosterCommunity, SubjectDID: rosterOutsider,
	}}, f.store.actionLogSnapshot(), "one transfer-ownership action naming the admin, admin-acting")
}
