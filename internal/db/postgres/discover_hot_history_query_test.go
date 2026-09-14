//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryDiscoverHotHistory_EligiblePublicCohort(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	rankingTime := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	communityDID := visibilityCommunity(t, db, "hothistory")
	authorDID := "did:plc:hothistoryauthor"
	createTestUser(t, db, "hothistoryauthor.test", authorDID)

	setScore := func(uri string, score int) {
		t.Helper()
		_, err := db.ExecContext(ctx, `UPDATE posts SET score = $2 WHERE uri = $1`, uri, score)
		require.NoError(t, err)
	}
	seedLegacy := func(rkey string, score int, createdAt time.Time) string {
		t.Helper()
		uri := seedFilterablePost(t, db, communityDID, authorDID, rkey, createdAt)
		setScore(uri, score)
		return uri
	}
	seedPostV2 := func(rkey string, score int, createdAt time.Time) string {
		t.Helper()
		uri := seedVisibilityPost(t, db, communityDID, authorDID, rkey, rkey, createdAt)
		setScore(uri, score)
		return uri
	}

	seedLegacy("history-exactly-14d", 14, rankingTime.Add(-14*24*time.Hour))
	seedLegacy("history-mature-zero", 0, rankingTime.Add(-7*24*time.Hour))
	acceptedPostV2 := seedPostV2("history-accepted-v2", 24, rankingTime.Add(-24*time.Hour-time.Microsecond))
	seedVisibilityAdmission(t, db, communityDID, acceptedPostV2, posts.AdmissionStatusAccepted, "", "")

	deleted := seedLegacy("history-deleted", 101, rankingTime.Add(-7*24*time.Hour))
	_, err := db.ExecContext(ctx, `UPDATE posts SET deleted_at = $2 WHERE uri = $1`, deleted, rankingTime)
	require.NoError(t, err)

	pending := seedPostV2("history-pending", 102, rankingTime.Add(-7*24*time.Hour))
	seedVisibilityAdmission(t, db, communityDID, pending, posts.AdmissionStatusPending, "", "")
	rejected := seedPostV2("history-rejected", 103, rankingTime.Add(-7*24*time.Hour))
	seedVisibilityAdmission(t, db, communityDID, rejected, posts.AdmissionStatusRejected, "", "")
	drifted := seedPostV2("history-drifted-cid", 104, rankingTime.Add(-7*24*time.Hour))
	seedVisibilityAdmissionDriftedCID(t, db, communityDID, drifted)
	seedPostV2("history-no-admission", 105, rankingTime.Add(-7*24*time.Hour))
	seedLegacy("history-younger-than-24h", 106, rankingTime.Add(-24*time.Hour+time.Microsecond))
	seedLegacy("history-exactly-24h", 107, rankingTime.Add(-24*time.Hour))
	seedLegacy("history-older-than-14d", 108, rankingTime.Add(-14*24*time.Hour-time.Microsecond))

	readHistory := func() map[string][]int {
		t.Helper()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		defer tx.Rollback()

		history, err := queryDiscoverHotHistory(ctx, tx, rankingTime)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return history
	}
	requireCohort := func(history map[string][]int) {
		t.Helper()
		require.Equal(t, map[string]bool{communityDID: true}, discoverHotHistoryCommunities(history))
		assert.ElementsMatch(t, []int{14, 0, 24}, history[communityDID],
			"history must be publicly visible posts in the created-at interval [T-14d, T-24h)")
	}

	requireCohort(readHistory())

	viewerDID := "did:plc:hothistoryviewer"
	createTestUser(t, db, "hothistoryviewer.test", viewerDID)
	insertUserBlock(t, db, viewerDID, authorDID)
	insertCommunityBlock(t, db, viewerDID, communityDID)
	requireCohort(readHistory())
}

func discoverHotHistoryCommunities(history map[string][]int) map[string]bool {
	communities := make(map[string]bool, len(history))
	for communityDID := range history {
		communities[communityDID] = true
	}
	return communities
}
