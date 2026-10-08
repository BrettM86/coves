package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFanoutCommentEdit_EventTimeFreshnessBoundary(t *testing.T) {
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
		{"no event time on a thirty-day-old comment", time.Time{}, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationEditComment()
			comment.CreatedAt = indexTime.Add(-30 * 24 * time.Hour)
			comment.EditEventTime = test.eventTime
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
			var indexCalls int
			var recipientFactsCalls [][]string
			var existingCalls []string
			lookups := notificationEditLookups(comment)
			lookups.activatedAt = indexTime.Add(-40 * 24 * time.Hour)
			lookups.indexTime = indexTime
			lookups.indexTimeCalls = &indexCalls
			lookups.recipientFactsCalls = &recipientFactsCalls
			lookups.existingMentionRecipientsCalls = &existingCalls
			if test.eventTime.IsZero() {
				lookups.indexTimeError = errors.New("no event time must not require IndexTime")
			}
			intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
			require.NoError(t, err)
			if test.wantMention {
				require.Equal(t, notificationEditExpectedMentions(comment, mentionRecipientDDID), intents,
					"a qualifying edit notifies despite the stored comment being thirty days old")
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

func TestFanoutCommentEdit_EventTimeIndexLookupErrorIsWrapped(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	comment := notificationEditComment()
	comment.CreatedAt = indexTime.Add(-30 * 24 * time.Hour)
	comment.EditEventTime = indexTime.Add(-time.Hour)
	comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
	sentinel := errors.New("edit index time lookup failed")
	var indexCalls int
	var recipientFactsCalls [][]string
	lookups := notificationEditLookups(comment)
	lookups.activatedAt = indexTime.Add(-40 * 24 * time.Hour)
	lookups.indexTime = indexTime
	lookups.indexTimeError = sentinel
	lookups.indexTimeCalls = &indexCalls
	lookups.recipientFactsCalls = &recipientFactsCalls
	intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
	require.ErrorIs(t, err, sentinel)
	require.ErrorContains(t, err, "look up notification")
	require.Empty(t, intents)
	require.Equal(t, 1, indexCalls)
	require.Empty(t, recipientFactsCalls, "failed freshness lookup must not read recipient facts")
}

func TestFanoutCommentEdit_PreActivationSkipsFreshnessAndBudget(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	comment := notificationEditComment()
	comment.CreatedAt = indexTime.Add(-2 * time.Hour)
	// A fresh event time means a freshness check would read IndexTime.
	comment.EditEventTime = indexTime
	comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
	var activationCalls, indexCalls int
	var recipientFactsCalls [][]string
	var existingCalls []string
	lookups := notificationEditLookups(comment)
	lookups.activatedAt = indexTime.Add(-time.Hour)
	lookups.indexTime = indexTime
	lookups.activatedAtCalls = &activationCalls
	lookups.indexTimeCalls = &indexCalls
	lookups.recipientFactsCalls = &recipientFactsCalls
	lookups.existingMentionRecipientsCalls = &existingCalls
	intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
	require.NoError(t, err)
	require.Nil(t, intents, "the stored createdAt predates activation despite a fresh edit event")
	require.Equal(t, 1, activationCalls)
	require.Zero(t, indexCalls, "activation must be checked before edit freshness")
	require.Empty(t, existingCalls, "a pre-activation edit must stop before reading the mention budget")
	require.Empty(t, recipientFactsCalls)
}
