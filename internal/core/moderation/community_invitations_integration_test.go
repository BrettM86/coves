//go:build integration

package moderation_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/notifications"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Invitations, acceptance and ownership transfer against real Postgres: the
// real community action store (invitations, notifications, intents, index),
// invitation reader, role index and authorizer over the fixture of
// community_actions_integration_test.go, with the community repo faked.

// rosterInvitationExpiresFromClockStart is rosterClockStart + 14 days.
var rosterInvitationExpiresFromClockStart = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// invite asks the invitation service, as actorDID, to invite invitee (a handle
// or DID) as moderator, or, with offer, to offer it the ownership.
func (f *communityActionDBFixture) invite(actorDID, invitee string, offer bool, key string) (*moderation.CommunityActionResult, error) {
	req := moderation.InviteRequest{
		ActorDID: actorDID, CommunityDID: rosterCommunity, Invitee: invitee,
		ExpectedVersion: rosterStartVersion, IdempotencyKey: key,
	}
	if offer {
		return f.invitations.OfferOwnership(context.Background(), req)
	}
	return f.invitations.InviteModerator(context.Background(), req)
}

// mustInvite invites and returns the new invitation's ID.
func (f *communityActionDBFixture) mustInvite(t *testing.T, actorDID, invitee string, offer bool, key string) string {
	t.Helper()
	result, err := f.invite(actorDID, invitee, offer, key)
	require.NoError(t, err, "the invitation is created")
	require.NotNil(t, result, "the invite returns its result")
	require.NotEmpty(t, result.InvitationID, "the invite result names the invitation")
	return result.InvitationID
}

// seedInvitation stores inv through the store's action transaction, as the
// invitation service does.
func (f *communityActionDBFixture) seedInvitation(t *testing.T, inv communities.Invitation) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.config.Store.BeginAction(ctx, moderation.CommunityActionLock{CommunityDID: inv.CommunityDID, Roster: true})
	require.NoError(t, err)
	if err := tx.CreateInvitation(ctx, inv); err != nil {
		_ = tx.Rollback()
		require.NoError(t, err, "store the invitation")
	}
	require.NoError(t, tx.Commit(), "commit the invitation")
}

func (f *communityActionDBFixture) acceptInvitation(actorDID, invitationID string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.AcceptInvitation(context.Background(), moderation.AcceptInvitationRequest{
		ActorDID: actorDID, InvitationID: invitationID, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

// indexedRole is one community_roles row's role and record CID.
type indexedRole struct {
	Role, CID string
}

// indexedRoles is the community's community_roles rows by subject DID.
func (f *communityActionDBFixture) indexedRoles(t *testing.T) map[string]indexedRole {
	t.Helper()
	rows, err := f.db.Query(`SELECT subject_did, role, record_cid FROM community_roles WHERE community_did = $1`, rosterCommunity)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]indexedRole{}
	for rows.Next() {
		var subject string
		var role indexedRole
		require.NoError(t, rows.Scan(&subject, &role.Role, &role.CID))
		out[subject] = role
	}
	require.NoError(t, rows.Err())
	return out
}

// seededRoles is the fixture's index: O owner, M moderator.
func seededRoles() map[string]indexedRole {
	return map[string]indexedRole{
		rosterOwner:     {Role: communities.RoleOwner, CID: rosterOwnerCID},
		rosterModerator: {Role: communities.RoleModerator, CID: rosterModeratorCID},
	}
}

func (f *communityActionDBFixture) gateRev(t *testing.T, uri string) string {
	t.Helper()
	var rev string
	require.NoError(t, f.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, uri).Scan(&rev),
		"read the rev gate of %s", uri)
	return rev
}

// invitationState is the stored state of one invitation.
func (f *communityActionDBFixture) invitationState(t *testing.T, invitationID string) string {
	t.Helper()
	var state string
	require.NoError(t, f.db.QueryRow(`SELECT state FROM community_role_invitations WHERE id = $1`, invitationID).Scan(&state),
		"read the invitation from community_role_invitations")
	return state
}

// invitationCount is the number of stored invitations in the community.
func (f *communityActionDBFixture) invitationCount(t *testing.T) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM community_role_invitations WHERE community_did = $1`, rosterCommunity).Scan(&count),
		"count the community's rows in community_role_invitations")
	return count
}

// idempotencyActors is the actor of each idempotency row the community holds
// under key.
func (f *communityActionDBFixture) idempotencyActors(t *testing.T, key string) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT actor_did FROM moderation_idempotency_keys WHERE authority_did = $1 AND key = $2`,
		rosterCommunity, key)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	actors := []string{}
	for rows.Next() {
		var actor string
		require.NoError(t, rows.Scan(&actor))
		actors = append(actors, actor)
	}
	require.NoError(t, rows.Err())
	return actors
}

// indexUser inserts a users row.
func (f *communityActionDBFixture) indexUser(t *testing.T, did, handle string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, 'https://native.pds.test', NOW())`, did, handle)
	require.NoError(t, err, "index user %s", did)
}

// storedNotification is one notifications row's fields these tests pin.
type storedNotification struct {
	Reason, RecipientDID, ActorDID, SubjectURI string
}

func (f *communityActionDBFixture) notificationRows(t *testing.T) []storedNotification {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT reason, recipient_did, COALESCE(actor_did, ''), COALESCE(subject_uri, '') FROM notifications ORDER BY id
	`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []storedNotification{}
	for rows.Next() {
		var row storedNotification
		require.NoError(t, rows.Scan(&row.Reason, &row.RecipientDID, &row.ActorDID, &row.SubjectURI))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// utcInvitations normalizes the invitations' times to UTC for comparison.
func utcInvitations(invitations []communities.Invitation) []communities.Invitation {
	out := make([]communities.Invitation, 0, len(invitations))
	for _, inv := range invitations {
		inv.CreatedAt, inv.ExpiresAt = inv.CreatedAt.UTC(), inv.ExpiresAt.UTC()
		out = append(out, inv)
	}
	return out
}

func roleChangeAction(action, actorDID string, adminActing bool, subjectDID string) storedAction {
	return storedAction{
		Action: action, ActorDID: actorDID, AdminActing: adminActing,
		AuthorityDID: rosterCommunity, ScopeKind: "community", ScopeCommunityDID: rosterCommunity,
		SubjectURI: "at://" + subjectDID, Origin: "local",
	}
}

// B1 with R1-7: the owner invites an account by handle. The invitation is
// pending and listed to the owner; the community repo is untouched, no action
// is logged and the roster version stays. The invitee gets one notification
// unless the existing recipient rules exclude it; the invitation is created
// either way.
func TestCommunityInvitationDB_OwnerInvitesByHandle(t *testing.T) {
	t.Parallel()
	invitationNotification := storedNotification{
		Reason: "communityInvitation", RecipientDID: rosterOutsider, ActorDID: rosterOwner,
		SubjectURI: "at://did:plc:rosteralpha",
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, f *communityActionDBFixture)
		want  []storedNotification
	}{
		{
			name:  "ordinary invitee is notified",
			setup: func(t *testing.T, f *communityActionDBFixture) { f.indexUser(t, rosterOutsider, "rosteroutsider.test") },
			want:  []storedNotification{invitationNotification},
		},
		{
			name:  "invitee without a users row",
			setup: func(*testing.T, *communityActionDBFixture) {},
			want:  []storedNotification{},
		},
		{
			name: "aggregator invitee",
			setup: func(t *testing.T, f *communityActionDBFixture) {
				f.indexUser(t, rosterOutsider, "rosteroutsider.test")
				_, err := f.db.Exec(`INSERT INTO aggregators (did, display_name, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
					rosterOutsider, "Invitee aggregator", "at://did:plc:rosteroutsider/social.coves.aggregator.service/self",
					"bafyreiinviteeaggregator")
				require.NoError(t, err)
			},
			want: []storedNotification{},
		},
		{
			name: "invitee blocked the inviter",
			setup: func(t *testing.T, f *communityActionDBFixture) {
				f.indexUser(t, rosterOutsider, "rosteroutsider.test")
				_, err := f.db.Exec(`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
					rosterOutsider, rosterOwner, "at://did:plc:rosteroutsider/social.coves.actor.block/3kinviteeblock",
					"bafyreiinviteeblock")
				require.NoError(t, err)
			},
			want: []storedNotification{},
		},
		{
			name: "inviter blocked the invitee",
			setup: func(t *testing.T, f *communityActionDBFixture) {
				f.indexUser(t, rosterOutsider, "rosteroutsider.test")
				_, err := f.db.Exec(`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
					rosterOwner, rosterOutsider, "at://did:plc:rosterowner/social.coves.actor.block/3kinviterblock",
					"bafyreiinviterblock")
				require.NoError(t, err)
			},
			want: []storedNotification{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			f.indexUser(t, rosterOwner, "rosterowner.test")
			tc.setup(t, f)

			result, err := f.invite(rosterOwner, "rosteroutsider.test", false, "invite-1")

			require.NoError(t, err)
			require.NotNil(t, result, "the invite returns its result")
			listed, err := f.invitations.ListCommunityInvitations(context.Background(), rosterOwner, rosterCommunity)
			require.NoError(t, err)
			assert.Equal(t, []communities.Invitation{{
				ID: result.InvitationID, CommunityDID: rosterCommunity, InviteeDID: rosterOutsider,
				Role: communities.RoleModerator, InviterDID: rosterOwner, State: communities.InvitationOpen,
				CreatedAt: rosterClockStart, ExpiresAt: rosterInvitationExpiresFromClockStart,
			}}, utcInvitations(listed), "one pending invitation, listed to the owner")
			assert.Empty(t, f.repo.batchesSnapshot(), "nothing written to the community repo")
			assert.Empty(t, f.factory.openedSnapshot(), "the community repo was never opened")
			assert.Empty(t, f.actions(t), "an invitation logs no action")
			assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
			assert.Equal(t, seededRoles(), f.indexedRoles(t), "the role index is unchanged")
			assert.Equal(t, tc.want, f.notificationRows(t))
		})
	}
}

// Chunk 07 R3 replaces the "erased invitee" row above: an invitation to an
// erased account is refused as AccountNotFound and nothing is written.
func TestCommunityInvitationDB_OwnerInvitesAnErasedAccount(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.indexUser(t, rosterOwner, "rosterowner.test")
	f.indexUser(t, rosterOutsider, "rosteroutsider.test")
	_, err := f.db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, rosterOutsider)
	require.NoError(t, err)

	result, err := f.invite(rosterOwner, "rosteroutsider.test", false, "invite-1")

	require.ErrorIs(t, err, moderation.ErrAccountNotFound, "an erased invitee is refused as an unknown account")
	assert.Nil(t, result)
	listed, err := f.invitations.ListCommunityInvitations(context.Background(), rosterOwner, rosterCommunity)
	require.NoError(t, err)
	assert.Empty(t, listed, "no invitation is created for an erased invitee")
	assert.Empty(t, f.notificationRows(t), "no notification is written")
	assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
}

// B19: listNotifications, through the read path its handler uses, serves the
// invitee its invitation with reason communityInvitation and the community.
func TestCommunityInvitationDB_ListNotificationsServesTheInvitation(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.indexUser(t, rosterOwner, "rosterowner.test")
	f.indexUser(t, rosterOutsider, "rosteroutsider.test")
	f.mustInvite(t, rosterOwner, "rosteroutsider.test", false, "invite-1")
	repo, ok := postgres.NewNotificationRepository(f.db).(notifications.ReadRepository)
	require.True(t, ok, "the notification repository serves reads")
	list := notifications.NewListService(repo, postgres.NewUserRepository(f.db), postgres.NewPostRepository(f.db),
		postgres.NewCommentRepository(f.db))

	output, err := list.ListNotifications(context.Background(), rosterOutsider, "", 50)

	require.NoError(t, err)
	require.Len(t, output.Notifications, 1, "the invitee's one notification is served")
	assert.Equal(t, notifications.ReasonCommunityInvitation, output.Notifications[0].Reason)
	if assert.NotNil(t, output.Notifications[0].Community, "the notification names the community") {
		assert.Equal(t, rosterCommunity, output.Notifications[0].Community.DID)
	}
}

// An invitation notification whose community was deleted reads hidden: the
// invitee's listNotifications neither fails nor serves it, and still serves
// its other notifications.
func TestCommunityInvitationDB_ListNotificationsAfterTheCommunityIsDeleted(t *testing.T) {
	t.Parallel()
	const otherCommunity = "did:plc:rosterbeta"
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	_, err := f.db.ExecContext(ctx, `
		INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at, pds_refresh_token_encrypted)
		VALUES ($1, 'rosterbeta.test', 'Roster Beta', $1, $1, $2, NOW(), $3)
	`, otherCommunity, rosterInstanceDID, []byte("hosted"))
	require.NoError(t, err, "index a second community")
	f.indexUser(t, rosterOwner, "rosterowner.test")
	f.indexUser(t, rosterOutsider, "rosteroutsider.test")
	f.mustInvite(t, rosterOwner, "rosteroutsider.test", false, "invite-1")
	other := rosterInvitation("inv-beta", rosterOutsider, communities.RoleModerator, rosterOwner, false)
	other.CommunityDID = otherCommunity
	f.seedInvitation(t, other)
	require.Len(t, f.notificationRows(t), 2, "fixture: an invitation notification from each community")
	require.NoError(t, postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed()).Delete(ctx, rosterCommunity),
		"delete the community")
	require.Len(t, f.notificationRows(t), 2, "fixture: the deleted community's notification row is kept")
	repo, ok := postgres.NewNotificationRepository(f.db).(notifications.ReadRepository)
	require.True(t, ok, "the notification repository serves reads")
	list := notifications.NewListService(repo, postgres.NewUserRepository(f.db), postgres.NewPostRepository(f.db),
		postgres.NewCommentRepository(f.db))

	output, err := list.ListNotifications(ctx, rosterOutsider, "", 50)

	require.NoError(t, err, "a deleted community does not fail the invitee's page")
	require.Len(t, output.Notifications, 1, "only the other community's invitation is served")
	assert.Equal(t, notifications.ReasonCommunityInvitation, output.Notifications[0].Reason)
	if assert.NotNil(t, output.Notifications[0].Community, "the notification names the community") {
		assert.Equal(t, otherCommunity, output.Notifications[0].Community.DID)
	}
}

// B4: A has an open moderator invitation; an admin offers A the ownership,
// which supersedes it. A accepts the offer and is the owner; accepting the
// moderator invitation afterwards is refused and changes nothing.
func TestCommunityInvitationDB_OwnershipOfferSupersedesAModeratorInvitation(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	moderatorInvitation := f.mustInvite(t, rosterOwner, rosterOutsider, false, "invite-1")
	offer := f.mustInvite(t, rosterAdmin, rosterOutsider, true, "offer-1")
	assert.Equal(t, "superseded", f.invitationState(t, moderatorInvitation), "the offer supersedes the moderator invitation")
	_, err := f.acceptInvitation(rosterOutsider, offer, rosterStartVersion, "accept-offer")
	require.NoError(t, err, "A accepts the ownership offer")
	ownerRecord, err := f.repo.GetRecord(context.Background(), communities.RoleCollection, rosterOutsiderKey)
	require.NoError(t, err, "A's owner record is in the repo")
	ownerRow := f.indexedRoles(t)[rosterOutsider]
	require.Equal(t, indexedRole{Role: communities.RoleOwner, CID: rosterOutsiderWrittenCID}, ownerRow, "A is the indexed owner")
	batches := len(f.repo.batchesSnapshot())

	_, err = f.acceptInvitation(rosterOutsider, moderatorInvitation, 8, "accept-moderator")

	assert.ErrorIs(t, err, moderation.ErrInvitationNotOpen)
	assert.Len(t, f.repo.batchesSnapshot(), batches, "the refused accept writes nothing to the repo")
	after, err := f.repo.GetRecord(context.Background(), communities.RoleCollection, rosterOutsiderKey)
	require.NoError(t, err)
	assert.Equal(t, ownerRecord, after, "A's owner record is unchanged")
	assert.Equal(t, ownerRow, f.indexedRoles(t)[rosterOutsider], "A's index row is unchanged")
	assert.EqualValues(t, 8, f.rosterVersion(t), "the roster version advanced only for the transfer")
	assert.Equal(t, "superseded", f.invitationState(t, moderatorInvitation))
}

// B6 static rows (Q05): an in-force community ban on the invitee refuses the
// invite, and refuses an accept of an invitation created before the ban; a
// lapsed ban refuses neither.
func TestCommunityInvitationDB_CommunityBanRefusesInviteAndAccept(t *testing.T) {
	t.Parallel()
	ban := func(expiresAt *time.Time) communities.CommunityBan {
		return communities.CommunityBan{
			CommunityDID: rosterCommunity, SubjectDID: rosterOutsider,
			RecordKey: "3kinviteeban", RecordCID: "bafyreiinviteeban", Reason: "spam", ExpiresAt: expiresAt,
		}
	}
	indexBan := func(t *testing.T, f *communityActionDBFixture, b communities.CommunityBan) {
		t.Helper()
		require.NoError(t, postgres.NewCommunityBanRepository(f.db).UpsertCommunityBan(context.Background(), b), "index the ban")
	}

	t.Run("in-force ban refuses the invite", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		indexBan(t, f, ban(nil))

		_, err := f.invite(rosterOwner, rosterOutsider, false, "invite-1")

		assert.ErrorIs(t, err, moderation.ErrInviteeBanned)
		assert.Zero(t, f.invitationCount(t), "no invitation")
	})

	t.Run("ban indexed after the invitation refuses the accept", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		invitation := rosterModeratorInvitationID
		f.seedInvitation(t, rosterInvitation(invitation, rosterOutsider, communities.RoleModerator, rosterOwner, false))
		indexBan(t, f, ban(nil))

		_, err := f.acceptInvitation(rosterOutsider, invitation, rosterStartVersion, "accept-1")

		assert.ErrorIs(t, err, moderation.ErrInviteeBanned)
		assert.Empty(t, f.repo.batchesSnapshot(), "no PDS write")
		assert.Equal(t, seededRoles(), f.indexedRoles(t), "the role index is unchanged")
		assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
		assert.Empty(t, f.actions(t), "no action")
		assert.Empty(t, f.intents(t), "no intent")
		assert.Equal(t, "open", f.invitationState(t, invitation), "the invitation stays open")
	})

	t.Run("lapsed ban refuses neither", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		lapsed := time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC)
		indexBan(t, f, ban(&lapsed))

		invitation := f.mustInvite(t, rosterOwner, rosterOutsider, false, "invite-1")
		_, err := f.acceptInvitation(rosterOutsider, invitation, rosterStartVersion, "accept-1")

		require.NoError(t, err)
		assert.Equal(t, indexedRole{Role: communities.RoleModerator, CID: rosterOutsiderWrittenCID}, f.indexedRoles(t)[rosterOutsider],
			"the invitee is indexed as moderator")
	})
}

// B8-T1 with R1-1: accepting a moderator invitation indexes the invitee before
// the call returns: the role row, the rev gate at the commit rev, the roster
// version, one grant-role action naming the inviter, the invitee's own
// idempotency row, and the invitation accepted.
func TestCommunityInvitationDB_AcceptModeratorInvitationIndexesLocally(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	invitation := rosterModeratorInvitationID
	f.seedInvitation(t, rosterInvitation(invitation, rosterOutsider, communities.RoleModerator, rosterOwner, false))

	result, err := f.acceptInvitation(rosterOutsider, invitation, rosterStartVersion, "accept-1")

	require.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.EqualValues(t, 8, result.Version)
	}
	want := seededRoles()
	want[rosterOutsider] = indexedRole{Role: communities.RoleModerator, CID: rosterOutsiderWrittenCID}
	assert.Equal(t, want, f.indexedRoles(t), "the invitee is indexed as moderator")
	_, err = f.authorizer.Authorize(context.Background(), rosterCommunity, rosterOutsider, communities.RoleModerator)
	assert.NoError(t, err, "the authorizer accepts the invitee as moderator")
	assert.Equal(t, rosterHeadOneRev, f.gateRev(t, rosterOutsiderURI), "the record's rev gate holds the commit rev")
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced by one")
	assert.Equal(t, []storedAction{roleChangeAction("grant-role", rosterOwner, false, rosterOutsider)}, f.actions(t),
		"one grant-role action naming the inviter")
	assert.Equal(t, []string{rosterOutsider}, f.idempotencyActors(t, "accept-1"), "the accept's idempotency row is the invitee's")
	assert.Equal(t, "accepted", f.invitationState(t, invitation))
}

// B11-T1: transferring ownership from O to M leaves M the only owner and O a
// moderator, both in the one completion transaction: a failure injected at
// the second index row change rolls back the first too.
func TestCommunityInvitationDB_TransferIsOneTransaction(t *testing.T) {
	t.Parallel()

	t.Run("transfer completes", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		offer := rosterOwnershipOfferID
		f.seedInvitation(t, rosterInvitation(offer, rosterModerator, communities.RoleOwner, rosterOwner, false))

		_, err := f.acceptInvitation(rosterModerator, offer, rosterStartVersion, "accept-1")

		require.NoError(t, err)
		assert.Equal(t, map[string]indexedRole{
			rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorWrittenCID},
			rosterOwner:     {Role: communities.RoleModerator, CID: rosterOwnerWrittenCID},
		}, f.indexedRoles(t), "M is the only owner and O a moderator")
		assert.Equal(t, rosterHeadOneRev, f.gateRev(t, rosterModeratorURI), "M's record gate holds the commit rev")
		assert.Equal(t, rosterHeadOneRev, f.gateRev(t, rosterOwnerURI), "O's record gate holds the commit rev")
		assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once")
		assert.Equal(t, []storedAction{roleChangeAction("transfer-ownership", rosterOwner, false, rosterModerator)}, f.actions(t),
			"one transfer-ownership action naming O")
		assert.Equal(t, "accepted", f.invitationState(t, offer))
	})

	t.Run("a failure at the second index change rolls back both", func(t *testing.T) {
		t.Parallel()
		f := newCommunityActionDBFixture(t)
		offer := rosterOwnershipOfferID
		f.seedInvitation(t, rosterInvitation(offer, rosterModerator, communities.RoleOwner, rosterOwner, false))
		// Each community_roles row change draws from a sequence, which a
		// rollback does not return; the second change raises. Only the
		// fixture's community is in this test's database.
		_, err := f.db.Exec(`CREATE SEQUENCE roster_role_changes`)
		require.NoError(t, err)
		_, err = f.db.Exec(`
			CREATE FUNCTION fail_second_role_change() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF nextval('roster_role_changes') >= 2 THEN
					RAISE EXCEPTION 'injected failure at the second community_roles change';
				END IF;
				RETURN NULL;
			END
			$$
		`)
		require.NoError(t, err)
		_, err = f.db.Exec(`
			CREATE TRIGGER fail_second_role_change AFTER INSERT OR UPDATE OR DELETE ON community_roles
			FOR EACH ROW EXECUTE FUNCTION fail_second_role_change()
		`)
		require.NoError(t, err)

		_, err = f.acceptInvitation(rosterModerator, offer, rosterStartVersion, "accept-1")

		assert.Error(t, err, "the injected failure fails the accept")
		assert.Equal(t, seededRoles(), f.indexedRoles(t), "neither index change is visible")
		assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
		assert.Empty(t, f.actions(t), "no action")
		assert.Equal(t, "open", f.invitationState(t, offer), "the offer stays open")
	})
}

// B12: an instance admin offers ownership of an ownerless community to an
// account with no role; once it accepts it is the sole owner, from one create
// write, and one transfer-ownership action names the admin as admin-acting.
func TestCommunityInvitationDB_AdminOffersOwnershipOfAnOwnerlessCommunity(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	_, err := f.db.Exec(`DELETE FROM community_roles WHERE community_did = $1 AND role = 'owner'`, rosterCommunity)
	require.NoError(t, err, "the community has no owner")
	f.repo.mu.Lock()
	delete(f.repo.records, communities.RoleCollection+"/"+rosterOwnerKey)
	f.repo.mu.Unlock()
	offer := rosterOwnershipOfferID
	f.seedInvitation(t, rosterInvitation(offer, rosterOutsider, communities.RoleOwner, rosterAdmin, true))

	_, err = f.acceptInvitation(rosterOutsider, offer, rosterStartVersion, "accept-1")

	require.NoError(t, err)
	assert.Equal(t, map[string]indexedRole{
		rosterModerator: {Role: communities.RoleModerator, CID: rosterModeratorCID},
		rosterOutsider:  {Role: communities.RoleOwner, CID: rosterOutsiderWrittenCID},
	}, f.indexedRoles(t), "the invitee is the sole owner")
	assert.Equal(t, []writtenBatch{{
		Writes: []writtenRecord{{
			Op: "create", Collection: "social.coves.community.moderator", RKey: rosterOutsiderKey,
			Record: roleBody(rosterOutsider, "owner", "2026-11-13T12:00:00.000Z"),
		}},
		SwapCommit: rosterHeadZeroCID,
	}}, decodeBatches(t, f.repo.batchesSnapshot()), "one create of the invitee's owner record, nothing else")
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once")
	assert.Equal(t, []storedAction{roleChangeAction("transfer-ownership", rosterAdmin, true, rosterOutsider)}, f.actions(t),
		"one transfer-ownership action naming the admin, admin-acting")
}

// Q5/Q10: an instance admin offers ownership of a community that has owner O
// to an account with no role. The accept widens its locks to O, then writes
// the invitee's owner record and O's record rewritten as moderator in one
// call; both are indexed with their gates at the commit rev, the version
// advances once and one transfer-ownership action names the admin as
// admin-acting.
func TestCommunityInvitationDB_AdminOffersOwnershipOfAnOwnedCommunity(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	offer := rosterOwnershipOfferID
	f.seedInvitation(t, rosterInvitation(offer, rosterOutsider, communities.RoleOwner, rosterAdmin, true))

	result, err := f.acceptInvitation(rosterOutsider, offer, rosterStartVersion, "accept-1")

	require.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.EqualValues(t, 8, result.Version)
	}
	batches := decodeBatches(t, f.repo.batchesSnapshot())
	require.Len(t, batches, 1, "exactly one ApplyWrites call")
	assert.Equal(t, rosterHeadZeroCID, batches[0].SwapCommit, "guarded by the head")
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
	assert.Equal(t, map[string]indexedRole{
		rosterOutsider:  {Role: communities.RoleOwner, CID: rosterOutsiderWrittenCID},
		rosterOwner:     {Role: communities.RoleModerator, CID: rosterOwnerWrittenCID},
		rosterModerator: {Role: communities.RoleModerator, CID: rosterModeratorCID},
	}, f.indexedRoles(t), "the invitee is the only owner and O a moderator")
	assert.Equal(t, rosterHeadOneRev, f.gateRev(t, rosterOutsiderURI), "the invitee's record gate holds the commit rev")
	assert.Equal(t, rosterHeadOneRev, f.gateRev(t, rosterOwnerURI), "O's record gate holds the commit rev")
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once")
	assert.Equal(t, []storedAction{roleChangeAction("transfer-ownership", rosterAdmin, true, rosterOutsider)}, f.actions(t),
		"one transfer-ownership action naming the admin, admin-acting")
	assert.Equal(t, "accepted", f.invitationState(t, offer))
}

// lockOutcome is what a call run in a goroutine returned.
type lockOutcome struct {
	result *moderation.CommunityActionResult
	err    error
}

// awaitAdvisoryWaiter polls until a backend in the test database waits on an
// advisory lock held by holderPID. It fails if the waiting call returns
// first, or at ctx's deadline.
func (f *communityActionDBFixture) awaitAdvisoryWaiter(ctx context.Context, t *testing.T, holderPID int, returned <-chan lockOutcome, waiter string) {
	t.Helper()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for blocked := false; !blocked; {
		select {
		case got := <-returned:
			t.Fatalf("%s returned while the lock was held: %v", waiter, got.err)
		case <-ctx.Done():
			t.Fatalf("%s never waited on an advisory lock held by backend %d", waiter, holderPID)
		case <-poll.C:
		}
		require.NoError(t, f.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock' AND wait_event = 'advisory'
				  AND $1 = ANY(pg_blocking_pids(pid))
			)
		`, holderPID).Scan(&blocked), "read the %s's lock wait", waiter)
	}
}

// pausedActionPID is the backend of the community action transaction paused
// in a hook: the one community-actions connection idle inside a transaction.
func (f *communityActionDBFixture) pausedActionPID(ctx context.Context, t *testing.T) int {
	t.Helper()
	var pid int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT pid FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = 'community-actions'
		  AND state = 'idle in transaction'
	`).Scan(&pid), "find the paused action's backend")
	return pid
}

// pauseAtBeforeWrite returns a service whose requests pause before their
// repo write, a channel closed when one pauses, and the function that
// resumes it.
func (f *communityActionDBFixture) pauseAtBeforeWrite(ctx context.Context) (*moderation.CommunityActionService, <-chan struct{}, func()) {
	paused := make(chan struct{})
	resume := make(chan struct{})
	service := f.serviceWithHooks(moderation.CommunityActionHooks{
		BeforeWrite: func(context.Context) {
			close(paused)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		},
	})
	return service, paused, func() { close(resume) }
}

// B6c with R1-5: the invitee's community ban-subject lock serializes ban
// indexing with invitation and acceptance. (i) and (ii): an invite and an
// accept wait on the lock held by a transaction indexing an in-force ban on
// the invitee, then are refused InviteeBanned and write nothing. (iii): an
// accept paused before its write holds the lock against the ban index.
func TestCommunityInvitationDB_BanSubjectLockSerializesBanIndexing(t *testing.T) {
	t.Parallel()
	// refusedAfterBanCommits holds the invitee's ban-subject lock in a test
	// transaction that indexes an in-force ban, runs call, observes it wait
	// on that lock, commits, and returns what call returned.
	refusedAfterBanCommits := func(ctx context.Context, t *testing.T, f *communityActionDBFixture,
		call func() (*moderation.CommunityActionResult, error),
	) lockOutcome {
		t.Helper()
		ban, err := f.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = ban.Rollback() }()
		var banPID int
		require.NoError(t, ban.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&banPID))
		_, err = ban.ExecContext(ctx, `SELECT pg_advisory_xact_lock(`+communities.BanSubjectLockKeySQL+`)`, rosterOutsider)
		require.NoError(t, err, "the ban transaction takes the invitee's ban-subject lock")
		_, err = ban.ExecContext(ctx, `
			INSERT INTO community_bans (community_did, subject_did, record_key, record_cid, reason, expires_at, indexed_at)
			VALUES ($1, $2, '3kinviteeban', 'bafyreiinviteeban', 'spam', NULL, NOW())
		`, rosterCommunity, rosterOutsider)
		require.NoError(t, err, "the ban transaction indexes an in-force ban")

		returned := make(chan lockOutcome, 1)
		go func() {
			result, err := call()
			returned <- lockOutcome{result, err}
		}()
		f.awaitAdvisoryWaiter(ctx, t, banPID, returned, "the invitation call")
		require.NoError(t, ban.Commit(), "the ban commits")
		select {
		case got := <-returned:
			return got
		case <-ctx.Done():
			t.Fatal("the invitation call did not return after the ban committed")
			return lockOutcome{}
		}
	}

	tests := []struct {
		name string
		run  func(ctx context.Context, t *testing.T, f *communityActionDBFixture)
	}{
		{
			name: "invite waits on the ban-subject lock and is refused",
			run: func(ctx context.Context, t *testing.T, f *communityActionDBFixture) {
				got := refusedAfterBanCommits(ctx, t, f, func() (*moderation.CommunityActionResult, error) {
					return f.invitations.InviteModerator(ctx, moderation.InviteRequest{
						ActorDID: rosterOwner, CommunityDID: rosterCommunity, Invitee: rosterOutsider,
						ExpectedVersion: rosterStartVersion, IdempotencyKey: "invite-1",
					})
				})

				assert.ErrorIs(t, got.err, moderation.ErrInviteeBanned)
				assert.Nil(t, got.result, "the refused invite has no result")
				assert.Zero(t, f.invitationCount(t), "no invitation")
				assert.Empty(t, f.notificationRows(t), "no notification")
				assert.Empty(t, f.idempotencyActors(t, "invite-1"), "no idempotency row")
				assert.Equal(t, seededRoles(), f.indexedRoles(t), "the role index is unchanged")
				assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
			},
		},
		{
			name: "accept waits on the ban-subject lock and is refused",
			run: func(ctx context.Context, t *testing.T, f *communityActionDBFixture) {
				f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))

				got := refusedAfterBanCommits(ctx, t, f, func() (*moderation.CommunityActionResult, error) {
					return f.service.AcceptInvitation(ctx, moderation.AcceptInvitationRequest{
						ActorDID: rosterOutsider, InvitationID: rosterModeratorInvitationID,
						ExpectedVersion: rosterStartVersion, IdempotencyKey: "accept-1",
					})
				})

				assert.ErrorIs(t, got.err, moderation.ErrInviteeBanned)
				assert.Nil(t, got.result, "the refused accept has no result")
				assert.Empty(t, f.repo.batchesSnapshot(), "no PDS write")
				assert.Equal(t, seededRoles(), f.indexedRoles(t), "the role index is unchanged")
				assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
				assert.Empty(t, f.actions(t), "no action")
				assert.Empty(t, f.intents(t), "no intent")
				assert.Empty(t, f.idempotencyActors(t, "accept-1"), "no idempotency row")
				assert.Equal(t, "open", f.invitationState(t, rosterModeratorInvitationID), "the invitation stays open")
			},
		},
		{
			name: "accept paused before its write holds the ban-subject lock against ban indexing",
			run: func(ctx context.Context, t *testing.T, f *communityActionDBFixture) {
				f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, rosterOutsider, communities.RoleModerator, rosterOwner, false))
				accepting, paused, resume := f.pauseAtBeforeWrite(ctx)
				accepted := make(chan lockOutcome, 1)
				go func() {
					result, err := accepting.AcceptInvitation(ctx, moderation.AcceptInvitationRequest{
						ActorDID: rosterOutsider, InvitationID: rosterModeratorInvitationID,
						ExpectedVersion: rosterStartVersion, IdempotencyKey: "accept-1",
					})
					accepted <- lockOutcome{result, err}
				}()
				select {
				case <-paused:
				case got := <-accepted:
					t.Fatalf("the accept returned before pausing with its locks held: %v", got.err)
				case <-ctx.Done():
					t.Fatal("the accept never reached the before-write pause")
				}
				acceptPID := f.pausedActionPID(ctx, t)

				banned := make(chan lockOutcome, 1)
				go func() {
					err := postgres.NewCommunityBanRepository(f.db).UpsertCommunityBan(ctx, communities.CommunityBan{
						CommunityDID: rosterCommunity, SubjectDID: rosterOutsider,
						RecordKey: "3kinviteeban", RecordCID: "bafyreiinviteeban", Reason: "spam",
					})
					banned <- lockOutcome{err: err}
				}()
				f.awaitAdvisoryWaiter(ctx, t, acceptPID, banned, "the ban indexing")

				resume()
				select {
				case got := <-accepted:
					require.NoError(t, got.err, "the accept completes once resumed")
				case <-ctx.Done():
					t.Fatal("the accept did not finish after resuming")
				}
				select {
				case got := <-banned:
					require.NoError(t, got.err, "the ban indexes once the accept commits")
				case <-ctx.Done():
					t.Fatal("the ban indexing did not finish after the accept committed")
				}

				want := seededRoles()
				want[rosterOutsider] = indexedRole{Role: communities.RoleModerator, CID: rosterOutsiderWrittenCID}
				assert.Equal(t, want, f.indexedRoles(t), "the accept indexed the invitee as moderator before the ban")
				assert.Equal(t, "accepted", f.invitationState(t, rosterModeratorInvitationID))
				var bans int
				require.NoError(t, f.db.QueryRow(`
					SELECT COUNT(*) FROM community_bans WHERE community_did = $1 AND subject_did = $2
				`, rosterCommunity, rosterOutsider).Scan(&bans))
				assert.Equal(t, 1, bans, "the ban is indexed after the accept")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			tc.run(ctx, t, f)
		})
	}
}

// B13 with R1-5: two admin offers of the ownership of an ownerless community
// to distinct invitees, moderator M and the outsider, whose accepts share no
// account, record or ban-subject lock: only the roster lock serializes them.
// M's accept paused before its write holds it; the outsider's accept waits on
// it and, once M's commits, is refused. The stored roles hold one owner: M.
func TestCommunityInvitationDB_ConcurrentOwnershipAcceptancesLeaveOneOwner(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := f.db.Exec(`DELETE FROM community_roles WHERE community_did = $1 AND role = 'owner'`, rosterCommunity)
	require.NoError(t, err, "the community has no owner")
	f.repo.mu.Lock()
	delete(f.repo.records, communities.RoleCollection+"/"+rosterOwnerKey)
	f.repo.mu.Unlock()
	const outsiderOfferID = "inv-ownership-outsider"
	f.seedInvitation(t, rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterAdmin, true))
	f.seedInvitation(t, rosterInvitation(outsiderOfferID, rosterOutsider, communities.RoleOwner, rosterAdmin, true))

	accepting, paused, resume := f.pauseAtBeforeWrite(ctx)
	acceptedA := make(chan lockOutcome, 1)
	go func() {
		result, err := accepting.AcceptInvitation(ctx, moderation.AcceptInvitationRequest{
			ActorDID: rosterModerator, InvitationID: rosterOwnershipOfferID,
			ExpectedVersion: rosterStartVersion, IdempotencyKey: "accept-a",
		})
		acceptedA <- lockOutcome{result, err}
	}()
	select {
	case <-paused:
	case got := <-acceptedA:
		t.Fatalf("M's accept returned before pausing with its locks held: %v", got.err)
	case <-ctx.Done():
		t.Fatal("M's accept never reached the before-write pause")
	}
	holderPID := f.pausedActionPID(ctx, t)

	acceptedB := make(chan lockOutcome, 1)
	go func() {
		result, err := f.service.AcceptInvitation(ctx, moderation.AcceptInvitationRequest{
			ActorDID: rosterOutsider, InvitationID: outsiderOfferID,
			ExpectedVersion: rosterStartVersion, IdempotencyKey: "accept-b",
		})
		acceptedB <- lockOutcome{result, err}
	}()
	f.awaitAdvisoryWaiter(ctx, t, holderPID, acceptedB, "the outsider's accept")

	resume()
	select {
	case got := <-acceptedA:
		require.NoError(t, got.err, "M's accept completes once resumed")
	case <-ctx.Done():
		t.Fatal("M's accept did not finish after resuming")
	}
	select {
	case got := <-acceptedB:
		assert.Error(t, got.err, "the outsider's accept is refused or a state conflict")
		assert.NotErrorIs(t, got.err, moderation.ErrModerationUnavailable, "a refusal, not an unavailable store")
		assert.Nil(t, got.result, "the refused accept has no result")
	case <-ctx.Done():
		t.Fatal("the outsider's accept did not finish after M's committed")
	}

	rows, err := f.db.Query(`SELECT subject_did FROM community_roles WHERE community_did = $1 AND role = 'owner'`, rosterCommunity)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	owners := []string{}
	for rows.Next() {
		var subject string
		require.NoError(t, rows.Scan(&subject))
		owners = append(owners, subject)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{rosterModerator}, owners, "exactly one stored owner row: M's")
	assert.Equal(t, map[string]indexedRole{
		rosterModerator: {Role: communities.RoleOwner, CID: rosterModeratorWrittenCID},
	}, f.indexedRoles(t), "M owns and the outsider holds no role")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "one PDS commit: M's")
	assert.False(t, f.repo.hasRecord(communities.RoleCollection, rosterOutsiderKey), "no record for the outsider")
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced once")
	assert.Equal(t, []storedAction{roleChangeAction("transfer-ownership", rosterAdmin, true, rosterModerator)}, f.actions(t),
		"one transfer-ownership action naming the admin, admin-acting")
	assert.Equal(t, "accepted", f.invitationState(t, rosterOwnershipOfferID))
	assert.Equal(t, "open", f.invitationState(t, outsiderOfferID), "the outsider's offer is not accepted")
}
