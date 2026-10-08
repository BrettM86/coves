package notifications

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFanoutPostEdit_OnlyNewNonAuthorMentions(t *testing.T) {
	post := notificationPostWithMentions(time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
		mentionRecipientBDID, mentionRecipientDDID, postMentionAuthorDID)
	post.CID = "bafyreipostedited"
	var activationCalls, indexTimeCalls int
	var recipientFactsCalls [][]string
	var existingCalls []string
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			mentionRecipientBDID: true, mentionRecipientDDID: true, postMentionAuthorDID: true,
		},
		activatedAt: post.CreatedAt.Add(-time.Minute), indexTime: post.CreatedAt.Add(time.Minute),
		activatedAtCalls: &activationCalls, indexTimeCalls: &indexTimeCalls,
		recipientFactsCalls: &recipientFactsCalls, existingMentionRecipientsCalls: &existingCalls,
	}
	intents, err := FanoutPostEdit(context.Background(), lookups, nil, post,
		notificationMentionFacets(mentionRecipientBDID))
	require.NoError(t, err)
	require.Equal(t, []Intent{notificationPostMentionIntent(post, mentionRecipientDDID)}, intents,
		"only edit-added E receives a mention; stored B and the author do not")
	require.Equal(t, 1, activationCalls)
	require.Zero(t, indexTimeCalls, "an edit with no event time must not gate on the record's age")
	require.Equal(t, []string{post.URI}, existingCalls)
	require.Equal(t, [][]string{{mentionRecipientDDID}}, recipientFactsCalls)
}

func TestFanoutPostEdit_UnsupportedURIHasNoLookups(t *testing.T) {
	for _, test := range []struct{ name, uri string }{
		{"legacy post", "at://" + postMentionAuthorDID + "/social.coves.community.post/post"},
		{"unparsable URI", "not-an-at-uri"},
	} {
		t.Run(test.name, func(t *testing.T) {
			post := notificationPostWithMentions(time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC), mentionRecipientDDID)
			post.URI = test.uri
			var activationCalls, indexTimeCalls, legacyCalls, aggregatorCalls int
			var recipientFactsCalls [][]string
			var existingCalls []string
			var earlierCalls []earlierUpvoteCall
			lookups := commentFanoutLookups{
				activatedAtCalls: &activationCalls, indexTimeCalls: &indexTimeCalls,
				legacyPostAuthorCalls: &legacyCalls, isAggregatorCalls: &aggregatorCalls,
				recipientFactsCalls:            &recipientFactsCalls,
				existingMentionRecipientsCalls: &existingCalls, earlierUpvoteCalls: &earlierCalls,
			}
			intents, err := FanoutPostEdit(context.Background(), lookups, nil, post, "")
			require.NoError(t, err)
			require.Nil(t, intents)
			require.Zero(t, activationCalls)
			require.Zero(t, indexTimeCalls)
			require.Zero(t, legacyCalls)
			require.Zero(t, aggregatorCalls)
			require.Empty(t, recipientFactsCalls)
			require.Empty(t, existingCalls)
			require.Empty(t, earlierCalls)
		})
	}
}

func TestFanoutPostEdit_NoAddedMentionsMakesNoLookups(t *testing.T) {
	for _, test := range []struct {
		name, previous string
		current        []string
	}{
		{"unchanged", notificationMentionFacets(mentionRecipientBDID), []string{mentionRecipientBDID}},
		{"only removals", notificationMentionFacets(mentionRecipientBDID, mentionRecipientDDID), []string{mentionRecipientBDID}},
		{"only author added", notificationMentionFacets(mentionRecipientBDID), []string{mentionRecipientBDID, postMentionAuthorDID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			post := notificationPostWithMentions(time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC), test.current...)
			var activationCalls, indexTimeCalls, legacyCalls, aggregatorCalls int
			var recipientFactsCalls [][]string
			var existingCalls []string
			var earlierCalls []earlierUpvoteCall
			lookups := commentFanoutLookups{
				activatedAtCalls: &activationCalls, indexTimeCalls: &indexTimeCalls,
				legacyPostAuthorCalls: &legacyCalls, isAggregatorCalls: &aggregatorCalls,
				recipientFactsCalls:            &recipientFactsCalls,
				existingMentionRecipientsCalls: &existingCalls, earlierUpvoteCalls: &earlierCalls,
			}
			intents, err := FanoutPostEdit(context.Background(), lookups, nil, post, test.previous)
			require.NoError(t, err)
			require.Nil(t, intents, "keeping or removing mentions must not create or retract rows")
			require.Zero(t, activationCalls)
			require.Zero(t, indexTimeCalls)
			require.Zero(t, legacyCalls)
			require.Zero(t, aggregatorCalls)
			require.Empty(t, recipientFactsCalls)
			require.Empty(t, existingCalls)
			require.Empty(t, earlierCalls)
		})
	}
}

func TestFanoutPostEdit_PreActivationStoredPostDoesNotNotify(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{mentionRecipientDDID: true},
		activatedAt:  indexTime.Add(-time.Hour), indexTime: indexTime,
	}
	control := notificationPostWithMentions(indexTime.Add(-time.Minute), mentionRecipientDDID)
	control.CID = "bafyreiposteditcontrol"
	controlIntents, err := FanoutPostEdit(context.Background(), lookups, nil, control, "")
	require.NoError(t, err)
	require.Equal(t, []Intent{notificationPostMentionIntent(control, mentionRecipientDDID)}, controlIntents,
		"fixture: a post created after activation must notify on edit")

	post := notificationPostWithMentions(indexTime.Add(-2*time.Hour), mentionRecipientDDID)
	post.CID = "bafyreiposteditpreactivation"
	// A fresh event time means a freshness check would read IndexTime.
	post.EditEventTime = indexTime
	var activationCalls, indexTimeCalls int
	var recipientFactsCalls [][]string
	var existingCalls []string
	lookups.activatedAtCalls, lookups.indexTimeCalls = &activationCalls, &indexTimeCalls
	lookups.recipientFactsCalls = &recipientFactsCalls
	lookups.existingMentionRecipientsCalls = &existingCalls
	intents, err := FanoutPostEdit(context.Background(), lookups, nil, post, "")
	require.NoError(t, err)
	require.Nil(t, intents, "the stored createdAt predates activation despite the edit happening now")
	require.Equal(t, 1, activationCalls)
	require.Zero(t, indexTimeCalls, "activation must be checked before edit freshness")
	require.Empty(t, existingCalls, "a pre-activation edit must stop before reading the mention budget")
	require.Empty(t, recipientFactsCalls)
}

func TestFanoutPostEdit_ExistingEightMentionsLeaveTwoSlots(t *testing.T) {
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	post := notificationPostWithMentions(createdAt)
	post.CID = "bafyreiposteditcapped"
	existing := make([]string, MaxMentionsPerRecord-2)
	for index := range existing {
		existing[index] = fmt.Sprintf("did:plc:posteditexisting%02d", index)
	}
	added := []string{
		"did:plc:posteditadded01", "did:plc:posteditadded02",
		"did:plc:posteditadded03", "did:plc:posteditadded04",
	}
	post.FacetsJSON = notificationMentionFacets(added...)
	indexed := make(map[string]bool, len(added))
	for _, did := range added {
		indexed[did] = true
	}
	var existingCalls []string
	lookups := commentFanoutLookups{
		indexedUsers: indexed, activatedAt: createdAt.Add(-time.Minute), indexTime: createdAt.Add(time.Minute),
		existingMentionRecipients:      map[string][]string{post.URI: existing},
		existingMentionRecipientsCalls: &existingCalls,
	}
	intents, err := FanoutPostEdit(context.Background(), lookups, nil, post, "")
	require.NoError(t, err)
	require.Equal(t, []Intent{
		notificationPostMentionIntent(post, added[0]),
		notificationPostMentionIntent(post, added[1]),
	}, intents, "eight existing mention rows leave exactly two slots, assigned in facet order")
	require.Equal(t, []string{post.URI}, existingCalls)
}

func TestFanoutPostEdit_AllLookupErrorsAreWrapped(t *testing.T) {
	for _, test := range []struct {
		name     string
		setError func(*commentFanoutLookups, error)
	}{
		{"ReferenceStates", func(lookups *commentFanoutLookups, err error) { lookups.referenceStatesError = err }},
		{"ActivatedAt", func(lookups *commentFanoutLookups, err error) { lookups.activatedAtError = err }},
		{"RecipientFacts", func(lookups *commentFanoutLookups, err error) { lookups.recipientFactsError = err }},
		{"ExistingMentionRecipients", func(lookups *commentFanoutLookups, err error) {
			lookups.existingMentionRecipientsError = err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New("post edit lookup failed")
			post := notificationPostWithMentions(time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC), mentionRecipientDDID)
			post.CID = "bafyreipostediterror"
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{mentionRecipientDDID: true},
				activatedAt:  post.CreatedAt.Add(-time.Minute), indexTime: post.CreatedAt.Add(time.Minute),
			}
			test.setError(&lookups, sentinel)
			intents, err := FanoutPostEdit(context.Background(), lookups, nil, post, "")
			require.ErrorIs(t, err, sentinel, "a failing lookup must abort post edit fan-out")
			if test.name == "ReferenceStates" {
				require.ErrorContains(t, err, "look up notification reference states")
			}
			require.ErrorContains(t, err, "look up notification", "wrap the lookup error with notification context")
			require.Empty(t, intents, "a failed lookup must not return partial intents")
		})
	}
}

func TestFanoutPostEdit_EventTimeFreshnessBoundary(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name          string
		eventTime     time.Time
		wantMention   bool
		wantIndexCall int
	}{
		{"older than seven days", indexTime.Add(-7*24*time.Hour - time.Second), false, 1},
		{"exactly seven days", indexTime.Add(-7 * 24 * time.Hour), true, 1},
		{"inside seven days", indexTime.Add(-7*24*time.Hour + time.Second), true, 1},
		{"no event time on a thirty-day-old post", time.Time{}, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			post := notificationPostWithMentions(indexTime.Add(-30*24*time.Hour), mentionRecipientDDID)
			post.CID = "bafyreipostediteventtime"
			post.EditEventTime = test.eventTime
			var indexCalls int
			var recipientFactsCalls [][]string
			var existingCalls []string
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{mentionRecipientDDID: true},
				activatedAt:  indexTime.Add(-40 * 24 * time.Hour), indexTime: indexTime,
				indexTimeCalls: &indexCalls, recipientFactsCalls: &recipientFactsCalls,
				existingMentionRecipientsCalls: &existingCalls,
			}
			if test.eventTime.IsZero() {
				lookups.indexTimeError = errors.New("no event time must not require IndexTime")
			}
			intents, err := FanoutPostEdit(context.Background(), lookups, nil, post, "")
			require.NoError(t, err)
			if test.wantMention {
				require.Equal(t, []Intent{notificationPostMentionIntent(post, mentionRecipientDDID)}, intents,
					"a qualifying edit notifies despite the stored post being thirty days old")
				require.Equal(t, [][]string{{mentionRecipientDDID}}, recipientFactsCalls)
			} else {
				require.Nil(t, intents, "an edit event older than seven days must not notify")
				require.Empty(t, recipientFactsCalls, "stale edits must stop before reading recipient facts")
				require.Empty(t, existingCalls, "stale edits must stop before reading the mention budget")
			}
			require.Equal(t, test.wantIndexCall, indexCalls)
		})
	}
}

func TestFanoutPostEdit_EventTimeIndexLookupErrorIsWrapped(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	post := notificationPostWithMentions(indexTime.Add(-30*24*time.Hour), mentionRecipientDDID)
	post.EditEventTime = indexTime.Add(-time.Hour)
	sentinel := errors.New("edit index time lookup failed")
	var indexCalls int
	var recipientFactsCalls [][]string
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{mentionRecipientDDID: true},
		activatedAt:  indexTime.Add(-40 * 24 * time.Hour), indexTime: indexTime,
		indexTimeError: sentinel, indexTimeCalls: &indexCalls, recipientFactsCalls: &recipientFactsCalls,
	}
	intents, err := FanoutPostEdit(context.Background(), lookups, nil, post, "")
	require.ErrorIs(t, err, sentinel)
	require.ErrorContains(t, err, "look up notification")
	require.Empty(t, intents)
	require.Equal(t, 1, indexCalls)
	require.Empty(t, recipientFactsCalls, "failed freshness lookup must not read recipient facts")
}
