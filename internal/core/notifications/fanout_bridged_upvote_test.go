package notifications

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFanoutBridgedUpvoteChange_Intents(t *testing.T) {
	const unsupportedSubject = "at://" + voteFanoutAuthor + "/app.bsky.feed.post/post"
	poison := errors.New("unrelated lookup must not run")
	for _, test := range []struct {
		name        string
		change      BridgedUpvoteChange
		setup       func(*commentFanoutLookups)
		bridgeHosts BridgeHostChecker
		want        UpvoteGroupIntent
		wantActor   bool
		poisonGates bool
	}{
		{
			name: "postv2_increase", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 0, Upvotes: 3},
			want:      UpvoteGroupIntent{Action: UpvoteGroupBump, RecipientDID: voteFanoutAuthor, SubjectURI: voteFanoutPost, RootPostURI: voteFanoutPost},
			wantActor: true, poisonGates: true,
		},
		{
			name:      "comment_increase_goes_to_commenter_not_root_author",
			change:    BridgedUpvoteChange{SubjectURI: voteFanoutCommenterComment, SubjectRootURI: voteFanoutPost, PreviousUpvotes: 3, Upvotes: 4},
			want:      UpvoteGroupIntent{Action: UpvoteGroupBump, RecipientDID: voteFanoutCommenter, SubjectURI: voteFanoutCommenterComment, RootPostURI: voteFanoutPost},
			wantActor: true, poisonGates: true,
		},
		{
			name: "legacy_post_resolves_row_author_not_community", change: BridgedUpvoteChange{SubjectURI: voteFanoutLegacyPost, PreviousUpvotes: 0, Upvotes: 3},
			want:      UpvoteGroupIntent{Action: UpvoteGroupBump, RecipientDID: voteFanoutAuthor, SubjectURI: voteFanoutLegacyPost, RootPostURI: voteFanoutLegacyPost},
			wantActor: true, poisonGates: true,
		},
		{name: "equal_total", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 4, Upvotes: 4}, want: UpvoteGroupIntent{}},
		{name: "decrease", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 4, Upvotes: 2},
			want: UpvoteGroupIntent{Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutAuthor, SubjectURI: voteFanoutPost}},
		{name: "decrease_to_zero", change: BridgedUpvoteChange{SubjectURI: voteFanoutLegacyPost, PreviousUpvotes: 3, Upvotes: 0},
			want: UpvoteGroupIntent{Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutAuthor, SubjectURI: voteFanoutLegacyPost}},
		{name: "withdrawn_decrease_ignores_withdrawal_and_recipient", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 4, Upvotes: 2},
			setup: func(lookups *commentFanoutLookups) {
				lookups.referenceStates = map[string]ReferenceState{voteFanoutPost: ReferenceDeleted}
				lookups.referenceStatesError = poison
				lookups.recipientFactsError = poison
			},
			want: UpvoteGroupIntent{Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutAuthor, SubjectURI: voteFanoutPost}},
		{name: "unparsable_subject_increase", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost + "/", PreviousUpvotes: 0, Upvotes: 3}, want: UpvoteGroupIntent{}},
		{name: "unparsable_subject_decrease", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost + "/", PreviousUpvotes: 3, Upvotes: 0}, want: UpvoteGroupIntent{}},
		{name: "other_collection_increase", change: BridgedUpvoteChange{SubjectURI: unsupportedSubject, PreviousUpvotes: 0, Upvotes: 3}, want: UpvoteGroupIntent{}},
		{name: "other_collection_decrease", change: BridgedUpvoteChange{SubjectURI: unsupportedSubject, PreviousUpvotes: 3, Upvotes: 0}, want: UpvoteGroupIntent{}},
		{name: "missing_legacy_post_increase", change: BridgedUpvoteChange{SubjectURI: voteFanoutLegacyPost, PreviousUpvotes: 0, Upvotes: 3},
			setup: func(lookups *commentFanoutLookups) { delete(lookups.legacyPostAuthors, voteFanoutLegacyPost) }, want: UpvoteGroupIntent{}},
		{name: "missing_legacy_post_decrease", change: BridgedUpvoteChange{SubjectURI: voteFanoutLegacyPost, PreviousUpvotes: 3, Upvotes: 0},
			setup: func(lookups *commentFanoutLookups) { delete(lookups.legacyPostAuthors, voteFanoutLegacyPost) }, want: UpvoteGroupIntent{}},
		{name: "comment_empty_root_increase", change: BridgedUpvoteChange{SubjectURI: voteFanoutCommenterComment, PreviousUpvotes: 0, Upvotes: 3}, want: UpvoteGroupIntent{}},
		{name: "comment_unparsable_root_increase", change: BridgedUpvoteChange{SubjectURI: voteFanoutCommenterComment, SubjectRootURI: voteFanoutPost + "/", PreviousUpvotes: 0, Upvotes: 3}, want: UpvoteGroupIntent{}},
		{name: "comment_nonpost_root_increase", change: BridgedUpvoteChange{SubjectURI: voteFanoutCommenterComment, SubjectRootURI: voteFanoutComment, PreviousUpvotes: 0, Upvotes: 3}, want: UpvoteGroupIntent{}},
		{name: "comment_without_post_root_decrease", change: BridgedUpvoteChange{SubjectURI: voteFanoutCommenterComment, PreviousUpvotes: 3, Upvotes: 0},
			want: UpvoteGroupIntent{Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutCommenter, SubjectURI: voteFanoutCommenterComment}},
		{name: "recipient_not_indexed", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 0, Upvotes: 3},
			setup: func(lookups *commentFanoutLookups) { delete(lookups.indexedUsers, voteFanoutAuthor) }, want: UpvoteGroupIntent{}},
		{name: "recipient_erased", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 0, Upvotes: 3},
			setup: func(lookups *commentFanoutLookups) { lookups.erasedAccounts = map[string]bool{voteFanoutAuthor: true} }, want: UpvoteGroupIntent{}},
		{name: "recipient_aggregator", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 0, Upvotes: 3},
			setup: func(lookups *commentFanoutLookups) {
				lookups.aggregatorAccounts = map[string]bool{voteFanoutAuthor: true}
			}, want: UpvoteGroupIntent{}},
		{name: "recipient_on_trusted_bridge_pds", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 0, Upvotes: 3},
			setup: func(lookups *commentFanoutLookups) {
				lookups.userPDSURLs = map[string]string{voteFanoutAuthor: voteFanoutBridgePDS}
			},
			bridgeHosts: &notificationTestBridgeHosts{trustedURLs: map[string]bool{voteFanoutBridgePDS: true}}, want: UpvoteGroupIntent{}},
		{name: "withdrawn_subject", change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 0, Upvotes: 3},
			setup: func(lookups *commentFanoutLookups) {
				lookups.referenceStates = map[string]ReferenceState{voteFanoutPost: ReferenceRemovedByModerator}
			}, want: UpvoteGroupIntent{}},
		{name: "withdrawn_comment_root", change: BridgedUpvoteChange{SubjectURI: voteFanoutCommenterComment, SubjectRootURI: voteFanoutPost, PreviousUpvotes: 0, Upvotes: 3},
			setup: func(lookups *commentFanoutLookups) {
				lookups.referenceStates = map[string]ReferenceState{voteFanoutPost: ReferenceDeleted}
			}, want: UpvoteGroupIntent{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups := commentFanoutLookups{
				indexedUsers:      map[string]bool{voteFanoutAuthor: true, voteFanoutCommenter: true, "did:plc:upvotefanoutcommunity": true},
				legacyPostAuthors: map[string]string{voteFanoutLegacyPost: voteFanoutAuthor},
			}
			if test.setup != nil {
				test.setup(&lookups)
			}
			if test.poisonGates {
				lookups.activatedAtError = poison
				lookups.indexTimeError = poison
				lookups.earlierUpvoteError = poison
				lookups.isAggregatorError = poison
			}
			var actors []string
			lookups.recipientFactsActors = &actors
			intent, err := FanoutBridgedUpvoteChange(context.Background(), lookups, test.bridgeHosts, test.change)
			require.NoError(t, err)
			require.Equal(t, test.want, intent)
			if test.wantActor {
				require.Equal(t, []string{""}, actors, "bridged totals have no actor DID")
			}
		})
	}
}

func TestFanoutBridgedUpvoteChange_LookupErrorsAreWrapped(t *testing.T) {
	for _, test := range []struct {
		name     string
		subject  string
		setError func(*commentFanoutLookups, error)
	}{
		{"LegacyPostAuthor", voteFanoutLegacyPost, func(lookups *commentFanoutLookups, err error) { lookups.legacyPostAuthorError = err }},
		{"ReferenceStates", voteFanoutPost, func(lookups *commentFanoutLookups, err error) { lookups.referenceStatesError = err }},
		{"RecipientFacts", voteFanoutPost, func(lookups *commentFanoutLookups, err error) { lookups.recipientFactsError = err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New("bridged upvote lookup failed")
			lookups := commentFanoutLookups{
				indexedUsers:      map[string]bool{voteFanoutAuthor: true},
				legacyPostAuthors: map[string]string{voteFanoutLegacyPost: voteFanoutAuthor},
			}
			var actors []string
			lookups.recipientFactsActors = &actors
			test.setError(&lookups, sentinel)
			intent, err := FanoutBridgedUpvoteChange(context.Background(), lookups, nil, BridgedUpvoteChange{
				SubjectURI: test.subject, PreviousUpvotes: 0, Upvotes: 3,
			})
			require.ErrorIs(t, err, sentinel)
			require.NotEqual(t, sentinel, err, "lookup error must be wrapped")
			require.Equal(t, UpvoteGroupIntent{}, intent)
			if test.name == "RecipientFacts" {
				require.Equal(t, []string{""}, actors, "bridged totals have no actor DID")
			}
		})
	}
}
