package notifications

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/richtext"
	"github.com/stretchr/testify/require"
)

const (
	mentionReplyRecipientDID = "did:plc:timegaterecipient"
	mentionRecipientBDID     = "did:plc:mentionrecipientb"
	mentionRecipientDDID     = "did:plc:mentionrecipientd"
	mentionRecipientMDID     = "did:plc:mentionrecipientm"
)

func notificationMentionComment() CommentRecord {
	comment := notificationTimeGateComment(time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC))
	comment.FacetsJSON = `[
		{"index":{"byteStart":0,"byteEnd":5},"features":[{"$type":"social.coves.richtext.facet#mention","did":"did:plc:mentionrecipientb"}]},
		{"index":{"byteStart":6,"byteEnd":11},"features":[{"$type":"social.coves.richtext.facet#mention","did":"did:plc:mentionrecipientb"}]},
		{"index":{"byteStart":12,"byteEnd":17},"features":[{"$type":"social.coves.richtext.facet#mention","did":"did:plc:mentionrecipientd"}]}
	]`
	return comment
}

func notificationMentionFacets(recipientDIDs ...string) string {
	facets := make([]string, 0, len(recipientDIDs))
	for index, did := range recipientDIDs {
		facets = append(facets, fmt.Sprintf(`{"index":{"byteStart":%d,"byteEnd":%d},"features":[{"$type":"social.coves.richtext.facet#mention","did":%q}]}`,
			index*6, index*6+5, did))
	}
	return "[" + strings.Join(facets, ",") + "]"
}

// notificationMentionFacetsPerFacet packs featuresPerFacet mention features
// into each facet, all over the same one-byte range: the shape that multiplies
// the facet cap by the per-facet feature cap.
func notificationMentionFacetsPerFacet(featuresPerFacet int, recipientDIDs ...string) string {
	var facets []string
	for start := 0; start < len(recipientDIDs); start += featuresPerFacet {
		end := min(start+featuresPerFacet, len(recipientDIDs))
		features := make([]string, 0, end-start)
		for _, did := range recipientDIDs[start:end] {
			features = append(features, fmt.Sprintf(`{"$type":"social.coves.richtext.facet#mention","did":%q}`, did))
		}
		facets = append(facets, `{"index":{"byteStart":0,"byteEnd":1},"features":[`+strings.Join(features, ",")+`]}`)
	}
	return "[" + strings.Join(facets, ",") + "]"
}

func notificationExpectedIntent(comment CommentRecord, reason Reason, recipientDID, subjectURI string) Intent {
	return Intent{
		Reason: reason, RecipientDID: recipientDID, ActorDID: comment.AuthorDID,
		RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: subjectURI,
		RootPostURI: comment.RootURI, RecordCreatedAt: comment.CreatedAt,
	}
}

func TestFanoutCommentCreate_MentionIntentsFollowReplyInFacetOrder(t *testing.T) {
	comment := notificationMentionComment()
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			mentionReplyRecipientDID: true, mentionRecipientBDID: true, mentionRecipientDDID: true,
		},
		activatedAt: comment.CreatedAt.Add(-time.Minute),
		indexTime:   comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{
		{
			Reason: ReasonPostReply, RecipientDID: mentionReplyRecipientDID, ActorDID: comment.AuthorDID,
			RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: comment.RootURI,
			RootPostURI: comment.RootURI, RecordCreatedAt: comment.CreatedAt,
		},
		{
			Reason: ReasonMention, RecipientDID: mentionRecipientBDID, ActorDID: comment.AuthorDID,
			RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: "",
			RootPostURI: comment.RootURI, RecordCreatedAt: comment.CreatedAt,
		},
		{
			Reason: ReasonMention, RecipientDID: mentionRecipientDDID, ActorDID: comment.AuthorDID,
			RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: "",
			RootPostURI: comment.RootURI, RecordCreatedAt: comment.CreatedAt,
		},
	}, intents, "duplicate mention facets notify B only once, and D follows B")
}

func TestFanoutCommentCreate_MentionCandidatesShareRecordGateAndRecipientLookup(t *testing.T) {
	comment := notificationMentionComment()
	var activationCalls, indexTimeCalls int
	var recipientFactsCalls [][]string
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			mentionReplyRecipientDID: true, mentionRecipientBDID: true, mentionRecipientDDID: true,
		},
		activatedAt:         comment.CreatedAt.Add(-time.Minute),
		indexTime:           comment.CreatedAt.Add(time.Minute),
		activatedAtCalls:    &activationCalls,
		indexTimeCalls:      &indexTimeCalls,
		recipientFactsCalls: &recipientFactsCalls,
	}

	_, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, 1, activationCalls, "check activation once for the whole comment")
	require.Equal(t, 1, indexTimeCalls, "check freshness once for the whole comment")
	require.Equal(t, [][]string{{mentionReplyRecipientDID, mentionRecipientBDID, mentionRecipientDDID}}, recipientFactsCalls,
		"read reply and distinct mention recipient facts together, in facet order")
}

func TestFanoutCommentCreate_MentionMalformedFacetsDoNotSuppressValidMention(t *testing.T) {
	comment := notificationMentionComment()
	comment.FacetsJSON = `[
		42,
		{"index":{"byteStart":0,"byteEnd":5},"features":[{"$type":"social.coves.richtext.facet#mention","did":17}]},
		{"index":{"byteStart":6,"byteEnd":11},"features":[{"$type":"social.coves.richtext.facet#mention","did":"not-a-did"}]},
		{"index":{"byteStart":12,"byteEnd":17},"features":[{"$type":"social.coves.richtext.facet#mention","did":"did:plc:mentionrecipientd"}]}
	]`
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			mentionReplyRecipientDID: true, mentionRecipientDDID: true, "not-a-did": true,
		},
		activatedAt: comment.CreatedAt.Add(-time.Minute),
		indexTime:   comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{
		{
			Reason: ReasonPostReply, RecipientDID: mentionReplyRecipientDID, ActorDID: comment.AuthorDID,
			RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: comment.RootURI,
			RootPostURI: comment.RootURI, RecordCreatedAt: comment.CreatedAt,
		},
		{
			Reason: ReasonMention, RecipientDID: mentionRecipientDDID, ActorDID: comment.AuthorDID,
			RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: "",
			RootPostURI: comment.RootURI, RecordCreatedAt: comment.CreatedAt,
		},
	}, intents, "malformed entries cannot hide or add a mention")
}

func TestFanoutCommentCreate_MentionRequiresParseableRootURI(t *testing.T) {
	comment := notificationMentionComment()
	comment.RootURI = "not-an-at-uri"
	comment.ParentURI = comment.RootURI
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			mentionReplyRecipientDID: true, mentionRecipientBDID: true, mentionRecipientDDID: true,
		},
		activatedAt: comment.CreatedAt.Add(-time.Minute),
		indexTime:   comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Empty(t, intents, "unparseable root cannot be stored as a mention's root post URI")
}

// Each root below parses but does not name a post, so it cannot be stored as a
// mention's root_post_uri.
func TestFanoutCommentCreate_MentionRequiresPostRoot(t *testing.T) {
	const nonPostRootAuthorDID = "did:plc:mentionnonpostroot"
	for _, test := range []struct {
		name      string
		rootURI   string
		parentURI string
	}{
		{
			name:      "bluesky_post_root_replied_to_directly",
			rootURI:   "at://" + nonPostRootAuthorDID + "/app.bsky.feed.post/root",
			parentURI: "at://" + nonPostRootAuthorDID + "/app.bsky.feed.post/root",
		},
		{
			name:      "comment_root_under_comment_parent",
			rootURI:   "at://" + nonPostRootAuthorDID + "/social.coves.community.comment/root",
			parentURI: "at://" + mentionReplyRecipientDID + "/social.coves.community.comment/parent",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationMentionComment()
			comment.RootURI = test.rootURI
			comment.ParentURI = test.parentURI
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{
					nonPostRootAuthorDID: true, mentionReplyRecipientDID: true, mentionRecipientDDID: true,
				},
				activatedAt: comment.CreatedAt.Add(-time.Minute),
				indexTime:   comment.CreatedAt.Add(time.Minute),
			}

			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.NoError(t, err)
			require.Empty(t, intents, "a root that is not a post cannot be stored as indexed D's mention root")
		})
	}
}

// PRD "Post author resolution": when a legacy root's posts row is missing, the
// event produces no notification, so D's mention is dropped along with the reply.
func TestFanoutCommentCreate_MissingLegacyPostAuthorSuppressesMentions(t *testing.T) {
	const legacyAuthorityDID = "did:plc:legacycommunity"
	comment := notificationMentionComment()
	comment.RootURI = "at://" + legacyAuthorityDID + "/social.coves.community.post/missing"
	comment.ParentURI = comment.RootURI
	comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{legacyAuthorityDID: true, mentionRecipientDDID: true},
		activatedAt:  comment.CreatedAt.Add(-time.Minute),
		indexTime:    comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Empty(t, intents, "a legacy root without a posts row produces no notification, not even indexed D's mention")
}

// One record may carry richtext.MaxFacets facets of richtext.MaxFeaturesPerFacet
// mentions each. Fan-out keeps only the first MaxFacets distinct mentioned DIDs,
// counted before self and reply-recipient suppression.
func TestFanoutCommentCreate_MentionsCappedAtMaxFacetsDistinctDIDs(t *testing.T) {
	const mentionCount = 15 * richtext.MaxFeaturesPerFacet
	require.Greater(t, mentionCount, richtext.MaxFacets, "fixture: more distinct mentions than the cap")
	cappedMentions := make([]string, 0, mentionCount)
	for index := 0; index < mentionCount; index++ {
		cappedMentions = append(cappedMentions, fmt.Sprintf("did:plc:cappedmention%03d", index))
	}
	for _, test := range []struct {
		name         string
		mentioned    func(comment CommentRecord) []string
		wantMentions []string
	}{
		{
			name:         "first_distinct_dids_in_facet_order",
			mentioned:    func(CommentRecord) []string { return cappedMentions },
			wantMentions: cappedMentions[:richtext.MaxFacets],
		},
		{
			name: "self_and_reply_recipient_count_toward_the_cap",
			mentioned: func(comment CommentRecord) []string {
				return append([]string{comment.AuthorDID, mentionReplyRecipientDID}, cappedMentions[:mentionCount-2]...)
			},
			wantMentions: cappedMentions[:richtext.MaxFacets-2],
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationMentionComment()
			comment.FacetsJSON = notificationMentionFacetsPerFacet(richtext.MaxFeaturesPerFacet, test.mentioned(comment)...)
			indexedUsers := map[string]bool{comment.AuthorDID: true, mentionReplyRecipientDID: true}
			for _, did := range cappedMentions {
				indexedUsers[did] = true
			}
			var recipientFactsCalls [][]string
			lookups := commentFanoutLookups{
				indexedUsers:        indexedUsers,
				activatedAt:         comment.CreatedAt.Add(-time.Minute),
				indexTime:           comment.CreatedAt.Add(time.Minute),
				recipientFactsCalls: &recipientFactsCalls,
			}

			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.NoError(t, err)
			require.Equal(t, 1+MaxMentionsPerRecord, len(intents), "one reply plus ten mention notifications")
			want := []Intent{notificationExpectedIntent(comment, ReasonPostReply, mentionReplyRecipientDID, comment.RootURI)}
			for _, did := range test.wantMentions[:MaxMentionsPerRecord] {
				want = append(want, notificationExpectedIntent(comment, ReasonMention, did, ""))
			}
			require.Equal(t, want, intents, "only the first ten eligible mentions inside the 200-DID parse bound notify")
			require.Equal(t, [][]string{append([]string{mentionReplyRecipientDID}, test.wantMentions...)}, recipientFactsCalls,
				"one recipient-facts read with the reply recipient and only the capped mentions")
		})
	}
}

func TestFanoutCommentCreate_MentionOfPostReplyRecipientIsSuppressed(t *testing.T) {
	comment := notificationMentionComment()
	comment.RootURI = "at://" + mentionRecipientBDID + "/social.coves.community.postv2/post"
	comment.ParentURI = comment.RootURI
	comment.FacetsJSON = notificationMentionFacets(mentionRecipientBDID, mentionRecipientDDID)
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{mentionRecipientBDID: true, mentionRecipientDDID: true},
		activatedAt:  comment.CreatedAt.Add(-time.Minute),
		indexTime:    comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{
		notificationExpectedIntent(comment, ReasonPostReply, mentionRecipientBDID, comment.RootURI),
		notificationExpectedIntent(comment, ReasonMention, mentionRecipientDDID, ""),
	}, intents, "post author B receives only the reply; eligible D receives the mention")
}

// The legacy root's authority is a community, not the author. Suppression keys
// on the author resolved from the posts row, so B gets only the reply.
func TestFanoutCommentCreate_MentionOfLegacyPostReplyRecipientIsSuppressed(t *testing.T) {
	const legacyAuthorityDID = "did:plc:legacycommunity"
	comment := notificationMentionComment()
	comment.RootURI = "at://" + legacyAuthorityDID + "/social.coves.community.post/post"
	comment.ParentURI = comment.RootURI
	comment.FacetsJSON = notificationMentionFacets(mentionRecipientBDID, mentionRecipientDDID)
	lookups := commentFanoutLookups{
		indexedUsers:      map[string]bool{legacyAuthorityDID: true, mentionRecipientBDID: true, mentionRecipientDDID: true},
		legacyPostAuthors: map[string]string{comment.RootURI: mentionRecipientBDID},
		activatedAt:       comment.CreatedAt.Add(-time.Minute),
		indexTime:         comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{
		notificationExpectedIntent(comment, ReasonPostReply, mentionRecipientBDID, comment.RootURI),
		notificationExpectedIntent(comment, ReasonMention, mentionRecipientDDID, ""),
	}, intents, "legacy post author B receives only the reply; eligible D receives the mention")
}

func TestFanoutCommentCreate_MentionOfCommentReplyRecipientIsSuppressedButRootAuthorIsNot(t *testing.T) {
	comment := notificationMentionComment()
	comment.RootURI = "at://" + mentionRecipientBDID + "/social.coves.community.postv2/post"
	comment.ParentURI = "at://" + mentionReplyRecipientDID + "/social.coves.community.comment/parent"
	comment.FacetsJSON = notificationMentionFacets(mentionReplyRecipientDID, mentionRecipientBDID)
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{mentionReplyRecipientDID: true, mentionRecipientBDID: true},
		activatedAt:  comment.CreatedAt.Add(-time.Minute),
		indexTime:    comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{
		notificationExpectedIntent(comment, ReasonCommentReply, mentionReplyRecipientDID, comment.ParentURI),
		notificationExpectedIntent(comment, ReasonMention, mentionRecipientBDID, ""),
	}, intents, "parent author C receives only the reply; root post author B is a distinct mention")
}

func TestFanoutCommentCreate_MentionOfSelfIsSuppressed(t *testing.T) {
	comment := notificationMentionComment()
	comment.FacetsJSON = notificationMentionFacets(comment.AuthorDID, mentionRecipientDDID)
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			comment.AuthorDID: true, mentionReplyRecipientDID: true, mentionRecipientDDID: true,
		},
		activatedAt: comment.CreatedAt.Add(-time.Minute),
		indexTime:   comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{
		notificationExpectedIntent(comment, ReasonPostReply, mentionReplyRecipientDID, comment.RootURI),
		notificationExpectedIntent(comment, ReasonMention, mentionRecipientDDID, ""),
	}, intents, "indexed, eligible author A cannot notify itself, but D still gets a mention")
}

func TestFanoutCommentCreate_MentionOfCommunityIsSuppressed(t *testing.T) {
	const communityDID = "did:plc:mentionedcommunity"
	comment := notificationMentionComment()
	comment.FacetsJSON = notificationMentionFacets(communityDID, mentionRecipientDDID)
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			mentionReplyRecipientDID: true, communityDID: true, mentionRecipientDDID: true,
		},
		communityAccounts: map[string]bool{communityDID: true},
		activatedAt:       comment.CreatedAt.Add(-time.Minute),
		indexTime:         comment.CreatedAt.Add(time.Minute),
	}

	intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{
		notificationExpectedIntent(comment, ReasonPostReply, mentionReplyRecipientDID, comment.RootURI),
		notificationExpectedIntent(comment, ReasonMention, mentionRecipientDDID, ""),
	}, intents, "an indexed community K does not receive a mention, while D does")
}

func TestFanoutCommentCreate_MentionSurvivesMissingOrSuppressedReply(t *testing.T) {
	const replyRecipientDID = "did:plc:missingreplyrecipient"
	const unsupportedParentDID = "did:plc:unsupportedparent"
	for _, test := range []struct {
		name      string
		parentURI string
		rootURI   string
		indexed   map[string]bool
		blocks    map[commentFanoutBlock]bool
	}{
		{
			name: "reply_to_own_post", parentURI: "at://did:plc:timegatecommenter/social.coves.community.postv2/own",
			rootURI: "at://did:plc:timegatecommenter/social.coves.community.postv2/own",
			indexed: map[string]bool{"did:plc:timegatecommenter": true, mentionRecipientDDID: true},
		},
		{
			name: "reply_recipient_not_indexed", parentURI: "at://" + replyRecipientDID + "/social.coves.community.postv2/post",
			rootURI: "at://" + replyRecipientDID + "/social.coves.community.postv2/post",
			indexed: map[string]bool{mentionRecipientDDID: true},
		},
		{
			name: "reply_recipient_blocks_actor", parentURI: "at://" + replyRecipientDID + "/social.coves.community.postv2/post",
			rootURI: "at://" + replyRecipientDID + "/social.coves.community.postv2/post",
			indexed: map[string]bool{replyRecipientDID: true, mentionRecipientDDID: true},
			blocks:  map[commentFanoutBlock]bool{{blockerDID: replyRecipientDID, blockedDID: "did:plc:timegatecommenter"}: true},
		},
		{
			name: "unsupported_parent_collection", parentURI: "at://" + unsupportedParentDID + "/app.bsky.feed.post/parent",
			rootURI: "at://" + mentionReplyRecipientDID + "/social.coves.community.postv2/post",
			indexed: map[string]bool{unsupportedParentDID: true, mentionReplyRecipientDID: true, mentionRecipientDDID: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationMentionComment()
			comment.ParentURI = test.parentURI
			comment.RootURI = test.rootURI
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
			lookups := commentFanoutLookups{
				indexedUsers: test.indexed,
				blocks:       test.blocks,
				activatedAt:  comment.CreatedAt.Add(-time.Minute),
				indexTime:    comment.CreatedAt.Add(time.Minute),
			}

			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.NoError(t, err)
			require.Equal(t, []Intent{
				notificationExpectedIntent(comment, ReasonMention, mentionRecipientDDID, ""),
			}, intents, "a missing or suppressed reply must not stop D's mention")
		})
	}
}

func TestFanoutCommentCreate_MentionRecipientEligibility(t *testing.T) {
	for _, test := range []struct {
		name        string
		setup       func(*commentFanoutLookups, string)
		bridgeHosts BridgeHostChecker
	}{
		{"missing_users_row", func(lookups *commentFanoutLookups, _ string) {
			delete(lookups.indexedUsers, mentionRecipientMDID)
		}, nil},
		{"erased", func(lookups *commentFanoutLookups, _ string) {
			lookups.erasedAccounts = map[string]bool{mentionRecipientMDID: true}
		}, nil},
		{"aggregator", func(lookups *commentFanoutLookups, _ string) {
			lookups.aggregatorAccounts = map[string]bool{mentionRecipientMDID: true}
		}, nil},
		{"trusted_bridge_host", func(lookups *commentFanoutLookups, _ string) {
			lookups.userPDSURLs = map[string]string{mentionRecipientMDID: "https://bridge.test"}
		}, &notificationTestBridgeHosts{trustedURLs: map[string]bool{"https://bridge.test": true}}},
		{"recipient_blocks_actor", func(lookups *commentFanoutLookups, actorDID string) {
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: mentionRecipientMDID, blockedDID: actorDID}: true}
		}, nil},
		{"actor_blocks_recipient", func(lookups *commentFanoutLookups, actorDID string) {
			lookups.blocks = map[commentFanoutBlock]bool{{blockerDID: actorDID, blockedDID: mentionRecipientMDID}: true}
		}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationMentionComment()
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientMDID, mentionRecipientDDID)
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{
					mentionReplyRecipientDID: true, mentionRecipientMDID: true, mentionRecipientDDID: true,
				},
				activatedAt: comment.CreatedAt.Add(-time.Minute),
				indexTime:   comment.CreatedAt.Add(time.Minute),
			}
			test.setup(&lookups, comment.AuthorDID)

			intents, err := FanoutCommentCreate(context.Background(), lookups, test.bridgeHosts, comment)
			require.NoError(t, err)
			require.Equal(t, []Intent{
				notificationExpectedIntent(comment, ReasonPostReply, mentionReplyRecipientDID, comment.RootURI),
				notificationExpectedIntent(comment, ReasonMention, mentionRecipientDDID, ""),
			}, intents, "ineligible M must not receive a mention; eligible D must")
		})
	}
}

func TestFanoutCommentCreate_MentionRecordTimeGateBoundaries(t *testing.T) {
	indexTime := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		createdAt   time.Time
		activatedAt time.Time
		indexTime   time.Time
		wantIntents bool
	}{
		{"before_activation", indexTime, indexTime.Add(time.Nanosecond), indexTime.Add(time.Minute), false},
		{"older_than_seven_days", indexTime.Add(-7*24*time.Hour - time.Microsecond), indexTime.Add(-30 * 24 * time.Hour), indexTime, false},
		{"exactly_at_activation", indexTime, indexTime, indexTime.Add(time.Minute), true},
		{"exactly_seven_days_old", indexTime.Add(-7 * 24 * time.Hour), indexTime.Add(-30 * 24 * time.Hour), indexTime, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationTimeGateComment(test.createdAt)
			comment.FacetsJSON = notificationMentionFacets(mentionRecipientMDID, mentionRecipientDDID)
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{
					mentionReplyRecipientDID: true, mentionRecipientMDID: true, mentionRecipientDDID: true,
				},
				activatedAt: test.activatedAt,
				indexTime:   test.indexTime,
			}

			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.NoError(t, err)
			var want []Intent
			if test.wantIntents {
				want = []Intent{
					notificationExpectedIntent(comment, ReasonPostReply, mentionReplyRecipientDID, comment.RootURI),
					notificationExpectedIntent(comment, ReasonMention, mentionRecipientMDID, ""),
					notificationExpectedIntent(comment, ReasonMention, mentionRecipientDDID, ""),
				}
			}
			require.Equal(t, want, intents, "record time gates apply to both reply and all mentions")
		})
	}
}
