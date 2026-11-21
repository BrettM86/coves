//go:build integration

package postgres

import (
	"context"
	"testing"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A redelivered ban event (the index transaction committed, the rev gate's
// claim did not) re-indexes the same record key and CID. The index already
// holds that ban, so the subject's version advances once and the history keeps
// one unlifted row.
func TestCommunityBanRepository_RedeliveredBanAdvancesVersionOnce(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	const (
		community = "did:plc:banredelivercommunity"
		subject   = "did:plc:banredeliversubject"
	)
	insertRoleTestCommunity(t, db, community)
	repo := NewCommunityBanRepository(db)
	reader := NewCommunitySubjectStateReader(db)
	ban := communities.CommunityBan{
		CommunityDID: community, SubjectDID: subject,
		RecordKey: "banredeliverkey", RecordCID: "bafyreibanredeliver", Reason: "spam",
	}

	require.NoError(t, repo.UpsertCommunityBan(ctx, ban))
	require.NoError(t, repo.UpsertCommunityBan(ctx, ban), "the redelivered event re-indexes the same ban")

	state, err := reader.ReadCommunitySubjectState(ctx, community, subject, moderation.CommunitySubjectAccount)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.EqualValues(t, 1, state.Version, "one ban event must advance the subject's version exactly once")

	var rows, unlifted int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*), COUNT(*) FILTER (WHERE lifted_order IS NULL)
		FROM community_ban_history WHERE community_did = $1 AND subject_did = $2`,
		community, subject).Scan(&rows, &unlifted))
	assert.Equal(t, 1, rows, "the redelivery must not add a history row")
	assert.Equal(t, 1, unlifted, "the ban's history row must stay unlifted")
}
