package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requireVoteDeleteIfEmpty(t *testing.T, lookups commentFanoutLookups, bridgeHosts BridgeHostChecker, vote VoteRecord, recipient string) {
	t.Helper()
	intent, err := FanoutVoteCreate(context.Background(), lookups, bridgeHosts, vote)
	require.NoError(t, err)
	require.Equal(t, UpvoteGroupDeleteIfEmpty, intent.Action, "a resolved non-bump vote must request delete-if-empty")
	require.Equal(t, recipient, intent.RecipientDID)
	require.Equal(t, vote.SubjectURI, intent.SubjectURI)
}

func TestFanoutVoteCreate_DeleteIfEmptyDownvotes(t *testing.T) {
	t.Parallel()
	for _, subject := range []struct {
		name, uri   string
		legacyCalls int
	}{
		{"postv2_no_lookups", voteFanoutPost, 0},
		{"legacy_only_author_lookup", voteFanoutLegacyPost, 1},
	} {
		t.Run(subject.name, func(t *testing.T) {
			t.Parallel()
			lookups, vote := qualifyingVoteFanout()
			vote.SubjectURI, vote.Direction = subject.uri, "down"
			var legacyCalls, activationCalls, indexCalls, aggregatorCalls int
			var recipientCalls [][]string
			lookups.legacyPostAuthorCalls = &legacyCalls
			lookups.activatedAtCalls = &activationCalls
			lookups.indexTimeCalls = &indexCalls
			lookups.isAggregatorCalls = &aggregatorCalls
			lookups.recipientFactsCalls = &recipientCalls
			requireVoteDeleteIfEmpty(t, lookups, nil, vote, voteFanoutAuthor)
			require.Equal(t, subject.legacyCalls, legacyCalls, "resolve only the legacy post author when necessary")
			require.Zero(t, activationCalls, "downvotes must not read activation")
			require.Zero(t, indexCalls, "downvotes must not read index time")
			require.Zero(t, aggregatorCalls, "downvotes must not read voter aggregator status")
			require.Empty(t, recipientCalls, "downvotes must not read recipient facts")
		})
	}
}

func TestFanoutVoteCreate_DeleteIfEmptySelfVotes(t *testing.T) {
	t.Parallel()
	for _, subject := range []struct{ name, uri, root string }{
		{"postv2", voteFanoutPost, ""},
		{"comment", voteFanoutComment, voteFanoutPost},
		{"legacy_post_resolves_row_author", voteFanoutLegacyPost, ""},
	} {
		t.Run(subject.name, func(t *testing.T) {
			t.Parallel()
			lookups, vote := qualifyingVoteFanout()
			vote.SubjectURI, vote.SubjectRootURI = subject.uri, subject.root
			vote.VoterDID = voteFanoutAuthor
			requireVoteDeleteIfEmpty(t, lookups, nil, vote, voteFanoutAuthor)
		})
	}
}

func TestFanoutVoteCreate_DeleteIfEmptyVoterEligibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(*commentFanoutLookups, *VoteRecord)
	}{
		{"erased_voter", func(_ *commentFanoutLookups, vote *VoteRecord) {
			vote.VoterErased = true
		}},
		{"aggregator_voter", func(lookups *commentFanoutLookups, _ *VoteRecord) {
			lookups.aggregatorAccounts = map[string]bool{voteFanoutVoter: true}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lookups, vote := qualifyingVoteFanout()
			test.setup(&lookups, &vote)
			requireVoteDeleteIfEmpty(t, lookups, nil, vote, voteFanoutAuthor)
		})
	}
}

func TestFanoutVoteCreate_DeleteIfEmptyBlocks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, blocker, blocked string }{
		{"recipient_blocks_voter", voteFanoutAuthor, voteFanoutVoter},
		{"voter_blocks_recipient", voteFanoutVoter, voteFanoutAuthor},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lookups, vote := qualifyingVoteFanout()
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: test.blocker, blockedDID: test.blocked}: true}
			requireVoteDeleteIfEmpty(t, lookups, nil, vote, voteFanoutAuthor)
		})
	}
}

func TestFanoutVoteCreate_DeleteIfEmptyTimeGates(t *testing.T) {
	t.Parallel()
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name                              string
		createdAt, activatedAt, indexTime time.Time
	}{
		{"before_activation", indexTime, indexTime.Add(time.Nanosecond), indexTime.Add(time.Minute)},
		{"eight_days_old", indexTime.Add(-8 * 24 * time.Hour), indexTime.Add(-30 * 24 * time.Hour), indexTime},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lookups, vote := qualifyingVoteFanout()
			vote.CreatedAt = test.createdAt
			lookups.activatedAt, lookups.indexTime = test.activatedAt, test.indexTime
			requireVoteDeleteIfEmpty(t, lookups, nil, vote, voteFanoutAuthor)
		})
	}
}

func TestFanoutVoteCreate_DeleteIfEmptyRecipientEligibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		setup       func(*commentFanoutLookups)
		bridgeHosts BridgeHostChecker
	}{
		{"unindexed", func(lookups *commentFanoutLookups) {
			delete(lookups.indexedUsers, voteFanoutAuthor)
		}, nil},
		{"erased", func(lookups *commentFanoutLookups) {
			lookups.erasedAccounts = map[string]bool{voteFanoutAuthor: true}
		}, nil},
		{"aggregator", func(lookups *commentFanoutLookups) {
			lookups.aggregatorAccounts = map[string]bool{voteFanoutAuthor: true}
		}, nil},
		{"trusted_bridge_pds", func(lookups *commentFanoutLookups) {
			lookups.userPDSURLs = map[string]string{voteFanoutAuthor: voteFanoutBridgePDS}
		}, &notificationTestBridgeHosts{trustedURLs: map[string]bool{voteFanoutBridgePDS: true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lookups, vote := qualifyingVoteFanout()
			test.setup(&lookups)
			requireVoteDeleteIfEmpty(t, lookups, test.bridgeHosts, vote, voteFanoutAuthor)
		})
	}
}

func TestFanoutVoteCreate_UnresolvableSubjectsStayNoChange(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, subject, root string
		missingLegacyPost   bool
	}{
		{"unparsable_uri", voteFanoutPost + "/", "", false},
		{"unsupported_collection", "at://" + voteFanoutAuthor + "/app.bsky.feed.post/post", "", false},
		{"legacy_post_without_row", voteFanoutLegacyPost, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lookups, vote := qualifyingVoteFanout()
			// Same fake and voter: a resolvable sibling must actually produce an intent.
			if test.missingLegacyPost {
				vote.SubjectURI = voteFanoutLegacyPost
			}
			requireVoteBump(t, lookups, nil, vote, voteFanoutAuthor, vote.SubjectURI)
			if test.missingLegacyPost {
				delete(lookups.legacyPostAuthors, voteFanoutLegacyPost)
			}
			vote.SubjectURI, vote.SubjectRootURI = test.subject, test.root
			intent, err := FanoutVoteCreate(context.Background(), lookups, nil, vote)
			require.NoError(t, err)
			require.Equal(t, UpvoteGroupIntent{}, intent, "unresolvable subjects must not target any group")
		})
	}
}

// The comment URI's authority names the recipient whatever the stored root is,
// for example after a resurrection re-creates the comment under a different
// root. Only a bump needs a post root, so without one every vote change asks to
// delete the commenter's group if empty, before any eligibility lookup.
func TestFanoutVote_CommentWithoutPostRootDeletesIfEmpty(t *testing.T) {
	t.Parallel()
	for _, root := range []struct{ name, uri string }{
		{"unparsable_root", voteFanoutPost + "/"},
		{"empty_root", ""},
		{"nonpost_root", voteFanoutComment},
	} {
		t.Run(root.name, func(t *testing.T) {
			t.Parallel()
			for _, operation := range []struct {
				name   string
				fanout func(commentFanoutLookups, VoteRecord) (UpvoteGroupIntent, error)
			}{
				{"create_upvote", func(lookups commentFanoutLookups, vote VoteRecord) (UpvoteGroupIntent, error) {
					return FanoutVoteCreate(context.Background(), lookups, nil, vote)
				}},
				{"create_downvote", func(lookups commentFanoutLookups, vote VoteRecord) (UpvoteGroupIntent, error) {
					vote.Direction = "down"
					return FanoutVoteCreate(context.Background(), lookups, nil, vote)
				}},
				{"removal", func(lookups commentFanoutLookups, vote VoteRecord) (UpvoteGroupIntent, error) {
					return FanoutVoteRemoval(context.Background(), lookups, vote)
				}},
			} {
				t.Run(operation.name, func(t *testing.T) {
					t.Parallel()
					lookups, vote := qualifyingVoteFanout()
					lookups.indexedUsers[voteFanoutCommenter] = true
					vote.SubjectURI, vote.SubjectRootURI = voteFanoutCommenterComment, voteFanoutPost
					// Same fake and voter: under a post root this upvote bumps the commenter's group.
					requireVoteBump(t, lookups, nil, vote, voteFanoutCommenter, voteFanoutPost)
					var legacyCalls, activationCalls, indexCalls, aggregatorCalls int
					var recipientCalls [][]string
					lookups.legacyPostAuthorCalls = &legacyCalls
					lookups.activatedAtCalls = &activationCalls
					lookups.indexTimeCalls = &indexCalls
					lookups.isAggregatorCalls = &aggregatorCalls
					lookups.recipientFactsCalls = &recipientCalls
					vote.SubjectRootURI = root.uri
					intent, err := operation.fanout(lookups, vote)
					require.NoError(t, err)
					require.Equal(t, UpvoteGroupIntent{
						Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutCommenter,
						SubjectURI: voteFanoutCommenterComment,
					}, intent, "a comment without a post root must ask to delete the commenter's group if empty")
					require.Zero(t, legacyCalls, "a comment vote must not look up a post author")
					require.Zero(t, activationCalls, "a comment without a post root must not read activation")
					require.Zero(t, indexCalls, "a comment without a post root must not read index time")
					require.Zero(t, aggregatorCalls, "a comment without a post root must not read voter aggregator status")
					require.Empty(t, recipientCalls, "a comment without a post root must not read recipient facts")
				})
			}
		})
	}
}

func TestFanoutVoteRemoval_ResolvedSubjectsIgnoreVoteEligibility(t *testing.T) {
	t.Parallel()
	for _, subject := range []struct {
		name, uri, root string
		legacyCalls     int
	}{
		{"postv2", voteFanoutPost, "", 0},
		{"comment", voteFanoutComment, voteFanoutPost, 0},
		{"legacy_post", voteFanoutLegacyPost, "", 1},
	} {
		t.Run(subject.name, func(t *testing.T) {
			t.Parallel()
			for _, scenario := range []struct {
				name  string
				setup func(*commentFanoutLookups, *VoteRecord)
			}{
				{"upvote", func(_ *commentFanoutLookups, _ *VoteRecord) {}},
				{"downvote", func(_ *commentFanoutLookups, vote *VoteRecord) {
					vote.Direction = "down"
				}},
				{"self_vote", func(_ *commentFanoutLookups, vote *VoteRecord) {
					vote.VoterDID = voteFanoutAuthor
				}},
				{"erased_voter_before_activation", func(lookups *commentFanoutLookups, vote *VoteRecord) {
					vote.VoterErased = true
					lookups.activatedAt = vote.CreatedAt.Add(time.Second)
					lookups.indexTime = vote.CreatedAt.Add(time.Minute)
				}},
				{"aggregator_voter_eight_days_old", func(lookups *commentFanoutLookups, vote *VoteRecord) {
					lookups.aggregatorAccounts = map[string]bool{voteFanoutVoter: true}
					lookups.indexTime = vote.CreatedAt.Add(8 * 24 * time.Hour)
					lookups.activatedAt = vote.CreatedAt.Add(-24 * time.Hour)
				}},
				// The vote consumer passes only the subject URI on removal.
				{"subject_uri_only", func(_ *commentFanoutLookups, vote *VoteRecord) {
					*vote = VoteRecord{SubjectURI: vote.SubjectURI}
				}},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					t.Parallel()
					lookups, vote := qualifyingVoteFanout()
					vote.SubjectURI, vote.SubjectRootURI = subject.uri, subject.root
					scenario.setup(&lookups, &vote)
					var legacyCalls, activationCalls, indexCalls, aggregatorCalls int
					var recipientCalls [][]string
					lookups.legacyPostAuthorCalls = &legacyCalls
					lookups.activatedAtCalls = &activationCalls
					lookups.indexTimeCalls = &indexCalls
					lookups.isAggregatorCalls = &aggregatorCalls
					lookups.recipientFactsCalls = &recipientCalls
					intent, err := FanoutVoteRemoval(context.Background(), lookups, vote)
					require.NoError(t, err)
					require.Equal(t, UpvoteGroupDeleteIfEmpty, intent.Action,
						"removing a vote from a resolved subject must request delete-if-empty")
					require.Equal(t, voteFanoutAuthor, intent.RecipientDID)
					require.Equal(t, vote.SubjectURI, intent.SubjectURI)
					require.Equal(t, subject.legacyCalls, legacyCalls,
						"only legacy posts need an author lookup")
					require.Zero(t, activationCalls, "removal must not check activation")
					require.Zero(t, indexCalls, "removal must not check freshness")
					require.Zero(t, aggregatorCalls, "removal must not check voter aggregator status")
					require.Empty(t, recipientCalls, "removal must not check recipient facts")
				})
			}
		})
	}
}

func TestFanoutVoteRemoval_UnresolvableSubjectsStayNoChange(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, subject, root string
		missingLegacyPost   bool
	}{
		{"unparsable_uri", voteFanoutPost + "/", "", false},
		{"unsupported_collection", "at://" + voteFanoutAuthor + "/app.bsky.feed.post/post", "", false},
		{"legacy_post_without_row", voteFanoutLegacyPost, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lookups, vote := qualifyingVoteFanout()
			// Same fake: a resolvable sibling must actually produce an intent.
			if test.missingLegacyPost {
				vote.SubjectURI = voteFanoutLegacyPost
			}
			control, err := FanoutVoteRemoval(context.Background(), lookups, vote)
			require.NoError(t, err)
			require.Equal(t, UpvoteGroupIntent{
				Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutAuthor, SubjectURI: vote.SubjectURI,
			}, control, "the resolvable sibling must request delete-if-empty")
			if test.missingLegacyPost {
				delete(lookups.legacyPostAuthors, voteFanoutLegacyPost)
			}
			vote.SubjectURI, vote.SubjectRootURI = test.subject, test.root
			intent, err := FanoutVoteRemoval(context.Background(), lookups, vote)
			require.NoError(t, err)
			require.Equal(t, UpvoteGroupIntent{}, intent, "unresolvable subjects must not target any group")
		})
	}
}

func TestFanoutVoteRemoval_LegacyAuthorLookupError(t *testing.T) {
	t.Parallel()
	lookups, vote := qualifyingVoteFanout()
	vote.SubjectURI = voteFanoutLegacyPost
	lookupError := errors.New("legacy post author lookup failed")
	lookups.legacyPostAuthorError = lookupError
	var legacyCalls int
	lookups.legacyPostAuthorCalls = &legacyCalls
	intent, err := FanoutVoteRemoval(context.Background(), lookups, vote)
	require.ErrorIs(t, err, lookupError, "legacy author lookup failure must propagate wrapped")
	require.Equal(t, UpvoteGroupIntent{}, intent)
	require.Equal(t, 1, legacyCalls)
}
