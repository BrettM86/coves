//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func (fixture upvoteGroupFixture) bridgedConsumer(options ...VoteEventConsumerOption) *VoteEventConsumer {
	return fixture.consumer(append([]VoteEventConsumerOption{
		WithVoteNotifications(postgres.NewNotificationRepository(fixture.db, postgres.WithBridgedUpvoteTotals())),
	}, options...)...)
}

func TestVoteConsumer_BridgedGroupSurvivesLastNativeVoteRemoval(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name           string
		bridgedUpvotes int
		replacement    bool
	}{
		{"delete_with_bridged_total", 2, false},
		{"downvote_replacement_with_bridged_total", 2, true},
		{"delete_without_bridged_total", 0, false},
		{"downvote_replacement_without_bridged_total", 0, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			consumer := fixture.bridgedConsumer()
			voter := fixture.voter(t)
			key := testkit.TID()
			original := maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revA, key)
			voteURI := "at://" + voter + "/social.coves.feed.vote/" + key
			require.NoError(t, consumer.HandleEvent(context.Background(), original))
			groupID, _ := maintenanceGroup(t, fixture)
			at := time.Date(2026, time.September, 30, 12, 0, 7, 123456000, time.UTC).Truncate(time.Microsecond)
			_, err := fixture.db.Exec(`UPDATE notifications SET sort_at = $2 WHERE id = $1`, groupID, at)
			require.NoError(t, err)
			_, err = fixture.db.Exec(`UPDATE posts SET bridged_upvote_count = $2, bridged_stats_as_of = NOW() WHERE uri = $1`, fixture.post, scenario.bridgedUpvotes)
			require.NoError(t, err)
			if scenario.replacement {
				replaceMaintenanceVote(t, fixture, consumer, voter, "down", voteURI)
			} else {
				require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
					voter, "social.coves.feed.vote", "delete", key, revB, "", original.TimeUS+1_000_000, nil)))
				exists, active := voteRowState(t, fixture.db, voteURI)
				require.True(t, exists && !active, "the delete must soft-delete A's upvote")
			}
			if scenario.bridgedUpvotes == 0 {
				require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post), "without native or bridged upvotes the group must be deleted")
				return
			}
			var storedID int64
			var storedSort time.Time
			require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
				WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, fixture.author, fixture.post).Scan(&storedID, &storedSort),
				"bridged upvotes must keep the same group after A's vote is withdrawn")
			require.Equal(t, groupID, storedID)
			require.True(t, storedSort.Equal(at), "withdrawing A's vote must not bump the group")
		})
	}
}
