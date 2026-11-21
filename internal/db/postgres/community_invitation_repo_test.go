//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	invitationCommunityOne   = "did:plc:invitationone"
	invitationCommunityTwo   = "did:plc:invitationtwo"
	invitationCommunityThree = "did:plc:invitationthree"
	invitationInviteeA       = "did:plc:inviteea"
	invitationInviteeB       = "did:plc:inviteeb"
	invitationInviter        = "did:plc:invitationinviter"
)

// invitationReaderNow is the read time of every query below.
var invitationReaderNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// invitationAt is an open moderator invitation created at created, expiring
// 14 days later.
func invitationAt(id, communityDID, inviteeDID string, created time.Time) communities.Invitation {
	return communities.Invitation{
		ID: id, CommunityDID: communityDID, InviteeDID: inviteeDID, Role: communities.RoleModerator,
		InviterDID: invitationInviter, State: communities.InvitationOpen,
		CreatedAt: created, ExpiresAt: created.Add(14 * 24 * time.Hour),
	}
}

// invitationStep is one store change, made in its own action transaction on
// the community's roster lock.
type invitationStep struct {
	communityDID string
	apply        func(tx moderation.CommunityActionTx) error
}

func applyInvitationSteps(t *testing.T, store *CommunityActionRepository, steps []invitationStep) {
	t.Helper()
	ctx := context.Background()
	for i, step := range steps {
		tx, err := store.BeginAction(ctx, moderation.CommunityActionLock{CommunityDID: step.communityDID, Roster: true})
		require.NoError(t, err)
		if err := step.apply(tx); err != nil {
			_ = tx.Rollback()
			require.NoError(t, err, "invitation step %d", i)
		}
		require.NoError(t, tx.Commit(), "commit invitation step %d", i)
	}
}

func utcInvitationList(invitations []communities.Invitation) []communities.Invitation {
	out := make([]communities.Invitation, 0, len(invitations))
	for _, inv := range invitations {
		inv.CreatedAt, inv.ExpiresAt = inv.CreatedAt.UTC(), inv.ExpiresAt.UTC()
		out = append(out, inv)
	}
	return out
}

// B7-T1: the reader returns only the asked invitee's or community's pending
// invitations, newest first, leaving out lapsed (at or after expires_at),
// superseded, declined and accepted ones.
func TestCommunityInvitationRepository_ListsOnlyPendingInvitations(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewCommunityActionRepository(db, db)
	reader := NewCommunityInvitationRepository(db)
	ctx := context.Background()
	day := 24 * time.Hour

	aSuperseded := invitationAt("inv-a1-superseded", invitationCommunityOne, invitationInviteeA, invitationReaderNow.Add(-3*day))
	aDeclined := invitationAt("inv-a1-declined", invitationCommunityOne, invitationInviteeA, invitationReaderNow.Add(-2*day))
	aAccepted := invitationAt("inv-a1-accepted", invitationCommunityOne, invitationInviteeA, invitationReaderNow.Add(-36*time.Hour))
	aPendingOne := invitationAt("inv-a1-pending", invitationCommunityOne, invitationInviteeA, invitationReaderNow.Add(-day))
	// Created exactly 14 days before now: it lapses at now.
	aLapsedTwo := invitationAt("inv-a2-lapsed", invitationCommunityTwo, invitationInviteeA, invitationReaderNow.Add(-14*day))
	aPendingThree := invitationAt("inv-a3-pending", invitationCommunityThree, invitationInviteeA, invitationReaderNow.Add(-2*time.Hour))
	bPendingOne := invitationAt("inv-b1-pending", invitationCommunityOne, invitationInviteeB, invitationReaderNow.Add(-12*time.Hour))
	// One second short of 14 days: still pending at now.
	bPendingTwo := invitationAt("inv-b2-pending", invitationCommunityTwo, invitationInviteeB, invitationReaderNow.Add(-14*day+time.Second))
	bPendingTwo.Role = communities.RoleOwner

	create := func(inv communities.Invitation) invitationStep {
		return invitationStep{inv.CommunityDID, func(tx moderation.CommunityActionTx) error { return tx.CreateInvitation(ctx, inv) }}
	}
	setState := func(inv communities.Invitation, state communities.InvitationState) invitationStep {
		return invitationStep{inv.CommunityDID, func(tx moderation.CommunityActionTx) error {
			return tx.SetInvitationState(ctx, inv.ID, state)
		}}
	}
	// Each create supersedes the invitee's open invitation in that community.
	applyInvitationSteps(t, store, []invitationStep{
		create(aSuperseded),
		create(aDeclined), setState(aDeclined, communities.InvitationDeclined),
		create(aAccepted), setState(aAccepted, communities.InvitationAccepted),
		create(aPendingOne),
		create(aLapsedTwo),
		create(aPendingThree),
		create(bPendingOne),
		create(bPendingTwo),
	})

	forA, err := reader.ListPendingInvitationsForInvitee(ctx, invitationInviteeA, invitationReaderNow)
	require.NoError(t, err)
	assert.Equal(t, []communities.Invitation{aPendingThree, aPendingOne}, utcInvitationList(forA),
		"A's pending invitations, newest first")

	forB, err := reader.ListPendingInvitationsForInvitee(ctx, invitationInviteeB, invitationReaderNow)
	require.NoError(t, err)
	assert.Equal(t, []communities.Invitation{bPendingOne, bPendingTwo}, utcInvitationList(forB),
		"B's pending invitations, newest first, including one a second short of lapsing")

	inOne, err := reader.ListPendingInvitationsForCommunity(ctx, invitationCommunityOne, invitationReaderNow)
	require.NoError(t, err)
	assert.Equal(t, []communities.Invitation{bPendingOne, aPendingOne}, utcInvitationList(inOne),
		"community one's pending invitations, newest first")

	inTwo, err := reader.ListPendingInvitationsForCommunity(ctx, invitationCommunityTwo, invitationReaderNow)
	require.NoError(t, err)
	assert.Equal(t, []communities.Invitation{bPendingTwo}, utcInvitationList(inTwo),
		"community two's lapsed invitation is left out")

	pending, err := reader.PendingInvitation(ctx, invitationCommunityOne, invitationInviteeA, invitationReaderNow)
	require.NoError(t, err)
	if assert.NotNil(t, pending, "A's pending invitation in community one") {
		assert.Equal(t, []communities.Invitation{aPendingOne}, utcInvitationList([]communities.Invitation{*pending}))
	}
	lapsed, err := reader.PendingInvitation(ctx, invitationCommunityTwo, invitationInviteeA, invitationReaderNow)
	require.NoError(t, err)
	assert.Nil(t, lapsed, "a lapsed invitation is not pending")
	// A second before it lapses it is still pending.
	beforeLapse, err := reader.PendingInvitation(ctx, invitationCommunityTwo, invitationInviteeA, invitationReaderNow.Add(-time.Second))
	require.NoError(t, err)
	assert.NotNil(t, beforeLapse, "the invitation is pending a second before it lapses")
	none, err := reader.PendingInvitation(ctx, invitationCommunityThree, invitationInviteeB, invitationReaderNow)
	require.NoError(t, err)
	assert.Nil(t, none, "B has no invitation in community three")
}

// An invitation authorized before its inviter's erasure, created while the
// erasure transaction holds the inviter's exclusive erasure lock, waits on the
// shared erasure gate and then writes nothing: no invitation and no
// notification naming the erased inviter (chunk 07 R3).
func TestCommunityInvitationRepository_CreateInvitationWaitsForInviterErasure(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	store := NewCommunityActionRepository(db, db)
	inviterDID := "did:plc:invitationerasedinviter" + testkit.UniqueID(t)
	inviteeDID := "did:plc:invitationerasureinvitee" + testkit.UniqueID(t)
	createTestUser(t, db, "inviter"+testkit.UniqueID(t)+".test", inviterDID)
	createTestUser(t, db, "invitee"+testkit.UniqueID(t)+".test", inviteeDID)
	inv := invitationAt("inv-erased-inviter", invitationCommunityOne, inviteeDID, invitationReaderNow)
	inv.InviterDID = inviterDID

	// The erasure side: the exclusive erasure lock, the marker, and the
	// actor's notifications and user row gone, uncommitted until released.
	erasure, erasureProcessID := notificationRaceTransaction(t, db, ctx)
	_, err := erasure.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+ErasureLockKeySQL+")", inviterDID)
	require.NoError(t, err)
	_, err = erasure.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, inviterDID)
	require.NoError(t, err)
	_, err = erasure.ExecContext(ctx, `DELETE FROM notifications WHERE actor_did = $1`, inviterDID)
	require.NoError(t, err)
	_, err = erasure.ExecContext(ctx, `DELETE FROM users WHERE did = $1`, inviterDID)
	require.NoError(t, err)

	type actionStart struct {
		processID int
		err       error
	}
	started := make(chan actionStart, 1)
	results := make(chan error, 1)
	go func() {
		tx, beginError := store.BeginAction(ctx, moderation.CommunityActionLock{CommunityDID: inv.CommunityDID, Roster: true})
		if beginError != nil {
			started <- actionStart{err: beginError}
			return
		}
		var processID int
		if processError := tx.(*communityActionTransaction).tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&processID); processError != nil {
			_ = tx.Rollback()
			started <- actionStart{err: processError}
			return
		}
		started <- actionStart{processID: processID}
		if createError := tx.CreateInvitation(ctx, inv); createError != nil {
			_ = tx.Rollback()
			results <- createError
			return
		}
		results <- tx.Commit()
	}()

	var action actionStart
	select {
	case action = <-started:
	case <-ctx.Done():
		require.FailNow(t, "action transaction did not start", ctx.Err().Error())
	}
	require.NoError(t, action.err)

	returnedBeforeErasure := false
	var result error
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		select {
		case result = <-results:
			returnedBeforeErasure = true
			return true, nil
		default:
		}
		var waiting bool
		err := erasure.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks waiter
				JOIN pg_locks holder ON holder.locktype = waiter.locktype
					AND holder.database = waiter.database
					AND holder.classid = waiter.classid
					AND holder.objid = waiter.objid
					AND holder.objsubid = waiter.objsubid
				WHERE holder.pid = $1 AND waiter.pid = $2
					AND holder.locktype = 'advisory'
					AND holder.granted AND holder.mode = 'ExclusiveLock'
					AND NOT waiter.granted AND waiter.mode = 'ShareLock'
			)`, erasureProcessID, action.processID).Scan(&waiting)
		return waiting, err
	}, testkit.WithDescription("CreateInvitation waiting on the inviter's exclusive erasure lock, or returning"))

	require.NoError(t, erasure.Commit())
	if !returnedBeforeErasure {
		select {
		case result = <-results:
		case <-ctx.Done():
			require.FailNow(t, "CreateInvitation did not return after the erasure committed", ctx.Err().Error())
		}
	}
	require.ErrorIs(t, result, communities.ErrInvitationPartyErased,
		"an invitation whose inviter was erased first is refused")

	var notificationCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE actor_did = $1`, inviterDID).Scan(&notificationCount))
	assert.Zero(t, notificationCount, "no notification may name an inviter erased before the invitation committed")
	assert.False(t, returnedBeforeErasure, "CreateInvitation must wait on the inviter's shared erasure gate")
	var invitationCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM community_role_invitations WHERE id = $1`, inv.ID).Scan(&invitationCount))
	assert.Zero(t, invitationCount, "no invitation names an inviter erased before it committed")
}

// B12 (chunk 07 R3): CreateInvitation naming an erased inviter or an erased
// invitee writes nothing: no new invitation, the invitee's open invitation is
// not superseded, and no notification is written. It returns
// communities.ErrInvitationPartyErased.
func TestCommunityInvitationRepository_CreateInvitationRefusesAnErasedParty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		erase func(inviterDID, inviteeDID string) string
	}{
		{name: "erased inviter", erase: func(inviterDID, _ string) string { return inviterDID }},
		{name: "erased invitee", erase: func(_, inviteeDID string) string { return inviteeDID }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx := context.Background()
			store := NewCommunityActionRepository(db, db)
			inviterDID := "did:plc:partyerasedinviter" + testkit.UniqueID(t)
			inviteeDID := "did:plc:partyerasedinvitee" + testkit.UniqueID(t)
			createTestUser(t, db, "inviter"+testkit.UniqueID(t)+".test", inviterDID)
			createTestUser(t, db, "invitee"+testkit.UniqueID(t)+".test", inviteeDID)
			createTestUser(t, db, "otherinviter"+testkit.UniqueID(t)+".test", invitationInviter)
			// The invitee's existing open invitation from another inviter.
			earlier := invitationAt("inv-party-earlier", invitationCommunityOne, inviteeDID, invitationReaderNow.Add(-time.Hour))
			applyInvitationSteps(t, store, []invitationStep{{earlier.CommunityDID, func(tx moderation.CommunityActionTx) error {
				return tx.CreateInvitation(ctx, earlier)
			}}})
			_, err := db.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, tc.erase(inviterDID, inviteeDID))
			require.NoError(t, err)
			invitationsBefore := erasureRoleRows(t, db, "community_role_invitations", "true")
			notificationsBefore := erasureRoleRows(t, db, "notifications", "true")
			require.Contains(t, invitationsBefore, `"state": "open"`, "checkpoint: the earlier invitation is open")

			inv := invitationAt("inv-party-erased", invitationCommunityOne, inviteeDID, invitationReaderNow)
			inv.InviterDID = inviterDID
			tx, err := store.BeginAction(ctx, moderation.CommunityActionLock{CommunityDID: inv.CommunityDID, Roster: true})
			require.NoError(t, err)
			createError := tx.CreateInvitation(ctx, inv)
			if createError == nil {
				require.NoError(t, tx.Commit())
			} else {
				require.NoError(t, tx.Rollback())
			}

			require.ErrorIs(t, createError, communities.ErrInvitationPartyErased)
			assert.Equal(t, invitationsBefore, erasureRoleRows(t, db, "community_role_invitations", "true"),
				"no invitation is created and the earlier one is not superseded")
			assert.Equal(t, notificationsBefore, erasureRoleRows(t, db, "notifications", "true"),
				"no notification is written")
		})
	}
}
