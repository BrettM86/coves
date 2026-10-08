package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func notificationTimeGateComment(createdAt time.Time) CommentRecord {
	postURI := "at://did:plc:timegaterecipient/social.coves.community.postv2/post"
	return CommentRecord{
		URI: "at://did:plc:timegatecommenter/social.coves.community.comment/reply",
		CID: "bafyreitimegatereply", AuthorDID: "did:plc:timegatecommenter",
		ParentURI: postURI, RootURI: postURI, CreatedAt: createdAt,
	}
}

func TestFanoutCommentCreate_TimeGateBoundaries(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		createdAt   time.Time
		activatedAt time.Time
		indexTime   time.Time
		wantIntent  bool
	}{
		{"before_activation", indexTime, indexTime.Add(time.Nanosecond), indexTime.Add(time.Minute), false},
		{"exactly_at_activation", indexTime, indexTime, indexTime.Add(time.Minute), true},
		{"older_than_seven_days", indexTime.Add(-7*24*time.Hour - time.Microsecond), indexTime.Add(-30 * 24 * time.Hour), indexTime, false},
		{"exactly_seven_days_old", indexTime.Add(-7 * 24 * time.Hour), indexTime.Add(-30 * 24 * time.Hour), indexTime, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var activationCalls, indexTimeCalls int
			comment := notificationTimeGateComment(test.createdAt)
			lookups := commentFanoutLookups{
				indexedUsers:     map[string]bool{"did:plc:timegaterecipient": true},
				activatedAt:      test.activatedAt,
				indexTime:        test.indexTime,
				activatedAtCalls: &activationCalls,
				indexTimeCalls:   &indexTimeCalls,
			}
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.NoError(t, err)
			if !test.wantIntent {
				require.Empty(t, intents, "a reply outside the time gate must not notify an indexed recipient")
				return
			}
			require.Equal(t, []Intent{{
				Reason: ReasonPostReply, RecipientDID: "did:plc:timegaterecipient", ActorDID: comment.AuthorDID,
				RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: comment.RootURI,
				RootPostURI: comment.RootURI, RecordCreatedAt: comment.CreatedAt,
			}}, intents, "a reply exactly on the boundary still notifies its post author")
			require.GreaterOrEqual(t, activationCalls, 1, "the activation boundary must come from ActivatedAt")
			require.GreaterOrEqual(t, indexTimeCalls, 1, "the seven-day boundary must come from IndexTime")
		})
	}
}

func TestFanoutCommentCreate_FutureCreatedAtRetainsOnlyDisplayTimestamp(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	comment := notificationTimeGateComment(indexTime.Add(time.Hour))
	var activationCalls, indexTimeCalls int
	lookups := commentFanoutLookups{
		indexedUsers:     map[string]bool{"did:plc:timegaterecipient": true},
		activatedAt:      indexTime.Add(-time.Hour),
		indexTime:        indexTime,
		activatedAtCalls: &activationCalls,
		indexTimeCalls:   &indexTimeCalls,
	}
	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{{
		Reason: ReasonPostReply, RecipientDID: "did:plc:timegaterecipient", ActorDID: comment.AuthorDID,
		RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: comment.RootURI,
		RootPostURI: comment.RootURI, RecordCreatedAt: comment.CreatedAt,
	}}, intents, "future createdAt is display-only and must not change identity or ordering fields")
	require.GreaterOrEqual(t, activationCalls, 1, "the future record must pass through the activation gate")
	require.GreaterOrEqual(t, indexTimeCalls, 1, "the future record must pass through the index-time gate")
}

func TestFanoutCommentCreate_OneTimeGateAndRecipientFactsLookupPerRecord(t *testing.T) {
	const recipientDID = "did:plc:timegaterecipient"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	comment := notificationTimeGateComment(createdAt)
	var activationCalls, indexTimeCalls int
	var recipientFactsCalls [][]string
	lookups := commentFanoutLookups{
		indexedUsers:        map[string]bool{recipientDID: true},
		activatedAt:         createdAt.Add(-time.Minute),
		indexTime:           createdAt.Add(time.Minute),
		activatedAtCalls:    &activationCalls,
		indexTimeCalls:      &indexTimeCalls,
		recipientFactsCalls: &recipientFactsCalls,
	}
	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{{
		Reason: ReasonPostReply, RecipientDID: recipientDID, ActorDID: comment.AuthorDID,
		RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: comment.RootURI,
		RootPostURI: comment.RootURI, RecordCreatedAt: createdAt,
	}}, intents, "eligible reply must still produce B's postReply")
	require.Equal(t, 1, activationCalls, "check the activation gate once per record")
	require.Equal(t, 1, indexTimeCalls, "check the freshness gate once per record")
	require.Equal(t, [][]string{{recipientDID}}, recipientFactsCalls,
		"read recipient facts once with exactly B as the candidate")
}

func TestFanoutCommentCreate_AllLookupErrorsAreWrapped(t *testing.T) {
	const recipientDID = "did:plc:timegaterecipient"
	const legacyPostURI = "at://did:plc:timegatecommunity/social.coves.community.post/post"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		legacyPost bool
		setError   func(*commentFanoutLookups, error)
	}{
		{"ReferenceStates", false, func(lookups *commentFanoutLookups, err error) { lookups.referenceStatesError = err }},
		{"LegacyPostAuthor", true, func(lookups *commentFanoutLookups, err error) { lookups.legacyPostAuthorError = err }},
		{"ActivatedAt", false, func(lookups *commentFanoutLookups, err error) { lookups.activatedAtError = err }},
		{"IndexTime", false, func(lookups *commentFanoutLookups, err error) { lookups.indexTimeError = err }},
		{"RecipientFacts", false, func(lookups *commentFanoutLookups, err error) {
			lookups.recipientFactsError = err
		}},
		{"ExistingMentionRecipients", false, func(lookups *commentFanoutLookups, err error) {
			lookups.existingMentionRecipientsError = err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New("notification lookup failed")
			comment := notificationTimeGateComment(createdAt)
			// Indexed D's mention would survive a fan-out that kept going after
			// the failed lookup, so an empty result proves fan-out stopped.
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
			lookups := commentFanoutLookups{
				indexedUsers:      map[string]bool{recipientDID: true, mentionRecipientDDID: true},
				legacyPostAuthors: map[string]string{legacyPostURI: recipientDID},
			}
			if test.legacyPost {
				comment.ParentURI, comment.RootURI = legacyPostURI, legacyPostURI
			}
			test.setError(&lookups, sentinel)
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.ErrorIs(t, err, sentinel, "a failing lookup must abort fan-out")
			if test.name == "ReferenceStates" {
				require.ErrorContains(t, err, "look up notification reference states")
			}
			require.NotEqual(t, sentinel, err, "the fan-out error must wrap the failing lookup with context")
			require.Empty(t, intents, "a failing lookup must never produce an intent")
		})
	}
}

func TestFanoutVoteCreate_AllLookupErrorsAreWrapped(t *testing.T) {
	for _, test := range []struct {
		name, subject string
		setError      func(*commentFanoutLookups, error)
	}{
		{"ReferenceStates", voteFanoutPost, func(lookups *commentFanoutLookups, err error) { lookups.referenceStatesError = err }},
		{"LegacyPostAuthor", voteFanoutLegacyPost, func(lookups *commentFanoutLookups, err error) { lookups.legacyPostAuthorError = err }},
		{"ActivatedAt", voteFanoutPost, func(lookups *commentFanoutLookups, err error) { lookups.activatedAtError = err }},
		{"IndexTime", voteFanoutPost, func(lookups *commentFanoutLookups, err error) { lookups.indexTimeError = err }},
		{"IsAggregator", voteFanoutPost, func(lookups *commentFanoutLookups, err error) { lookups.isAggregatorError = err }},
		{"RecipientFacts", voteFanoutPost, func(lookups *commentFanoutLookups, err error) { lookups.recipientFactsError = err }},
		{"EarlierUpvoteExists", voteFanoutPost, func(lookups *commentFanoutLookups, err error) { lookups.earlierUpvoteError = err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New("vote notification lookup failed")
			lookups, vote := qualifyingVoteFanout()
			vote.SubjectURI = test.subject
			lookups.activatedAt = vote.CreatedAt.Add(-time.Minute)
			lookups.indexTime = vote.CreatedAt.Add(time.Minute)
			test.setError(&lookups, sentinel)
			intent, err := FanoutVoteCreate(context.Background(), lookups, nil, vote)
			require.NotEqual(t, UpvoteGroupBump, intent.Action, "a failed lookup must not bump the group")
			require.ErrorIs(t, err, sentinel, "a failed %s lookup must abort vote fan-out", test.name)
			if test.name == "ReferenceStates" {
				require.ErrorContains(t, err, "look up notification reference states")
			}
			require.NotEqual(t, sentinel, err, "the %s error must be wrapped with context", test.name)
		})
	}
}
