package notifications

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func notificationCapRecipients(count int) []string {
	recipients := make([]string, count)
	for index := range recipients {
		recipients[index] = fmt.Sprintf("did:plc:notificationcap%03d", index)
	}
	return recipients
}

func notificationCapIndexedUsers(recipientDIDs ...string) map[string]bool {
	indexedUsers := map[string]bool{mentionReplyRecipientDID: true}
	for _, did := range recipientDIDs {
		indexedUsers[did] = true
	}
	return indexedUsers
}

func notificationCapReplyIntent(comment CommentRecord) Intent {
	return notificationExpectedIntent(comment, ReasonPostReply, mentionReplyRecipientDID, comment.RootURI)
}

func TestFanoutCommentCreate_MentionsUseOnlyTenSlotsAfterReply(t *testing.T) {
	comment := notificationMentionComment()
	recipients := notificationCapRecipients(12)
	comment.FacetsJSON = notificationMentionFacets(recipients...)
	var existingCalls []string
	lookups := commentFanoutLookups{
		indexedUsers:                   notificationCapIndexedUsers(recipients...),
		activatedAt:                    comment.CreatedAt.Add(-time.Minute),
		indexTime:                      comment.CreatedAt.Add(time.Minute),
		existingMentionRecipientsCalls: &existingCalls,
	}
	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	want := []Intent{notificationCapReplyIntent(comment)}
	for _, did := range recipients[:MaxMentionsPerRecord] {
		want = append(want, notificationExpectedIntent(comment, ReasonMention, did, ""))
	}
	require.Equal(t, len(want), len(intents), "reply plus ten mention slots")
	require.Equal(t, want, intents, "the reply does not consume a slot; only the first ten mentions do")
	require.Equal(t, []string{comment.URI}, existingCalls, "read the record's existing mentions once")
}

func TestFanoutCommentCreate_IneligibleMentionsDoNotConsumeSlots(t *testing.T) {
	comment := notificationMentionComment()
	recipients := notificationCapRecipients(12)
	const (
		blockedByAuthorDID = "did:plc:capblockedbyauthor"
		blockedAuthorDID   = "did:plc:capblockedauthor"
		erasedDID          = "did:plc:caperased"
		communityDID       = "did:plc:capcommunity"
	)
	initial := []string{blockedByAuthorDID, blockedAuthorDID, erasedDID, communityDID, comment.AuthorDID, mentionReplyRecipientDID}
	comment.FacetsJSON = notificationMentionFacets(append(initial, recipients...)...)
	lookups := commentFanoutLookups{
		indexedUsers:      notificationCapIndexedUsers(append(initial, recipients...)...),
		activatedAt:       comment.CreatedAt.Add(-time.Minute),
		indexTime:         comment.CreatedAt.Add(time.Minute),
		erasedAccounts:    map[string]bool{erasedDID: true},
		communityAccounts: map[string]bool{communityDID: true},
		blocks: map[commentFanoutBlock]bool{
			{blockerDID: comment.AuthorDID, blockedDID: blockedByAuthorDID}: true,
			{blockerDID: blockedAuthorDID, blockedDID: comment.AuthorDID}:   true,
		},
	}
	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	want := []Intent{notificationCapReplyIntent(comment)}
	for _, did := range recipients[:MaxMentionsPerRecord] {
		want = append(want, notificationExpectedIntent(comment, ReasonMention, did, ""))
	}
	require.Equal(t, len(want), len(intents), "excluded facets must not consume mention slots")
	require.Equal(t, want, intents, "excluded facets before eligible users cannot spend mention slots")
}

func TestFanoutCommentCreate_PreexistingHolderDoesNotUseRemainingBudget(t *testing.T) {
	comment := notificationMentionComment()
	recipients := notificationCapRecipients(10)
	comment.FacetsJSON = notificationMentionFacets(append([]string{mentionRecipientBDID}, recipients...)...)
	var existingCalls []string
	lookups := commentFanoutLookups{
		indexedUsers: notificationCapIndexedUsers(append([]string{mentionRecipientBDID}, recipients...)...),
		activatedAt:  comment.CreatedAt.Add(-time.Minute), indexTime: comment.CreatedAt.Add(time.Minute),
		existingMentionRecipients: map[string][]string{
			comment.URI: {mentionRecipientBDID, mentionRecipientDDID},
		},
		existingMentionRecipientsCalls: &existingCalls,
	}
	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	want := []Intent{notificationCapReplyIntent(comment)}
	for _, did := range recipients[:MaxMentionsPerRecord-2] {
		want = append(want, notificationExpectedIntent(comment, ReasonMention, did, ""))
	}
	require.Equal(t, len(want), len(intents), "two existing rows leave eight new mention slots")
	require.Equal(t, want, intents, "the already-notified recipient is excluded before filling the eight remaining slots")
	require.Equal(t, []string{comment.URI}, existingCalls)
}

func TestFanoutCommentCreate_FullMentionBudgetPreservesReplyWithoutMentionFactsLookup(t *testing.T) {
	comment := notificationMentionComment()
	comment.FacetsJSON = notificationMentionFacets(mentionRecipientBDID, mentionRecipientDDID)
	var recipientFactsCalls [][]string
	var existingCalls []string
	lookups := commentFanoutLookups{
		indexedUsers:                   notificationCapIndexedUsers(mentionRecipientBDID, mentionRecipientDDID),
		activatedAt:                    comment.CreatedAt.Add(-time.Minute),
		indexTime:                      comment.CreatedAt.Add(time.Minute),
		existingMentionRecipients:      map[string][]string{comment.URI: notificationCapRecipients(MaxMentionsPerRecord)},
		existingMentionRecipientsCalls: &existingCalls,
		recipientFactsCalls:            &recipientFactsCalls,
	}
	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, 1, len(intents), "a full mention budget still allows the reply")
	require.Equal(t, []Intent{notificationCapReplyIntent(comment)}, intents,
		"a full mention budget does not consume or suppress the reply intent")
	require.Equal(t, [][]string{{mentionReplyRecipientDID}}, recipientFactsCalls,
		"only the reply recipient needs a facts lookup at a full mention budget")
	require.Equal(t, []string{comment.URI}, existingCalls)
}

func TestFanoutCommentCreate_NoMentionCandidatesSkipExistingMentionsLookup(t *testing.T) {
	comment := notificationMentionComment()
	comment.FacetsJSON = ""
	var existingCalls []string
	lookups := commentFanoutLookups{
		indexedUsers:                   notificationCapIndexedUsers(),
		activatedAt:                    comment.CreatedAt.Add(-time.Minute),
		indexTime:                      comment.CreatedAt.Add(time.Minute),
		existingMentionRecipientsCalls: &existingCalls,
	}
	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{notificationCapReplyIntent(comment)}, intents)
	require.Empty(t, existingCalls, "a reply without mention candidates must not read the mention budget")
}

func TestFanoutCommentEdit_UsesRemainingRecordMentionBudget(t *testing.T) {
	for _, test := range []struct {
		name          string
		existingCount int
		wantCount     int
	}{
		{"seven_used_three_remain", 7, 3},
		{"nine_used_one_remains", 9, 1},
		{"ten_used_none_remain", 10, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationEditComment()
			added := notificationCapRecipients(5)
			comment.FacetsJSON = notificationMentionFacets(added...)
			lookups := notificationEditLookups(comment)
			lookups.indexedUsers = notificationCapIndexedUsers(added...)
			lookups.existingMentionRecipients = map[string][]string{comment.URI: notificationCapRecipients(20)[10 : 10+test.existingCount]}
			var recipientFactsCalls [][]string
			var existingCalls []string
			lookups.recipientFactsCalls = &recipientFactsCalls
			lookups.existingMentionRecipientsCalls = &existingCalls
			intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
			require.NoError(t, err)
			require.Equal(t, test.wantCount, len(intents), "the edit must respect the remaining mention budget")
			require.Equal(t, notificationEditExpectedMentions(comment, added[:test.wantCount]...), intents,
				"an edit uses only the slots remaining after all earlier edits and re-creates")
			if test.wantCount == 0 {
				require.Empty(t, recipientFactsCalls, "do not read recipient facts when the mention budget is full")
			}
			require.Equal(t, []string{comment.URI}, existingCalls)
		})
	}
}

func TestFanoutCommentEdit_ReaddedExistingHolderDoesNotUseSlot(t *testing.T) {
	comment := notificationEditComment()
	added := notificationCapRecipients(4)
	comment.FacetsJSON = notificationMentionFacets(append([]string{mentionRecipientBDID}, added...)...)
	lookups := notificationEditLookups(comment)
	lookups.indexedUsers = notificationCapIndexedUsers(append([]string{mentionRecipientBDID}, added...)...)
	lookups.existingMentionRecipients = map[string][]string{comment.URI: []string{mentionRecipientBDID}}
	intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
	require.NoError(t, err)
	require.Equal(t, len(added), len(intents), "an already-notified recipient must not receive another intent")
	require.Equal(t, notificationEditExpectedMentions(comment, added...), intents,
		"a previously notified DID removed by an edit cannot be notified again when re-added")
}

func TestFanoutPostCreate_MentionsUseOnlyTenSlots(t *testing.T) {
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	recipients := notificationCapRecipients(12)
	post := notificationPostWithMentions(createdAt, recipients...)
	var existingCalls []string
	lookups := commentFanoutLookups{
		indexedUsers:                   notificationCapIndexedUsers(recipients...),
		activatedAt:                    createdAt.Add(-time.Minute),
		indexTime:                      createdAt.Add(time.Minute),
		existingMentionRecipientsCalls: &existingCalls,
	}
	intents, err := FanoutPostCreate(context.Background(), lookups, nil, post)
	require.NoError(t, err)
	var want []Intent
	for _, did := range recipients[:MaxMentionsPerRecord] {
		want = append(want, notificationPostMentionIntent(post, did))
	}
	require.Equal(t, len(want), len(intents), "a post has at most ten mention slots")
	require.Equal(t, want, intents, "a new post notifies only the first ten eligible mentions")
	require.Equal(t, []string{post.URI}, existingCalls)
}

func TestFanoutPostCreate_FullMentionBudgetSkipsRecipientFacts(t *testing.T) {
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	post := notificationPostWithMentions(createdAt, mentionRecipientBDID, mentionRecipientDDID)
	var recipientFactsCalls [][]string
	var existingCalls []string
	lookups := commentFanoutLookups{
		indexedUsers:                   notificationCapIndexedUsers(mentionRecipientBDID, mentionRecipientDDID),
		activatedAt:                    createdAt.Add(-time.Minute),
		indexTime:                      createdAt.Add(time.Minute),
		existingMentionRecipients:      map[string][]string{post.URI: notificationCapRecipients(MaxMentionsPerRecord)},
		existingMentionRecipientsCalls: &existingCalls,
		recipientFactsCalls:            &recipientFactsCalls,
	}
	intents, err := FanoutPostCreate(context.Background(), lookups, nil, post)
	require.NoError(t, err)
	require.Empty(t, intents, "ten existing mention rows exhaust this post's budget")
	require.Empty(t, recipientFactsCalls, "no recipient facts should be read at a full mention budget")
	require.Equal(t, []string{post.URI}, existingCalls, "a full budget must be established by looking up the existing rows")
}

func TestFanoutPostCreate_ExistingHolderDoesNotUseSlot(t *testing.T) {
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	recipients := notificationCapRecipients(10)
	post := notificationPostWithMentions(createdAt, append([]string{mentionRecipientBDID}, recipients...)...)
	lookups := commentFanoutLookups{
		indexedUsers:              notificationCapIndexedUsers(append([]string{mentionRecipientBDID}, recipients...)...),
		activatedAt:               createdAt.Add(-time.Minute),
		indexTime:                 createdAt.Add(time.Minute),
		existingMentionRecipients: map[string][]string{post.URI: {mentionRecipientBDID, mentionRecipientDDID}},
	}
	intents, err := FanoutPostCreate(context.Background(), lookups, nil, post)
	require.NoError(t, err)
	var want []Intent
	for _, did := range recipients[:MaxMentionsPerRecord-2] {
		want = append(want, notificationPostMentionIntent(post, did))
	}
	require.Equal(t, len(want), len(intents), "two existing rows leave eight new post mention slots")
	require.Equal(t, want, intents, "the existing holder is excluded before filling eight remaining slots")
}
