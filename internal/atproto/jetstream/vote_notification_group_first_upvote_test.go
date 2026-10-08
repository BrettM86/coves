//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestVoteConsumer_UpvoteGroupRepeatUpvoteAfterDeleteDoesNotBump(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	firstVoter, companion := fixture.voter(t), fixture.voter(t)
	firstKey := testkit.TID()
	firstURI := "at://" + firstVoter + "/social.coves.feed.vote/" + firstKey
	first := maintenanceVoteAtKey(firstVoter, fixture.post, fixture.createdAt, revA, firstKey)
	require.NoError(t, consumer.HandleEvent(context.Background(), first))
	companionURI := deliverGroupVote(t, consumer, companion, fixture.post, "up", fixture.createdAt)
	groupID, _ := maintenanceGroup(t, fixture)

	require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
		firstVoter, "social.coves.feed.vote", "delete", firstKey, revB, "", first.TimeUS+1_000_000, nil)))
	exists, active := voteRowState(t, fixture.db, firstURI)
	require.True(t, exists && !active, "A's original upvote must remain as a soft-deleted row")
	exists, active = voteRowState(t, fixture.db, companionURI)
	require.True(t, exists && active, "D's upvote must keep the group alive")
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
	past := ageMaintenanceGroup(t, fixture.db, groupID)

	secondKey := testkit.TID()
	require.NotEqual(t, firstKey, secondKey, "A must use a new record key")
	secondURI := "at://" + firstVoter + "/social.coves.feed.vote/" + secondKey
	require.NoError(t, consumer.HandleEvent(context.Background(), maintenanceVoteAtKey(
		firstVoter, fixture.post, time.Now().UTC().Format(time.RFC3339Nano), revC, secondKey)))
	exists, active = voteRowState(t, fixture.db, secondURI)
	require.True(t, exists && active, "A's new-rkey upvote must be indexed and live")
	storedID, storedSort := maintenanceGroup(t, fixture)
	require.Equal(t, groupID, storedID, "D's group must keep its original id")
	require.Truef(t, storedSort.Equal(past), "A's second upvote moved sort_at from %s to %s", past, storedSort)
}

func TestVoteConsumer_UpvoteGroupEarlierIneligibleUpvoteStillBlocksBump(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"pre_activation", "older_than_seven_days", "blocked", "recipient_ineligible"} {
		t.Run(gate, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			consumer := fixture.consumer()
			voter := fixture.voter(t)
			createdAt := fixture.createdAt
			switch gate {
			case "pre_activation":
				createdAt = ineligibleUpvoteTime(t, fixture, "before_activation", voter)
			case "older_than_seven_days":
				createdAt = ineligibleUpvoteTime(t, fixture, gate, voter)
			case "blocked":
				ineligibleUpvoteTime(t, fixture, "recipient_blocks_voter", voter)
			case "recipient_ineligible":
				_, err := fixture.db.Exec(`INSERT INTO aggregators (did, display_name, record_uri, record_cid)
					VALUES ($1, 'Ineligible recipient', $2, 'bafupvoteservice')`, fixture.author,
					"at://"+fixture.author+"/social.coves.aggregator.service/self")
				require.NoError(t, err)
			}

			firstKey := testkit.TID()
			firstURI := "at://" + voter + "/social.coves.feed.vote/" + firstKey
			first := maintenanceVoteAtKey(voter, fixture.post, createdAt, revA, firstKey)
			require.NoError(t, consumer.HandleEvent(context.Background(), first))
			exists, active := voteRowState(t, fixture.db, firstURI)
			require.True(t, exists && active, "A's ineligible first upvote must still be indexed")
			require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
				"%s must suppress the first upvote's group", gate)

			require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
				voter, "social.coves.feed.vote", "delete", firstKey, revB, "", first.TimeUS+1_000_000, nil)))
			exists, active = voteRowState(t, fixture.db, firstURI)
			require.True(t, exists && !active, "the first upvote must be retained as a soft-deleted row")
			switch gate {
			case "blocked":
				result, err := fixture.db.Exec(`DELETE FROM user_blocks WHERE blocker_did = $1 AND blocked_did = $2`, fixture.author, voter)
				require.NoError(t, err)
				removed, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, removed, "B's block on A must be lifted")
			case "recipient_ineligible":
				result, err := fixture.db.Exec(`DELETE FROM aggregators WHERE did = $1`, fixture.author)
				require.NoError(t, err)
				removed, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, removed, "B's recipient ineligibility must be lifted")
			}

			secondKey := testkit.TID()
			require.NotEqual(t, firstKey, secondKey, "A's repeat upvote must have a new rkey")
			secondURI := "at://" + voter + "/social.coves.feed.vote/" + secondKey
			require.NoError(t, consumer.HandleEvent(context.Background(), maintenanceVoteAtKey(
				voter, fixture.post, time.Now().UTC().Format(time.RFC3339Nano), revC, secondKey)))
			exists, active = voteRowState(t, fixture.db, secondURI)
			require.True(t, exists && active, "A's eligible repeat upvote must be indexed")
			require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
				"%s: A's earlier ineligible upvote must prevent a later group from being created", gate)

			controlURI := deliverGroupVote(t, consumer, fixture.voter(t), fixture.post, "up", fixture.createdAt)
			exists, active = voteRowState(t, fixture.db, controlURI)
			require.True(t, exists && active)
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
				"%s: a different voter's first upvote must create B's group after eligibility is restored", gate)
		})
	}
}

func TestVoteConsumer_UpvoteGroupEarlierDownvoteDoesNotBlockBump(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"deleted_downvote", "replaced_downvote"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			consumer := fixture.consumer()
			voter := fixture.voter(t)
			firstKey := testkit.TID()
			first := upvoteGroupEvent(voter, fixture.post, "down", fixture.createdAt, revA)
			first.Commit.RKey = firstKey
			firstURI := "at://" + voter + "/social.coves.feed.vote/" + firstKey
			require.NoError(t, consumer.HandleEvent(context.Background(), first))
			require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post), "a downvote must not create a group")
			if scenario == "deleted_downvote" {
				require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
					voter, "social.coves.feed.vote", "delete", firstKey, revB, "", first.TimeUS+1_000_000, nil)))
				exists, active := voteRowState(t, fixture.db, firstURI)
				require.True(t, exists && !active, "the downvote must remain as a soft-deleted row")
				secondKey := testkit.TID()
				require.NotEqual(t, firstKey, secondKey)
				secondURI := "at://" + voter + "/social.coves.feed.vote/" + secondKey
				require.NoError(t, consumer.HandleEvent(context.Background(), maintenanceVoteAtKey(
					voter, fixture.post, fixture.createdAt, revC, secondKey)))
				exists, active = voteRowState(t, fixture.db, secondURI)
				require.True(t, exists && active, "the first upvote must be indexed and live")
			} else {
				replaceMaintenanceVote(t, fixture, consumer, voter, "up", firstURI)
			}
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
				"%s: an earlier downvote must not disqualify the voter's first upvote", scenario)
		})
	}
}
