package comments

import (
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Mock implementations for testing

// mockCommentRepo is a mock implementation of the comment Repository interface
type mockCommentRepo struct {
	comments                    map[string]*Comment
	listByParentWithHotRankFunc func(ctx context.Context, parentURI, sort, timeframe string, limit int, cursor *string) ([]*Comment, *string, error)
	listByParentsBatchFunc      func(ctx context.Context, parentURIs []string, sort string, limitPerParent int) (map[string][]*Comment, error)
	getVoteStateForCommentsFunc func(ctx context.Context, viewerDID string, commentURIs []string) (map[string]interface{}, error)
}

func newMockCommentRepo() *mockCommentRepo {
	return &mockCommentRepo{
		comments: make(map[string]*Comment),
	}
}

func (m *mockCommentRepo) Create(ctx context.Context, comment *Comment) error {
	m.comments[comment.URI] = comment
	return nil
}

func (m *mockCommentRepo) Update(ctx context.Context, comment *Comment) error {
	if _, ok := m.comments[comment.URI]; !ok {
		return ErrCommentNotFound
	}
	m.comments[comment.URI] = comment
	return nil
}

func (m *mockCommentRepo) GetByURI(ctx context.Context, uri string) (*Comment, error) {
	if c, ok := m.comments[uri]; ok {
		return c, nil
	}
	return nil, ErrCommentNotFound
}

func (m *mockCommentRepo) Delete(ctx context.Context, uri string) error {
	delete(m.comments, uri)
	return nil
}

func (m *mockCommentRepo) ListByRoot(ctx context.Context, rootURI string, limit, offset int) ([]*Comment, error) {
	return nil, nil
}

func (m *mockCommentRepo) ListByParent(ctx context.Context, parentURI string, limit, offset int) ([]*Comment, error) {
	return nil, nil
}

func (m *mockCommentRepo) CountByParent(ctx context.Context, parentURI string) (int, error) {
	return 0, nil
}

func (m *mockCommentRepo) ListByCommenter(ctx context.Context, commenterDID string, limit, offset int) ([]*Comment, error) {
	return nil, nil
}

func (m *mockCommentRepo) ListByParentWithHotRank(
	ctx context.Context,
	parentURI string,
	sort string,
	timeframe string,
	limit int,
	cursor *string,
) ([]*Comment, *string, error) {
	if m.listByParentWithHotRankFunc != nil {
		return m.listByParentWithHotRankFunc(ctx, parentURI, sort, timeframe, limit, cursor)
	}
	return []*Comment{}, nil, nil
}

func (m *mockCommentRepo) GetByURIsBatch(ctx context.Context, uris []string) (map[string]*Comment, error) {
	result := make(map[string]*Comment)
	for _, uri := range uris {
		if c, ok := m.comments[uri]; ok {
			result[uri] = c
		}
	}
	return result, nil
}

func (m *mockCommentRepo) GetVoteStateForComments(ctx context.Context, viewerDID string, commentURIs []string) (map[string]interface{}, error) {
	if m.getVoteStateForCommentsFunc != nil {
		return m.getVoteStateForCommentsFunc(ctx, viewerDID, commentURIs)
	}
	return make(map[string]interface{}), nil
}

func (m *mockCommentRepo) ListByParentsBatch(
	ctx context.Context,
	parentURIs []string,
	sort string,
	limitPerParent int,
) (map[string][]*Comment, error) {
	if m.listByParentsBatchFunc != nil {
		return m.listByParentsBatchFunc(ctx, parentURIs, sort, limitPerParent)
	}
	return make(map[string][]*Comment), nil
}

// mockUserRepo is a mock implementation of the users.UserRepository interface
type mockUserRepo struct {
	users map[string]*users.User
}

func newMockUserRepo() *mockUserRepo {
	return &mockUserRepo{
		users: make(map[string]*users.User),
	}
}

func (m *mockUserRepo) Create(ctx context.Context, user *users.User) (*users.User, error) {
	m.users[user.DID] = user
	return user, nil
}

func (m *mockUserRepo) GetByDID(ctx context.Context, did string) (*users.User, error) {
	if u, ok := m.users[did]; ok {
		return u, nil
	}
	return nil, errors.New("user not found")
}

func (m *mockUserRepo) GetByHandle(ctx context.Context, handle string) (*users.User, error) {
	for _, u := range m.users {
		if u.Handle == handle {
			return u, nil
		}
	}
	return nil, errors.New("user not found")
}

func (m *mockUserRepo) UpdateHandle(ctx context.Context, did, newHandle string) (*users.User, error) {
	if u, ok := m.users[did]; ok {
		u.Handle = newHandle
		return u, nil
	}
	return nil, errors.New("user not found")
}

func (m *mockUserRepo) GetByDIDs(ctx context.Context, dids []string) (map[string]*users.User, error) {
	result := make(map[string]*users.User, len(dids))
	for _, did := range dids {
		if u, ok := m.users[did]; ok {
			result[did] = u
		}
	}
	return result, nil
}

// mockPostRepo is a mock implementation of the posts.Repository interface
type mockPostRepo struct {
	posts map[string]*posts.Post
}

func newMockPostRepo() *mockPostRepo {
	return &mockPostRepo{
		posts: make(map[string]*posts.Post),
	}
}

func (m *mockPostRepo) Create(ctx context.Context, post *posts.Post) error {
	m.posts[post.URI] = post
	return nil
}

func (m *mockPostRepo) GetByURI(ctx context.Context, uri string) (*posts.Post, error) {
	if p, ok := m.posts[uri]; ok {
		return p, nil
	}
	return nil, posts.NewNotFoundError("post", uri)
}

// mockCommunityRepo is a mock implementation of the communities.Repository interface
type mockCommunityRepo struct {
	communities map[string]*communities.Community
}

func newMockCommunityRepo() *mockCommunityRepo {
	return &mockCommunityRepo{
		communities: make(map[string]*communities.Community),
	}
}

func (m *mockCommunityRepo) Create(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	m.communities[community.DID] = community
	return community, nil
}

func (m *mockCommunityRepo) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	if c, ok := m.communities[did]; ok {
		return c, nil
	}
	return nil, communities.ErrCommunityNotFound
}

func (m *mockCommunityRepo) GetByHandle(ctx context.Context, handle string) (*communities.Community, error) {
	for _, c := range m.communities {
		if c.Handle == handle {
			return c, nil
		}
	}
	return nil, communities.ErrCommunityNotFound
}

func (m *mockCommunityRepo) Update(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	m.communities[community.DID] = community
	return community, nil
}

func (m *mockCommunityRepo) Delete(ctx context.Context, did string) error {
	delete(m.communities, did)
	return nil
}

func (m *mockCommunityRepo) UpdateCredentials(ctx context.Context, did, accessToken, refreshToken string) error {
	return nil
}

func (m *mockCommunityRepo) List(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, nil
}

func (m *mockCommunityRepo) Search(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, nil
}

func (m *mockCommunityRepo) Subscribe(ctx context.Context, subscription *communities.Subscription) (*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityRepo) SubscribeWithCount(ctx context.Context, subscription *communities.Subscription) (*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityRepo) Unsubscribe(ctx context.Context, userDID, communityDID string) error {
	return nil
}

func (m *mockCommunityRepo) UnsubscribeWithCount(ctx context.Context, userDID, communityDID string) error {
	return nil
}

func (m *mockCommunityRepo) GetSubscription(ctx context.Context, userDID, communityDID string) (*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityRepo) GetSubscriptionByURI(ctx context.Context, recordURI string) (*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityRepo) ListSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityRepo) ListSubscribers(ctx context.Context, communityDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityRepo) BlockCommunity(ctx context.Context, block *communities.CommunityBlock) (*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *mockCommunityRepo) UnblockCommunity(ctx context.Context, userDID, communityDID string) error {
	return nil
}

func (m *mockCommunityRepo) GetBlock(ctx context.Context, userDID, communityDID string) (*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *mockCommunityRepo) GetBlockByURI(ctx context.Context, recordURI string) (*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *mockCommunityRepo) ListBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *mockCommunityRepo) IsBlocked(ctx context.Context, userDID, communityDID string) (bool, error) {
	return false, nil
}

func (m *mockCommunityRepo) CreateMembership(ctx context.Context, membership *communities.Membership) (*communities.Membership, error) {
	return nil, nil
}

func (m *mockCommunityRepo) GetMembership(ctx context.Context, userDID, communityDID string) (*communities.Membership, error) {
	return nil, nil
}

func (m *mockCommunityRepo) UpdateMembership(ctx context.Context, membership *communities.Membership) (*communities.Membership, error) {
	return nil, nil
}

func (m *mockCommunityRepo) ListMembers(ctx context.Context, communityDID string, limit, offset int) ([]*communities.Membership, error) {
	return nil, nil
}

func (m *mockCommunityRepo) CreateModerationAction(ctx context.Context, action *communities.ModerationAction) (*communities.ModerationAction, error) {
	return nil, nil
}

func (m *mockCommunityRepo) ListModerationActions(ctx context.Context, communityDID string, limit, offset int) ([]*communities.ModerationAction, error) {
	return nil, nil
}

func (m *mockCommunityRepo) IncrementMemberCount(ctx context.Context, communityDID string) error {
	return nil
}

func (m *mockCommunityRepo) DecrementMemberCount(ctx context.Context, communityDID string) error {
	return nil
}

func (m *mockCommunityRepo) IncrementSubscriberCount(ctx context.Context, communityDID string) error {
	return nil
}

func (m *mockCommunityRepo) DecrementSubscriberCount(ctx context.Context, communityDID string) error {
	return nil
}

func (m *mockCommunityRepo) IncrementPostCount(ctx context.Context, communityDID string) error {
	return nil
}

// Helper functions to create test data

func createTestPost(uri, authorDID, communityDID string) *posts.Post {
	title := "Test Post"
	content := "Test content"
	return &posts.Post{
		URI:           uri,
		CID:           "bafytest123",
		RKey:          "testrkey",
		AuthorDID:     authorDID,
		CommunityDID:  communityDID,
		Title:         &title,
		Content:       &content,
		CreatedAt:     time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IndexedAt:     time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		UpvoteCount:   10,
		DownvoteCount: 2,
		Score:         8,
		CommentCount:  5,
	}
}

func createTestComment(uri, commenterDID, commenterHandle, rootURI, parentURI string, replyCount int) *Comment {
	return &Comment{
		URI:             uri,
		CID:             "bafycomment123",
		RKey:            "commentrkey",
		CommenterDID:    commenterDID,
		CommenterHandle: commenterHandle,
		Content:         "Test comment content",
		RootURI:         rootURI,
		RootCID:         "bafyroot123",
		ParentURI:       parentURI,
		ParentCID:       "bafyparent123",
		CreatedAt:       time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		IndexedAt:       time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		UpvoteCount:     5,
		DownvoteCount:   1,
		Score:           4,
		ReplyCount:      replyCount,
		Langs:           []string{"en"},
	}
}

func createTestUser(did, handle string) *users.User {
	return &users.User{
		DID:       did,
		Handle:    handle,
		PDSURL:    "https://test.pds.local",
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func createTestCommunity(did, handle string) *communities.Community {
	return &communities.Community{
		DID:          did,
		Handle:       handle,
		Name:         "test",
		DisplayName:  "Test Community",
		Description:  "Test description",
		Visibility:   "public",
		OwnerDID:     did,
		CreatedByDID: "did:plc:creator",
		HostedByDID:  "did:web:coves.social",
		CreatedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Test suite for GetComments

func TestCommentService_GetComments_ValidRequest(t *testing.T) {
	// Setup
	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	authorDID := "did:plc:author123"
	communityDID := "did:plc:community123"
	commenterDID := "did:plc:commenter123"
	viewerDID := "did:plc:viewer123"

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	// Setup test data
	post := createTestPost(postURI, authorDID, communityDID)
	_ = postRepo.Create(context.Background(), post)

	author := createTestUser(authorDID, "author.test")
	_, _ = userRepo.Create(context.Background(), author)

	community := createTestCommunity(communityDID, "test.community.coves.social")
	_, _ = communityRepo.Create(context.Background(), community)

	comment1 := createTestComment("at://did:plc:commenter123/comment/1", commenterDID, "commenter.test", postURI, postURI, 0)
	comment2 := createTestComment("at://did:plc:commenter123/comment/2", commenterDID, "commenter.test", postURI, postURI, 0)

	commentRepo.listByParentWithHotRankFunc = func(ctx context.Context, parentURI, sort, timeframe string, limit int, cursor *string) ([]*Comment, *string, error) {
		if parentURI == postURI {
			return []*Comment{comment1, comment2}, nil, nil
		}
		return []*Comment{}, nil, nil
	}

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo)

	// Execute
	req := &GetCommentsRequest{
		PostURI:   postURI,
		ViewerDID: &viewerDID,
		Sort:      "hot",
		Depth:     10,
		Limit:     50,
	}

	resp, err := service.GetComments(context.Background(), req)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Len(t, resp.Comments, 2)
	assert.NotNil(t, resp.Post)
	assert.Nil(t, resp.Cursor)
}

func TestCommentService_GetComments_InvalidPostURI(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo)

	tests := []struct {
		name    string
		postURI string
		wantErr string
	}{
		{
			name:    "empty post URI",
			postURI: "",
			wantErr: "post URI is required",
		},
		{
			name:    "invalid URI format",
			postURI: "http://invalid.com/post",
			wantErr: "invalid AT-URI format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &GetCommentsRequest{
				PostURI: tt.postURI,
				Sort:    "hot",
				Depth:   10,
				Limit:   50,
			}

			resp, err := service.GetComments(context.Background(), req)

			assert.Error(t, err)
			assert.Nil(t, resp)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestCommentService_GetComments_PostNotFound(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo)

	// Execute
	req := &GetCommentsRequest{
		PostURI: "at://did:plc:post123/app.bsky.feed.post/nonexistent",
		Sort:    "hot",
		Depth:   10,
		Limit:   50,
	}

	resp, err := service.GetComments(context.Background(), req)

	// Verify
	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, ErrRootNotFound, err)
}

func TestCommentService_GetComments_EmptyComments(t *testing.T) {
	// Setup
	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	authorDID := "did:plc:author123"
	communityDID := "did:plc:community123"

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	// Setup test data
	post := createTestPost(postURI, authorDID, communityDID)
	_ = postRepo.Create(context.Background(), post)

	author := createTestUser(authorDID, "author.test")
	_, _ = userRepo.Create(context.Background(), author)

	community := createTestCommunity(communityDID, "test.community.coves.social")
	_, _ = communityRepo.Create(context.Background(), community)

	commentRepo.listByParentWithHotRankFunc = func(ctx context.Context, parentURI, sort, timeframe string, limit int, cursor *string) ([]*Comment, *string, error) {
		return []*Comment{}, nil, nil
	}

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo)

	// Execute
	req := &GetCommentsRequest{
		PostURI: postURI,
		Sort:    "hot",
		Depth:   10,
		Limit:   50,
	}

	resp, err := service.GetComments(context.Background(), req)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Len(t, resp.Comments, 0)
	assert.NotNil(t, resp.Post)
}

func TestCommentService_GetComments_WithViewerVotes(t *testing.T) {
	// Setup
	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	authorDID := "did:plc:author123"
	communityDID := "did:plc:community123"
	commenterDID := "did:plc:commenter123"
	viewerDID := "did:plc:viewer123"

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	// Setup test data
	post := createTestPost(postURI, authorDID, communityDID)
	_ = postRepo.Create(context.Background(), post)

	author := createTestUser(authorDID, "author.test")
	_, _ = userRepo.Create(context.Background(), author)

	community := createTestCommunity(communityDID, "test.community.coves.social")
	_, _ = communityRepo.Create(context.Background(), community)

	comment1URI := "at://did:plc:commenter123/comment/1"
	comment1 := createTestComment(comment1URI, commenterDID, "commenter.test", postURI, postURI, 0)

	commentRepo.listByParentWithHotRankFunc = func(ctx context.Context, parentURI, sort, timeframe string, limit int, cursor *string) ([]*Comment, *string, error) {
		if parentURI == postURI {
			return []*Comment{comment1}, nil, nil
		}
		return []*Comment{}, nil, nil
	}

	// Mock vote state
	commentRepo.getVoteStateForCommentsFunc = func(ctx context.Context, viewerDID string, commentURIs []string) (map[string]interface{}, error) {
		voteURI := "at://did:plc:viewer123/vote/1"
		return map[string]interface{}{
			comment1URI: map[string]interface{}{
				"direction": "up",
				"uri":       voteURI,
			},
		}, nil
	}

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo)

	// Execute
	req := &GetCommentsRequest{
		PostURI:   postURI,
		ViewerDID: &viewerDID,
		Sort:      "hot",
		Depth:     10,
		Limit:     50,
	}

	resp, err := service.GetComments(context.Background(), req)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Len(t, resp.Comments, 1)

	// Check viewer state
	commentView := resp.Comments[0].Comment
	assert.NotNil(t, commentView.Viewer)
	assert.NotNil(t, commentView.Viewer.Vote)
	assert.Equal(t, "up", *commentView.Viewer.Vote)
	assert.NotNil(t, commentView.Viewer.VoteURI)
}

func TestCommentService_GetComments_WithoutViewer(t *testing.T) {
	// Setup
	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	authorDID := "did:plc:author123"
	communityDID := "did:plc:community123"
	commenterDID := "did:plc:commenter123"

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	// Setup test data
	post := createTestPost(postURI, authorDID, communityDID)
	_ = postRepo.Create(context.Background(), post)

	author := createTestUser(authorDID, "author.test")
	_, _ = userRepo.Create(context.Background(), author)

	community := createTestCommunity(communityDID, "test.community.coves.social")
	_, _ = communityRepo.Create(context.Background(), community)

	comment1 := createTestComment("at://did:plc:commenter123/comment/1", commenterDID, "commenter.test", postURI, postURI, 0)

	commentRepo.listByParentWithHotRankFunc = func(ctx context.Context, parentURI, sort, timeframe string, limit int, cursor *string) ([]*Comment, *string, error) {
		if parentURI == postURI {
			return []*Comment{comment1}, nil, nil
		}
		return []*Comment{}, nil, nil
	}

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo)

	// Execute without viewer
	req := &GetCommentsRequest{
		PostURI:   postURI,
		ViewerDID: nil,
		Sort:      "hot",
		Depth:     10,
		Limit:     50,
	}

	resp, err := service.GetComments(context.Background(), req)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Len(t, resp.Comments, 1)

	// Viewer state should be nil
	commentView := resp.Comments[0].Comment
	assert.Nil(t, commentView.Viewer)
}

func TestCommentService_GetComments_SortingOptions(t *testing.T) {
	// Setup
	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	authorDID := "did:plc:author123"
	communityDID := "did:plc:community123"
	commenterDID := "did:plc:commenter123"

	tests := []struct {
		name      string
		sort      string
		timeframe string
		wantErr   bool
	}{
		{"hot sorting", "hot", "", false},
		{"top sorting", "top", "day", false},
		{"new sorting", "new", "", false},
		{"invalid sorting", "invalid", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commentRepo := newMockCommentRepo()
			userRepo := newMockUserRepo()
			postRepo := newMockPostRepo()
			communityRepo := newMockCommunityRepo()

			if !tt.wantErr {
				post := createTestPost(postURI, authorDID, communityDID)
				_ = postRepo.Create(context.Background(), post)

				author := createTestUser(authorDID, "author.test")
				_, _ = userRepo.Create(context.Background(), author)

				community := createTestCommunity(communityDID, "test.community.coves.social")
				_, _ = communityRepo.Create(context.Background(), community)

				comment1 := createTestComment("at://did:plc:commenter123/comment/1", commenterDID, "commenter.test", postURI, postURI, 0)

				commentRepo.listByParentWithHotRankFunc = func(ctx context.Context, parentURI, sort, timeframe string, limit int, cursor *string) ([]*Comment, *string, error) {
					return []*Comment{comment1}, nil, nil
				}
			}

			service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo)

			req := &GetCommentsRequest{
				PostURI:   postURI,
				Sort:      tt.sort,
				Timeframe: tt.timeframe,
				Depth:     10,
				Limit:     50,
			}

			resp, err := service.GetComments(context.Background(), req)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, resp)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, resp)
			}
		})
	}
}

func TestCommentService_GetComments_RepositoryError(t *testing.T) {
	// Setup
	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	authorDID := "did:plc:author123"
	communityDID := "did:plc:community123"

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	// Setup test data
	post := createTestPost(postURI, authorDID, communityDID)
	_ = postRepo.Create(context.Background(), post)

	author := createTestUser(authorDID, "author.test")
	_, _ = userRepo.Create(context.Background(), author)

	community := createTestCommunity(communityDID, "test.community.coves.social")
	_, _ = communityRepo.Create(context.Background(), community)

	// Mock repository error
	commentRepo.listByParentWithHotRankFunc = func(ctx context.Context, parentURI, sort, timeframe string, limit int, cursor *string) ([]*Comment, *string, error) {
		return nil, nil, errors.New("database error")
	}

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo)

	// Execute
	req := &GetCommentsRequest{
		PostURI: postURI,
		Sort:    "hot",
		Depth:   10,
		Limit:   50,
	}

	resp, err := service.GetComments(context.Background(), req)

	// Verify
	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "failed to fetch top-level comments")
}

// Test suite for buildThreadViews

func TestCommentService_buildThreadViews_EmptyInput(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute
	result := service.buildThreadViews(context.Background(), []*Comment{}, 10, "hot", nil)

	// Verify - should return empty slice, not nil
	assert.NotNil(t, result)
	assert.Len(t, result, 0)
}

func TestCommentService_buildThreadViews_SkipsDeletedComments(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	deletedAt := time.Now()

	// Create a deleted comment
	deletedComment := createTestComment("at://did:plc:commenter123/comment/1", "did:plc:commenter123", "commenter.test", postURI, postURI, 0)
	deletedComment.DeletedAt = &deletedAt

	// Create a normal comment
	normalComment := createTestComment("at://did:plc:commenter123/comment/2", "did:plc:commenter123", "commenter.test", postURI, postURI, 0)

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute
	result := service.buildThreadViews(context.Background(), []*Comment{deletedComment, normalComment}, 10, "hot", nil)

	// Verify - should only include non-deleted comment
	assert.Len(t, result, 1)
	assert.Equal(t, normalComment.URI, result[0].Comment.URI)
}

func TestCommentService_buildThreadViews_WithNestedReplies(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	parentURI := "at://did:plc:commenter123/comment/1"
	childURI := "at://did:plc:commenter123/comment/2"

	// Parent comment with replies
	parentComment := createTestComment(parentURI, "did:plc:commenter123", "commenter.test", postURI, postURI, 1)

	// Child comment
	childComment := createTestComment(childURI, "did:plc:commenter123", "commenter.test", postURI, parentURI, 0)

	// Mock batch loading of replies
	commentRepo.listByParentsBatchFunc = func(ctx context.Context, parentURIs []string, sort string, limitPerParent int) (map[string][]*Comment, error) {
		return map[string][]*Comment{
			parentURI: {childComment},
		}, nil
	}

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute with depth > 0 to load replies
	result := service.buildThreadViews(context.Background(), []*Comment{parentComment}, 1, "hot", nil)

	// Verify
	assert.Len(t, result, 1)
	assert.Equal(t, parentURI, result[0].Comment.URI)

	// Check nested replies
	assert.NotNil(t, result[0].Replies)
	assert.Len(t, result[0].Replies, 1)
	assert.Equal(t, childURI, result[0].Replies[0].Comment.URI)
}

func TestCommentService_buildThreadViews_DepthLimit(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"

	// Comment with replies but depth = 0
	parentComment := createTestComment("at://did:plc:commenter123/comment/1", "did:plc:commenter123", "commenter.test", postURI, postURI, 5)

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute with depth = 0 (should not load replies)
	result := service.buildThreadViews(context.Background(), []*Comment{parentComment}, 0, "hot", nil)

	// Verify
	assert.Len(t, result, 1)
	assert.Nil(t, result[0].Replies)
	assert.True(t, result[0].HasMore) // Should indicate more replies exist
}

// Test suite for buildCommentView

func TestCommentService_buildCommentView_BasicFields(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	commentURI := "at://did:plc:commenter123/comment/1"

	comment := createTestComment(commentURI, "did:plc:commenter123", "commenter.test", postURI, postURI, 0)

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute
	result := service.buildCommentView(comment, nil, nil, make(map[string]*users.User))

	// Verify basic fields
	assert.Equal(t, commentURI, result.URI)
	assert.Equal(t, comment.CID, result.CID)
	assert.Equal(t, comment.Content, result.Content)
	assert.NotNil(t, result.Author)
	assert.Equal(t, "did:plc:commenter123", result.Author.DID)
	assert.Equal(t, "commenter.test", result.Author.Handle)
	assert.NotNil(t, result.Stats)
	assert.Equal(t, 5, result.Stats.Upvotes)
	assert.Equal(t, 1, result.Stats.Downvotes)
	assert.Equal(t, 4, result.Stats.Score)
	assert.Equal(t, 0, result.Stats.ReplyCount)
}

func TestCommentService_buildCommentView_TopLevelComment(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	commentURI := "at://did:plc:commenter123/comment/1"

	// Top-level comment (parent = root)
	comment := createTestComment(commentURI, "did:plc:commenter123", "commenter.test", postURI, postURI, 0)

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute
	result := service.buildCommentView(comment, nil, nil, make(map[string]*users.User))

	// Verify - parent should be nil for top-level comments
	assert.NotNil(t, result.Post)
	assert.Equal(t, postURI, result.Post.URI)
	assert.Nil(t, result.Parent)
}

func TestCommentService_buildCommentView_NestedComment(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	parentCommentURI := "at://did:plc:commenter123/comment/1"
	childCommentURI := "at://did:plc:commenter123/comment/2"

	// Nested comment (parent != root)
	comment := createTestComment(childCommentURI, "did:plc:commenter123", "commenter.test", postURI, parentCommentURI, 0)

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute
	result := service.buildCommentView(comment, nil, nil, make(map[string]*users.User))

	// Verify - both post and parent should be present
	assert.NotNil(t, result.Post)
	assert.Equal(t, postURI, result.Post.URI)
	assert.NotNil(t, result.Parent)
	assert.Equal(t, parentCommentURI, result.Parent.URI)
}

func TestCommentService_buildCommentView_WithViewerVote(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	commentURI := "at://did:plc:commenter123/comment/1"
	viewerDID := "did:plc:viewer123"
	voteURI := "at://did:plc:viewer123/vote/1"

	comment := createTestComment(commentURI, "did:plc:commenter123", "commenter.test", postURI, postURI, 0)

	// Mock vote state
	voteStates := map[string]interface{}{
		commentURI: map[string]interface{}{
			"direction": "down",
			"uri":       voteURI,
		},
	}

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute
	result := service.buildCommentView(comment, &viewerDID, voteStates, make(map[string]*users.User))

	// Verify viewer state
	assert.NotNil(t, result.Viewer)
	assert.NotNil(t, result.Viewer.Vote)
	assert.Equal(t, "down", *result.Viewer.Vote)
	assert.NotNil(t, result.Viewer.VoteURI)
	assert.Equal(t, voteURI, *result.Viewer.VoteURI)
}

func TestCommentService_buildCommentView_NoViewerVote(t *testing.T) {
	// Setup
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	commentURI := "at://did:plc:commenter123/comment/1"
	viewerDID := "did:plc:viewer123"

	comment := createTestComment(commentURI, "did:plc:commenter123", "commenter.test", postURI, postURI, 0)

	// Empty vote states
	voteStates := map[string]interface{}{}

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Execute
	result := service.buildCommentView(comment, &viewerDID, voteStates, make(map[string]*users.User))

	// Verify viewer state exists but has no votes
	assert.NotNil(t, result.Viewer)
	assert.Nil(t, result.Viewer.Vote)
	assert.Nil(t, result.Viewer.VoteURI)
}

// Test suite for validateGetCommentsRequest

func TestValidateGetCommentsRequest_NilRequest(t *testing.T) {
	err := validateGetCommentsRequest(nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "request cannot be nil")
}

func TestValidateGetCommentsRequest_Defaults(t *testing.T) {
	req := &GetCommentsRequest{
		PostURI: "at://did:plc:post123/app.bsky.feed.post/test",
		// Depth and Limit are 0 (zero values)
	}

	err := validateGetCommentsRequest(req)
	assert.NoError(t, err)

	// Check defaults applied
	assert.Equal(t, "hot", req.Sort)
	// Depth 0 is valid (means no replies), only negative values get set to 10
	assert.Equal(t, 0, req.Depth)
	// Limit <= 0 gets set to 50
	assert.Equal(t, 50, req.Limit)
}

func TestValidateGetCommentsRequest_BoundsEnforcement(t *testing.T) {
	tests := []struct {
		name          string
		depth         int
		limit         int
		expectedDepth int
		expectedLimit int
	}{
		{"negative depth", -1, 10, 10, 10},
		{"depth too high", 150, 10, 100, 10},
		{"limit too low", 10, 0, 10, 50},
		{"limit too high", 10, 200, 10, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &GetCommentsRequest{
				PostURI: "at://did:plc:post123/app.bsky.feed.post/test",
				Depth:   tt.depth,
				Limit:   tt.limit,
			}

			err := validateGetCommentsRequest(req)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedDepth, req.Depth)
			assert.Equal(t, tt.expectedLimit, req.Limit)
		})
	}
}

func TestValidateGetCommentsRequest_InvalidSort(t *testing.T) {
	req := &GetCommentsRequest{
		PostURI: "at://did:plc:post123/app.bsky.feed.post/test",
		Sort:    "invalid",
		Depth:   10,
		Limit:   50,
	}

	err := validateGetCommentsRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sort")
}

func TestValidateGetCommentsRequest_InvalidTimeframe(t *testing.T) {
	req := &GetCommentsRequest{
		PostURI:   "at://did:plc:post123/app.bsky.feed.post/test",
		Sort:      "top",
		Timeframe: "invalid",
		Depth:     10,
		Limit:     50,
	}

	err := validateGetCommentsRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid timeframe")
}

// Test suite for mockUserRepo.GetByDIDs

func TestMockUserRepo_GetByDIDs_EmptyArray(t *testing.T) {
	userRepo := newMockUserRepo()
	ctx := context.Background()

	result, err := userRepo.GetByDIDs(ctx, []string{})

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 0)
}

func TestMockUserRepo_GetByDIDs_SingleDID(t *testing.T) {
	userRepo := newMockUserRepo()
	ctx := context.Background()

	// Add test user
	testUser := createTestUser("did:plc:user1", "user1.test")
	_, _ = userRepo.Create(ctx, testUser)

	result, err := userRepo.GetByDIDs(ctx, []string{"did:plc:user1"})

	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, "user1.test", result["did:plc:user1"].Handle)
}

func TestMockUserRepo_GetByDIDs_MultipleDIDs(t *testing.T) {
	userRepo := newMockUserRepo()
	ctx := context.Background()

	// Add multiple test users
	user1 := createTestUser("did:plc:user1", "user1.test")
	user2 := createTestUser("did:plc:user2", "user2.test")
	user3 := createTestUser("did:plc:user3", "user3.test")
	_, _ = userRepo.Create(ctx, user1)
	_, _ = userRepo.Create(ctx, user2)
	_, _ = userRepo.Create(ctx, user3)

	result, err := userRepo.GetByDIDs(ctx, []string{"did:plc:user1", "did:plc:user2", "did:plc:user3"})

	assert.NoError(t, err)
	assert.Len(t, result, 3)
	assert.Equal(t, "user1.test", result["did:plc:user1"].Handle)
	assert.Equal(t, "user2.test", result["did:plc:user2"].Handle)
	assert.Equal(t, "user3.test", result["did:plc:user3"].Handle)
}

func TestMockUserRepo_GetByDIDs_MissingDIDs(t *testing.T) {
	userRepo := newMockUserRepo()
	ctx := context.Background()

	// Add only one user
	user1 := createTestUser("did:plc:user1", "user1.test")
	_, _ = userRepo.Create(ctx, user1)

	// Query for two users, one missing
	result, err := userRepo.GetByDIDs(ctx, []string{"did:plc:user1", "did:plc:missing"})

	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, "user1.test", result["did:plc:user1"].Handle)
	assert.Nil(t, result["did:plc:missing"]) // Missing users not in map
}

func TestMockUserRepo_GetByDIDs_PreservesAllFields(t *testing.T) {
	userRepo := newMockUserRepo()
	ctx := context.Background()

	// Create user with all fields populated
	testUser := &users.User{
		DID:       "did:plc:user1",
		Handle:    "user1.test",
		PDSURL:    "https://pds.example.com",
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	_, _ = userRepo.Create(ctx, testUser)

	result, err := userRepo.GetByDIDs(ctx, []string{"did:plc:user1"})

	assert.NoError(t, err)
	user := result["did:plc:user1"]
	assert.Equal(t, "did:plc:user1", user.DID)
	assert.Equal(t, "user1.test", user.Handle)
	assert.Equal(t, "https://pds.example.com", user.PDSURL)
	assert.Equal(t, testUser.CreatedAt, user.CreatedAt)
	assert.Equal(t, testUser.UpdatedAt, user.UpdatedAt)
}

// Test suite for JSON deserialization in buildCommentView and buildCommentRecord

func TestBuildCommentView_ValidFacetsDeserialization(t *testing.T) {
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	facetsJSON := `[{"index":{"byteStart":0,"byteEnd":10},"features":[{"$type":"app.bsky.richtext.facet#mention","did":"did:plc:user123"}]}]`

	comment := createTestComment("at://did:plc:commenter123/comment/1", "did:plc:commenter123", "commenter.test", postURI, postURI, 0)
	comment.ContentFacets = &facetsJSON

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	result := service.buildCommentView(comment, nil, nil, make(map[string]*users.User))

	assert.NotNil(t, result.ContentFacets)
	assert.Len(t, result.ContentFacets, 1)
}

func TestBuildCommentView_ValidEmbedDeserialization(t *testing.T) {
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	embedJSON := `{"$type":"app.bsky.embed.images","images":[{"alt":"test","image":{"$type":"blob","ref":"bafytest"}}]}`

	comment := createTestComment("at://did:plc:commenter123/comment/1", "did:plc:commenter123", "commenter.test", postURI, postURI, 0)
	comment.Embed = &embedJSON

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	result := service.buildCommentView(comment, nil, nil, make(map[string]*users.User))

	assert.NotNil(t, result.Embed)
	embedMap, ok := result.Embed.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "app.bsky.embed.images", embedMap["$type"])
}

func TestBuildCommentRecord_ValidLabelsDeserialization(t *testing.T) {
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	labelsJSON := `{"$type":"com.atproto.label.defs#selfLabels","values":[{"val":"nsfw"}]}`

	comment := createTestComment("at://did:plc:commenter123/comment/1", "did:plc:commenter123", "commenter.test", postURI, postURI, 0)
	comment.ContentLabels = &labelsJSON

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	record := service.buildCommentRecord(comment)

	assert.NotNil(t, record.Labels)
}

func TestBuildCommentView_MalformedJSONLogsWarning(t *testing.T) {
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"
	malformedJSON := `{"invalid": json`

	comment := createTestComment("at://did:plc:commenter123/comment/1", "did:plc:commenter123", "commenter.test", postURI, postURI, 0)
	comment.ContentFacets = &malformedJSON

	service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

	// Should not panic, should log warning and return view with nil facets
	result := service.buildCommentView(comment, nil, nil, make(map[string]*users.User))

	assert.NotNil(t, result)
	assert.Nil(t, result.ContentFacets)
}

func TestBuildCommentView_EmptyStringVsNilHandling(t *testing.T) {
	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	postURI := "at://did:plc:post123/app.bsky.feed.post/test"

	tests := []struct {
		facetsValue        *string
		embedValue         *string
		labelsValue        *string
		name               string
		expectFacetsNil    bool
		expectEmbedNil     bool
		expectRecordLabels bool
	}{
		{
			name:               "All nil",
			facetsValue:        nil,
			embedValue:         nil,
			labelsValue:        nil,
			expectFacetsNil:    true,
			expectEmbedNil:     true,
			expectRecordLabels: false,
		},
		{
			name:               "All empty strings",
			facetsValue:        strPtr(""),
			embedValue:         strPtr(""),
			labelsValue:        strPtr(""),
			expectFacetsNil:    true,
			expectEmbedNil:     true,
			expectRecordLabels: false,
		},
		{
			name:               "Valid JSON strings",
			facetsValue:        strPtr(`[]`),
			embedValue:         strPtr(`{}`),
			labelsValue:        strPtr(`{"$type":"com.atproto.label.defs#selfLabels","values":[]}`),
			expectFacetsNil:    false,
			expectEmbedNil:     false,
			expectRecordLabels: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comment := createTestComment("at://did:plc:commenter123/comment/1", "did:plc:commenter123", "commenter.test", postURI, postURI, 0)
			comment.ContentFacets = tt.facetsValue
			comment.Embed = tt.embedValue
			comment.ContentLabels = tt.labelsValue

			service := NewCommentService(commentRepo, userRepo, postRepo, communityRepo).(*commentService)

			result := service.buildCommentView(comment, nil, nil, make(map[string]*users.User))

			if tt.expectFacetsNil {
				assert.Nil(t, result.ContentFacets)
			} else {
				assert.NotNil(t, result.ContentFacets)
			}

			if tt.expectEmbedNil {
				assert.Nil(t, result.Embed)
			} else {
				assert.NotNil(t, result.Embed)
			}

			record := service.buildCommentRecord(comment)
			if tt.expectRecordLabels {
				assert.NotNil(t, record.Labels)
			} else {
				assert.Nil(t, record.Labels)
			}
		})
	}
}

// Helper function to create string pointers
func strPtr(s string) *string {
	return &s
}
