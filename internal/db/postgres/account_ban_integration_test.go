//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/labels"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	accountBanStartTime = "2029-12-31T23:00:00Z"
	accountBanSpam      = "social.coves.moderation.defs#reasonSpam"
	accountBanHarass    = "social.coves.moderation.defs#reasonHarassment"
	accountBanDoxing    = "social.coves.moderation.defs#reasonDoxing"
	accountBanIllegal   = "social.coves.moderation.defs#reasonIllegalContent"
)

// accountBanInstant parses an RFC 3339 literal.
func accountBanInstant(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed.UTC()
}

func accountBanInstantPointer(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed := accountBanInstant(t, value)
	return &parsed
}

// accountBanTestClock is the service clock a test moves by hand.
type accountBanTestClock struct {
	now time.Time
}

func (clock *accountBanTestClock) Now() time.Time {
	return clock.now
}

// accountBanHarness is an indexed user account, the moderation service over
// Postgres publishing labels with the issuance key, and its hand-moved clock.
type accountBanHarness struct {
	db      *sql.DB
	service moderation.Service
	clock   *accountBanTestClock
	account string
	subject string
	actor   string
}

// newAccountBanHarness builds the harness; store, when not nil, wraps the
// Postgres moderation repository for the indexed account.
func newAccountBanHarness(t *testing.T, store func(repository *postgres.ModerationRepository, accountDID string) moderation.Store) *accountBanHarness {
	t.Helper()
	db := testkit.DB(t)
	name := testkit.UniqueIDWithPrefix(t, "banned")
	account := fixtures.DID(name)
	fixtures.User(t, db, name+".test", account)
	repository := postgres.NewModerationRepository(db)
	var moderationStore moderation.Store = repository
	if store != nil {
		moderationStore = store(repository, account)
	}
	clock := &accountBanTestClock{now: accountBanInstant(t, accountBanStartTime)}
	return &accountBanHarness{
		db:      db,
		service: newPostgresAccountModerationService(db, moderationStore, issuanceLabelSigningKey(t), clock.Now),
		clock:   clock,
		account: account,
		subject: "at://" + account,
		actor:   fixtures.DID("accountbanadmin"),
	}
}

func (h *accountBanHarness) banRequest(key, version string) moderation.BanAccountRequest {
	return moderation.BanAccountRequest{Subject: h.account, ExpectedVersion: version, IdempotencyKey: key, Reason: accountBanSpam}
}

func (h *accountBanHarness) unbanRequest(key, version string) moderation.UnbanAccountRequest {
	return moderation.UnbanAccountRequest{Subject: h.account, ExpectedVersion: version, IdempotencyKey: key, Reason: issuanceRestoreReason}
}

// ban applies request and requires an applied remove action on the account.
func (h *accountBanHarness) ban(t *testing.T, request moderation.BanAccountRequest) *moderation.MutationResult {
	t.Helper()
	result, err := h.service.BanAccount(t.Context(), h.actor, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	require.Equal(t, moderation.ActionRemove, result.Action.Action)
	return result
}

// unban applies request and requires an applied restore action on the account.
func (h *accountBanHarness) unban(t *testing.T, request moderation.UnbanAccountRequest) *moderation.MutationResult {
	t.Helper()
	result, err := h.service.UnbanAccount(t.Context(), h.actor, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	require.Equal(t, moderation.ActionRestore, result.Action.Action)
	return result
}

func (h *accountBanHarness) state(t *testing.T) *moderation.SubjectState {
	t.Helper()
	state, err := h.service.GetSubjectState(t.Context(), h.subject)
	require.NoError(t, err)
	require.NotNil(t, state)
	return state
}

// labels reads the account's published labels by its bare DID, oldest first.
func (h *accountBanHarness) labels(t *testing.T) []labels.Label {
	t.Helper()
	return storedLabels(t, h.db, h.account)
}

// accountBanRows counts every row a ban or unban of the account writes.
type accountBanRows struct {
	Actions, ActiveDecisions, InactiveDecisions, Labels, IdempotencyKeys int
	Version                                                              int64
}

func (h *accountBanHarness) rows(t *testing.T) accountBanRows {
	t.Helper()
	var rows accountBanRows
	require.NoError(t, h.db.QueryRowContext(t.Context(), `
		SELECT
			(SELECT count(*) FROM moderation_actions WHERE subject_uri = $1),
			(SELECT count(*) FROM moderation_decisions WHERE subject_uri = $1 AND active),
			(SELECT count(*) FROM moderation_decisions WHERE subject_uri = $1 AND NOT active),
			(SELECT count(*) FROM labels WHERE uri = $2),
			(SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $3),
			COALESCE((SELECT version FROM moderation_subjects WHERE subject_uri = $1), 0)
	`, h.subject, h.account, h.actor).Scan(
		&rows.Actions, &rows.ActiveDecisions, &rows.InactiveDecisions, &rows.Labels, &rows.IdempotencyKeys, &rows.Version,
	))
	return rows
}

// accountBanDecision is the account's instance removal decision row.
type accountBanDecision struct {
	Active   bool
	ActionID string
}

func (h *accountBanHarness) decision(t *testing.T) accountBanDecision {
	t.Helper()
	var decision accountBanDecision
	require.NoError(t, h.db.QueryRowContext(t.Context(), `
		SELECT active, active_action_id FROM moderation_decisions
		WHERE authority_did = $1 AND scope_kind = 'instance' AND subject_uri = $2 AND kind = 'removal'
	`, fixtures.InstanceDID(), h.subject).Scan(&decision.Active, &decision.ActionID))
	return decision
}

// storedAccountAction is the stored shape of one account action row.
type storedAccountAction struct {
	Action           string
	Collection       *string
	ExpiresAt        *time.Time
	ReversesActionID string
}

func (h *accountBanHarness) storedAction(t *testing.T, actionID string) storedAccountAction {
	t.Helper()
	var (
		stored     storedAccountAction
		collection sql.NullString
		expiresAt  sql.NullTime
		reverses   sql.NullString
	)
	require.NoError(t, h.db.QueryRowContext(t.Context(), `
		SELECT action, subject_collection, expires_at, reverses_action_id FROM moderation_actions WHERE id = $1
	`, actionID).Scan(&stored.Action, &collection, &expiresAt, &reverses))
	if collection.Valid {
		stored.Collection = &collection.String
	}
	if expiresAt.Valid {
		instant := expiresAt.Time.UTC()
		stored.ExpiresAt = &instant
	}
	stored.ReversesActionID = reverses.String
	return stored
}

// requireAccountLabel checks a stored account label and its signature, and
// that its exp is exactly expiresAt (nil for none).
func requireAccountLabel(t *testing.T, got labels.Label, account, actionID string, neg bool, expiresAt *string) {
	t.Helper()
	requireSignedLabel(t, got, expectedLabel{uri: account, val: labels.TakedownValue, actionID: actionID, neg: neg})
	assert.Equal(t, expiresAt, got.ExpiresAt)
}

func accountBanString(value string) *string {
	return &value
}

func TestAccountBanAndUnban(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(t *testing.T, h *accountBanHarness)
	}{
		{name: "ban then unban", run: func(t *testing.T, h *accountBanHarness) {
			banned := h.ban(t, h.banRequest("ban", "v0"))
			assert.Equal(t, h.subject, banned.Action.SubjectURI)
			assert.Empty(t, banned.Action.SubjectCollection)
			assert.Equal(t, h.actor, banned.Action.ActorDID)
			assert.Equal(t, moderation.ScopeInstance, banned.Action.ScopeKind)
			assert.Equal(t, accountBanSpam, banned.Action.Reason)
			assert.Nil(t, banned.Action.ExpiresAt)
			assert.Equal(t, "v1", banned.State.Version)
			assert.Equal(t, moderation.ModerationStateRemoved, banned.State.Moderation.State)
			assert.Equal(t, &moderation.ActionRef{ServiceDID: fixtures.InstanceDID(), ActionID: banned.Action.ID}, banned.State.LocalRemoval)
			assert.Equal(t, storedAccountAction{Action: moderation.ActionRemove}, h.storedAction(t, banned.Action.ID))
			assert.Equal(t, accountBanDecision{Active: true, ActionID: banned.Action.ID}, h.decision(t))
			assert.Equal(t, accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1}, h.rows(t))
			rows := h.labels(t)
			require.Len(t, rows, 1, "a ban publishes one label")
			requireAccountLabel(t, rows[0], h.account, banned.Action.ID, false, nil)

			unbanned := h.unban(t, h.unbanRequest("unban", "v1"))
			assert.Equal(t, h.subject, unbanned.Action.SubjectURI)
			assert.Equal(t, banned.Action.ID, unbanned.Action.ReversesActionID)
			assert.Equal(t, "v2", unbanned.State.Version)
			assert.Equal(t, moderation.ModerationStateClear, unbanned.State.Moderation.State)
			assert.Nil(t, unbanned.State.LocalRemoval)
			assert.Equal(t, storedAccountAction{Action: moderation.ActionRestore, ReversesActionID: banned.Action.ID}, h.storedAction(t, unbanned.Action.ID))
			assert.Equal(t, accountBanDecision{Active: false, ActionID: banned.Action.ID}, h.decision(t))
			assert.Equal(t, accountBanRows{Actions: 2, InactiveDecisions: 1, Labels: 2, IdempotencyKeys: 2, Version: 2}, h.rows(t))
			rows = h.labels(t)
			require.Len(t, rows, 2, "an unban publishes one negation")
			requireAccountLabel(t, rows[1], h.account, unbanned.Action.ID, true, nil)
			requireStrictlyIncreasingLabels(t, rows)
		}},
		{name: "a replay with the same key returns the original result and writes nothing", run: func(t *testing.T, h *accountBanHarness) {
			original := h.ban(t, h.banRequest("ban", "v0"))
			before := h.rows(t)
			replayed, err := h.service.BanAccount(t.Context(), h.actor, h.banRequest("ban", "v0"))
			require.NoError(t, err)
			assert.Equal(t, original, replayed)
			assert.Equal(t, before, h.rows(t))
			assert.Equal(t, accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1}, h.rows(t))
		}},
		{name: "a stale expected version is a state conflict", run: func(t *testing.T, h *accountBanHarness) {
			h.ban(t, h.banRequest("ban", "v0"))
			_, err := h.service.BanAccount(t.Context(), h.actor, h.banRequest("ban-stale", "v0"))
			require.ErrorIs(t, err, moderation.ErrStateConflict)
			assert.Equal(t, accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1}, h.rows(t))
		}},
		{name: "the same key with a different reason is an idempotency conflict", run: func(t *testing.T, h *accountBanHarness) {
			h.ban(t, h.banRequest("ban", "v0"))
			changed := h.banRequest("ban", "v0")
			changed.Reason = accountBanHarass
			_, err := h.service.BanAccount(t.Context(), h.actor, changed)
			require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
			assert.Equal(t, accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1}, h.rows(t))
		}},
		{name: "the same key with a different expiry is an idempotency conflict", run: func(t *testing.T, h *accountBanHarness) {
			h.ban(t, h.banRequest("ban", "v0"))
			changed := h.banRequest("ban", "v0")
			changed.ExpiresAt = accountBanInstantPointer(t, "2030-06-01T00:00:00Z")
			_, err := h.service.BanAccount(t.Context(), h.actor, changed)
			require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
			assert.Equal(t, accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1}, h.rows(t))
		}},
		{name: "unban without an active ban is a state conflict", run: func(t *testing.T, h *accountBanHarness) {
			_, err := h.service.UnbanAccount(t.Context(), h.actor, h.unbanRequest("unban", "v0"))
			require.ErrorIs(t, err, moderation.ErrStateConflict)
			assert.Equal(t, accountBanRows{}, h.rows(t))
		}},
		{name: "unban after an unban is a state conflict", run: func(t *testing.T, h *accountBanHarness) {
			h.ban(t, h.banRequest("ban", "v0"))
			h.unban(t, h.unbanRequest("unban", "v1"))
			_, err := h.service.UnbanAccount(t.Context(), h.actor, h.unbanRequest("unban-again", "v2"))
			require.ErrorIs(t, err, moderation.ErrStateConflict)
			assert.Equal(t, accountBanRows{Actions: 2, InactiveDecisions: 1, Labels: 2, IdempotencyKeys: 2, Version: 2}, h.rows(t))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, newAccountBanHarness(t, nil))
		})
	}
}

var errAccountLabelInsertFailed = errors.New("injected account label insert failure")

// failingLabelInsertStore is the Postgres moderation store whose transactions
// fail InsertLabel for every issue fail reports.
type failingLabelInsertStore struct {
	*postgres.ModerationRepository
	fail func(issue labels.Issue) bool
}

func (s failingLabelInsertStore) InTransaction(ctx context.Context, fn func(ctx context.Context, tx moderation.Transaction) error) error {
	return s.ModerationRepository.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
		return fn(ctx, failingLabelInsertTransaction{Transaction: tx, fail: s.fail})
	})
}

type failingLabelInsertTransaction struct {
	moderation.Transaction
	fail func(issue labels.Issue) bool
}

func (tx failingLabelInsertTransaction) InsertLabel(ctx context.Context, issue labels.Issue) error {
	if tx.fail(issue) {
		return errAccountLabelInsertFailed
	}
	return tx.Transaction.InsertLabel(ctx, issue)
}

func TestAccountBanRollsBackOnLabelFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		// failNegation picks which label insert fails: the ban's takedown or the unban's negation.
		failNegation bool
		run          func(t *testing.T, h *accountBanHarness) error
		wantRows     accountBanRows
	}{
		{
			name: "ban", failNegation: false,
			run: func(t *testing.T, h *accountBanHarness) error {
				_, err := h.service.BanAccount(t.Context(), h.actor, h.banRequest("ban", "v0"))
				return err
			},
			wantRows: accountBanRows{},
		},
		{
			name: "unban", failNegation: true,
			run: func(t *testing.T, h *accountBanHarness) error {
				h.ban(t, h.banRequest("ban", "v0"))
				_, err := h.service.UnbanAccount(t.Context(), h.actor, h.unbanRequest("unban", "v1"))
				return err
			},
			wantRows: accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAccountBanHarness(t, func(repository *postgres.ModerationRepository, accountDID string) moderation.Store {
				return failingLabelInsertStore{ModerationRepository: repository, fail: func(issue labels.Issue) bool {
					return issue.URI == accountDID && issue.Neg == test.failNegation
				}}
			})
			err := test.run(t, h)
			require.Error(t, err)
			assert.ErrorContains(t, err, "injected account label insert failure")
			assert.Equal(t, test.wantRows, h.rows(t))
		})
	}
}

// accountBanActionViews returns the public action log page decoded from its
// served JSON, newest first.
func accountBanActionViews(t *testing.T, service moderation.Service) []map[string]any {
	t.Helper()
	page, err := service.ListActions(t.Context(), moderation.ListActionsParams{})
	require.NoError(t, err)
	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	var decoded struct {
		Actions []map[string]any `json:"actions"`
	}
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	return decoded.Actions
}

func TestAccountBanTemporary(t *testing.T) {
	const (
		expiryInput    = "2030-01-01T00:00:00.123456Z"
		expiryStored   = "2030-01-01T00:00:00.123Z"
		afterExpiry    = "2030-01-01T00:00:01Z"
		laterInForce   = "2029-12-31T23:30:00Z"
		otherExpiry    = "2030-02-01T00:00:00Z"
		otherExpiryCTS = "2030-02-01T00:00:00.000Z"
	)
	temporary := func(t *testing.T, h *accountBanHarness, key string) (moderation.BanAccountRequest, *moderation.MutationResult) {
		t.Helper()
		request := h.banRequest(key, "v0")
		request.ExpiresAt = accountBanInstantPointer(t, expiryInput)
		return request, h.ban(t, request)
	}
	for _, test := range []struct {
		name string
		run  func(t *testing.T, h *accountBanHarness)
	}{
		{name: "the expiry is stored, signed and served at whole milliseconds", run: func(t *testing.T, h *accountBanHarness) {
			_, banned := temporary(t, h, "ban")
			stored := accountBanInstantPointer(t, expiryStored)
			assert.Equal(t, stored, banned.Action.ExpiresAt)
			assert.Equal(t, storedAccountAction{Action: moderation.ActionRemove, ExpiresAt: stored}, h.storedAction(t, banned.Action.ID))
			rows := h.labels(t)
			require.Len(t, rows, 1)
			requireAccountLabel(t, rows[0], h.account, banned.Action.ID, false, accountBanString(expiryStored))

			state := h.state(t)
			assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
			assert.Equal(t, &moderation.ActionRef{ServiceDID: fixtures.InstanceDID(), ActionID: banned.Action.ID, ExpiresAt: stored}, state.LocalRemoval)

			views := accountBanActionViews(t, h.service)
			require.Len(t, views, 1)
			assert.Equal(t, expiryStored, views[0]["expiresAt"])
		}},
		{name: "an expiry in the past is refused and changes nothing", run: func(t *testing.T, h *accountBanHarness) {
			request := h.banRequest("ban", "v0")
			request.ExpiresAt = accountBanInstantPointer(t, "2029-12-31T22:59:59Z")
			_, err := h.service.BanAccount(t.Context(), h.actor, request)
			require.ErrorIs(t, err, moderation.ErrInvalidRequest)
			assert.Equal(t, accountBanRows{}, h.rows(t))
			assert.Empty(t, h.labels(t))
		}},
		{name: "a re-ban without an expiry while in force makes the ban permanent", run: func(t *testing.T, h *accountBanHarness) {
			_, first := temporary(t, h, "ban")
			h.clock.now = accountBanInstant(t, laterInForce)
			permanent := h.ban(t, h.banRequest("ban-permanent", "v1"))
			assert.NotEqual(t, first.Action.ID, permanent.Action.ID)
			assert.Nil(t, permanent.Action.ExpiresAt)
			assert.Equal(t, storedAccountAction{Action: moderation.ActionRemove}, h.storedAction(t, permanent.Action.ID))
			assert.Equal(t, accountBanDecision{Active: true, ActionID: permanent.Action.ID}, h.decision(t))
			assert.Equal(t, "v2", permanent.State.Version)
			rows := h.labels(t)
			require.Len(t, rows, 2)
			requireAccountLabel(t, rows[0], h.account, first.Action.ID, false, accountBanString(expiryStored))
			requireAccountLabel(t, rows[1], h.account, permanent.Action.ID, false, nil)
			assert.Equal(t, "2029-12-31T23:00:00.000Z", rows[0].CreatedAt)
			assert.Equal(t, "2029-12-31T23:30:00.000Z", rows[1].CreatedAt)
			state := h.state(t)
			assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
			assert.Equal(t, &moderation.ActionRef{ServiceDID: fixtures.InstanceDID(), ActionID: permanent.Action.ID}, state.LocalRemoval)
		}},
		{name: "a re-ban with a different expiry replaces the expiry", run: func(t *testing.T, h *accountBanHarness) {
			temporary(t, h, "ban")
			request := h.banRequest("ban-extended", "v1")
			request.ExpiresAt = accountBanInstantPointer(t, otherExpiry)
			extended := h.ban(t, request)
			rows := h.labels(t)
			require.Len(t, rows, 2)
			requireAccountLabel(t, rows[1], h.account, extended.Action.ID, false, accountBanString(otherExpiryCTS))
			assert.Equal(t, accountBanDecision{Active: true, ActionID: extended.Action.ID}, h.decision(t))
			assert.Equal(t, &moderation.ActionRef{
				ServiceDID: fixtures.InstanceDID(), ActionID: extended.Action.ID, ExpiresAt: accountBanInstantPointer(t, otherExpiry),
			}, h.state(t).LocalRemoval)
		}},
		{name: "a lapsed ban reads clear and writes nothing", run: func(t *testing.T, h *accountBanHarness) {
			_, banned := temporary(t, h, "ban")
			h.clock.now = accountBanInstant(t, afterExpiry)
			state := h.state(t)
			assert.Equal(t, moderation.ModerationStateClear, state.Moderation.State)
			assert.Equal(t, &moderation.ActionRef{
				ServiceDID: fixtures.InstanceDID(), ActionID: banned.Action.ID, ExpiresAt: accountBanInstantPointer(t, expiryStored),
			}, state.LocalRemoval, "the decision stays active after it lapses")
			assert.Equal(t, "v1", state.Version)
			assert.Equal(t, accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1}, h.rows(t))
		}},
		{name: "a ban after a lapse is in force again", run: func(t *testing.T, h *accountBanHarness) {
			_, lapsed := temporary(t, h, "ban")
			h.clock.now = accountBanInstant(t, afterExpiry)
			again := h.ban(t, h.banRequest("ban-again", "v1"))
			assert.NotEqual(t, lapsed.Action.ID, again.Action.ID)
			assert.Equal(t, moderation.ModerationStateRemoved, again.State.Moderation.State)
			assert.Equal(t, accountBanDecision{Active: true, ActionID: again.Action.ID}, h.decision(t))
			rows := h.labels(t)
			require.Len(t, rows, 2)
			requireAccountLabel(t, rows[1], h.account, again.Action.ID, false, nil)
			assert.Equal(t, "2030-01-01T00:00:01.000Z", rows[1].CreatedAt)
			assert.Equal(t, moderation.ModerationStateRemoved, h.state(t).Moderation.State)
		}},
		{name: "an unban after a lapse negates without an expiry", run: func(t *testing.T, h *accountBanHarness) {
			_, lapsed := temporary(t, h, "ban")
			h.clock.now = accountBanInstant(t, afterExpiry)
			unbanned := h.unban(t, h.unbanRequest("unban", "v1"))
			assert.Equal(t, lapsed.Action.ID, unbanned.Action.ReversesActionID)
			assert.Nil(t, unbanned.Action.ExpiresAt)
			assert.Equal(t, accountBanDecision{Active: false, ActionID: lapsed.Action.ID}, h.decision(t))
			rows := h.labels(t)
			require.Len(t, rows, 2)
			requireAccountLabel(t, rows[1], h.account, unbanned.Action.ID, true, nil)
			state := h.state(t)
			assert.Equal(t, moderation.ModerationStateClear, state.Moderation.State)
			assert.Nil(t, state.LocalRemoval)
			assert.Equal(t, "v2", state.Version)
		}},
		{name: "a replay after the expiry returns the original result", run: func(t *testing.T, h *accountBanHarness) {
			request, original := temporary(t, h, "ban")
			h.clock.now = accountBanInstant(t, afterExpiry)
			replayed, err := h.service.BanAccount(t.Context(), h.actor, request)
			require.NoError(t, err)
			assert.Equal(t, original, replayed)
			assert.Equal(t, accountBanRows{Actions: 1, ActiveDecisions: 1, Labels: 1, IdempotencyKeys: 1, Version: 1}, h.rows(t))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, newAccountBanHarness(t, nil))
		})
	}
}

func TestAccountBanPublicActionLog(t *testing.T) {
	for _, test := range []struct {
		name string
		// reason is the ban's reason; a hidden reason withholds the subject of
		// the ban and of the unban reversing it.
		reason   string
		withheld bool
	}{
		{name: "a spam ban and its unban name the account", reason: accountBanSpam},
		{name: "a doxing ban and its unban withhold the account", reason: accountBanDoxing, withheld: true},
		{name: "an illegal-content ban and its unban withhold the account", reason: accountBanIllegal, withheld: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAccountBanHarness(t, nil)
			request := h.banRequest("ban", "v0")
			request.Reason = test.reason
			banned := h.ban(t, request)
			unbanned := h.unban(t, h.unbanRequest("unban", "v1"))

			page, err := h.service.ListActions(t.Context(), moderation.ListActionsParams{})
			require.NoError(t, err)
			require.Len(t, page.Actions, 2)
			restore, remove := page.Actions[0], page.Actions[1]
			assert.Equal(t, unbanned.Action.ID, restore.Ref.ActionID)
			assert.Equal(t, banned.Action.ID, remove.Ref.ActionID)
			assert.Equal(t, moderation.ActionRestore, restore.Action)
			assert.Equal(t, moderation.ActionRemove, remove.Action)
			assert.Equal(t, test.reason, remove.Reason)
			assert.Equal(t, &moderation.ActionRefView{ServiceDID: fixtures.InstanceDID(), ActionID: banned.Action.ID}, restore.Reverses)
			for _, view := range page.Actions {
				assert.Equal(t, &moderation.ActorRefView{DID: h.actor}, view.Actor, view.Ref.ActionID)
				assert.Equal(t, moderation.ScopeView{Kind: moderation.ScopeInstance}, view.Scope, view.Ref.ActionID)
				assert.Nil(t, view.ContentSnapshot, "an account action has no content snapshot")
				if test.withheld {
					assert.Nil(t, view.Subject, "a hidden-reason action withholds its subject")
				} else {
					assert.Equal(t, &moderation.SubjectRefView{URI: h.subject}, view.Subject, view.Ref.ActionID)
				}
			}

			admin, err := h.service.ListAdminActions(t.Context(), moderation.ListAdminActionsParams{})
			require.NoError(t, err)
			require.Len(t, admin.Actions, 2)
			for _, view := range admin.Actions {
				assert.Equal(t, h.actor, view.ActorDID)
			}
		})
	}
}

func TestAccountBanReaderCountsOnlyThisInstancesBans(t *testing.T) {
	h := newAccountBanHarness(t, nil)
	banned := h.ban(t, h.banRequest("ban", "v0"))
	foreignAccount := fixtures.DID(testkit.UniqueIDWithPrefix(t, "foreignban"))
	foreignActionID := "foreign-account-ban-" + testkit.UniqueID(t)
	_, err := h.db.ExecContext(t.Context(), `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, subject_kind, action, reason, origin, created_at)
		VALUES ($1, 'did:plc:foreignbanadmin', 'did:web:foreign-instance.test', 'instance', $2, NULL, 'account',
		        'remove', $3, 'local', NOW())
	`, foreignActionID, "at://"+foreignAccount, accountBanSpam)
	require.NoError(t, err)
	_, err = h.db.ExecContext(t.Context(), `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ('did:web:foreign-instance.test', 'instance', NULL, $1, 'removal', NULL, $2, TRUE)
	`, "at://"+foreignAccount, foreignActionID)
	require.NoError(t, err)
	reader := postgres.NewAccountBanReader(h.db, fixtures.InstanceDID())

	own, err := reader.ActiveBan(t.Context(), h.account)
	require.NoError(t, err)
	require.NotNil(t, own, "this instance's ban is read")
	assert.Equal(t, banned.Action.ID, own.ActionID)

	foreign, err := reader.ActiveBan(t.Context(), foreignAccount)
	require.NoError(t, err)
	assert.Nil(t, foreign, "another authority's instance-scope removal is not this instance's ban")
}
