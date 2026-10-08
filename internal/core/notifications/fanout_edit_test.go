package notifications

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/richtext"

	"github.com/stretchr/testify/require"
)

const editMentionRecipientFDID = "did:plc:editmentionrecipientf"

func notificationEditComment() CommentRecord {
	comment := notificationTimeGateComment(time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC))
	comment.CID = "bafyreieditedcomment"
	return comment
}

func notificationEditLookups(comment CommentRecord) commentFanoutLookups {
	return commentFanoutLookups{
		indexedUsers: map[string]bool{
			mentionReplyRecipientDID: true, mentionRecipientBDID: true,
			mentionRecipientDDID: true, editMentionRecipientFDID: true,
		},
		activatedAt: comment.CreatedAt.Add(-time.Minute),
		indexTime:   comment.CreatedAt.Add(time.Minute),
	}
}

func notificationEditExpectedMentions(comment CommentRecord, dids ...string) []Intent {
	var intents []Intent
	for _, did := range dids {
		intents = append(intents, notificationExpectedIntent(comment, ReasonMention, did, ""))
	}
	return intents
}

func TestFanoutCommentEdit_DiffsStoredAndEditedFacets(t *testing.T) {
	for _, test := range []struct {
		name, stored string
		mentioned    []string
		want         []string
	}{
		{"adds D, not previously mentioned B", notificationMentionFacets(mentionRecipientBDID),
			[]string{mentionRecipientBDID, mentionRecipientDDID}, []string{mentionRecipientDDID}},
		{"preserves new facet order", notificationMentionFacets(mentionRecipientBDID),
			[]string{mentionRecipientDDID, editMentionRecipientFDID, mentionRecipientBDID},
			[]string{mentionRecipientDDID, editMentionRecipientFDID}},
		{"SQL NULL stored facets", "", []string{mentionRecipientDDID}, []string{mentionRecipientDDID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationEditComment()
			comment.FacetsJSON = notificationMentionFacets(test.mentioned...)
			lookups := notificationEditLookups(comment)
			var activationCalls, indexTimeCalls int
			var recipientFactsCalls [][]string
			lookups.activatedAtCalls = &activationCalls
			lookups.indexTimeCalls = &indexTimeCalls
			lookups.recipientFactsCalls = &recipientFactsCalls
			intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, test.stored)
			require.NoError(t, err)
			require.Equal(t, notificationEditExpectedMentions(comment, test.want...), intents,
				"only edit-added DIDs receive complete mention intents in facet order")
			require.Equal(t, 1, activationCalls, "check activation once for an edit with added mentions")
			require.Zero(t, indexTimeCalls, "edits have no createdAt freshness lookup")
			require.Equal(t, [][]string{test.want}, recipientFactsCalls,
				"one recipient-facts lookup containing only added eligible candidates")
		})
	}
	for _, test := range []struct {
		name, stored string
		mentioned    []string
	}{
		{"unchanged facets", notificationMentionFacets(mentionRecipientBDID), []string{mentionRecipientBDID}},
		{"removed B, kept D", notificationMentionFacets(mentionRecipientBDID, mentionRecipientDDID), []string{mentionRecipientDDID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationEditComment()
			comment.FacetsJSON = notificationMentionFacets(test.mentioned...)
			// Even a legacy root must not be looked up if nothing was added.
			comment.RootURI = "at://did:plc:editcommunity/social.coves.community.post/post"
			comment.ParentURI = comment.RootURI
			var legacyCalls, activationCalls, indexTimeCalls int
			var recipientFactsCalls [][]string
			lookups := notificationEditLookups(comment)
			lookups.legacyPostAuthorCalls = &legacyCalls
			lookups.activatedAtCalls = &activationCalls
			lookups.indexTimeCalls = &indexTimeCalls
			lookups.recipientFactsCalls = &recipientFactsCalls
			intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, test.stored)
			require.NoError(t, err)
			require.Empty(t, intents, "keeping or removing a stored mention creates no notification or retraction")
			require.Zero(t, legacyCalls, "no added candidates means no legacy author lookup")
			require.Zero(t, activationCalls)
			require.Zero(t, indexTimeCalls)
			require.Empty(t, recipientFactsCalls)
		})
	}
}

func TestFanoutCommentEdit_SuppressesResolvedReplyRecipient(t *testing.T) {
	for _, test := range []struct {
		name           string
		configure      func(*CommentRecord, *commentFanoutLookups)
		replyRecipient string
	}{
		{"postv2 post author", func(comment *CommentRecord, _ *commentFanoutLookups) {
			comment.RootURI = "at://" + mentionRecipientBDID + "/social.coves.community.postv2/post"
			comment.ParentURI = comment.RootURI
		}, mentionRecipientBDID},
		{"nested comment parent, not root author", func(comment *CommentRecord, _ *commentFanoutLookups) {
			comment.RootURI = "at://" + mentionReplyRecipientDID + "/social.coves.community.postv2/post"
			comment.ParentURI = "at://" + mentionRecipientBDID + "/social.coves.community.comment/parent"
		}, mentionRecipientBDID},
		{"legacy post row author, not URI authority", func(comment *CommentRecord, lookups *commentFanoutLookups) {
			comment.RootURI = "at://did:plc:editcommunity/social.coves.community.post/post"
			comment.ParentURI = comment.RootURI
			lookups.indexedUsers["did:plc:editcommunity"] = true
			lookups.legacyPostAuthors = map[string]string{comment.RootURI: mentionRecipientBDID}
		}, mentionRecipientBDID},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationEditComment()
			lookups := notificationEditLookups(comment)
			test.configure(&comment, &lookups)
			comment.FacetsJSON = notificationMentionFacets(test.replyRecipient, mentionRecipientDDID)
			var recipientFactsCalls [][]string
			lookups.recipientFactsCalls = &recipientFactsCalls
			intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
			require.NoError(t, err)
			require.Equal(t, notificationEditExpectedMentions(comment, mentionRecipientDDID), intents,
				"the resolved reply recipient never receives a second notification, even without a reply row")
			require.Equal(t, [][]string{{mentionRecipientDDID}}, recipientFactsCalls,
				"resolve and remove the reply recipient before checking recipient facts")
		})
	}

	t.Run("only the reply recipient was added", func(t *testing.T) {
		comment := notificationEditComment()
		comment.FacetsJSON = notificationMentionFacets(mentionReplyRecipientDID)
		lookups := notificationEditLookups(comment)
		var activationCalls, indexTimeCalls int
		var recipientFactsCalls [][]string
		lookups.activatedAtCalls, lookups.indexTimeCalls = &activationCalls, &indexTimeCalls
		lookups.recipientFactsCalls = &recipientFactsCalls
		intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
		require.NoError(t, err)
		require.Empty(t, intents)
		require.Zero(t, activationCalls, "remove the reply recipient before activation")
		require.Zero(t, indexTimeCalls)
		require.Empty(t, recipientFactsCalls, "remove the reply recipient before reading facts")
	})
	t.Run("only the actor was added", func(t *testing.T) {
		comment := notificationEditComment()
		comment.RootURI = "at://did:plc:editcommunity/social.coves.community.post/post"
		comment.ParentURI = comment.RootURI
		comment.FacetsJSON = notificationMentionFacets(comment.AuthorDID)
		lookups := notificationEditLookups(comment)
		lookups.indexedUsers[comment.AuthorDID] = true
		var legacyCalls, activationCalls, indexTimeCalls int
		var recipientFactsCalls [][]string
		lookups.legacyPostAuthorCalls = &legacyCalls
		lookups.activatedAtCalls, lookups.indexTimeCalls = &activationCalls, &indexTimeCalls
		lookups.recipientFactsCalls = &recipientFactsCalls
		intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
		require.NoError(t, err)
		require.Empty(t, intents)
		require.Zero(t, legacyCalls, "self-mentions exit before resolving a legacy reply")
		require.Zero(t, activationCalls)
		require.Zero(t, indexTimeCalls)
		require.Empty(t, recipientFactsCalls)
	})
}

func TestFanoutCommentEdit_FiltersIneligibleRecipients(t *testing.T) {
	const ineligibleDID = mentionRecipientMDID
	for _, test := range []struct {
		name        string
		candidate   string
		configure   func(*commentFanoutLookups, string)
		bridgeHosts BridgeHostChecker
	}{
		{"community", ineligibleDID, func(lookups *commentFanoutLookups, _ string) {
			lookups.communityAccounts = map[string]bool{ineligibleDID: true}
		}, nil},
		{"actor", "", func(*commentFanoutLookups, string) {}, nil},
		{"recipient blocks actor", ineligibleDID, func(lookups *commentFanoutLookups, actor string) {
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: ineligibleDID, blockedDID: actor}: true}
		}, nil},
		{"actor blocks recipient", ineligibleDID, func(lookups *commentFanoutLookups, actor string) {
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: actor, blockedDID: ineligibleDID}: true}
		}, nil},
		{"unindexed", ineligibleDID, func(lookups *commentFanoutLookups, _ string) {
			delete(lookups.indexedUsers, ineligibleDID)
		}, nil},
		{"erased", ineligibleDID, func(lookups *commentFanoutLookups, _ string) {
			lookups.erasedAccounts = map[string]bool{ineligibleDID: true}
		}, nil},
		{"aggregator", ineligibleDID, func(lookups *commentFanoutLookups, _ string) {
			lookups.aggregatorAccounts = map[string]bool{ineligibleDID: true}
		}, nil},
		{"trusted bridge host", ineligibleDID, func(lookups *commentFanoutLookups, _ string) {
			lookups.userPDSURLs = map[string]string{ineligibleDID: "https://bridge.test"}
		}, &notificationTestBridgeHosts{trustedURLs: map[string]bool{"https://bridge.test": true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationEditComment()
			candidate := test.candidate
			if candidate == "" {
				candidate = comment.AuthorDID
			}
			comment.FacetsJSON = notificationMentionFacets(candidate, mentionRecipientDDID)
			lookups := notificationEditLookups(comment)
			lookups.indexedUsers[candidate] = true
			test.configure(&lookups, comment.AuthorDID)
			var recipientFactsCalls [][]string
			lookups.recipientFactsCalls = &recipientFactsCalls
			intents, err := FanoutCommentEdit(context.Background(), lookups, test.bridgeHosts, comment, "")
			require.NoError(t, err)
			require.Equal(t, notificationEditExpectedMentions(comment, mentionRecipientDDID), intents,
				"ineligible candidate cannot receive an edit mention; eligible D must")
			if candidate == comment.AuthorDID {
				require.Equal(t, [][]string{{mentionRecipientDDID}}, recipientFactsCalls,
					"actor must be excluded before the one recipient-facts read")
			} else {
				require.Equal(t, [][]string{{candidate, mentionRecipientDDID}}, recipientFactsCalls,
					"check the ineligible candidate alongside the eligible control")
			}
		})
	}
}

func TestFanoutCommentEdit_ActivationWithoutRecordFreshness(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		createdAt   time.Time
		activatedAt time.Time
		wantMention bool
	}{
		{"thirty-day-old record after activation", indexTime.Add(-30 * 24 * time.Hour), indexTime.Add(-40 * 24 * time.Hour), true},
		{"exactly at activation", indexTime.Add(-time.Hour), indexTime.Add(-time.Hour), true},
		{"before activation despite new index time", indexTime.Add(-2 * time.Hour), indexTime.Add(-time.Hour), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationTimeGateComment(test.createdAt)
			comment.CID = "bafyreieditedcomment"
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
			lookups := notificationEditLookups(comment)
			lookups.activatedAt, lookups.indexTime = test.activatedAt, indexTime
			// A stale record must not be rejected by IndexTime (which is not an edit gate).
			lookups.indexTimeError = errors.New("edit must not check record freshness")
			intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
			require.NoError(t, err)
			if !test.wantMention {
				require.Empty(t, intents, "a pre-activation stored record cannot notify from an edit")
				return
			}
			require.Equal(t, notificationEditExpectedMentions(comment, mentionRecipientDDID), intents,
				"an edit is gated by record activation, never the original record's age")
		})
	}
}

func TestFanoutCommentEdit_CapsNewAndStoredMentionsIndependently(t *testing.T) {
	const count = richtext.MaxFacets + 1
	mentioned := make([]string, count)
	indexedUsers := map[string]bool{mentionReplyRecipientDID: true}
	for index := range mentioned {
		mentioned[index] = fmt.Sprintf("did:plc:editcapped%03d", index)
		indexedUsers[mentioned[index]] = true
	}
	t.Run("new mentions stop at the first 200", func(t *testing.T) {
		comment := notificationEditComment()
		comment.FacetsJSON = notificationMentionFacetsPerFacet(richtext.MaxFeaturesPerFacet, mentioned...)
		lookups := notificationEditLookups(comment)
		lookups.indexedUsers = indexedUsers
		var recipientFactsCalls [][]string
		lookups.recipientFactsCalls = &recipientFactsCalls
		intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
		require.NoError(t, err)
		require.Equal(t, MaxMentionsPerRecord, len(intents), "only the first ten distinct edit mentions are sent")
		for index, did := range mentioned[:MaxMentionsPerRecord] {
			require.Equal(t, notificationExpectedIntent(comment, ReasonMention, did, ""), intents[index],
				"edit mention %d must retain facet order and every intent field", index)
		}
		require.Equal(t, [][]string{mentioned[:richtext.MaxFacets]}, recipientFactsCalls,
			"read facts once for only the capped candidates")
	})
	t.Run("stored cap excludes the former 201st DID", func(t *testing.T) {
		comment := notificationEditComment()
		editedOrder := append([]string{mentioned[richtext.MaxFacets]}, mentioned[:richtext.MaxFacets]...)
		comment.FacetsJSON = notificationMentionFacetsPerFacet(richtext.MaxFeaturesPerFacet, editedOrder...)
		lookups := notificationEditLookups(comment)
		lookups.indexedUsers = indexedUsers
		intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment,
			notificationMentionFacetsPerFacet(richtext.MaxFeaturesPerFacet, mentioned...))
		require.NoError(t, err)
		require.Equal(t, notificationEditExpectedMentions(comment, mentioned[richtext.MaxFacets]), intents,
			"X201 is newly added because it was outside the stored 200-DID cap")
	})
	t.Run("new DID past the new cap is not added although the previous record lacked it", func(t *testing.T) {
		comment := notificationEditComment()
		comment.FacetsJSON = notificationMentionFacetsPerFacet(richtext.MaxFeaturesPerFacet, mentioned...)
		lookups := notificationEditLookups(comment)
		lookups.indexedUsers = indexedUsers
		var recipientFactsCalls [][]string
		lookups.recipientFactsCalls = &recipientFactsCalls
		intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment,
			notificationMentionFacetsPerFacet(richtext.MaxFeaturesPerFacet, mentioned[:richtext.MaxFacets]...))
		require.NoError(t, err)
		require.Empty(t, intents, "cap the new facets before the diff, so the 201st new DID is dropped, not added")
		require.Empty(t, recipientFactsCalls, "nothing was added, so no recipient facts are read")
	})
	t.Run("reply recipient counts toward the new cap before it is filtered", func(t *testing.T) {
		comment := notificationEditComment()
		editedOrder := append([]string{mentionReplyRecipientDID}, mentioned[:richtext.MaxFacets]...)
		comment.FacetsJSON = notificationMentionFacetsPerFacet(richtext.MaxFeaturesPerFacet, editedOrder...)
		lookups := notificationEditLookups(comment)
		lookups.indexedUsers = indexedUsers
		var recipientFactsCalls [][]string
		lookups.recipientFactsCalls = &recipientFactsCalls
		intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
		require.NoError(t, err)
		require.Equal(t, MaxMentionsPerRecord, len(intents), "the reply recipient does not use a mention notification slot")
		require.Equal(t, notificationEditExpectedMentions(comment, mentioned[:MaxMentionsPerRecord]...), intents,
			"the reply recipient counts toward the 200-DID parse bound, but not the ten notification slots")
		require.Equal(t, [][]string{mentioned[:richtext.MaxFacets-1]}, recipientFactsCalls,
			"read facts once for the 199 capped mentions left after removing the reply recipient")
	})
}

func TestFanoutCommentEdit_RequiresPostRootAndResolvesLegacyOnlyForDirectReplies(t *testing.T) {
	for _, test := range []struct {
		name             string
		rootURI          string
		parentURI        string
		wantMention      bool
		wantLegacyLookup int
	}{
		{"unparsable root", "at://did:plc:editcommunity/social.coves.community.postv2/post/", "", false, 0},
		{"non-post collection root", "at://did:plc:editcommunity/social.coves.community.comment/root", "", false, 0},
		{"missing legacy root replied to directly", "at://did:plc:editcommunity/social.coves.community.post/missing", "", false, 1},
		{"nested reply under missing legacy root", "at://did:plc:editcommunity/social.coves.community.post/missing",
			"at://did:plc:editparent/social.coves.community.comment/parent", true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationEditComment()
			comment.RootURI, comment.ParentURI = test.rootURI, test.parentURI
			if comment.ParentURI == "" {
				comment.ParentURI = comment.RootURI
			}
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
			lookups := notificationEditLookups(comment)
			lookups.indexedUsers["did:plc:editcommunity"] = true
			lookups.indexedUsers["did:plc:editparent"] = true
			var legacyCalls int
			lookups.legacyPostAuthorCalls = &legacyCalls
			intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
			require.NoError(t, err, "an unsupported root or missing posts row is not a transient failure")
			if test.wantMention {
				require.Equal(t, notificationEditExpectedMentions(comment, mentionRecipientDDID), intents,
					"nested reply under legacy root still notifies newly mentioned D")
			} else {
				require.Empty(t, intents, "no mention may use a non-post root or a missing direct legacy post")
			}
			require.Equal(t, test.wantLegacyLookup, legacyCalls,
				"look up the legacy posts row only for a direct reply")
		})
	}
}

func TestFanoutCommentEdit_WrapsLookupErrors(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*CommentRecord, *commentFanoutLookups, error)
	}{
		{"reference states", func(_ *CommentRecord, lookups *commentFanoutLookups, failure error) {
			lookups.referenceStatesError = failure
		}},
		{"legacy post author", func(comment *CommentRecord, lookups *commentFanoutLookups, failure error) {
			comment.RootURI = "at://did:plc:editcommunity/social.coves.community.post/post"
			comment.ParentURI = comment.RootURI
			lookups.legacyPostAuthorError = failure
		}},
		{"activation", func(_ *CommentRecord, lookups *commentFanoutLookups, failure error) {
			lookups.activatedAtError = failure
		}},
		{"recipient facts", func(_ *CommentRecord, lookups *commentFanoutLookups, failure error) {
			lookups.recipientFactsError = failure
		}},
		{"existing mention recipients", func(_ *CommentRecord, lookups *commentFanoutLookups, failure error) {
			lookups.existingMentionRecipientsError = failure
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := errors.New("injected edit lookup failure")
			comment := notificationEditComment()
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
			lookups := notificationEditLookups(comment)
			test.configure(&comment, &lookups, failure)
			intents, err := FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
			require.ErrorIs(t, err, failure, "an edit with eligible D must propagate the failed lookup")
			if test.name == "reference states" {
				require.ErrorContains(t, err, "look up notification reference states")
			}
			require.ErrorContains(t, err, "look up notification", "wrap edit lookup errors with notification context")
			require.Empty(t, intents, "failed lookups must not return partial intents")
		})
	}
}
