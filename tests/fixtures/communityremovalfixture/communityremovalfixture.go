//go:build integration

// Package communityremovalfixture removes and restores communities through
// the real moderation service over Postgres, for the community surface and
// write-refusal wiring tests. It is a separate package from tests/fixtures
// because it imports internal/db/postgres, whose in-package tests import
// tests/fixtures.
package communityremovalfixture

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"

	"github.com/stretchr/testify/require"
)

const (
	adminDID      = "did:plc:communityremovalfixtureadmin"
	removeReason  = "social.coves.moderation.defs#reasonSpam"
	restoreReason = "social.coves.moderation.defs#reasonModeratorDiscretion"
)

// Moderator is the instance moderation service over db, holding each
// community's current subject version so tests can remove and restore in
// sequence.
type Moderator struct {
	service  moderation.Service
	versions map[string]string
	keys     int
}

// New returns a Moderator over db with the indexed community repository as
// the community check and labels unpublished.
func New(db *sql.DB) *Moderator {
	return &Moderator{
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

// Remove removes communityDID at the instance.
func (m *Moderator) Remove(t *testing.T, communityDID string) {
	t.Helper()
	result, err := m.service.RemoveCommunity(t.Context(), adminDID, moderation.RemoveCommunityRequest{
		Subject: communityDID, ExpectedVersion: m.version(communityDID), IdempotencyKey: m.key(), Reason: removeReason,
	})
	m.record(t, communityDID, moderation.ActionRemove, result, err)
}

// Restore lifts communityDID's removal.
func (m *Moderator) Restore(t *testing.T, communityDID string) {
	t.Helper()
	result, err := m.service.RestoreCommunity(t.Context(), adminDID, moderation.RestoreCommunityRequest{
		Subject: communityDID, ExpectedVersion: m.version(communityDID), IdempotencyKey: m.key(), Reason: restoreReason,
	})
	m.record(t, communityDID, moderation.ActionRestore, result, err)
}

func (m *Moderator) version(communityDID string) string {
	if version, ok := m.versions[communityDID]; ok {
		return version
	}
	return "v0"
}

func (m *Moderator) key() string {
	m.keys++
	return fmt.Sprintf("community-removal-fixture-%d", m.keys)
}

func (m *Moderator) record(t *testing.T, communityDID, action string, result *moderation.MutationResult, err error) {
	t.Helper()
	require.NoError(t, err)
	require.NotNil(t, result, "the moderation service returned no mutation result")
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action, "an applied community mutation records an action")
	require.Equal(t, action, result.Action.Action)
	m.versions[communityDID] = result.State.Version
}
