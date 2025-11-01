package votes

import (
	"Coves/internal/core/posts"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Mock repositories for testing
type mockVoteRepository struct {
	mock.Mock
}

func (m *mockVoteRepository) Create(ctx context.Context, vote *Vote) error {
	args := m.Called(ctx, vote)
	return args.Error(0)
}

func (m *mockVoteRepository) GetByURI(ctx context.Context, uri string) (*Vote, error) {
	args := m.Called(ctx, uri)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*Vote), args.Error(1)
}

func (m *mockVoteRepository) GetByVoterAndSubject(ctx context.Context, voterDID string, subjectURI string) (*Vote, error) {
	args := m.Called(ctx, voterDID, subjectURI)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*Vote), args.Error(1)
}

func (m *mockVoteRepository) Delete(ctx context.Context, uri string) error {
	args := m.Called(ctx, uri)
	return args.Error(0)
}

func (m *mockVoteRepository) ListBySubject(ctx context.Context, subjectURI string, limit, offset int) ([]*Vote, error) {
	args := m.Called(ctx, subjectURI, limit, offset)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*Vote), args.Error(1)
}

func (m *mockVoteRepository) ListByVoter(ctx context.Context, voterDID string, limit, offset int) ([]*Vote, error) {
	args := m.Called(ctx, voterDID, limit, offset)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*Vote), args.Error(1)
}

type mockPostRepository struct {
	mock.Mock
}

func (m *mockPostRepository) GetByURI(ctx context.Context, uri string) (*posts.Post, error) {
	args := m.Called(ctx, uri)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*posts.Post), args.Error(1)
}

func (m *mockPostRepository) Create(ctx context.Context, post *posts.Post) error {
	args := m.Called(ctx, post)
	return args.Error(0)
}

func (m *mockPostRepository) GetByRkey(ctx context.Context, communityDID, rkey string) (*posts.Post, error) {
	args := m.Called(ctx, communityDID, rkey)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*posts.Post), args.Error(1)
}

func (m *mockPostRepository) ListByCommunity(ctx context.Context, communityDID string, limit, offset int) ([]*posts.Post, error) {
	args := m.Called(ctx, communityDID, limit, offset)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*posts.Post), args.Error(1)
}

func (m *mockPostRepository) Delete(ctx context.Context, uri string) error {
	args := m.Called(ctx, uri)
	return args.Error(0)
}

// TestVoteService_CreateVote_NoExistingVote tests creating a vote when no vote exists
// NOTE: This test is skipped because we need to refactor service to inject HTTP client
// for testing PDS writes. The full flow is covered by E2E tests.
func TestVoteService_CreateVote_NoExistingVote(t *testing.T) {
	t.Skip("Skipping because we need to refactor service to inject HTTP client for testing PDS writes - covered by E2E tests")

	// This test would verify:
	// - Post exists check
	// - No existing vote
	// - PDS write succeeds
	// - Response contains vote URI and CID
}

// TestVoteService_ValidateInput tests input validation
func TestVoteService_ValidateInput(t *testing.T) {
	mockVoteRepo := new(mockVoteRepository)
	mockPostRepo := new(mockPostRepository)

	service := &voteService{
		repo:     mockVoteRepo,
		postRepo: mockPostRepo,
		pdsURL:   "http://mock-pds.test",
	}

	ctx := context.Background()

	tests := []struct {
		name          string
		voterDID      string
		accessToken   string
		req           CreateVoteRequest
		expectedError string
	}{
		{
			name:          "missing voter DID",
			voterDID:      "",
			accessToken:   "token123",
			req:           CreateVoteRequest{Subject: "at://test", Direction: "up"},
			expectedError: "voterDid",
		},
		{
			name:          "missing access token",
			voterDID:      "did:plc:test",
			accessToken:   "",
			req:           CreateVoteRequest{Subject: "at://test", Direction: "up"},
			expectedError: "userAccessToken",
		},
		{
			name:          "missing subject",
			voterDID:      "did:plc:test",
			accessToken:   "token123",
			req:           CreateVoteRequest{Subject: "", Direction: "up"},
			expectedError: "subject",
		},
		{
			name:          "invalid direction",
			voterDID:      "did:plc:test",
			accessToken:   "token123",
			req:           CreateVoteRequest{Subject: "at://test", Direction: "invalid"},
			expectedError: "invalid vote direction",
		},
		{
			name:          "invalid subject format",
			voterDID:      "did:plc:test",
			accessToken:   "token123",
			req:           CreateVoteRequest{Subject: "http://not-at-uri", Direction: "up"},
			expectedError: "invalid subject URI",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := service.CreateVote(ctx, tt.voterDID, tt.accessToken, tt.req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedError)
		})
	}
}

// TestVoteService_GetVote tests retrieving a vote
func TestVoteService_GetVote(t *testing.T) {
	mockVoteRepo := new(mockVoteRepository)
	mockPostRepo := new(mockPostRepository)

	service := &voteService{
		repo:     mockVoteRepo,
		postRepo: mockPostRepo,
		pdsURL:   "http://mock-pds.test",
	}

	ctx := context.Background()
	voterDID := "did:plc:voter123"
	subjectURI := "at://did:plc:community/social.coves.post.record/abc123"

	expectedVote := &Vote{
		ID:         1,
		URI:        "at://did:plc:voter123/social.coves.interaction.vote/xyz789",
		VoterDID:   voterDID,
		SubjectURI: subjectURI,
		Direction:  "up",
		CreatedAt:  time.Now(),
	}

	mockVoteRepo.On("GetByVoterAndSubject", ctx, voterDID, subjectURI).Return(expectedVote, nil)

	result, err := service.GetVote(ctx, voterDID, subjectURI)
	assert.NoError(t, err)
	assert.Equal(t, expectedVote.URI, result.URI)
	assert.Equal(t, expectedVote.Direction, result.Direction)

	mockVoteRepo.AssertExpectations(t)
}

// TestVoteService_GetVote_NotFound tests getting a non-existent vote
func TestVoteService_GetVote_NotFound(t *testing.T) {
	mockVoteRepo := new(mockVoteRepository)
	mockPostRepo := new(mockPostRepository)

	service := &voteService{
		repo:     mockVoteRepo,
		postRepo: mockPostRepo,
		pdsURL:   "http://mock-pds.test",
	}

	ctx := context.Background()
	voterDID := "did:plc:voter123"
	subjectURI := "at://did:plc:community/social.coves.post.record/noexist"

	mockVoteRepo.On("GetByVoterAndSubject", ctx, voterDID, subjectURI).Return(nil, ErrVoteNotFound)

	result, err := service.GetVote(ctx, voterDID, subjectURI)
	assert.ErrorIs(t, err, ErrVoteNotFound)
	assert.Nil(t, result)

	mockVoteRepo.AssertExpectations(t)
}

// TestVoteService_SubjectNotFound tests voting on non-existent post
func TestVoteService_SubjectNotFound(t *testing.T) {
	mockVoteRepo := new(mockVoteRepository)
	mockPostRepo := new(mockPostRepository)

	service := &voteService{
		repo:     mockVoteRepo,
		postRepo: mockPostRepo,
		pdsURL:   "http://mock-pds.test",
	}

	ctx := context.Background()
	voterDID := "did:plc:voter123"
	subjectURI := "at://did:plc:community/social.coves.post.record/noexist"

	// Mock post not found
	mockPostRepo.On("GetByURI", ctx, subjectURI).Return(nil, posts.ErrNotFound)

	req := CreateVoteRequest{
		Subject:   subjectURI,
		Direction: "up",
	}

	_, err := service.CreateVote(ctx, voterDID, "token123", req)
	assert.ErrorIs(t, err, ErrSubjectNotFound)

	mockPostRepo.AssertExpectations(t)
}

// NOTE: Testing toggle logic (same direction, different direction) requires mocking HTTP client
// These tests are covered by integration tests in tests/integration/vote_e2e_test.go
// To add unit tests for toggle logic, we would need to:
// 1. Refactor voteService to accept an HTTP client interface
// 2. Mock the PDS createRecord and deleteRecord calls
// 3. Verify the correct sequence of operations

// Example of what toggle tests would look like (requires refactoring):
/*
func TestVoteService_ToggleSameDirection(t *testing.T) {
	// Setup
	mockVoteRepo := new(mockVoteRepository)
	mockPostRepo := new(mockPostRepository)
	mockPDSClient := new(mockPDSClient)

	service := &voteService{
		repo:      mockVoteRepo,
		postRepo:  mockPostRepo,
		pdsClient: mockPDSClient, // Would need to refactor to inject this
	}

	ctx := context.Background()
	voterDID := "did:plc:voter123"
	subjectURI := "at://did:plc:community/social.coves.post.record/abc123"

	// Mock existing upvote
	existingVote := &Vote{
		URI:        "at://did:plc:voter123/social.coves.interaction.vote/existing",
		VoterDID:   voterDID,
		SubjectURI: subjectURI,
		Direction:  "up",
	}
	mockVoteRepo.On("GetByVoterAndSubject", ctx, voterDID, subjectURI).Return(existingVote, nil)

	// Mock post exists
	mockPostRepo.On("GetByURI", ctx, subjectURI).Return(&posts.Post{
		URI: subjectURI,
		CID: "bafyreigpost123",
	}, nil)

	// Mock PDS delete
	mockPDSClient.On("DeleteRecord", voterDID, "social.coves.interaction.vote", "existing").Return(nil)

	// Execute: Click upvote when already upvoted -> should delete
	req := CreateVoteRequest{
		Subject:   subjectURI,
		Direction: "up", // Same direction
	}

	response, err := service.CreateVote(ctx, voterDID, "token123", req)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, "", response.URI, "Should return empty URI when toggled off")
	mockPDSClient.AssertCalled(t, "DeleteRecord", voterDID, "social.coves.interaction.vote", "existing")
	mockVoteRepo.AssertExpectations(t)
	mockPostRepo.AssertExpectations(t)
}

func TestVoteService_ToggleDifferentDirection(t *testing.T) {
	// Similar test but existing vote is "up" and new vote is "down"
	// Should delete old vote and create new vote
	// Would verify:
	// 1. DeleteRecord called for old vote
	// 2. CreateRecord called for new vote
	// 3. Response contains new vote URI
}
*/

// Documentation test to explain toggle logic (verified by E2E tests)
func TestVoteService_ToggleLogicDocumentation(t *testing.T) {
	t.Log("Toggle Logic (verified by E2E tests in tests/integration/vote_e2e_test.go):")
	t.Log("1. No existing vote + upvote clicked → Create upvote")
	t.Log("2. Upvote exists + upvote clicked → Delete upvote (toggle off)")
	t.Log("3. Upvote exists + downvote clicked → Delete upvote + Create downvote (switch)")
	t.Log("4. Downvote exists + downvote clicked → Delete downvote (toggle off)")
	t.Log("5. Downvote exists + upvote clicked → Delete downvote + Create upvote (switch)")
	t.Log("")
	t.Log("To add unit tests for toggle logic, refactor service to accept HTTP client interface")
}
