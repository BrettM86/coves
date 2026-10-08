package notifications

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	voteFanoutVoter      = "did:plc:upvotefanoutvoter"
	voteFanoutAuthor     = "did:plc:upvotefanoutauthor"
	voteFanoutPost       = "at://" + voteFanoutAuthor + "/social.coves.community.postv2/post"
	voteFanoutComment    = "at://" + voteFanoutAuthor + "/social.coves.community.comment/comment"
	voteFanoutLegacyPost = "at://did:plc:upvotefanoutcommunity/social.coves.community.post/post"
	voteFanoutBridgePDS  = "https://bridge.test"
	// The commenter's comment sits under voteFanoutPost, whose author differs.
	voteFanoutCommenter        = "did:plc:upvotefanoutcommenter"
	voteFanoutCommenterComment = "at://" + voteFanoutCommenter + "/social.coves.community.comment/comment"
)

func qualifyingVoteFanout() (commentFanoutLookups, VoteRecord) {
	return commentFanoutLookups{
		indexedUsers:      map[string]bool{voteFanoutAuthor: true, voteFanoutVoter: true},
		legacyPostAuthors: map[string]string{voteFanoutLegacyPost: voteFanoutAuthor},
	}, VoteRecord{
		VoterDID: voteFanoutVoter, SubjectURI: voteFanoutPost, Direction: "up",
		CreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
	}
}

func requireVoteBump(t *testing.T, lookups commentFanoutLookups, bridgeHosts BridgeHostChecker, vote VoteRecord, recipient, root string) {
	t.Helper()
	intent, err := FanoutVoteCreate(context.Background(), lookups, bridgeHosts, vote)
	require.NoError(t, err)
	require.Equal(t, UpvoteGroupIntent{
		Action: UpvoteGroupBump, RecipientDID: recipient,
		SubjectURI: vote.SubjectURI, RootPostURI: root,
	}, intent, "a qualifying upvote must request exactly this recipient/subject group")
}

func requireVoteNoBump(t *testing.T, lookups commentFanoutLookups, bridgeHosts BridgeHostChecker, vote VoteRecord) {
	t.Helper()
	intent, err := FanoutVoteCreate(context.Background(), lookups, bridgeHosts, vote)
	require.NoError(t, err)
	require.NotEqual(t, UpvoteGroupBump, intent.Action, "this vote must not bump a group")
}

func TestFanoutVoteCreate_QualifyingSubjects(t *testing.T) {
	for _, test := range []struct {
		name, subject, root string
	}{
		{"postv2", voteFanoutPost, voteFanoutPost},
		{"comment", voteFanoutComment, voteFanoutPost},
		{"legacy_post_author_not_community", voteFanoutLegacyPost, voteFanoutLegacyPost},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			vote.SubjectURI = test.subject
			if test.name == "comment" {
				vote.SubjectRootURI = test.root
			}
			requireVoteBump(t, lookups, nil, vote, voteFanoutAuthor, test.root)
		})
	}
}

// The comment URI's authority names the recipient and the self rule. The root
// post's author is someone else, and a legacy root's author is never looked up.
func TestFanoutVoteCreate_CommentRecipientIsCommenterNotRootAuthor(t *testing.T) {
	for _, test := range []struct {
		name, voter, root string
		wantBump          bool
	}{
		{"third_party_voter", voteFanoutVoter, voteFanoutPost, true},
		{"root_post_author_voter", voteFanoutAuthor, voteFanoutPost, true},
		{"commenter_self_upvote", voteFanoutCommenter, voteFanoutPost, false},
		{"legacy_root_third_party_voter", voteFanoutVoter, voteFanoutLegacyPost, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			lookups.indexedUsers[voteFanoutCommenter] = true
			var legacyCalls int
			lookups.legacyPostAuthorCalls = &legacyCalls
			vote.VoterDID = test.voter
			vote.SubjectURI, vote.SubjectRootURI = voteFanoutCommenterComment, test.root
			intent, err := FanoutVoteCreate(context.Background(), lookups, nil, vote)
			require.NoError(t, err)
			require.Zero(t, legacyCalls, "a comment upvote must not look up its root post's author")
			if !test.wantBump {
				require.NotEqual(t, UpvoteGroupBump, intent.Action, "the commenter's own upvote must not bump a group")
				return
			}
			require.Equal(t, UpvoteGroupIntent{
				Action: UpvoteGroupBump, RecipientDID: voteFanoutCommenter,
				SubjectURI: voteFanoutCommenterComment, RootPostURI: test.root,
			}, intent, "the commenter, not the root post's author, receives the group")
		})
	}
}

func TestFanoutVoteCreate_DownSkipsEveryLookup(t *testing.T) {
	lookups, vote := qualifyingVoteFanout()
	vote.Direction = "down"
	var legacyCalls, activationCalls, indexCalls, aggregatorCalls int
	var recipientCalls [][]string
	lookups.legacyPostAuthorCalls = &legacyCalls
	lookups.activatedAtCalls = &activationCalls
	lookups.indexTimeCalls = &indexCalls
	lookups.isAggregatorCalls = &aggregatorCalls
	lookups.recipientFactsCalls = &recipientCalls
	requireVoteNoBump(t, lookups, nil, vote)
	require.Zero(t, legacyCalls, "downvotes must not resolve a legacy author")
	require.Zero(t, activationCalls, "downvotes must not read activation")
	require.Zero(t, indexCalls, "downvotes must not read index time")
	require.Zero(t, aggregatorCalls, "downvotes must not check voter aggregator status")
	require.Empty(t, recipientCalls, "downvotes must not look up recipients")
}

func TestFanoutVoteCreate_SelfUpvotesDoNotBump(t *testing.T) {
	for _, test := range []struct{ name, subject, root string }{
		{"own_postv2", voteFanoutPost, ""},
		{"own_comment", voteFanoutComment, voteFanoutPost},
		{"own_legacy_post", voteFanoutLegacyPost, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			vote.VoterDID = voteFanoutAuthor
			vote.SubjectURI, vote.SubjectRootURI = test.subject, test.root
			requireVoteNoBump(t, lookups, nil, vote)
		})
	}
}

func TestFanoutVoteCreate_VoterEligibility(t *testing.T) {
	for _, test := range []struct {
		name       string
		setup      func(*commentFanoutLookups, *VoteRecord)
		bridgeHost BridgeHostChecker
		wantBump   bool
	}{
		{"erased_voter", func(_ *commentFanoutLookups, vote *VoteRecord) {
			vote.VoterErased = true
		}, nil, false},
		{"aggregator_voter", func(lookups *commentFanoutLookups, _ *VoteRecord) {
			lookups.aggregatorAccounts = map[string]bool{voteFanoutVoter: true}
		}, nil, false},
		{"voter_without_users_row", func(lookups *commentFanoutLookups, _ *VoteRecord) {
			delete(lookups.indexedUsers, voteFanoutVoter)
		}, nil, true},
		{"bridge_hosted_voter_native_recipient", func(lookups *commentFanoutLookups, _ *VoteRecord) {
			lookups.userPDSURLs = map[string]string{voteFanoutVoter: voteFanoutBridgePDS}
		}, &notificationTestBridgeHosts{trustedURLs: map[string]bool{voteFanoutBridgePDS: true}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			test.setup(&lookups, &vote)
			if !test.wantBump {
				requireVoteNoBump(t, lookups, test.bridgeHost, vote)
				return
			}
			requireVoteBump(t, lookups, test.bridgeHost, vote, voteFanoutAuthor, voteFanoutPost)
			if checker, ok := test.bridgeHost.(*notificationTestBridgeHosts); ok {
				require.Equal(t, []string{"https://native.pds.test"}, checker.checkedURLs,
					"only the recipient's PDS is checked for bridge trust")
			}
		})
	}
}

func TestFanoutVoteCreate_BlocksBothDirections(t *testing.T) {
	for _, test := range []struct{ name, blocker, blocked string }{
		{"recipient_blocks_voter", voteFanoutAuthor, voteFanoutVoter},
		{"voter_blocks_recipient", voteFanoutVoter, voteFanoutAuthor},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: test.blocker, blockedDID: test.blocked}: true}
			requireVoteNoBump(t, lookups, nil, vote)
		})
	}
}

func TestFanoutVoteCreate_TimeGateBoundaries(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name                              string
		createdAt, activatedAt, indexTime time.Time
		wantBump                          bool
	}{
		{"before_activation", indexTime, indexTime.Add(time.Nanosecond), indexTime.Add(time.Minute), false},
		{"exactly_at_activation", indexTime, indexTime, indexTime.Add(time.Minute), true},
		{"older_than_seven_days", indexTime.Add(-7*24*time.Hour - time.Microsecond), indexTime.Add(-30 * 24 * time.Hour), indexTime, false},
		{"exactly_seven_days_old", indexTime.Add(-7 * 24 * time.Hour), indexTime.Add(-30 * 24 * time.Hour), indexTime, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			vote.CreatedAt = test.createdAt
			lookups.activatedAt, lookups.indexTime = test.activatedAt, test.indexTime
			if test.wantBump {
				requireVoteBump(t, lookups, nil, vote, voteFanoutAuthor, voteFanoutPost)
			} else {
				requireVoteNoBump(t, lookups, nil, vote)
			}
		})
	}
}

func TestFanoutVoteCreate_RecipientEligibility(t *testing.T) {
	for _, test := range []struct {
		name        string
		setup       func(*commentFanoutLookups)
		bridgeHosts BridgeHostChecker
		wantBump    bool
	}{
		{"unindexed_recipient", func(lookups *commentFanoutLookups) {
			delete(lookups.indexedUsers, voteFanoutAuthor)
		}, nil, false},
		{"erased_recipient", func(lookups *commentFanoutLookups) {
			lookups.erasedAccounts = map[string]bool{voteFanoutAuthor: true}
		}, nil, false},
		{"aggregator_recipient", func(lookups *commentFanoutLookups) {
			lookups.aggregatorAccounts = map[string]bool{voteFanoutAuthor: true}
		}, nil, false},
		{"bridge_hosted_recipient", func(lookups *commentFanoutLookups) {
			lookups.userPDSURLs = map[string]string{voteFanoutAuthor: voteFanoutBridgePDS}
		}, &notificationTestBridgeHosts{trustedURLs: map[string]bool{voteFanoutBridgePDS: true}}, false},
		{"bridge_hosted_recipient_nil_checker", func(lookups *commentFanoutLookups) {
			lookups.userPDSURLs = map[string]string{voteFanoutAuthor: voteFanoutBridgePDS}
		}, nil, true},
		{"bridge_hosted_recipient_nil_pointer_checker", func(lookups *commentFanoutLookups) {
			lookups.userPDSURLs = map[string]string{voteFanoutAuthor: voteFanoutBridgePDS}
		}, (*notificationTestBridgeHosts)(nil), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			test.setup(&lookups)
			if test.wantBump {
				requireVoteBump(t, lookups, test.bridgeHosts, vote, voteFanoutAuthor, voteFanoutPost)
			} else {
				requireVoteNoBump(t, lookups, test.bridgeHosts, vote)
			}
		})
	}
}

func TestFanoutVoteCreate_UnsupportedSubjectsHaveNoIntentOrError(t *testing.T) {
	for _, test := range []struct {
		name, subject, root string
		missingLegacyPost   bool
	}{
		{"unparsable_subject", voteFanoutPost + "/", "", false},
		{"unsupported_collection", "at://" + voteFanoutAuthor + "/app.bsky.feed.post/post", "", false},
		{"legacy_post_without_row", voteFanoutLegacyPost, "", true},
		{"comment_with_unparsable_root", voteFanoutComment, voteFanoutPost + "/", false},
		{"comment_with_empty_root", voteFanoutComment, "", false},
		{"comment_with_comment_root", voteFanoutComment, voteFanoutComment, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			vote.SubjectURI, vote.SubjectRootURI = test.subject, test.root
			if test.missingLegacyPost {
				delete(lookups.legacyPostAuthors, voteFanoutLegacyPost)
				lookups.indexedUsers["did:plc:upvotefanoutcommunity"] = true
			}
			requireVoteNoBump(t, lookups, nil, vote)
		})
	}
}
