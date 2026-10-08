package notifications

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// commentFanoutLookups is the T0 Lookups fake. Its zero value never suppresses
// through a gate a test did not configure:
//   - unset activatedAt and indexTime are the zero time.Time, which leaves both
//     time gates open for every createdAt, including a zero one; tests of the
//     activation or seven-day gate set activatedAt and indexTime explicitly;
//   - an indexed user with no userPDSURLs entry is on a native, untrusted PDS.
type commentFanoutLookups struct {
	referenceStates                map[string]ReferenceState
	referenceStatesError           error
	referenceStatesCalls           *[][]string
	indexedUsers                   map[string]bool
	legacyPostAuthors              map[string]string
	legacyPostAuthorError          error
	legacyPostAuthorCalls          *int
	activatedAt                    time.Time
	indexTime                      time.Time
	activatedAtError               error
	indexTimeError                 error
	activatedAtCalls               *int
	indexTimeCalls                 *int
	erasedAccounts                 map[string]bool
	aggregatorAccounts             map[string]bool
	isAggregatorError              error
	isAggregatorCalls              *int
	communityAccounts              map[string]bool
	userPDSURLs                    map[string]string
	blocks                         map[commentFanoutBlock]bool
	recipientFactsError            error
	recipientFactsCalls            *[][]string
	recipientFactsActors           *[]string
	existingMentionRecipients      map[string][]string
	existingMentionRecipientsError error
	existingMentionRecipientsCalls *[]string
	earlierUpvoteExists            bool
	earlierUpvoteError             error
	earlierUpvoteCalls             *[]earlierUpvoteCall
}

func (lookup commentFanoutLookups) ReferenceStates(_ context.Context, uris []string) (map[string]ReferenceState, error) {
	if lookup.referenceStatesCalls != nil {
		*lookup.referenceStatesCalls = append(*lookup.referenceStatesCalls, append([]string{}, uris...))
	}
	if lookup.referenceStatesError != nil {
		return nil, lookup.referenceStatesError
	}
	return lookup.referenceStates, nil
}

type earlierUpvoteCall struct {
	voterDID   string
	subjectURI string
	voteURI    string
}

type commentFanoutBlock struct {
	blockerDID string
	blockedDID string
}

func (lookup commentFanoutLookups) LegacyPostAuthor(_ context.Context, postURI string) (string, bool, error) {
	if lookup.legacyPostAuthorCalls != nil {
		(*lookup.legacyPostAuthorCalls)++
	}
	if lookup.legacyPostAuthorError != nil {
		return "", false, lookup.legacyPostAuthorError
	}
	authorDID, found := lookup.legacyPostAuthors[postURI]
	return authorDID, found, nil
}

func (lookup commentFanoutLookups) ActivatedAt(_ context.Context) (time.Time, error) {
	if lookup.activatedAtCalls != nil {
		(*lookup.activatedAtCalls)++
	}
	if lookup.activatedAtError != nil {
		return time.Time{}, lookup.activatedAtError
	}
	return lookup.activatedAt, nil
}

func (lookup commentFanoutLookups) IndexTime(_ context.Context) (time.Time, error) {
	if lookup.indexTimeCalls != nil {
		(*lookup.indexTimeCalls)++
	}
	if lookup.indexTimeError != nil {
		return time.Time{}, lookup.indexTimeError
	}
	return lookup.indexTime, nil
}

func (lookup commentFanoutLookups) IsAggregator(_ context.Context, did string) (bool, error) {
	if lookup.isAggregatorCalls != nil {
		(*lookup.isAggregatorCalls)++
	}
	if lookup.isAggregatorError != nil {
		return false, lookup.isAggregatorError
	}
	return lookup.aggregatorAccounts[did], nil
}

func (lookup commentFanoutLookups) EarlierUpvoteExists(_ context.Context, voterDID, subjectURI, voteURI string) (bool, error) {
	if lookup.earlierUpvoteCalls != nil {
		*lookup.earlierUpvoteCalls = append(*lookup.earlierUpvoteCalls, earlierUpvoteCall{voterDID, subjectURI, voteURI})
	}
	if lookup.earlierUpvoteError != nil {
		return false, lookup.earlierUpvoteError
	}
	return lookup.earlierUpvoteExists, nil
}

func (lookup commentFanoutLookups) RecipientFacts(_ context.Context, actorDID string, recipientDIDs []string) (map[string]RecipientFacts, error) {
	if lookup.recipientFactsActors != nil {
		*lookup.recipientFactsActors = append(*lookup.recipientFactsActors, actorDID)
	}
	if lookup.recipientFactsCalls != nil {
		*lookup.recipientFactsCalls = append(*lookup.recipientFactsCalls, append([]string{}, recipientDIDs...))
	}
	if lookup.recipientFactsError != nil {
		return nil, lookup.recipientFactsError
	}
	facts := make(map[string]RecipientFacts)
	for _, did := range recipientDIDs {
		if !lookup.indexedUsers[did] {
			continue
		}
		pdsURL, ok := lookup.userPDSURLs[did]
		if !ok {
			pdsURL = "https://native.pds.test"
		}
		facts[did] = RecipientFacts{
			PDSURL:     pdsURL,
			Erased:     lookup.erasedAccounts[did],
			Aggregator: lookup.aggregatorAccounts[did],
			Community:  lookup.communityAccounts[did],
			BlockedWithActor: lookup.blocks[commentFanoutBlock{blockerDID: actorDID, blockedDID: did}] ||
				lookup.blocks[commentFanoutBlock{blockerDID: did, blockedDID: actorDID}],
		}
	}
	return facts, nil
}

func (lookup commentFanoutLookups) ExistingMentionRecipients(_ context.Context, recordURI string) ([]string, error) {
	if lookup.existingMentionRecipientsCalls != nil {
		*lookup.existingMentionRecipientsCalls = append(*lookup.existingMentionRecipientsCalls, recordURI)
	}
	if lookup.existingMentionRecipientsError != nil {
		return nil, lookup.existingMentionRecipientsError
	}
	return lookup.existingMentionRecipients[recordURI], nil
}

// A negative test that leaves the gate times unset must not pass because a
// hidden default time gate suppressed its intent.
func TestCommentFanoutLookups_DefaultTimeGatesNeverSuppress(t *testing.T) {
	const (
		commenterDID  = "did:plc:defaultgatecommenter"
		postAuthorDID = "did:plc:defaultgatepostauthor"
	)
	postURI := "at://" + postAuthorDID + "/social.coves.community.postv2/post"
	lookups := commentFanoutLookups{indexedUsers: map[string]bool{postAuthorDID: true}}
	for _, test := range []struct {
		name      string
		createdAt time.Time
	}{
		{"zero_createdAt", time.Time{}},
		{"old_createdAt", time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)},
		{"far_future_createdAt", time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := CommentRecord{
				URI: "at://" + commenterDID + "/social.coves.community.comment/reply", CID: "bafyreply",
				AuthorDID: commenterDID, ParentURI: postURI, RootURI: postURI, CreatedAt: test.createdAt,
			}
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.NoError(t, err)
			require.Equal(t, []Intent{{
				Reason: ReasonPostReply, RecipientDID: postAuthorDID, ActorDID: commenterDID,
				RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: postURI,
				RootPostURI: postURI, RecordCreatedAt: test.createdAt,
			}}, intents, "the fake's unset activation and index times must leave both time gates open")
		})
	}
}

func TestFanoutCommentCreate_PositiveReplyIntents(t *testing.T) {
	const (
		commenterDID    = "did:plc:commenter"
		postAuthorDID   = "did:plc:postauthor"
		legacyAuthorDID = "did:plc:legacyauthor"
		parentAuthorDID = "did:plc:parentauthor"
		rootAuthorDID   = "did:plc:rootauthor"
	)
	postURI := "at://" + postAuthorDID + "/social.coves.community.postv2/postkey"
	legacyPostURI := "at://did:plc:community/social.coves.community.post/legacykey"
	rootURI := "at://" + rootAuthorDID + "/social.coves.community.postv2/rootkey"
	parentURI := "at://" + parentAuthorDID + "/social.coves.community.comment/parentkey"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	// Every would-be recipient is indexed, so an extra intent to the nested
	// reply's distinct root author would survive RecipientFacts and be counted.
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{
			postAuthorDID: true, legacyAuthorDID: true, parentAuthorDID: true, rootAuthorDID: true,
		},
		legacyPostAuthors: map[string]string{legacyPostURI: legacyAuthorDID},
	}

	for _, test := range []struct {
		name       string
		comment    CommentRecord
		wantReason Reason
		wantDID    string
		wantURI    string
	}{
		{
			name: "postv2 post reply goes to indexed post author",
			comment: CommentRecord{
				URI: "at://" + commenterDID + "/social.coves.community.comment/top",
				CID: "bafycommenttop", AuthorDID: commenterDID,
				ParentURI: postURI, RootURI: postURI, CreatedAt: createdAt,
			},
			wantReason: ReasonPostReply, wantDID: postAuthorDID, wantURI: postURI,
		},
		{
			name: "legacy post reply goes to indexed row author rather than community",
			comment: CommentRecord{
				URI: "at://" + commenterDID + "/social.coves.community.comment/legacy",
				CID: "bafycommentlegacy", AuthorDID: commenterDID,
				ParentURI: legacyPostURI, RootURI: legacyPostURI, CreatedAt: createdAt,
			},
			wantReason: ReasonPostReply, wantDID: legacyAuthorDID, wantURI: legacyPostURI,
		},
		{
			name: "nested reply goes only to parent comment author",
			comment: CommentRecord{
				URI: "at://" + commenterDID + "/social.coves.community.comment/nested",
				CID: "bafycommentnested", AuthorDID: commenterDID,
				ParentURI: parentURI, RootURI: rootURI, CreatedAt: createdAt,
			},
			wantReason: ReasonCommentReply, wantDID: parentAuthorDID, wantURI: parentURI,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, test.comment)
			require.NoError(t, err)
			require.Len(t, intents, 1, "missing reply intent for %s", test.comment.URI)
			require.Equal(t, Intent{
				Reason:          test.wantReason,
				RecipientDID:    test.wantDID,
				ActorDID:        commenterDID,
				RecordURI:       test.comment.URI,
				RecordCID:       test.comment.CID,
				SubjectURI:      test.wantURI,
				RootPostURI:     test.comment.RootURI,
				RecordCreatedAt: createdAt,
			}, intents[0])
		})
	}
}

func TestFanoutCommentCreate_LegacyPostMissing(t *testing.T) {
	const communityDID = "did:plc:community"
	postURI := "at://" + communityDID + "/social.coves.community.post/missing"
	comment := CommentRecord{
		URI: "at://did:plc:commenter/social.coves.community.comment/reply", CID: "bafyreply",
		AuthorDID: "did:plc:commenter", ParentURI: postURI, RootURI: postURI,
		CreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
	}
	// The URI authority is indexed, so falling back to it would produce an intent.
	intents, err := FanoutCommentCreate(context.Background(), commentFanoutLookups{
		indexedUsers: map[string]bool{communityDID: true},
	}, nil, comment)
	require.NoError(t, err)
	require.Empty(t, intents, "a legacy post without a posts row has no author to notify")
}

func TestFanoutCommentCreate_ParentCommentTakesPrecedenceOverRootPost(t *testing.T) {
	const authorDID = "did:plc:postandcommentauthor"
	rootURI := "at://" + authorDID + "/social.coves.community.postv2/post"
	parentURI := "at://" + authorDID + "/social.coves.community.comment/parent"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	comment := CommentRecord{
		URI: "at://did:plc:commenter/social.coves.community.comment/reply", CID: "bafyreireply",
		AuthorDID: "did:plc:commenter", ParentURI: parentURI, RootURI: rootURI, CreatedAt: createdAt,
	}
	intents, err := FanoutCommentCreate(context.Background(), commentFanoutLookups{
		indexedUsers: map[string]bool{authorDID: true},
	}, nil, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{{
		Reason: ReasonCommentReply, RecipientDID: authorDID, ActorDID: comment.AuthorDID,
		RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: parentURI,
		RootPostURI: rootURI, RecordCreatedAt: createdAt,
	}}, intents, "a reply to B's comment on B's post must notify B exactly once as commentReply")
}

func TestFanoutCommentCreate_SuppressesSelfReplies(t *testing.T) {
	const authorDID = "did:plc:author"
	postURI := "at://" + authorDID + "/social.coves.community.postv2/post"
	parentURI := "at://" + authorDID + "/social.coves.community.comment/parent"
	legacyPostURI := "at://did:plc:community/social.coves.community.post/post"
	lookups := commentFanoutLookups{
		indexedUsers:      map[string]bool{authorDID: true},
		legacyPostAuthors: map[string]string{legacyPostURI: authorDID},
	}
	for _, test := range []struct {
		name      string
		parentURI string
		rootURI   string
	}{
		{"own postv2 post", postURI, postURI},
		{"own comment", parentURI, postURI},
		{"own legacy post", legacyPostURI, legacyPostURI},
	} {
		t.Run(test.name, func(t *testing.T) {
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, CommentRecord{
				URI: "at://" + authorDID + "/social.coves.community.comment/reply", CID: "bafyreply",
				AuthorDID: authorDID, ParentURI: test.parentURI, RootURI: test.rootURI,
				CreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
			require.Empty(t, intents, "replies to one's own post or comment must not notify oneself")
		})
	}
}

func TestFanoutCommentCreate_SelfReplyOnOthersPostDoesNotNotifyPostAuthor(t *testing.T) {
	const (
		commenterDID  = "did:plc:commenter"
		postAuthorDID = "did:plc:postauthor"
	)
	postURI := "at://" + postAuthorDID + "/social.coves.community.postv2/post"
	parentURI := "at://" + commenterDID + "/social.coves.community.comment/parent"
	intents, err := FanoutCommentCreate(context.Background(), commentFanoutLookups{
		indexedUsers: map[string]bool{commenterDID: true, postAuthorDID: true},
	}, nil, CommentRecord{
		URI: "at://" + commenterDID + "/social.coves.community.comment/reply", CID: "bafyreply",
		AuthorDID: commenterDID, ParentURI: parentURI, RootURI: postURI,
		CreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.Empty(t, intents, "A replying to A's own comment on B's post must not fall back to notifying B")
}

func TestFanoutCommentCreate_RecipientNotIndexed(t *testing.T) {
	postURI := "at://did:plc:postauthor/social.coves.community.postv2/post"
	parentURI := "at://did:plc:commentauthor/social.coves.community.comment/parent"
	for _, test := range []struct {
		name      string
		parentURI string
	}{
		{"postv2 post reply", postURI},
		{"comment reply", parentURI},
	} {
		t.Run(test.name, func(t *testing.T) {
			intents, err := FanoutCommentCreate(context.Background(), commentFanoutLookups{}, nil, CommentRecord{
				URI: "at://did:plc:commenter/social.coves.community.comment/reply", CID: "bafyreply",
				AuthorDID: "did:plc:commenter", ParentURI: test.parentURI, RootURI: postURI,
				CreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
			require.Empty(t, intents, "a recipient without a users row must not receive a reply notification")
		})
	}
}

func TestFanoutCommentCreate_UnsupportedParentCollection(t *testing.T) {
	const (
		parentAuthorDID = "did:plc:parentauthor"
		rootAuthorDID   = "did:plc:rootauthor"
	)
	unknownParentURI := "at://" + parentAuthorDID + "/social.coves.community.unknown/record"
	rootURI := "at://" + rootAuthorDID + "/social.coves.community.postv2/post"
	// Both URI authorities are indexed, so a fallback to either would produce an intent.
	lookups := commentFanoutLookups{indexedUsers: map[string]bool{parentAuthorDID: true, rootAuthorDID: true}}
	for _, test := range []struct {
		name      string
		parentURI string
		rootURI   string
	}{
		{"unsupported root replied to directly", unknownParentURI, unknownParentURI},
		{"unsupported parent under a post root", unknownParentURI, rootURI},
	} {
		t.Run(test.name, func(t *testing.T) {
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, CommentRecord{
				URI: "at://did:plc:commenter/social.coves.community.comment/reply", CID: "bafyreply",
				AuthorDID: "did:plc:commenter", ParentURI: test.parentURI, RootURI: test.rootURI,
				CreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
			require.Empty(t, intents, "a parent in an unsupported collection has no reply recipient")
		})
	}
}

func TestFanoutCommentCreate_CommentReplyRootMustBeAPost(t *testing.T) {
	const (
		parentAuthorDID = "did:plc:parentauthor"
		rootAuthorDID   = "did:plc:rootauthor"
	)
	parentURI := "at://" + parentAuthorDID + "/social.coves.community.comment/parent"
	lookups := commentFanoutLookups{indexedUsers: map[string]bool{parentAuthorDID: true, rootAuthorDID: true}}
	for _, test := range []struct {
		name    string
		rootURI string
	}{
		{"comment root", "at://" + rootAuthorDID + "/social.coves.community.comment/root"},
		{"unknown collection root", "at://" + rootAuthorDID + "/social.coves.community.unknown/root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, CommentRecord{
				URI: "at://did:plc:commenter/social.coves.community.comment/reply", CID: "bafyreply",
				AuthorDID: "did:plc:commenter", ParentURI: parentURI, RootURI: test.rootURI,
				CreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
			require.Empty(t, intents, "a commentReply notification's root must name a post")
		})
	}
}

// These thread URIs pass the comment consumer's lenient structural check but
// not syntax.ParseATURI. A payload defect is not transient: it must yield no
// recipient rather than an error that stalls indexing.
func TestFanoutCommentCreate_UnparsableThreadURIsHaveNoRecipient(t *testing.T) {
	const (
		postAuthorDID   = "did:plc:postauthor"
		parentAuthorDID = "did:plc:parentauthor"
	)
	postURI := "at://" + postAuthorDID + "/social.coves.community.postv2/post"
	parentURI := "at://" + parentAuthorDID + "/social.coves.community.comment/parent"
	lookups := commentFanoutLookups{indexedUsers: map[string]bool{
		postAuthorDID: true, parentAuthorDID: true, mentionRecipientDDID: true,
	}}
	for _, test := range []struct {
		name         string
		parentURI    string
		rootURI      string
		facetsJSON   string
		wantMentions []string
	}{
		{name: "trailing-slash post replied to directly", parentURI: postURI + "/", rootURI: postURI + "/"},
		{
			name: "trailing-slash parent comment does not fall back to the root author", parentURI: parentURI + "/", rootURI: postURI,
			facetsJSON: notificationMentionFacets(mentionRecipientDDID), wantMentions: []string{mentionRecipientDDID},
		},
		{name: "trailing-slash root post under a valid parent comment", parentURI: parentURI, rootURI: postURI + "/"},
		{name: "parent that is not an at-uri", parentURI: "not-an-at-uri", rootURI: postURI},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := CommentRecord{
				URI: "at://did:plc:commenter/social.coves.community.comment/reply", CID: "bafyreply",
				AuthorDID: "did:plc:commenter", ParentURI: test.parentURI, RootURI: test.rootURI,
				CreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC), FacetsJSON: test.facetsJSON,
			}
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.NoError(t, err, "an unparsable thread URI is not a transient failure")
			var want []Intent
			for _, did := range test.wantMentions {
				want = append(want, notificationExpectedIntent(comment, ReasonMention, did, ""))
			}
			require.Equal(t, want, intents,
				"an unparsable thread URI has no derivable reply recipient; a valid post root still carries mentions")
		})
	}
}
