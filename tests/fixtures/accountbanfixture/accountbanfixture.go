//go:build integration

// Package accountbanfixture bans and unbans accounts through the real
// moderation service over Postgres, for the write-path wiring tests of posts,
// comments and votes. It is a separate package from tests/fixtures because it
// imports internal/db/postgres, whose in-package tests import tests/fixtures.
package accountbanfixture

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"

	"github.com/stretchr/testify/require"
)

const (
	adminDID    = "did:plc:accountbanfixtureadmin"
	banReason   = "social.coves.moderation.defs#reasonSpam"
	unbanReason = "social.coves.moderation.defs#reasonModeratorDiscretion"
)

// Moderator is the instance moderation service over db, holding each
// account's current subject version so tests can ban and unban in sequence.
type Moderator struct {
	db       *sql.DB
	service  moderation.Service
	versions map[string]string
	keys     int
}

// New returns a Moderator over db with the indexed community repository as
// the community check and labels unpublished.
func New(db *sql.DB) *Moderator {
	return &Moderator{
		db: db,
		service: moderation.NewService(
			moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
			postgres.NewModerationRepository(db),
			moderation.Config{
				InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
				Communities: postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
			},
		),
		versions: map[string]string{},
	}
}

// Reader is the Postgres account-ban read the write paths consult.
func (m *Moderator) Reader() accountbans.Reader {
	return postgres.NewAccountBanReader(m.db, fixtures.InstanceDID())
}

// Ban bans accountDID until expiresAt, or permanently when it is nil.
func (m *Moderator) Ban(t *testing.T, accountDID string, expiresAt *time.Time) {
	t.Helper()
	m.BanWithReason(t, accountDID, banReason, expiresAt)
}

// BanWithReason bans accountDID for reason until expiresAt, or permanently
// when it is nil.
func (m *Moderator) BanWithReason(t *testing.T, accountDID, reason string, expiresAt *time.Time) {
	t.Helper()
	result, err := m.service.BanAccount(t.Context(), adminDID, moderation.BanAccountRequest{
		Subject: accountDID, ExpectedVersion: m.version(accountDID), IdempotencyKey: m.key(), Reason: reason, ExpiresAt: expiresAt,
	})
	m.record(t, accountDID, result, err)
}

// Unban lifts accountDID's ban.
func (m *Moderator) Unban(t *testing.T, accountDID string) {
	t.Helper()
	result, err := m.service.UnbanAccount(t.Context(), adminDID, moderation.UnbanAccountRequest{
		Subject: accountDID, ExpectedVersion: m.version(accountDID), IdempotencyKey: m.key(), Reason: unbanReason,
	})
	m.record(t, accountDID, result, err)
}

// Lapse moves accountDID's stored temporary ban expiry an hour into the past,
// the state a temporary ban reaches when its time runs out; nothing else is
// written at lapse.
func (m *Moderator) Lapse(t *testing.T, accountDID string) {
	t.Helper()
	result, err := m.db.ExecContext(t.Context(), `
		UPDATE moderation_actions SET expires_at = NOW() - interval '1 hour'
		WHERE subject_uri = $1 AND action = 'remove' AND expires_at IS NOT NULL
	`, "at://"+accountDID)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), updated, "exactly one temporary ban action of the account is lapsed")
}

func (m *Moderator) version(accountDID string) string {
	if version, ok := m.versions[accountDID]; ok {
		return version
	}
	return "v0"
}

func (m *Moderator) key() string {
	m.keys++
	return fmt.Sprintf("account-ban-fixture-%d", m.keys)
}

func (m *Moderator) record(t *testing.T, accountDID string, result *moderation.MutationResult, err error) {
	t.Helper()
	require.NoError(t, err)
	require.NotNil(t, result, "the moderation service returned no mutation result")
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	m.versions[accountDID] = result.State.Version
}
