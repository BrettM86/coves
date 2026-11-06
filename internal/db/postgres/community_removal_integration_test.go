//go:build integration

package postgres_test

import (
	"database/sql"
	"testing"

	"Coves/internal/core/labels"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// communityRemovalHarness is the account-ban harness pointed at an indexed
// community instead of a user account: account is the bare community DID and
// subject is at://<communityDID>, so its row, decision, action and label
// readers apply unchanged.
type communityRemovalHarness struct {
	*accountBanHarness
}

// newCommunityRemovalHarness builds the harness over a fresh indexed
// community; store, when not nil, wraps the Postgres moderation repository.
func newCommunityRemovalHarness(t *testing.T, store func(repository *postgres.ModerationRepository, communityDID string) moderation.Store) *communityRemovalHarness {
	t.Helper()
	db := testkit.DB(t)
	name := testkit.UniqueIDWithPrefix(t, "removedcomm")
	community, err := fixtures.Community(t.Context(), db, name, "owner"+name)
	require.NoError(t, err)
	repository := postgres.NewModerationRepository(db)
	var moderationStore moderation.Store = repository
	if store != nil {
		moderationStore = store(repository, community)
	}
	clock := &accountBanTestClock{now: accountBanInstant(t, accountBanStartTime)}
	return &communityRemovalHarness{accountBanHarness: &accountBanHarness{
		db:      db,
		service: newPostgresAccountModerationService(db, moderationStore, issuanceLabelSigningKey(t), clock.Now),
		clock:   clock,
		account: community,
		subject: "at://" + community,
		actor:   fixtures.DID("communityremovaladmin"),
	}}
}

func (h *communityRemovalHarness) removeRequest(key, version string) moderation.RemoveCommunityRequest {
	return moderation.RemoveCommunityRequest{Subject: h.account, ExpectedVersion: version, IdempotencyKey: key, Reason: accountBanSpam}
}

func (h *communityRemovalHarness) restoreRequest(key, version string) moderation.RestoreCommunityRequest {
	return moderation.RestoreCommunityRequest{Subject: h.account, ExpectedVersion: version, IdempotencyKey: key, Reason: issuanceRestoreReason}
}

// remove applies request and requires an applied remove action on the community.
func (h *communityRemovalHarness) remove(t *testing.T, request moderation.RemoveCommunityRequest) *moderation.MutationResult {
	t.Helper()
	result, err := h.service.RemoveCommunity(t.Context(), h.actor, request)
	require.NoError(t, err)
	require.NotNil(t, result, "removeCommunity returned no mutation result")
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	require.Equal(t, moderation.ActionRemove, result.Action.Action)
	return result
}

// restore applies request and requires an applied restore action on the community.
func (h *communityRemovalHarness) restore(t *testing.T, request moderation.RestoreCommunityRequest) *moderation.MutationResult {
	t.Helper()
	result, err := h.service.RestoreCommunity(t.Context(), h.actor, request)
	require.NoError(t, err)
	require.NotNil(t, result, "restoreCommunity returned no mutation result")
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	require.Equal(t, moderation.ActionRestore, result.Action.Action)
	return result
}

// failCommunityLabel is the failingLabelInsertStore that fails the community's
// takedown (neg false) or its negation (neg true).
func failCommunityLabel(neg bool) func(repository *postgres.ModerationRepository, communityDID string) moderation.Store {
	return func(repository *postgres.ModerationRepository, communityDID string) moderation.Store {
		return failingLabelInsertStore{ModerationRepository: repository, fail: func(issue labels.Issue) bool {
			return issue.URI == communityDID && issue.Neg == neg
		}}
	}
}

func TestCommunityRemovalAndRestore(t *testing.T) {
	removedRows := accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1}
	for _, test := range []struct {
		name  string
		store func(repository *postgres.ModerationRepository, communityDID string) moderation.Store
		run   func(t *testing.T, h *communityRemovalHarness)
	}{
		{name: "remove then restore", run: func(t *testing.T, h *communityRemovalHarness) {
			removed := h.remove(t, h.removeRequest("remove", "v0"))
			assert.Equal(t, h.subject, removed.Action.SubjectURI)
			assert.Empty(t, removed.Action.SubjectCollection)
			assert.Equal(t, h.actor, removed.Action.ActorDID)
			assert.Equal(t, moderation.ScopeInstance, removed.Action.ScopeKind)
			assert.Equal(t, accountBanSpam, removed.Action.Reason)
			assert.Equal(t, "v1", removed.State.Version)
			assert.Equal(t, moderation.ModerationStateRemoved, removed.State.Moderation.State)
			assert.Equal(t, &moderation.ActionRef{ServiceDID: fixtures.InstanceDID(), ActionID: removed.Action.ID}, removed.State.LocalRemoval)
			assert.Equal(t, storedAccountAction{Action: moderation.ActionRemove}, h.storedAction(t, removed.Action.ID))
			assert.Equal(t, accountBanDecision{Active: true, ActionID: removed.Action.ID}, h.decision(t))
			assert.Equal(t, removedRows, h.rows(t))
			rows := h.labels(t)
			require.Len(t, rows, 1, "a community removal publishes one label")
			requireAccountLabel(t, rows[0], h.account, removed.Action.ID, false, nil)

			restored := h.restore(t, h.restoreRequest("restore", "v1"))
			assert.Equal(t, h.subject, restored.Action.SubjectURI)
			assert.Equal(t, removed.Action.ID, restored.Action.ReversesActionID)
			assert.Equal(t, "v2", restored.State.Version)
			assert.Equal(t, moderation.ModerationStateClear, restored.State.Moderation.State)
			assert.Nil(t, restored.State.LocalRemoval)
			assert.Equal(t, storedAccountAction{Action: moderation.ActionRestore, ReversesActionID: removed.Action.ID}, h.storedAction(t, restored.Action.ID))
			assert.Equal(t, accountBanDecision{Active: false, ActionID: removed.Action.ID}, h.decision(t))
			assert.Equal(t, accountBanRows{Actions: 2, InactiveDecisions: 1, Labels: 2, IdempotencyKeys: 2, Version: 2}, h.rows(t))
			rows = h.labels(t)
			require.Len(t, rows, 2, "a community restore publishes one negation")
			requireAccountLabel(t, rows[1], h.account, restored.Action.ID, true, nil)
			requireStrictlyIncreasingLabels(t, rows)
		}},
		{name: "a replay with the same key returns the original result and writes nothing", run: func(t *testing.T, h *communityRemovalHarness) {
			original := h.remove(t, h.removeRequest("remove", "v0"))
			replayed, err := h.service.RemoveCommunity(t.Context(), h.actor, h.removeRequest("remove", "v0"))
			require.NoError(t, err)
			assert.Equal(t, original, replayed)
			assert.Equal(t, removedRows, h.rows(t))
		}},
		{name: "a stale expected version is a state conflict", run: func(t *testing.T, h *communityRemovalHarness) {
			h.remove(t, h.removeRequest("remove", "v0"))
			_, err := h.service.RemoveCommunity(t.Context(), h.actor, h.removeRequest("remove-stale", "v0"))
			require.ErrorIs(t, err, moderation.ErrStateConflict)
			assert.Equal(t, removedRows, h.rows(t))
		}},
		{name: "the same key with a different reason is an idempotency conflict", run: func(t *testing.T, h *communityRemovalHarness) {
			h.remove(t, h.removeRequest("remove", "v0"))
			changed := h.removeRequest("remove", "v0")
			changed.Reason = accountBanHarass
			_, err := h.service.RemoveCommunity(t.Context(), h.actor, changed)
			require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
			assert.Equal(t, removedRows, h.rows(t))
		}},
		{name: "a restore reusing the removal's key is an idempotency conflict", run: func(t *testing.T, h *communityRemovalHarness) {
			removed := h.remove(t, h.removeRequest("shared-key", "v0"))
			_, err := h.service.RestoreCommunity(t.Context(), h.actor, moderation.RestoreCommunityRequest{
				Subject: h.account, ExpectedVersion: "v0", IdempotencyKey: "shared-key", Reason: accountBanSpam,
			})
			require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
			assert.Equal(t, removedRows, h.rows(t))
			assert.Equal(t, accountBanDecision{Active: true, ActionID: removed.Action.ID}, h.decision(t))
			assert.Len(t, h.labels(t), 1, "a refused restore publishes no negation")
		}},
		{name: "removing an already removed community is unchanged and writes no action, label or version", run: func(t *testing.T, h *communityRemovalHarness) {
			removed := h.remove(t, h.removeRequest("remove", "v0"))
			again, err := h.service.RemoveCommunity(t.Context(), h.actor, h.removeRequest("remove-again", "v1"))
			require.NoError(t, err)
			require.NotNil(t, again, "removeCommunity returned no mutation result")
			assert.Equal(t, moderation.OutcomeUnchanged, again.Outcome)
			assert.Nil(t, again.Action)
			assert.Equal(t, "v1", again.State.Version)
			rows := h.rows(t)
			assert.Equal(t, 1, rows.Actions)
			assert.Equal(t, 1, rows.ActiveDecisions)
			assert.Equal(t, 1, rows.Labels)
			assert.Equal(t, int64(1), rows.Version)
			assert.Equal(t, accountBanDecision{Active: true, ActionID: removed.Action.ID}, h.decision(t))
		}},
		{name: "restore without an active removal is a state conflict", run: func(t *testing.T, h *communityRemovalHarness) {
			_, err := h.service.RestoreCommunity(t.Context(), h.actor, h.restoreRequest("restore", "v0"))
			require.ErrorIs(t, err, moderation.ErrStateConflict)
			assert.Equal(t, accountBanRows{}, h.rows(t))
		}},
		{name: "subject state shows the version and the active removal", run: func(t *testing.T, h *communityRemovalHarness) {
			removed := h.remove(t, h.removeRequest("remove", "v0"))
			state := h.state(t)
			assert.Equal(t, h.subject, state.Subject)
			assert.Equal(t, "v1", state.Version)
			assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
			assert.Equal(t, &moderation.ActionRef{ServiceDID: fixtures.InstanceDID(), ActionID: removed.Action.ID}, state.LocalRemoval)

			h.restore(t, h.restoreRequest("restore", "v1"))
			state = h.state(t)
			assert.Equal(t, "v2", state.Version)
			assert.Equal(t, moderation.ModerationStateClear, state.Moderation.State)
			assert.Nil(t, state.LocalRemoval)
		}},
		{name: "the public action log lists the removal and the restore", run: func(t *testing.T, h *communityRemovalHarness) {
			removed := h.remove(t, h.removeRequest("remove", "v0"))
			restored := h.restore(t, h.restoreRequest("restore", "v1"))
			page, err := h.service.ListActions(t.Context(), moderation.ListActionsParams{})
			require.NoError(t, err)
			require.Len(t, page.Actions, 2)
			restoreView, removeView := page.Actions[0], page.Actions[1]
			assert.Equal(t, restored.Action.ID, restoreView.Ref.ActionID)
			assert.Equal(t, removed.Action.ID, removeView.Ref.ActionID)
			assert.Equal(t, moderation.ActionRestore, restoreView.Action)
			assert.Equal(t, moderation.ActionRemove, removeView.Action)
			assert.Equal(t, accountBanSpam, removeView.Reason)
			assert.Equal(t, &moderation.ActionRefView{ServiceDID: fixtures.InstanceDID(), ActionID: removed.Action.ID}, restoreView.Reverses)
			for _, view := range page.Actions {
				assert.Equal(t, &moderation.ActorRefView{DID: h.actor}, view.Actor, view.Ref.ActionID)
				assert.Equal(t, moderation.ScopeView{Kind: moderation.ScopeInstance}, view.Scope, view.Ref.ActionID)
				assert.Equal(t, &moderation.SubjectRefView{URI: h.subject}, view.Subject, view.Ref.ActionID)
				assert.Nil(t, view.ContentSnapshot, "a community action has no content snapshot")
			}
		}},
		{name: "a fresh publisher exports a takedown for an active community removal", run: func(t *testing.T, h *communityRemovalHarness) {
			unpublished := newPostgresAccountModerationService(h.db, postgres.NewModerationRepository(h.db), nil, nil)
			removed, err := unpublished.RemoveCommunity(t.Context(), h.actor, h.removeRequest("remove-unpublished", "v0"))
			require.NoError(t, err)
			require.NotNil(t, removed, "removeCommunity returned no mutation result")
			require.NotNil(t, removed.Action)
			require.Empty(t, h.labels(t), "a removal made with publication disabled publishes nothing")

			_, err = startLabelPublisher(t, h.db, issuanceLabelSigningKey(t), "")
			require.NoError(t, err)

			rows := h.labels(t)
			require.Len(t, rows, 1, "export publishes one label for the active community removal")
			requireAccountLabel(t, rows[0], h.account, removed.Action.ID, false, nil)
		}},
		{name: "a failed takedown label insert rolls the removal back", store: failCommunityLabel(false), run: func(t *testing.T, h *communityRemovalHarness) {
			_, err := h.service.RemoveCommunity(t.Context(), h.actor, h.removeRequest("remove", "v0"))
			require.Error(t, err)
			assert.ErrorContains(t, err, "injected account label insert failure")
			assert.Equal(t, accountBanRows{}, h.rows(t))
		}},
		{name: "a failed negation label insert rolls the restore back", store: failCommunityLabel(true), run: func(t *testing.T, h *communityRemovalHarness) {
			h.remove(t, h.removeRequest("remove", "v0"))
			_, err := h.service.RestoreCommunity(t.Context(), h.actor, h.restoreRequest("restore", "v1"))
			require.Error(t, err)
			assert.ErrorContains(t, err, "injected account label insert failure")
			assert.Equal(t, removedRows, h.rows(t))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, newCommunityRemovalHarness(t, test.store))
		})
	}
}

// A timed instance removal on at://<did> is always an account ban: RemoveCommunity
// never sets an expiry. So a communities row later written for a timed-banned
// account's DID must not turn the ban into a permanent community removal.
func TestCommunityRemovedIgnoresTimedAccountBan(t *testing.T) {
	t.Run("a permanent community removal is read", func(t *testing.T) {
		h := newCommunityRemovalHarness(t, nil)
		h.remove(t, h.removeRequest("remove", "v0"))
		removed, err := postgres.NewCommunityRemovalReader(h.db, fixtures.InstanceDID()).CommunityRemoved(t.Context(), h.account)
		require.NoError(t, err)
		assert.True(t, removed)
	})
	t.Run("a timed account ban on a DID indexed afterwards is not", func(t *testing.T) {
		h := newAccountBanHarness(t, nil)
		request := h.banRequest("ban", "v0")
		request.ExpiresAt = accountBanInstantPointer(t, "2030-01-01T00:00:00Z")
		h.ban(t, request)
		indexAccountAsCommunity(t, h)

		removed, err := postgres.NewCommunityRemovalReader(h.db, fixtures.InstanceDID()).CommunityRemoved(t.Context(), h.account)
		require.NoError(t, err)
		assert.False(t, removed, "a timed ban must not read as a community removal")
	})
	t.Run("removeCommunity supersedes a lapsed timed ban on a DID indexed afterwards", func(t *testing.T) {
		h := newAccountBanHarness(t, nil)
		request := h.banRequest("ban", "v0")
		request.ExpiresAt = accountBanInstantPointer(t, "2030-01-01T00:00:00Z")
		ban := h.ban(t, request)
		h.clock.now = accountBanInstant(t, "2030-01-02T00:00:00Z")
		indexAccountAsCommunity(t, h)
		reader := postgres.NewCommunityRemovalReader(h.db, fixtures.InstanceDID())

		state := h.state(t)
		assert.Equal(t, "v1", state.Version)
		assert.Equal(t, moderation.ModerationStateClear, state.Moderation.State, "a lapsed ban is no community removal")
		require.NotNil(t, state.LocalRemoval, "a lapsed account ban still shows as the active decision")
		require.NotNil(t, state.LocalRemoval.ExpiresAt)
		assert.Equal(t, accountBanInstant(t, "2030-01-01T00:00:00Z"), state.LocalRemoval.ExpiresAt.UTC())
		_, err := h.service.RestoreCommunity(t.Context(), h.actor, moderation.RestoreCommunityRequest{
			Subject: h.account, ExpectedVersion: "v1", IdempotencyKey: "restore", Reason: issuanceRestoreReason,
		})
		require.ErrorIs(t, err, moderation.ErrStateConflict, "restoreCommunity must not reverse a lapsed ban")
		assert.Equal(t, accountBanDecision{Active: true, ActionID: ban.Action.ID}, h.decision(t))

		removedResult, err := h.service.RemoveCommunity(t.Context(), h.actor, moderation.RemoveCommunityRequest{
			Subject: h.account, ExpectedVersion: "v1", IdempotencyKey: "remove", Reason: accountBanSpam,
		})
		require.NoError(t, err)
		require.NotNil(t, removedResult)
		require.Equal(t, moderation.OutcomeApplied, removedResult.Outcome)
		require.NotNil(t, removedResult.Action)
		assert.Nil(t, removedResult.Action.ExpiresAt)
		assert.Equal(t, accountBanDecision{Active: true, ActionID: removedResult.Action.ID}, h.decision(t))
		removed, err := reader.CommunityRemoved(t.Context(), h.account)
		require.NoError(t, err)
		assert.True(t, removed, "the community reads as removed")

		state = h.state(t)
		assert.Equal(t, "v2", state.Version)
		assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
		assert.Equal(t, &moderation.ActionRef{ServiceDID: fixtures.InstanceDID(), ActionID: removedResult.Action.ID}, state.LocalRemoval)
		rows := h.labels(t)
		require.Len(t, rows, 2, "the ban's expiring label and the community takedown")
		requireAccountLabel(t, rows[1], h.account, removedResult.Action.ID, false, nil)
	})
}

// indexAccountAsCommunity writes a communities row for the harness account's
// DID, as the consumer does once no ban is in force.
func indexAccountAsCommunity(t *testing.T, h *accountBanHarness) {
	t.Helper()
	insertCommunityRowForDID(t, h.db, h.account)
}

// insertCommunityRowForDID writes a communities row at did directly.
func insertCommunityRowForDID(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO communities (did, name, owner_did, created_by_did, hosted_by_did, handle, pds_url, created_at)
		VALUES ($1, $2, $1, $1, $3, $4, $5, NOW())
	`, did, "timedban", fixtures.InstanceDID(), testkit.UniqueIDWithPrefix(t, "c-timedban")+".coves.social", testkit.Endpoints().PDS.BaseURL)
	require.NoError(t, err)
}
