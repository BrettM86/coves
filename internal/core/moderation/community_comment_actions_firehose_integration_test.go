//go:build integration

package moderation_test

import (
	"testing"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Comment removals written outside Coves and indexed from the firehose, with
// no local intent behind them: the community-scope actions they record name no
// actor and carry the record's mapped code as the reason.

const commentDBExternalSpamCID = "bafyreiacommentremovalexternalspamaaaaaaaaaaaaaaaaaaaaa"

// I8 and I11 (Round 1 #6): a removal record with code spam in a hosted
// community, in an indexed community this instance does not host, and in the
// hosted community before its comment is indexed (held until the comment
// arrives). Its application records an apply-removal with no actor, reason
// #reasonSpam, origin inherited, scope the community; the record's deletion
// records a retract-removal the same way.
func TestCommunityCommentFirehoseDB_ExternalRemovalHasNoActor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		communityDID string
		// seed indexes the thread and returns the comment; indexLater, when
		// set, indexes the comment itself after the removal arrives.
		seed func(t *testing.T, f *communityCommentDBFixture) (comment seededCommunityComment, indexLater func())
	}{
		{
			name:         "hosted community",
			communityDID: rosterCommunity,
			seed: func(t *testing.T, f *communityCommentDBFixture) (seededCommunityComment, func()) {
				return f.seedThread(t).Comment, nil
			},
		},
		{
			name:         "indexed community this instance does not host",
			communityDID: postDBUnhostedCommunity,
			seed: func(t *testing.T, f *communityCommentDBFixture) (seededCommunityComment, func()) {
				post := f.seedUnhostedCommunityPost(t, postDBSeedFor("3kpostbridged1"))
				return f.indexComment(t, "3kcommentbridged1", "bafyreiacommentbridged1", "a comment in a bridged community",
					post.URI, post.CID, post.URI, post.CID, ""), nil
			},
		},
		{
			name:         "hosted community, removal indexed before its comment",
			communityDID: rosterCommunity,
			seed: func(t *testing.T, f *communityCommentDBFixture) (seededCommunityComment, func()) {
				post := f.seedAcceptedPost(t, postDBAlpha)
				const rkey, cid = "3kcommentheld1", "bafyreiacommentheld1"
				uri := "at://" + postDBAuthor + "/" + moderation.CommentCollection + "/" + rkey
				comment := seededCommunityComment{URI: uri, CID: cid, SubjectRkey: posts.SubjectRkey(uri)}
				return comment, func() {
					f.indexComment(t, rkey, cid, "a comment removed before it arrived", post.URI, post.CID, post.URI, post.CID, "")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			comment, indexLater := tt.seed(t, f)

			require.NoError(t, f.deliverPost(postStandEventIn(tt.communityDID, "create", posts.RemovalCollection, comment.SubjectRkey,
				"3krosterrev21", commentDBExternalSpamCID, commentRemovalBody(comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))))
			if indexLater != nil {
				assert.Empty(t, f.commentActions(t, comment.URI), "the removal is held until its comment is indexed")
				indexLater()
			}

			apply := externalCommentAction(tt.communityDID, "apply-removal", comment.URI, commentDBReasonSpam)
			assert.Equal(t, []storedPostAction{apply}, f.commentActions(t, comment.URI),
				"the application has no actor and the spam code's mapped reason")

			require.NoError(t, f.deliverPost(postStandEventIn(tt.communityDID, "delete", posts.RemovalCollection, comment.SubjectRkey,
				"3krosterrev22", "", nil)))

			assert.ElementsMatch(t, []storedPostAction{
				apply,
				externalCommentAction(tt.communityDID, "retract-removal", comment.URI, commentDBReasonSpam),
			}, f.commentActions(t, comment.URI), "the deletion's retract-removal has no actor and the deleted record's mapped reason")
		})
	}
}
