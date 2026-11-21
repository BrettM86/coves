//go:build integration

package moderation_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Requests that meet an accept or a roster write still in flight: its PDS
// write committed, its response lost, its intent open.

func (f *communityActionDBFixture) declineInvitation(actorDID, invitationID, key string) (*moderation.CommunityActionResult, error) {
	return f.invitations.DeclineInvitation(context.Background(), moderation.DeclineInvitationRequest{
		ActorDID: actorDID, InvitationID: invitationID, ExpectedVersion: rosterStartVersion, IdempotencyKey: key,
	})
}

// storedIdempotency is the fingerprint and result stored under (actor, key).
func (f *communityActionDBFixture) storedIdempotency(t *testing.T, actorDID, key string) (string, moderation.CommunityActionResult) {
	t.Helper()
	var fingerprint string
	var stored []byte
	require.NoError(t, f.db.QueryRow(`
		SELECT fingerprint, stored_result FROM moderation_idempotency_keys
		WHERE actor_did = $1 AND authority_did = $2 AND key = $3
	`, actorDID, rosterCommunity, key).Scan(&fingerprint, &stored), "read the idempotency row of %s", key)
	var result moderation.CommunityActionResult
	require.NoError(t, json.Unmarshal(stored, &result))
	return fingerprint, result
}

// A lost-response accept leaves its invitation open and its intent open. A
// decline or a re-invite meanwhile is refused with StateConflict, so the
// firehose copy that later completes the accept finds the invitation it
// accepts still open.
func TestCommunityInvitationDB_InFlightAcceptRefusesDeclineAndReinvite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		meddle  func(f *communityActionDBFixture) (*moderation.CommunityActionResult, error)
		checkUp func(t *testing.T, f *communityActionDBFixture)
	}{
		{
			name: "decline",
			meddle: func(f *communityActionDBFixture) (*moderation.CommunityActionResult, error) {
				return f.declineInvitation(rosterOutsider, rosterModeratorInvitationID, "decline-1")
			},
			checkUp: func(t *testing.T, f *communityActionDBFixture) {
				assert.Empty(t, f.idempotencyActors(t, "decline-1"), "the refused decline stores no result")
			},
		},
		{
			name: "re-invite",
			meddle: func(f *communityActionDBFixture) (*moderation.CommunityActionResult, error) {
				return f.invite(rosterOwner, rosterOutsider, false, "reinvite-1")
			},
			checkUp: func(t *testing.T, f *communityActionDBFixture) {
				assert.Equal(t, 1, f.invitationCount(t), "the refused re-invite creates no invitation")
				assert.Empty(t, f.idempotencyActors(t, "reinvite-1"), "the refused re-invite stores no result")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			f.writeBodyCIDs()
			f.loseAcceptResponse(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider,
				communities.RoleModerator, rosterOwner, false), "accept-1")

			result, err := tc.meddle(f)

			assert.ErrorIs(t, err, moderation.ErrStateConflict, "the accept still in flight refuses the request")
			assert.Nil(t, result)
			assert.Equal(t, "open", f.invitationState(t, rosterModeratorInvitationID), "the invitation stays open")
			tc.checkUp(t, f)

			require.NoError(t, f.deliver(outsiderGrantCopy(rosterHeadOneRev)))

			assert.Equal(t, acceptedOutsiderRoles(), f.indexedRoles(t), "the copy indexes the invitee as moderator")
			assert.Equal(t, "accepted", f.invitationState(t, rosterModeratorInvitationID),
				"the invitation the copy completes is accepted")
			assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t))
		})
	}
}

// Completing an accept marks only an open invitation accepted: one no longer
// open keeps its state.
func TestCommunityInvitationDB_CompletionLeavesANonOpenInvitation(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.writeBodyCIDs()
	f.loseAcceptResponse(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider,
		communities.RoleModerator, rosterOwner, false), "accept-1")
	_, err := f.db.Exec(`UPDATE community_role_invitations SET state = 'declined' WHERE id = $1`, rosterModeratorInvitationID)
	require.NoError(t, err, "fixture: the invitation is no longer open")

	require.NoError(t, f.deliver(outsiderGrantCopy(rosterHeadOneRev)))

	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
		"the committed write still completes its intent")
	assert.Equal(t, "declined", f.invitationState(t, rosterModeratorInvitationID), "the declined invitation is not relabelled")
}

// A first ownership acceptance in an ownerless community commits with its
// response lost. A second acceptance before the firehose copy settles the
// first, whose targets it does not share, before planning: the community
// repo never holds two owner records.
func TestCommunityInvitationDB_SecondOwnershipAcceptanceSettlesTheFirst(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	_, err := f.db.Exec(`DELETE FROM community_roles WHERE community_did = $1 AND role = 'owner'`, rosterCommunity)
	require.NoError(t, err, "the community has no owner")
	f.repo.mu.Lock()
	delete(f.repo.records, communities.RoleCollection+"/"+rosterOwnerKey)
	f.repo.mu.Unlock()
	const outsiderOfferID = "inv-ownership-outsider"
	f.loseAcceptResponse(t, rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterAdmin, true), "accept-a")
	f.seedInvitation(t, rosterInvitation(outsiderOfferID, rosterOutsider, communities.RoleOwner, rosterAdmin, true))

	result, err := f.acceptInvitation(rosterOutsider, outsiderOfferID, rosterStartVersion, "accept-b")

	assert.ErrorIs(t, err, moderation.ErrStateConflict, "the settled first acceptance moved the roster version")
	assert.Nil(t, result)
	assert.Len(t, f.repo.batchesSnapshot(), 1, "one PDS commit: M's")
	assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterOutsiderKey), "no owner record for the outsider")
	assert.Equal(t, map[string]indexedRole{
		rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorWrittenCID},
	}, f.indexedRoles(t), "M is the one owner")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
		"the first acceptance's intent is completed; the second stored none")
	assert.Equal(t, "accepted", f.invitationState(t, rosterOwnershipOfferID))
	assert.Equal(t, "open", f.invitationState(t, outsiderOfferID))
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once")
}

// One idempotency key used for a roster write still in flight and then for an
// invitation request is IdempotencyConflict, and the roster write's later
// completion keeps its own result under the key.
func TestCommunityInvitationDB_KeySharedWithAnInFlightRosterWrite(t *testing.T) {
	t.Parallel()
	const key = "shared-1"

	t.Run("invite and offer after the owner's removal", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		f.repo.applyThenErr = errors.New("pds: connection reset by peer")
		_, err := f.remove(context.Background(), rosterOwner, key)
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the removal's response is lost")

		_, inviteErr := f.invite(rosterOwner, rosterOutsider, false, key)
		_, offerErr := f.invite(rosterOwner, rosterModerator, true, key)

		assert.ErrorIs(t, inviteErr, moderation.ErrIdempotencyConflict)
		assert.ErrorIs(t, offerErr, moderation.ErrIdempotencyConflict)
		assert.Equal(t, 0, f.invitationCount(t), "no invitation is created")
		assert.Empty(t, f.idempotencyActors(t, key), "no invitation result is stored under the key")

		removed, err := f.remove(context.Background(), rosterOwner, key)

		require.NoError(t, err, "the removal's retry completes it")
		_, stored := f.storedIdempotency(t, rosterOwner, key)
		assert.Equal(t, *removed, stored, "the key holds the removal's result")
		assert.Empty(t, stored.InvitationID)
	})

	t.Run("decline after the invitee's resignation", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		f.seedInvitation(t, rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false))
		f.repo.applyThenErr = errors.New("pds: connection reset by peer")
		_, err := f.service.ResignRole(context.Background(), moderation.ResignRoleRequest{
			ActorDID: rosterModerator, CommunityDID: rosterCommunity, ExpectedVersion: rosterStartVersion, IdempotencyKey: key,
		})
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the resignation's response is lost")

		_, err = f.declineInvitation(rosterModerator, rosterOwnershipOfferID, key)

		assert.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
		assert.Equal(t, "open", f.invitationState(t, rosterOwnershipOfferID), "the offer is not declined")
		assert.Empty(t, f.idempotencyActors(t, key), "no decline result is stored under the key")
	})

	t.Run("completion keeps another request's stored result", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		f.repo.applyThenErr = errors.New("pds: connection reset by peer")
		_, err := f.remove(context.Background(), rosterOwner, key)
		require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the removal's response is lost")
		const otherFingerprint = "0000000000000000000000000000000000000000000000000000000000000001"
		_, err = f.db.Exec(`
			INSERT INTO moderation_idempotency_keys (actor_did, authority_did, key, fingerprint, stored_result, created_at, expires_at)
			VALUES ($1, $2, $3, $4, '{"InvitationID":"inv-other"}', '2026-11-13T12:05:00Z', '2026-12-31T00:00:00Z')
		`, rosterOwner, rosterCommunity, key, otherFingerprint)
		require.NoError(t, err, "fixture: another request's result is stored under the key")
		f.pastIntentExpiry()

		swept, err := f.service.SweepExpiredIntents(context.Background())

		require.NoError(t, err)
		assert.Equal(t, 1, swept)
		assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
			"the committed removal completes")
		assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
		fingerprint, stored := f.storedIdempotency(t, rosterOwner, key)
		assert.Equal(t, otherFingerprint, fingerprint, "the other request's fingerprint stays")
		assert.Equal(t, "inv-other", stored.InvitationID, "the other request's result stays")
	})
}

// An accept whose PDS write failed with an unknown outcome before it
// committed leaves its intent open at an unmoved head. A same-key retry
// rechecks the accept's preconditions before it writes again: a ban indexed,
// or the invitation lapsing, between the two attempts fails the intent and
// writes nothing.
func TestCommunityInvitationDB_RetryRechecksTheAcceptPreconditions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		expires time.Time
		between func(t *testing.T, f *communityActionDBFixture)
		wantErr error
	}{
		{
			name:    "a ban indexed between the attempts",
			expires: rosterInvitationExpires,
			between: func(t *testing.T, f *communityActionDBFixture) {
				require.NoError(t, postgres.NewCommunityBanRepository(f.db).UpsertCommunityBan(context.Background(), communities.CommunityBan{
					CommunityDID: rosterCommunity, SubjectDID: rosterOutsider,
					RecordKey: "3kinviteeban", RecordCID: "bafyreiinviteeban", Reason: "spam",
				}), "index the ban")
			},
			wantErr: moderation.ErrInviteeBanned,
		},
		{
			name:    "the invitation lapsing between the attempts",
			expires: rosterClockStart.Add(5 * time.Minute),
			between: func(t *testing.T, f *communityActionDBFixture) {
				f.clock.Set(rosterClockStart.Add(6 * time.Minute))
			},
			wantErr: moderation.ErrInvitationLapsed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			inv := rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false)
			inv.ExpiresAt = tc.expires
			f.seedInvitation(t, inv)
			f.repo.applyErr = errors.New("pds: 502 bad gateway")
			_, err := f.acceptInvitation(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the first write's outcome is unknown")
			require.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "fixture: the accept's intent is open")
			tc.between(t, f)

			result, err := f.acceptInvitation(rosterOutsider, rosterModeratorInvitationID, rosterStartVersion, "accept-1")

			assert.ErrorIs(t, err, tc.wantErr)
			assert.Nil(t, result)
			assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no second write")
			assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterOutsiderKey), "no record for the invitee")
			assert.Equal(t, seededRoles(), f.indexedRoles(t), "no role is indexed")
			assert.Equal(t, []storedIntent{{State: "failed"}}, f.intents(t), "the accept's intent failed")
			assert.Empty(t, f.actions(t), "no action")
			assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
		})
	}
}

// Writes from outside Coves after an accept or transfer committed: a rewrite
// at rosterHeadTwoRev of one role record, at createdAt
// rosterExternalCreatedAt.
const rosterExternalCreatedAt = "2026-11-13T12:30:00.000Z"

const (
	// The outsider as moderator, rosterExternalCreatedAt.
	rosterOutsiderExternalBodyCID = "bafyreibvtr5ox4p3bsub6xyomwf7wgadbzpnntuktt2uodrqnzqonvt66e"
	// O as moderator, rosterExternalCreatedAt.
	rosterOwnerExternalBodyCID = "bafyreifdepeqwxb7iwmx6emzpsbdiaofygqaugzs6oa6ezghcxxehd22yi"
)

// externalRewrite is a rewrite from outside Coves of one role record: the
// subject keeps role, with a new createdAt and so a new CID.
type externalRewrite struct {
	subjectDID, rkey, role, cid string
}

var (
	moderatorExternalRewrite = externalRewrite{rosterModerator, rosterModeratorKey, communities.RoleModerator, rosterModeratorExternalBodyCID}
	ownerExternalRewrite     = externalRewrite{rosterOwner, rosterOwnerKey, communities.RoleModerator, rosterOwnerExternalBodyCID}
	outsiderExternalRewrite  = externalRewrite{rosterOutsider, rosterOutsiderKey, communities.RoleModerator, rosterOutsiderExternalBodyCID}
)

// rewriteExternally commits the rewrites to the fake community repo and
// moves its head to head two.
func (f *communityActionDBFixture) rewriteExternally(rewrites ...externalRewrite) {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	for _, rewrite := range rewrites {
		f.repo.records[communities.RoleCollection+"/"+rewrite.rkey] = &pds.RecordResponse{
			URI:   roleRecordURIFor(rewrite.rkey),
			CID:   rewrite.cid,
			Value: roleBody(rewrite.subjectDID, rewrite.role, rosterExternalCreatedAt),
		}
	}
	f.repo.head = pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev}
}

// roleRecordURIFor is the AT-URI of the role record at rkey in the roster
// community.
func roleRecordURIFor(rkey string) string {
	return "at://" + rosterCommunity + "/" + communities.RoleCollection + "/" + rkey
}

// copy is the firehose copy of the rewrite at head two.
func (rewrite externalRewrite) copy() *jetstream.JetstreamEvent {
	return roleWriteEvent(rosterCommunity, "update", rewrite.rkey, rosterHeadTwoRev, rewrite.cid,
		roleBody(rewrite.subjectDID, rewrite.role, rosterExternalCreatedAt))
}

// rewrittenRoles is a copy of roles with each rewrite indexed.
func rewrittenRoles(roles map[string]indexedRole, rewrites ...externalRewrite) map[string]indexedRole {
	out := make(map[string]indexedRole, len(roles))
	for subjectDID, role := range roles {
		out[subjectDID] = role
	}
	for _, rewrite := range rewrites {
		out[rewrite.subjectDID] = indexedRole{Role: rewrite.role, CID: rewrite.cid}
	}
	return out
}

// roleActionCIDs is each role action's role_record_cid in the community,
// oldest first, with NULL as "<null>".
func (f *communityActionDBFixture) roleActionCIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT COALESCE(role_record_cid, '<null>') FROM moderation_actions
		WHERE scope_community_did = $1 ORDER BY created_at, id
	`, rosterCommunity)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var cid string
		require.NoError(t, rows.Scan(&cid))
		out = append(out, cid)
	}
	require.NoError(t, rows.Err())
	return out
}

// intentRecordCIDs is each intent's record_cid in the community, with NULL
// as "<null>".
func (f *communityActionDBFixture) intentRecordCIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT COALESCE(record_cid, '<null>') FROM community_write_intents
		WHERE community_did = $1 ORDER BY created_at, id
	`, rosterCommunity)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var cid string
		require.NoError(t, rows.Scan(&cid))
		out = append(out, cid)
	}
	require.NoError(t, rows.Err())
	return out
}

// A transfer commits with its response lost, then a write from outside Coves
// rewrites one of its two records and moves the head. The retry still finds
// the transfer committed from the record that holds the intent's body: the
// intent completes with the inviter's attribution and the invitation is
// accepted. Both records are indexed from the transfer's bodies at the CIDs
// the PDS assigned them, so the transfer's own firehose copies, whenever they
// arrive and however often, change nothing, and the rewrite is indexed once
// as an external change, before or after them.
func TestCommunityInvitationDB_RetryCompletesATransferWhosePrimaryRecordChangedLater(t *testing.T) {
	t.Parallel()
	transferByOwner := roleChangeAction("transfer-ownership", rosterOwner, false, rosterModerator)
	for _, changed := range []struct {
		name    string
		rewrite externalRewrite
	}{
		{"the invitee's (primary) record", moderatorExternalRewrite},
		{"the previous owner's (paired) record", ownerExternalRewrite},
	} {
		for _, order := range []struct {
			name   string
			copies func(rewrite externalRewrite) []*jetstream.JetstreamEvent
		}{
			{"transfer copies, then the rewrite", func(rewrite externalRewrite) []*jetstream.JetstreamEvent {
				return []*jetstream.JetstreamEvent{
					moderatorPromotionCopy(rosterHeadOneRev), ownerDemotionCopy(rosterHeadOneRev), rewrite.copy(),
				}
			}},
			{"the rewrite, then the transfer copies", func(rewrite externalRewrite) []*jetstream.JetstreamEvent {
				return []*jetstream.JetstreamEvent{
					rewrite.copy(), ownerDemotionCopy(rosterHeadOneRev), moderatorPromotionCopy(rosterHeadOneRev),
				}
			}},
		} {
			t.Run(changed.name+": "+order.name, func(t *testing.T) {
				t.Parallel()
				f := newCommunityActionDBFixture(t)
				f.writeBodyCIDs()
				f.loseAcceptResponse(t, rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false), "accept-1")
				f.rewriteExternally(changed.rewrite)

				result, err := f.acceptInvitation(rosterModerator, rosterOwnershipOfferID, rosterStartVersion, "accept-1")

				require.NoError(t, err, "the committed transfer completes")
				if assert.NotNil(t, result) {
					assert.Equal(t, rosterOwnershipOfferID, result.InvitationID)
				}
				assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadTwoRev}}, f.intents(t), "the intent completed, not failed")
				assert.Equal(t, []string{rosterModeratorOwnerBodyCID}, f.intentRecordCIDs(t),
					"the intent keeps the CID of the invitee's record as the transfer wrote it")
				assert.Equal(t, "accepted", f.invitationState(t, rosterOwnershipOfferID))
				assert.Equal(t, []storedAction{transferByOwner}, f.actions(t), "one transfer-ownership action naming the inviter")
				assert.Equal(t, []string{rosterModeratorOwnerBodyCID}, f.roleActionCIDs(t),
					"the action names the invitee's record as the transfer wrote it")
				assert.Equal(t, transferredRoles(), f.indexedRoles(t),
					"both records are indexed as the transfer wrote them; the later rewrite is the firehose's")
				assert.EqualValues(t, 8, f.rosterVersion(t))
				assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry writes nothing")

				copies := order.copies(changed.rewrite)
				for i := 0; i < 2; i++ {
					for _, event := range copies {
						require.NoErrorf(t, f.deliver(event), "delivery %d", i+1)
					}
				}

				assert.Equal(t, rewrittenRoles(transferredRoles(), changed.rewrite), f.indexedRoles(t),
					"the index converges on the rewrite")
				assert.Equal(t, []storedAction{transferByOwner, actorlessRoleAction("grant-role", rosterCommunity, changed.rewrite.subjectDID)},
					f.actions(t), "the transfer's copies add no action; the rewrite adds one actorless action")
				assert.Equal(t, []string{rosterModeratorOwnerBodyCID, changed.rewrite.cid}, f.roleActionCIDs(t))
				assert.EqualValues(t, 9, f.rosterVersion(t), "one version for the transfer, one for the rewrite")
				assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadTwoRev}}, f.intents(t))
			})
		}
	}
}

// An accept commits with its response lost, then writes from outside Coves
// rewrite every record it wrote before any firehose copy arrives. Neither the
// retry nor the sweep can tell from the repo whether the accept committed,
// so the intent stays open: a decline is still refused, and the accept's
// firehose copy later completes it with the inviter's attribution, after
// which the rewrites are indexed as external changes.
func TestCommunityInvitationDB_RewrittenAcceptAfterALostResponseStaysOpen(t *testing.T) {
	t.Parallel()
	moderatorInvitation := rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false)
	ownershipOffer := rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false)
	for _, accept := range []struct {
		name       string
		invitation communities.Invitation
		rewrites   []externalRewrite
		copies     []*jetstream.JetstreamEvent
		accepted   map[string]indexedRole
		acceptance storedAction
	}{
		{
			name: "moderator accept", invitation: moderatorInvitation,
			rewrites:   []externalRewrite{outsiderExternalRewrite},
			copies:     []*jetstream.JetstreamEvent{outsiderGrantCopy(rosterHeadOneRev)},
			accepted:   acceptedOutsiderRoles(),
			acceptance: roleChangeAction("grant-role", rosterOwner, false, rosterOutsider),
		},
		{
			name: "transfer with both records rewritten", invitation: ownershipOffer,
			rewrites:   []externalRewrite{moderatorExternalRewrite, ownerExternalRewrite},
			copies:     []*jetstream.JetstreamEvent{moderatorPromotionCopy(rosterHeadOneRev), ownerDemotionCopy(rosterHeadOneRev)},
			accepted:   transferredRoles(),
			acceptance: roleChangeAction("transfer-ownership", rosterOwner, false, rosterModerator),
		},
	} {
		for _, settler := range []struct {
			name   string
			settle func(t *testing.T, f *communityActionDBFixture)
		}{
			{"the retry", func(t *testing.T, f *communityActionDBFixture) {
				_, err := f.acceptInvitation(accept.invitation.InviteeDID, accept.invitation.ID, rosterStartVersion, "accept-1")
				assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the retry cannot tell whether the accept committed")
			}},
			{"the sweep", func(t *testing.T, f *communityActionDBFixture) {
				f.pastIntentExpiry()
				swept, err := f.service.SweepExpiredIntents(context.Background())
				assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the sweep cannot tell whether the accept committed")
				assert.Zero(t, swept, "the sweep settles nothing")
			}},
		} {
			t.Run(accept.name+": "+settler.name, func(t *testing.T) {
				t.Parallel()
				f := newCommunityActionDBFixture(t)
				f.writeBodyCIDs()
				f.loseAcceptResponse(t, accept.invitation, "accept-1")
				f.rewriteExternally(accept.rewrites...)

				settler.settle(t, f)

				assert.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "the intent stays open, not failed")
				assert.Equal(t, "open", f.invitationState(t, accept.invitation.ID))
				assert.Equal(t, seededRoles(), f.indexedRoles(t), "the role index is unchanged")
				assert.Empty(t, f.actions(t), "no action")
				assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t))

				_, err := f.declineInvitation(accept.invitation.InviteeDID, accept.invitation.ID, "decline-1")
				assert.ErrorIs(t, err, moderation.ErrStateConflict, "the decline is refused while the accept may have committed")
				assert.Equal(t, "open", f.invitationState(t, accept.invitation.ID))

				for _, event := range accept.copies {
					require.NoError(t, f.deliver(event))
				}

				assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
					"the accept's firehose copy completes the intent")
				assert.Equal(t, "accepted", f.invitationState(t, accept.invitation.ID))
				assert.Equal(t, accept.accepted, f.indexedRoles(t))
				assert.Equal(t, []storedAction{accept.acceptance}, f.actions(t), "one action naming the inviter")
				assert.EqualValues(t, 8, f.rosterVersion(t))

				for _, rewrite := range accept.rewrites {
					require.NoError(t, f.deliver(rewrite.copy()))
				}

				assert.Equal(t, rewrittenRoles(accept.accepted, accept.rewrites...), f.indexedRoles(t))
				want := []storedAction{accept.acceptance}
				for _, rewrite := range accept.rewrites {
					want = append(want, actorlessRoleAction("grant-role", rosterCommunity, rewrite.subjectDID))
				}
				assert.Equal(t, want, f.actions(t), "each rewrite adds one actorless action")
			})
		}
	}
}

// A valid admin ownership offer accepted in a community with an owner, while
// another key's expired, uncommitted intent sits on the invitee's record: the
// first attempt widens the locks to the owner, the second to the other
// intent's actor, the third settles that intent, and the fourth accepts.
func TestCommunityInvitationDB_AdminOfferAcceptedAfterTwoWideningsAndASettle(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	_, err := f.remove(context.Background(), rosterAdmin, "admin-remove-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "fixture: the admin's removal of M has an unknown outcome")
	f.seedInvitation(t, rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterAdmin, true))
	f.pastIntentExpiry()

	result, err := f.acceptInvitation(rosterModerator, rosterOwnershipOfferID, rosterStartVersion, "accept-1")

	require.NoError(t, err, "the offer is accepted")
	if assert.NotNil(t, result) {
		assert.EqualValues(t, 8, result.Version)
	}
	assert.Equal(t, []storedIntent{{State: "expired"}, {State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
		"the removal's intent expired; the transfer's completed")
	assert.Equal(t, "accepted", f.invitationState(t, rosterOwnershipOfferID))
	assert.Equal(t, communities.RoleOwner, f.indexedRoles(t)[rosterModerator].Role, "M is the owner")
	assert.Equal(t, communities.RoleModerator, f.indexedRoles(t)[rosterOwner].Role, "O is a moderator")
}
