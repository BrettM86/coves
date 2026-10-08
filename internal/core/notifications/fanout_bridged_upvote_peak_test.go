package notifications

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFanoutBridgedUpvoteChange_PeakRule(t *testing.T) {
	for _, test := range []struct {
		name   string
		change BridgedUpvoteChange
		want   UpvoteGroupIntent
	}{
		{
			name:   "post_below_peak",
			change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 3, PeakUpvotes: 5, Upvotes: 4},
			want:   UpvoteGroupIntent{},
		},
		{
			name:   "post_returns_to_peak",
			change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 3, PeakUpvotes: 5, Upvotes: 5},
			want:   UpvoteGroupIntent{},
		},
		{
			name:   "post_exceeds_peak",
			change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 3, PeakUpvotes: 5, Upvotes: 6},
			want:   UpvoteGroupIntent{Action: UpvoteGroupBump, RecipientDID: voteFanoutAuthor, SubjectURI: voteFanoutPost, RootPostURI: voteFanoutPost},
		},
		{
			name:   "comment_exceeds_peak",
			change: BridgedUpvoteChange{SubjectURI: voteFanoutCommenterComment, SubjectRootURI: voteFanoutPost, PreviousUpvotes: 3, PeakUpvotes: 5, Upvotes: 6},
			want:   UpvoteGroupIntent{Action: UpvoteGroupBump, RecipientDID: voteFanoutCommenter, SubjectURI: voteFanoutCommenterComment, RootPostURI: voteFanoutPost},
		},
		{
			name:   "missing_peak_equal_to_previous",
			change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 5, PeakUpvotes: 0, Upvotes: 5},
			want:   UpvoteGroupIntent{},
		},
		{
			name:   "missing_peak_exceeds_previous",
			change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 5, PeakUpvotes: 0, Upvotes: 6},
			want:   UpvoteGroupIntent{Action: UpvoteGroupBump, RecipientDID: voteFanoutAuthor, SubjectURI: voteFanoutPost, RootPostURI: voteFanoutPost},
		},
		{
			name:   "decrease_from_peak",
			change: BridgedUpvoteChange{SubjectURI: voteFanoutPost, PreviousUpvotes: 5, PeakUpvotes: 7, Upvotes: 3},
			want:   UpvoteGroupIntent{Action: UpvoteGroupDeleteIfEmpty, RecipientDID: voteFanoutAuthor, SubjectURI: voteFanoutPost},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups := commentFanoutLookups{indexedUsers: map[string]bool{
				voteFanoutAuthor: true, voteFanoutCommenter: true,
			}}
			if test.want == (UpvoteGroupIntent{}) {
				poison := errors.New("peak-suppressed total must not look up withdrawn references or recipients")
				lookups.referenceStatesError = poison
				lookups.recipientFactsError = poison
			}
			intent, err := FanoutBridgedUpvoteChange(context.Background(), lookups, nil, test.change)
			require.NoError(t, err)
			require.Equal(t, test.want, intent)
		})
	}
}
