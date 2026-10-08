package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFanoutVoteCreate_EarlierUpvoteDeletesIfEmpty(t *testing.T) {
	for _, subject := range []struct {
		name, uri, root string
	}{
		{"post", voteFanoutPost, voteFanoutPost},
		{"comment", voteFanoutComment, voteFanoutPost},
	} {
		t.Run(subject.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			vote.URI = "at://" + voteFanoutVoter + "/social.coves.community.vote/revote"
			vote.SubjectURI, vote.SubjectRootURI = subject.uri, subject.root
			lookups.earlierUpvoteExists = true
			var calls []earlierUpvoteCall
			lookups.earlierUpvoteCalls = &calls

			intent, err := FanoutVoteCreate(context.Background(), lookups, nil, vote)
			require.NoError(t, err)
			require.Equal(t, UpvoteGroupIntent{
				Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutAuthor,
				SubjectURI: subject.uri,
			}, intent, "another upvote by the same voter must not re-bump this group")
			require.Equal(t, []earlierUpvoteCall{{voteFanoutVoter, subject.uri, vote.URI}}, calls,
				"check exactly once for another upvote, excluding this vote URI")
		})
	}
}

func TestFanoutVoteCreate_FirstUpvoteBumps(t *testing.T) {
	for _, subject := range []struct {
		name, uri, root string
	}{
		{"post", voteFanoutPost, voteFanoutPost},
		{"comment", voteFanoutComment, voteFanoutPost},
	} {
		t.Run(subject.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			vote.URI = "at://" + voteFanoutVoter + "/social.coves.community.vote/first"
			vote.SubjectURI, vote.SubjectRootURI = subject.uri, subject.root
			var calls []earlierUpvoteCall
			lookups.earlierUpvoteCalls = &calls

			intent, err := FanoutVoteCreate(context.Background(), lookups, nil, vote)
			require.NoError(t, err)
			require.Equal(t, UpvoteGroupIntent{
				Action: UpvoteGroupBump, RecipientDID: voteFanoutAuthor,
				SubjectURI: subject.uri, RootPostURI: subject.root,
			}, intent)
			require.Equal(t, []earlierUpvoteCall{{voteFanoutVoter, subject.uri, vote.URI}}, calls,
				"a qualifying first upvote must check prior upvotes exactly once")
		})
	}
}

func TestFanoutVoteCreate_NonBumpingVotesSkipEarlierUpvoteLookup(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*commentFanoutLookups, *VoteRecord)
	}{
		{"downvote", func(_ *commentFanoutLookups, vote *VoteRecord) { vote.Direction = "down" }},
		{"self_vote", func(_ *commentFanoutLookups, vote *VoteRecord) { vote.VoterDID = voteFanoutAuthor }},
		{"before_activation", func(lookups *commentFanoutLookups, vote *VoteRecord) {
			lookups.activatedAt = vote.CreatedAt.Add(time.Nanosecond)
		}},
		{"older_than_seven_days", func(lookups *commentFanoutLookups, vote *VoteRecord) {
			lookups.indexTime = vote.CreatedAt.Add(7*24*time.Hour + time.Nanosecond)
		}},
		{"erased_voter", func(_ *commentFanoutLookups, vote *VoteRecord) {
			vote.VoterErased = true
		}},
		{"aggregator_voter", func(lookups *commentFanoutLookups, _ *VoteRecord) {
			lookups.aggregatorAccounts = map[string]bool{voteFanoutVoter: true}
		}},
		{"recipient_not_indexed", func(lookups *commentFanoutLookups, _ *VoteRecord) {
			delete(lookups.indexedUsers, voteFanoutAuthor)
		}},
		{"recipient_blocks_voter", func(lookups *commentFanoutLookups, _ *VoteRecord) {
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: voteFanoutAuthor, blockedDID: voteFanoutVoter}: true}
		}},
		{"voter_blocks_recipient", func(lookups *commentFanoutLookups, _ *VoteRecord) {
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: voteFanoutVoter, blockedDID: voteFanoutAuthor}: true}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			vote.URI = "at://" + voteFanoutVoter + "/social.coves.community.vote/ineligible"
			test.setup(&lookups, &vote)
			// A lookup here would fail rather than silently return an earlier vote.
			lookups.earlierUpvoteError = errors.New("earlier-upvote lookup must not run")
			var calls []earlierUpvoteCall
			lookups.earlierUpvoteCalls = &calls

			intent, err := FanoutVoteCreate(context.Background(), lookups, nil, vote)
			require.NoError(t, err)
			require.Equal(t, UpvoteGroupIntent{
				Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutAuthor,
				SubjectURI: voteFanoutPost,
			}, intent)
			require.Empty(t, calls, "non-bumping votes must not check for earlier upvotes")
		})
	}
}
