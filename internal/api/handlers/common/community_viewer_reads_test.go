package common

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/moderation/moderationtest"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	readsHostedCommunity = "did:plc:readshosted"
	readsModerator       = "did:plc:readsmoderator"
	readsNoRole          = "did:plc:readsnorole"
)

// communityReads is how many per-community authority reads the authorizer saw.
func communityReads(authorizer *moderationtest.RoleAuthorizer) int {
	return authorizer.Calls("Authorize") + authorizer.Calls("ViewerRole") + authorizer.Calls("RequireHostedCommunity")
}

func readsRequest(viewerDID string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/xrpc/feed", nil)
	return request.WithContext(middleware.SetTestUserDID(request.Context(), viewerDID))
}

func readsFeedPost(communityDID, authorDID string) *posts.FeedViewPost {
	return &posts.FeedViewPost{Post: &posts.PostView{
		Community: &posts.CommunityRef{DID: communityDID},
		Author:    &posts.AuthorView{DID: authorDID},
		Status:    string(posts.AdmissionStatusAccepted),
	}}
}

// TestPopulateCommunityViewerModeration_NoRoleViewerMakesNoPerCommunityReads
// proves a viewer who is neither an instance admin nor holds a role anywhere
// costs one role read per request, however many communities the feed spans.
func TestPopulateCommunityViewerModeration_NoRoleViewerMakesNoPerCommunityReads(t *testing.T) {
	authorizer := &moderationtest.RoleAuthorizer{Hosted: map[string]bool{}}
	index := &moderationtest.RoleIndex{}
	var feed []*posts.FeedViewPost
	for i := 0; i < 20; i++ {
		communityDID := fmt.Sprintf("did:plc:readscommunity%d", i)
		authorizer.Hosted[communityDID] = i%2 == 0
		feed = append(feed, readsFeedPost(communityDID, "did:plc:readsauthor"))
	}

	PopulateCommunityViewerModeration(readsRequest(readsNoRole), moderation.NewCommunityViewer(authorizer, index, authorizer), feed)

	assert.Zero(t, communityReads(authorizer), "no per-community authority read for a viewer with no role")
	assert.Zero(t, index.Calls("ListCommunityRoles"), "no roster read for a viewer with no role")
	assert.LessOrEqual(t, authorizer.Calls("HasUsableRole"), 1)
	for _, feedPost := range feed {
		assert.Nil(t, feedPost.Post.Viewer)
	}
}

// TestPopulateCommunityViewerModeration_ReadsEachCommunityOnce proves a
// moderator's feed with many authors in one community reads the community's
// authority once and its roster once.
func TestPopulateCommunityViewerModeration_ReadsEachCommunityOnce(t *testing.T) {
	authorizer := &moderationtest.RoleAuthorizer{
		Hosted: map[string]bool{readsHostedCommunity: true},
		Roles:  map[string]map[string]string{readsHostedCommunity: {readsModerator: communities.RoleModerator}},
	}
	index := &moderationtest.RoleIndex{Rows: map[string][]communities.CommunityRole{readsHostedCommunity: {
		{CommunityDID: readsHostedCommunity, SubjectDID: readsModerator, Role: communities.RoleModerator, RecordKey: "moderator"},
	}}}
	var feed []*posts.FeedViewPost
	for i := 0; i < 50; i++ {
		feed = append(feed, readsFeedPost(readsHostedCommunity, fmt.Sprintf("did:plc:readsauthor%d", i)))
	}
	feed = append(feed, readsFeedPost(readsHostedCommunity, readsModerator))

	PopulateCommunityViewerModeration(readsRequest(readsModerator), moderation.NewCommunityViewer(authorizer, index, authorizer), feed)

	assert.Equal(t, 1, communityReads(authorizer), "one authority read for the community")
	assert.Equal(t, 1, index.Calls("ListCommunityRoles"), "one roster read for the community")
	require.NotNil(t, feed[0].Post.Viewer)
	assert.Equal(t, []string{"remove", "ban-author"}, feed[0].Post.Viewer.CommunityModeration)
	require.NotNil(t, feed[50].Post.Viewer)
	assert.Equal(t, []string{"remove"}, feed[50].Post.Viewer.CommunityModeration, "a role holder's post offers no ban")
}

// TestPopulateThreadCommunityViewerModeration_ReadsTheCommunityOnce proves a
// comment thread's tree and header share one set of reads.
func TestPopulateThreadCommunityViewerModeration_ReadsTheCommunityOnce(t *testing.T) {
	authorizer := &moderationtest.RoleAuthorizer{
		Hosted: map[string]bool{readsHostedCommunity: true},
		Roles:  map[string]map[string]string{readsHostedCommunity: {readsModerator: communities.RoleModerator}},
	}
	index := &moderationtest.RoleIndex{}
	header := readsFeedPost(readsHostedCommunity, "did:plc:readsauthor").Post
	views := []*comments.CommentView{
		{CommunityDID: readsHostedCommunity, Author: &posts.AuthorView{DID: "did:plc:readsauthor"}},
		{CommunityDID: readsHostedCommunity, Author: &posts.AuthorView{DID: "did:plc:readsotherauthor"}},
	}

	PopulateThreadCommunityViewerModeration(readsRequest(readsModerator), moderation.NewCommunityViewer(authorizer, index, authorizer), views, header)

	assert.Equal(t, 1, communityReads(authorizer), "one authority read across tree and header")
	assert.Equal(t, 1, index.Calls("ListCommunityRoles"), "one roster read across tree and header")
	require.NotNil(t, header.Viewer)
	assert.Equal(t, []string{"remove", "ban-author"}, header.Viewer.CommunityModeration)
	for _, view := range views {
		require.NotNil(t, view.Viewer)
		assert.Equal(t, []string{"remove", "ban-author"}, view.Viewer.CommunityModeration)
	}
}
