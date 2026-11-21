//go:build integration

package moderation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Firehose convergence of acceptances and ownership transfers: the production
// role consumer of community_actions_firehose_integration_test.go over the
// same database as the accept, with the community repo faked.

const (
	// rosterAcceptCreatedAt is the createdAt an accept at rosterClockStart
	// stamps on the records it writes.
	rosterAcceptCreatedAt = "2026-11-13T12:00:00.000Z"

	// The CID the PDS assigns each record body (DAG-CBOR, CIDv1, sha256),
	// computed once with indigo's atdata.MarshalCBOR and go-cid outside the
	// code under test. The bodies are roleBody(subject, role, createdAt).
	//
	// The outsider as moderator, rosterAcceptCreatedAt.
	rosterOutsiderModeratorBodyCID = "bafyreiezufu2upasgp66botg4xi7t3w2utq3eugwslukle3icndro55slm"
	// M as owner, rosterAcceptCreatedAt.
	rosterModeratorOwnerBodyCID = "bafyreihle6mm4vin5r3liwhwrmnhdgprciyv5xdqzqa4clbxzgaedgcuk4"
	// O as moderator, rosterAcceptCreatedAt.
	rosterOwnerModeratorBodyCID = "bafyreifpnkazr7bykctuzftrk3q5j4tdtlyfnqz6vaxis6ij7g2w64p2ge"
	// M as moderator, createdAt "2026-11-13T12:30:00.000Z": a write from
	// outside Coves.
	rosterModeratorExternalBodyCID = "bafyreido2gn7rtto54g6jdk2ep3tqk5vsa66gm2npl7rj3zstwj7yxmvfy"
)

// writeBodyCIDs makes the fake repo answer each accept or transfer write with
// the CID the PDS assigns its body.
func (f *communityActionDBFixture) writeBodyCIDs() {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	f.repo.writeCIDs[communities.RoleCollection+"/"+rosterOutsiderKey] = rosterOutsiderModeratorBodyCID
	f.repo.writeCIDs[communities.RoleCollection+"/"+rosterModeratorKey] = rosterModeratorOwnerBodyCID
	f.repo.writeCIDs[communities.RoleCollection+"/"+rosterOwnerKey] = rosterOwnerModeratorBodyCID
}

// loseAcceptResponse seeds inv, then its invitee accepts with key while the
// PDS commits the write at head one and the response is lost.
func (f *communityActionDBFixture) loseAcceptResponse(t *testing.T, inv communities.Invitation, key string) {
	t.Helper()
	f.seedInvitation(t, inv)
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err := f.acceptInvitation(inv.InviteeDID, inv.ID, rosterStartVersion, key)
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
}

// The firehose copies of the accept and transfer writes at rev.
func outsiderGrantCopy(rev string) *jetstream.JetstreamEvent {
	return roleWriteEvent(rosterCommunity, "create", rosterOutsiderKey, rev, rosterOutsiderModeratorBodyCID,
		roleBody(rosterOutsider, communities.RoleModerator, rosterAcceptCreatedAt))
}

func moderatorPromotionCopy(rev string) *jetstream.JetstreamEvent {
	return roleWriteEvent(rosterCommunity, "update", rosterModeratorKey, rev, rosterModeratorOwnerBodyCID,
		roleBody(rosterModerator, communities.RoleOwner, rosterAcceptCreatedAt))
}

func ownerDemotionCopy(rev string) *jetstream.JetstreamEvent {
	return roleWriteEvent(rosterCommunity, "update", rosterOwnerKey, rev, rosterOwnerModeratorBodyCID,
		roleBody(rosterOwner, communities.RoleModerator, rosterAcceptCreatedAt))
}

// transferredRoles is the index after O's ownership moved to M.
func transferredRoles() map[string]indexedRole {
	return map[string]indexedRole{
		rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorOwnerBodyCID},
		rosterOwner:     {Role: communities.RoleModerator, CID: rosterOwnerModeratorBodyCID},
	}
}

// acceptedOutsiderRoles is the index after the outsider accepted O's
// moderator invitation.
func acceptedOutsiderRoles() map[string]indexedRole {
	roles := seededRoles()
	roles[rosterOutsider] = indexedRole{Role: communities.RoleModerator, CID: rosterOutsiderModeratorBodyCID}
	return roles
}

// B15 with R1-1, R1-2, R2-1 and R2-2: an accept whose PDS write committed (or
// did not) without the response reaching Coves converges once, whichever of
// the retry, the sweep, another action or the firehose copies settles it.
func TestCommunityInvitationFirehose_RecoverableAccept(t *testing.T) {
	t.Parallel()
	moderatorInvitation := rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false)
	ownershipOffer := rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false)
	grantByOwner := roleChangeAction("grant-role", rosterOwner, false, rosterOutsider)
	transferByOwner := roleChangeAction("transfer-ownership", rosterOwner, false, rosterModerator)

	// transferCopiesFirst: after a lost transfer response, the two firehose
	// copies arrive in order; the first already leaves exactly one owner.
	transferCopiesFirst := func(first, second *jetstream.JetstreamEvent) func(t *testing.T, f *communityActionDBFixture) {
		return func(t *testing.T, f *communityActionDBFixture) {
			require.NoError(t, f.deliver(first))

			assert.Equal(t, transferredRoles(), f.indexedRoles(t),
				"after the first copy the stored rows hold exactly one owner, M, and O a moderator")
			assert.Equal(t, rosterHeadOneRev, f.gateRev(t, rosterModeratorURI), "M's gate holds the transfer's commit rev")
			assert.Equal(t, rosterHeadOneRev, f.gateRev(t, rosterOwnerURI), "O's gate holds the transfer's commit rev")
			assert.Equal(t, []storedAction{transferByOwner}, f.actions(t), "one transfer-ownership action naming O")
			assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once")
			assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
				"the first copy completes the intent at its rev")
			assert.Equal(t, "accepted", f.invitationState(t, rosterOwnershipOfferID))

			require.NoError(t, f.deliver(second))

			assert.Equal(t, transferredRoles(), f.indexedRoles(t), "the second copy changes no row")
			assert.Equal(t, []storedAction{transferByOwner}, f.actions(t),
				"still one transfer-ownership action, and no actorless role action")
			assert.EqualValues(t, 8, f.rosterVersion(t), "the second copy advances no version")
		}
	}

	tests := []struct {
		name       string
		invitation communities.Invitation
		// lose sets up the accept's failure: true commits the write and loses
		// the response; false fails the write before it commits.
		lose bool
		then func(t *testing.T, f *communityActionDBFixture)
	}{
		{
			name:       "(a) moderator accept: the retry reconciles, then the firehose copy changes nothing",
			invitation: moderatorInvitation, lose: true,
			then: func(t *testing.T, f *communityActionDBFixture) {
				result, err := f.acceptInvitation(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

				require.NoError(t, err, "the retry completes the accept from the intent")
				if assert.NotNil(t, result) {
					assert.EqualValues(t, 8, result.Version)
					assert.Equal(t, rosterModeratorInvitationID, result.InvitationID)
				}
				assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no second ApplyWrites")
				assert.Equal(t, acceptedOutsiderRoles(), f.indexedRoles(t), "one moderator record for the invitee")
				assert.Equal(t, []storedAction{grantByOwner}, f.actions(t), "one grant-role action naming the inviter")
				assert.Equal(t, []string{rosterOutsider}, f.idempotencyActors(t, "accept-1"),
					"the accept's idempotency row is the invitee's")
				assert.Equal(t, "accepted", f.invitationState(t, rosterModeratorInvitationID))
				assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t))
				assert.Equal(t, rosterHeadZeroRev, f.gateRev(t, rosterOutsiderURI),
					"the reconciliation stamps the record's gate at the intent's guard rev")
				assert.EqualValues(t, 8, f.rosterVersion(t))

				require.NoError(t, f.deliver(outsiderGrantCopy(rosterHeadOneRev)))

				assert.Equal(t, acceptedOutsiderRoles(), f.indexedRoles(t), "the firehose copy changes no row")
				assert.Equal(t, []storedAction{grantByOwner}, f.actions(t), "the firehose copy adds no action")
				assert.EqualValues(t, 8, f.rosterVersion(t), "the firehose copy advances no version")
			},
		},
		{
			name:       "(b) moderator accept: the firehose copy completes the intent, then the retry replays",
			invitation: moderatorInvitation, lose: true,
			then: func(t *testing.T, f *communityActionDBFixture) {
				require.NoError(t, f.deliver(outsiderGrantCopy(rosterHeadOneRev)))

				assert.Equal(t, acceptedOutsiderRoles(), f.indexedRoles(t), "the copy indexes the invitee as moderator")
				assert.Equal(t, []storedAction{grantByOwner}, f.actions(t), "one grant-role action naming the inviter")
				assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
					"the firehose copy completes the intent at its rev")
				assert.Equal(t, "accepted", f.invitationState(t, rosterModeratorInvitationID))
				assert.EqualValues(t, 8, f.rosterVersion(t))
				ids := f.actionIDs(t)
				require.Len(t, ids, 1)

				result, err := f.acceptInvitation(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

				require.NoError(t, err, "the retry replays the firehose completion")
				require.NotNil(t, result)
				assert.Equal(t, ids[0], result.ActionID, "the replayed result names the one action")
				assert.EqualValues(t, 8, result.Version)
				assert.Equal(t, rosterCommunity, result.CommunityDID)
				assert.Equal(t, rosterOutsider, result.SubjectDID)
				assert.Equal(t, rosterModeratorInvitationID, result.InvitationID)
				assert.Equal(t, []storedAction{grantByOwner}, f.actions(t), "the retry adds no action")
				assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no ApplyWrites")
				assert.Equal(t, []string{rosterOutsider}, f.idempotencyActors(t, "accept-1"),
					"the accept's idempotency row is the invitee's")
			},
		},
		{
			name:       "(c1) transfer: firehose copies first, M's promotion before O's demotion",
			invitation: ownershipOffer, lose: true,
			then: transferCopiesFirst(moderatorPromotionCopy(rosterHeadOneRev), ownerDemotionCopy(rosterHeadOneRev)),
		},
		{
			name:       "(c2) transfer: firehose copies first, O's demotion before M's promotion",
			invitation: ownershipOffer, lose: true,
			then: transferCopiesFirst(ownerDemotionCopy(rosterHeadOneRev), moderatorPromotionCopy(rosterHeadOneRev)),
		},
		{
			name:       "(d) the sweep expires an uncommitted accept",
			invitation: moderatorInvitation, lose: false,
			then: func(t *testing.T, f *communityActionDBFixture) {
				require.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "fixture: an open accept intent")
				f.pastIntentExpiry()

				swept, err := f.service.SweepExpiredIntents(context.Background())

				require.NoError(t, err)
				assert.Equal(t, 1, swept, "the one expired intent is reconciled")
				assert.Equal(t, []storedIntent{{State: "expired"}}, f.intents(t))
				assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterOutsiderKey), "no record in the community repo")
				assert.Equal(t, seededRoles(), f.indexedRoles(t), "the role index is unchanged")
				assert.Empty(t, f.actions(t), "no action")
				assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
				assert.Equal(t, "open", f.invitationState(t, rosterModeratorInvitationID), "the invitation stays open")
				pending, err := postgres.NewCommunityInvitationRepository(f.db).PendingInvitation(context.Background(),
					rosterCommunity, rosterOutsider, f.clock.Now())
				require.NoError(t, err)
				if assert.NotNil(t, pending, "the invitation is still pending") {
					assert.Equal(t, rosterModeratorInvitationID, pending.ID)
				}
			},
		},
		{
			name:       "(e) an admin's removal of M settles the open transfer first, then refuses itself",
			invitation: ownershipOffer, lose: true,
			then: func(t *testing.T, f *communityActionDBFixture) {
				require.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "fixture: the transfer's intent is open")
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				_, err := f.remove(ctx, rosterAdmin, "admin-remove-1")

				require.NoError(t, ctx.Err(), "the removal finished before the deadline (no deadlock)")
				assert.True(t, errors.Is(err, moderation.ErrCannotRemoveOwner) || errors.Is(err, moderation.ErrStateConflict),
					"the removal of M, now the owner, is refused (CannotRemoveOwner or StateConflict); got %v", err)
				assert.Equal(t, transferredRoles(), f.indexedRoles(t), "the transfer is settled: M owner, O moderator")
				assert.Equal(t, []storedAction{transferByOwner}, f.actions(t),
					"one transfer-ownership action naming O, and no actorless role action")
				assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once, for the transfer")
				assert.Equal(t, "accepted", f.invitationState(t, rosterOwnershipOfferID))
				intents := f.intents(t)
				if assert.Len(t, intents, 1, "the refused removal stores no intent") {
					assert.Equal(t, "completed", intents[0].State, "the transfer's intent is completed")
				}
				assert.Len(t, f.repo.batchesSnapshot(), 1, "nothing further is written to the community repo")
			},
		},
		{
			name:       "(f) an unrelated write to M's record does not complete the transfer",
			invitation: ownershipOffer, lose: true,
			then: func(t *testing.T, f *communityActionDBFixture) {
				require.NoError(t, f.deliver(roleWriteEvent(rosterCommunity, "update", rosterModeratorKey, rosterHeadTwoRev,
					rosterModeratorExternalBodyCID, roleBody(rosterModerator, communities.RoleModerator, "2026-11-13T12:30:00.000Z"))))

				assert.Equal(t, map[string]indexedRole{
					rosterOwner:     {Role: communities.RoleOwner, CID: rosterOwnerCID},
					rosterModerator: {Role: communities.RoleModerator, CID: rosterModeratorExternalBodyCID},
				}, f.indexedRoles(t), "M's row takes the external write; O stays the owner")
				assert.Equal(t, []storedAction{actorlessRoleAction("grant-role", rosterCommunity, rosterModerator)}, f.actions(t),
					"one actorless action for the external change, no transfer-ownership action")
				assert.EqualValues(t, 8, f.rosterVersion(t), "the external change advances the version once")
				assert.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "the transfer's intent stays open")
				assert.Equal(t, "open", f.invitationState(t, rosterOwnershipOfferID), "the offer is not accepted")
			},
		},
		{
			name:       "(g) a newer delete of O's record is kept when M's promotion copy completes the transfer",
			invitation: ownershipOffer, lose: true,
			then: func(t *testing.T, f *communityActionDBFixture) {
				require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterOwnerKey, rosterHeadTwoRev)))
				require.Equal(t, []storedAction{actorlessRoleAction("remove-role", rosterCommunity, rosterOwner)}, f.actions(t),
					"fixture: O's later delete is indexed as an external change")

				require.NoError(t, f.deliver(moderatorPromotionCopy(rosterHeadOneRev)))

				assert.Equal(t, map[string]indexedRole{
					rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorOwnerBodyCID},
				}, f.indexedRoles(t), "M is the owner and O stays absent, not restored as moderator")
				assert.Equal(t, rosterHeadTwoRev, f.gateRev(t, rosterOwnerURI), "O's gate keeps the newer delete's rev")
				assert.Equal(t, rosterHeadOneRev, f.gateRev(t, rosterModeratorURI), "M's gate holds the transfer's commit rev")
				assert.Equal(t, []storedAction{
					actorlessRoleAction("remove-role", rosterCommunity, rosterOwner),
					transferByOwner,
				}, f.actions(t), "one transfer-ownership action naming O after the external delete")
				assert.EqualValues(t, 9, f.rosterVersion(t), "one version for the delete, one for the transfer")
				assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t))
				assert.Equal(t, "accepted", f.invitationState(t, rosterOwnershipOfferID))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			f.writeBodyCIDs()
			if tc.lose {
				f.loseAcceptResponse(t, tc.invitation, "accept-1")
			} else {
				f.seedInvitation(t, tc.invitation)
				f.repo.applyErr = errors.New("pds: 502 bad gateway")
				_, err := f.acceptInvitation(tc.invitation.InviteeDID, tc.invitation.ID, rosterStartVersion, "accept-1")
				assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			}

			tc.then(t, f)
		})
	}
}

// B16: a local accept and a local transfer whose responses arrive, then each
// write's firehose copy delivered twice: one action per change naming the
// inviter, one roster version per change, and rows the copies leave alone.
func TestCommunityInvitationFirehose_LocalAcceptThenItsCopiesTwice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		invitation  communities.Invitation
		copies      []*jetstream.JetstreamEvent
		wantRoles   map[string]indexedRole
		wantActions []storedAction
	}{
		{
			name:        "moderator accept",
			invitation:  rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false),
			copies:      []*jetstream.JetstreamEvent{outsiderGrantCopy(rosterHeadOneRev)},
			wantRoles:   acceptedOutsiderRoles(),
			wantActions: []storedAction{roleChangeAction("grant-role", rosterOwner, false, rosterOutsider)},
		},
		{
			name:       "ownership transfer",
			invitation: rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false),
			copies: []*jetstream.JetstreamEvent{
				moderatorPromotionCopy(rosterHeadOneRev), ownerDemotionCopy(rosterHeadOneRev),
			},
			wantRoles:   transferredRoles(),
			wantActions: []storedAction{roleChangeAction("transfer-ownership", rosterOwner, false, rosterModerator)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			f.writeBodyCIDs()
			f.seedInvitation(t, tc.invitation)
			_, err := f.acceptInvitation(tc.invitation.InviteeDID, tc.invitation.ID, rosterStartVersion, "accept-1")
			require.NoError(t, err, "the local accept completes")
			require.Equal(t, tc.wantRoles, f.indexedRoles(t), "fixture: the local accept indexed its change")

			for i := 0; i < 2; i++ {
				for _, event := range tc.copies {
					require.NoErrorf(t, f.deliver(event), "delivery %d", i+1)
				}
			}

			assert.Equal(t, tc.wantRoles, f.indexedRoles(t), "the copies leave the rows unchanged")
			assert.Equal(t, tc.wantActions, f.actions(t), "exactly one action for the change, naming the inviter")
			assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced exactly once")
			assert.Equal(t, "accepted", f.invitationState(t, tc.invitation.ID))
		})
	}
}
