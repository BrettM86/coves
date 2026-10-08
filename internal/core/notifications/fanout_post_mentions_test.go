package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const postMentionAuthorDID = "did:plc:postmentionauthor"

func notificationPostWithMentions(createdAt time.Time, recipientDIDs ...string) PostRecord {
	return PostRecord{
		URI: "at://" + postMentionAuthorDID + "/social.coves.community.postv2/post",
		CID: "bafyreipostmentions", AuthorDID: postMentionAuthorDID,
		CreatedAt: createdAt, FacetsJSON: notificationMentionFacets(recipientDIDs...),
	}
}

func notificationPostMentionIntent(post PostRecord, recipientDID string) Intent {
	return Intent{
		Reason: ReasonMention, RecipientDID: recipientDID, ActorDID: post.AuthorDID,
		RecordURI: post.URI, RecordCID: post.CID, SubjectURI: "",
		RootPostURI: post.URI, RecordCreatedAt: post.CreatedAt,
	}
}

func TestFanoutPostCreate_MentionsEligibleUsersOnceInFacetOrder(t *testing.T) {
	const communityDID = "did:plc:postmentioncommunity"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	post := notificationPostWithMentions(createdAt,
		mentionRecipientBDID, mentionRecipientDDID, communityDID, postMentionAuthorDID, mentionRecipientBDID)
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			mentionRecipientBDID: true, mentionRecipientDDID: true, communityDID: true, postMentionAuthorDID: true,
		},
		communityAccounts: map[string]bool{communityDID: true},
		activatedAt:       createdAt.Add(-time.Minute),
		indexTime:         createdAt.Add(time.Minute),
	}

	intents, err := FanoutPostCreate(context.Background(), lookups, nil, post)
	require.NoError(t, err)
	require.Len(t, intents, 2, "B and D must each receive a post mention")
	require.Equal(t, []Intent{
		notificationPostMentionIntent(post, mentionRecipientBDID),
		notificationPostMentionIntent(post, mentionRecipientDDID),
	}, intents, "B and D each receive one mention in facet order; a community and the author do not")
}

func TestFanoutPostCreate_RejectsUnparsableAndLegacyPostURIsWithoutLookups(t *testing.T) {
	for _, test := range []struct {
		name string
		uri  string
	}{
		{"unparsable_URI", "not-an-at-uri"},
		{"legacy_post", "at://" + postMentionAuthorDID + "/social.coves.community.post/post"},
	} {
		t.Run(test.name, func(t *testing.T) {
			post := notificationPostWithMentions(time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC), mentionRecipientBDID)
			post.URI = test.uri
			var activationCalls, indexTimeCalls, legacyPostAuthorCalls int
			var recipientFactsCalls [][]string
			lookupError := errors.New("unexpected lookup for an unsupported post URI")
			lookups := commentFanoutLookups{
				indexedUsers:          map[string]bool{mentionRecipientBDID: true},
				activatedAtError:      lookupError,
				indexTimeError:        lookupError,
				recipientFactsError:   lookupError,
				legacyPostAuthorError: lookupError,
				activatedAtCalls:      &activationCalls,
				indexTimeCalls:        &indexTimeCalls,
				recipientFactsCalls:   &recipientFactsCalls,
				legacyPostAuthorCalls: &legacyPostAuthorCalls,
			}
			intents, err := FanoutPostCreate(context.Background(), lookups, nil, post)
			require.NoError(t, err)
			require.Nil(t, intents)
			require.Zero(t, activationCalls)
			require.Zero(t, indexTimeCalls)
			require.Zero(t, legacyPostAuthorCalls)
			require.Empty(t, recipientFactsCalls)
		})
	}
}

func TestFanoutPostCreate_MentionRecordTimeGates(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		createdAt   time.Time
		activatedAt time.Time
	}{
		{"before_activation", indexTime.Add(-2 * time.Hour), indexTime.Add(-time.Hour)},
		{"older_than_seven_days", indexTime.Add(-7*24*time.Hour - time.Microsecond), indexTime.Add(-30 * 24 * time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{mentionRecipientBDID: true},
				activatedAt:  test.activatedAt,
				indexTime:    indexTime,
			}
			control := notificationPostWithMentions(indexTime.Add(-time.Minute), mentionRecipientBDID)
			control.URI += "control"
			controlIntents, err := FanoutPostCreate(context.Background(), lookups, nil, control)
			require.NoError(t, err)
			require.Len(t, controlIntents, 1, "the eligible in-window control post must notify B")
			require.Equal(t, []Intent{notificationPostMentionIntent(control, mentionRecipientBDID)}, controlIntents,
				"the eligible in-window control post must still notify B")

			post := notificationPostWithMentions(test.createdAt, mentionRecipientBDID)
			intents, err := FanoutPostCreate(context.Background(), lookups, nil, post)
			require.NoError(t, err)
			require.Nil(t, intents, "a post outside the activation or seven-day freshness window must not notify B")
		})
	}
}

func TestFanoutPostCreate_MentionRecipientEligibility(t *testing.T) {
	const bridgePDSURL = "https://bridge.test"
	for _, test := range []struct {
		name        string
		setup       func(*commentFanoutLookups)
		bridgeHosts BridgeHostChecker
	}{
		{"unindexed", func(lookups *commentFanoutLookups) {
			delete(lookups.indexedUsers, mentionRecipientMDID)
		}, nil},
		{"erased", func(lookups *commentFanoutLookups) {
			lookups.erasedAccounts = map[string]bool{mentionRecipientMDID: true}
		}, nil},
		{"aggregator", func(lookups *commentFanoutLookups) {
			lookups.aggregatorAccounts = map[string]bool{mentionRecipientMDID: true}
		}, nil},
		{"trusted_bridge_PDS", func(lookups *commentFanoutLookups) {
			lookups.userPDSURLs = map[string]string{mentionRecipientMDID: bridgePDSURL}
		}, &notificationTestBridgeHosts{trustedURLs: map[string]bool{bridgePDSURL: true}}},
		{"recipient_blocks_author", func(lookups *commentFanoutLookups) {
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: mentionRecipientMDID, blockedDID: postMentionAuthorDID}: true}
		}, nil},
		{"author_blocks_recipient", func(lookups *commentFanoutLookups) {
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: postMentionAuthorDID, blockedDID: mentionRecipientMDID}: true}
		}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
			post := notificationPostWithMentions(createdAt, mentionRecipientMDID, mentionRecipientDDID)
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{mentionRecipientMDID: true, mentionRecipientDDID: true},
				activatedAt:  createdAt.Add(-time.Minute),
				indexTime:    createdAt.Add(time.Minute),
			}
			test.setup(&lookups)
			intents, err := FanoutPostCreate(context.Background(), lookups, test.bridgeHosts, post)
			require.NoError(t, err)
			require.Len(t, intents, 1, "eligible control D must receive a mention")
			require.Equal(t, []Intent{notificationPostMentionIntent(post, mentionRecipientDDID)}, intents,
				"ineligible M receives no mention while eligible D does")
		})
	}
}

func TestFanoutPostCreate_AllLookupErrorsAreWrapped(t *testing.T) {
	for _, test := range []struct {
		name     string
		setError func(*commentFanoutLookups, error)
	}{
		{"ReferenceStates", func(lookups *commentFanoutLookups, err error) { lookups.referenceStatesError = err }},
		{"ActivatedAt", func(lookups *commentFanoutLookups, err error) { lookups.activatedAtError = err }},
		{"IndexTime", func(lookups *commentFanoutLookups, err error) { lookups.indexTimeError = err }},
		{"RecipientFacts", func(lookups *commentFanoutLookups, err error) { lookups.recipientFactsError = err }},
		{"ExistingMentionRecipients", func(lookups *commentFanoutLookups, err error) {
			lookups.existingMentionRecipientsError = err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New("post mention lookup failed")
			createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
			post := notificationPostWithMentions(createdAt, mentionRecipientBDID)
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{mentionRecipientBDID: true},
				activatedAt:  createdAt.Add(-time.Minute),
				indexTime:    createdAt.Add(time.Minute),
			}
			test.setError(&lookups, sentinel)
			intents, err := FanoutPostCreate(context.Background(), lookups, nil, post)
			require.ErrorIs(t, err, sentinel, "a failing lookup must abort post mention fan-out")
			if test.name == "ReferenceStates" {
				require.ErrorContains(t, err, "look up notification reference states")
			}
			require.NotEqual(t, sentinel, err, "the lookup error must be wrapped with context")
			require.Empty(t, intents, "a failed lookup must not produce partial intents")
		})
	}
}
