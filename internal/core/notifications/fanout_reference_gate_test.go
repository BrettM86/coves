package notifications

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFanoutComment_WithdrawnReferences(t *testing.T) {
	const (
		root          = "at://did:plc:referencepostauthor/social.coves.community.postv2/root"
		parentComment = "at://did:plc:referencecommentauthor/social.coves.community.comment/parent"
		distinctPost  = "at://did:plc:referenceotherauthor/social.coves.community.postv2/other"
		mention       = "did:plc:referencemention"
		actor         = "did:plc:referenceactor"
		postAuthor    = "did:plc:referencepostauthor"
		commentAuthor = "did:plc:referencecommentauthor"
		ownComment    = "at://" + actor + "/social.coves.community.comment/reply"
	)
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, parent               string
		states                     map[string]ReferenceState
		wantReply                  Reason
		wantRecipient, wantSubject string
	}{
		{"top_level_live", root, nil, ReasonPostReply, postAuthor, root},
		{"root_deleted", root, map[string]ReferenceState{root: ReferenceDeleted}, "", "", ""},
		{"root_removed", root, map[string]ReferenceState{root: ReferenceRemovedByModerator}, "", "", ""},
		{"nested_live", parentComment, nil, ReasonCommentReply, commentAuthor, parentComment},
		{"parent_comment_deleted", parentComment, map[string]ReferenceState{parentComment: ReferenceDeleted}, "", "", ""},
		{"distinct_parent_live", distinctPost, nil, "", "", ""},
		{"distinct_parent_deleted", distinctPost, map[string]ReferenceState{distinctPost: ReferenceDeleted}, "", "", ""},
		{"distinct_parent_removed", distinctPost, map[string]ReferenceState{distinctPost: ReferenceRemovedByModerator}, "", "", ""},
		{"own_record_removed_by_server_admin", root, map[string]ReferenceState{ownComment: ReferenceRemovedByServerAdmin}, "", "", ""},
		{"nested_own_record_removed_by_server_admin", parentComment, map[string]ReferenceState{ownComment: ReferenceRemovedByServerAdmin}, "", "", ""},
	} {
		for _, operation := range []string{"create", "edit"} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				comment := CommentRecord{
					URI: ownComment, CID: "bafyreferencecomment",
					AuthorDID: actor, RootURI: root, ParentURI: test.parent, CreatedAt: createdAt,
					FacetsJSON: notificationMentionFacets(mention), EditEventTime: createdAt,
				}
				var activationCalls, indexCalls int
				var recipientCalls [][]string
				var existingCalls []string
				lookups := commentFanoutLookups{
					indexedUsers:    map[string]bool{postAuthor: true, commentAuthor: true, mention: true},
					referenceStates: test.states,
					activatedAt:     createdAt.Add(-time.Minute), indexTime: createdAt.Add(time.Minute),
					activatedAtCalls: &activationCalls, indexTimeCalls: &indexCalls,
					recipientFactsCalls: &recipientCalls, existingMentionRecipientsCalls: &existingCalls,
				}
				var intents []Intent
				var err error
				if operation == "create" {
					intents, err = FanoutCommentCreate(context.Background(), lookups, nil, comment)
				} else {
					intents, err = FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
				}
				require.NoError(t, err)
				if len(test.states) > 0 {
					require.Empty(t, intents, "withdrawn root or parent cannot generate notifications")
					require.Zero(t, activationCalls)
					require.Zero(t, indexCalls)
					require.Empty(t, recipientCalls)
					require.Empty(t, existingCalls)
					return
				}
				want := []Intent{}
				if operation == "create" && test.wantReply != "" {
					want = append(want, Intent{Reason: test.wantReply, RecipientDID: test.wantRecipient,
						ActorDID: actor, RecordURI: comment.URI, RecordCID: comment.CID,
						SubjectURI: test.wantSubject, RootPostURI: root, RecordCreatedAt: createdAt})
				}
				want = append(want, Intent{Reason: ReasonMention, RecipientDID: mention, ActorDID: actor,
					RecordURI: comment.URI, RecordCID: comment.CID, RootPostURI: root, RecordCreatedAt: createdAt})
				require.Equal(t, want, intents)
			})
		}
	}
}

func TestFanoutComment_DistinctParentReferencesLookedUpTogether(t *testing.T) {
	comment := notificationEditComment()
	comment.ParentURI = "at://did:plc:referenceotherauthor/social.coves.community.postv2/other"
	comment.FacetsJSON = notificationMentionFacets(mentionRecipientDDID)
	for _, operation := range []string{"create", "edit"} {
		t.Run(operation, func(t *testing.T) {
			var calls [][]string
			lookups := notificationEditLookups(comment)
			lookups.referenceStatesCalls = &calls
			var intents []Intent
			var err error
			if operation == "create" {
				intents, err = FanoutCommentCreate(context.Background(), lookups, nil, comment)
			} else {
				intents, err = FanoutCommentEdit(context.Background(), lookups, nil, comment, "")
			}
			require.NoError(t, err)
			require.Equal(t, []Intent{notificationExpectedIntent(comment, ReasonMention, mentionRecipientDDID, "")}, intents)
			require.Len(t, calls, 1, "one reference lookup for the comment, its root and distinct parent")
			require.ElementsMatch(t, []string{comment.URI, comment.RootURI, comment.ParentURI}, calls[0])
		})
	}
}

func TestFanoutPost_WithdrawnReferences(t *testing.T) {
	for _, operation := range []string{"create", "edit"} {
		for _, state := range []struct {
			name  string
			value ReferenceState
		}{
			{"live", ReferenceLive}, {"deleted", ReferenceDeleted}, {"removed", ReferenceRemovedByModerator},
		} {
			t.Run(operation+"/"+state.name, func(t *testing.T) {
				post := notificationPostWithMentions(time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC), mentionRecipientDDID)
				post.EditEventTime = post.CreatedAt
				var activationCalls, indexCalls int
				var recipientCalls [][]string
				var existingCalls []string
				lookups := commentFanoutLookups{
					indexedUsers:    map[string]bool{mentionRecipientDDID: true},
					referenceStates: map[string]ReferenceState{post.URI: state.value},
					activatedAt:     post.CreatedAt.Add(-time.Minute), indexTime: post.CreatedAt.Add(time.Minute),
					activatedAtCalls: &activationCalls, indexTimeCalls: &indexCalls,
					recipientFactsCalls: &recipientCalls, existingMentionRecipientsCalls: &existingCalls,
				}
				var intents []Intent
				var err error
				if operation == "create" {
					intents, err = FanoutPostCreate(context.Background(), lookups, nil, post)
				} else {
					intents, err = FanoutPostEdit(context.Background(), lookups, nil, post, "")
				}
				require.NoError(t, err)
				if state.value != ReferenceLive {
					require.Empty(t, intents)
					require.Zero(t, activationCalls)
					require.Zero(t, indexCalls)
					require.Empty(t, recipientCalls)
					require.Empty(t, existingCalls)
					return
				}
				require.Equal(t, []Intent{notificationPostMentionIntent(post, mentionRecipientDDID)}, intents)
			})
		}
	}
}

func TestFanoutVoteCreate_WithdrawnReferences(t *testing.T) {
	for _, test := range []struct {
		name, subject, root, withdrawn string
		state                          ReferenceState
		wantRecipient                  string
	}{
		{"post_live", voteFanoutPost, "", "", ReferenceLive, voteFanoutAuthor},
		{"post_removed", voteFanoutPost, "", voteFanoutPost, ReferenceRemovedByModerator, voteFanoutAuthor},
		{"post_deleted", voteFanoutPost, "", voteFanoutPost, ReferenceDeleted, voteFanoutAuthor},
		{"comment_live", voteFanoutCommenterComment, voteFanoutPost, "", ReferenceLive, voteFanoutCommenter},
		{"comment_deleted", voteFanoutCommenterComment, voteFanoutPost, voteFanoutCommenterComment, ReferenceDeleted, voteFanoutCommenter},
		{"comment_root_deleted", voteFanoutCommenterComment, voteFanoutPost, voteFanoutPost, ReferenceDeleted, voteFanoutCommenter},
		{"comment_root_removed", voteFanoutCommenterComment, voteFanoutPost, voteFanoutPost, ReferenceRemovedByModerator, voteFanoutCommenter},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, vote := qualifyingVoteFanout()
			lookups.indexedUsers[voteFanoutCommenter] = true
			vote.SubjectURI, vote.SubjectRootURI = test.subject, test.root
			if test.withdrawn != "" {
				lookups.referenceStates = map[string]ReferenceState{test.withdrawn: test.state}
			}
			var activationCalls, indexCalls int
			lookups.activatedAtCalls, lookups.indexTimeCalls = &activationCalls, &indexCalls
			intent, err := FanoutVoteCreate(context.Background(), lookups, nil, vote)
			require.NoError(t, err)
			if test.withdrawn != "" {
				require.Equal(t, UpvoteGroupIntent{Action: UpvoteGroupDeleteIfEmpty,
					RecipientDID: test.wantRecipient, SubjectURI: test.subject}, intent)
				require.NotEqual(t, UpvoteGroupBump, intent.Action)
				require.Zero(t, activationCalls)
				require.Zero(t, indexCalls)
				return
			}
			root := test.root
			if root == "" {
				root = test.subject
			}
			require.Equal(t, UpvoteGroupIntent{Action: UpvoteGroupBump,
				RecipientDID: test.wantRecipient, SubjectURI: test.subject, RootPostURI: root}, intent)
		})
	}
}
