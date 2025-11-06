package comments

import (
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// Service defines the business logic interface for comment operations
// Orchestrates repository calls and builds view models for API responses
type Service interface {
	// GetComments retrieves and builds a threaded comment tree for a post
	// Supports hot, top, and new sorting with configurable depth and pagination
	GetComments(ctx context.Context, req *GetCommentsRequest) (*GetCommentsResponse, error)
}

// GetCommentsRequest defines the parameters for fetching comments
type GetCommentsRequest struct {
	PostURI    string  // AT-URI of the post to fetch comments for
	Sort       string  // "hot", "top", "new" - sorting algorithm
	Timeframe  string  // "hour", "day", "week", "month", "year", "all" - for "top" sort only
	Depth      int     // 0-100 - how many levels of nested replies to load (default 10)
	Limit      int     // 1-100 - max top-level comments per page (default 50)
	Cursor     *string // Pagination cursor from previous response
	ViewerDID  *string // Optional DID of authenticated viewer (for vote state)
}

// commentService implements the Service interface
// Coordinates between repository layer and view model construction
type commentService struct {
	commentRepo     Repository                   // Comment data access
	userRepo        users.UserRepository         // User lookup for author hydration
	postRepo        posts.Repository             // Post lookup for building post views
	communityRepo   communities.Repository       // Community lookup for community hydration
}

// NewCommentService creates a new comment service instance
// All repositories are required for proper view construction per lexicon requirements
func NewCommentService(
	commentRepo Repository,
	userRepo users.UserRepository,
	postRepo posts.Repository,
	communityRepo communities.Repository,
) Service {
	return &commentService{
		commentRepo:   commentRepo,
		userRepo:      userRepo,
		postRepo:      postRepo,
		communityRepo: communityRepo,
	}
}

// GetComments retrieves comments for a post with threading and pagination
// Algorithm:
// 1. Validate input parameters and apply defaults
// 2. Fetch top-level comments with specified sorting
// 3. Recursively load nested replies up to depth limit
// 4. Build view models with author info and stats
// 5. Return response with pagination cursor
func (s *commentService) GetComments(ctx context.Context, req *GetCommentsRequest) (*GetCommentsResponse, error) {
	// 1. Validate inputs and apply defaults/bounds FIRST (before expensive operations)
	if err := validateGetCommentsRequest(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// Add timeout to prevent runaway queries with deep nesting
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 2. Fetch post for context
	post, err := s.postRepo.GetByURI(ctx, req.PostURI)
	if err != nil {
		// Translate post not-found errors to comment-layer errors for proper HTTP status
		if posts.IsNotFound(err) {
			return nil, ErrRootNotFound
		}
		return nil, fmt.Errorf("failed to fetch post: %w", err)
	}

	// Build post view for response (hydrates author handle and community name)
	postView := s.buildPostView(ctx, post, req.ViewerDID)

	// 3. Fetch top-level comments with pagination
	// Uses repository's hot rank sorting and cursor-based pagination
	topComments, nextCursor, err := s.commentRepo.ListByParentWithHotRank(
		ctx,
		req.PostURI,
		req.Sort,
		req.Timeframe,
		req.Limit,
		req.Cursor,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch top-level comments: %w", err)
	}

	// 4. Build threaded view with nested replies up to depth limit
	// This iteratively loads child comments and builds the tree structure
	threadViews := s.buildThreadViews(ctx, topComments, req.Depth, req.Sort, req.ViewerDID)

	// 5. Return response with comments, post reference, and cursor
	return &GetCommentsResponse{
		Comments: threadViews,
		Post:     postView,
		Cursor:   nextCursor,
	}, nil
}

// buildThreadViews constructs threaded comment views with nested replies using batch loading
// Uses batch queries to prevent N+1 query problem when loading nested replies
// Loads replies level-by-level up to the specified depth limit
func (s *commentService) buildThreadViews(
	ctx context.Context,
	comments []*Comment,
	remainingDepth int,
	sort string,
	viewerDID *string,
) []*ThreadViewComment {
	// Always return an empty slice, never nil (important for JSON serialization)
	result := make([]*ThreadViewComment, 0, len(comments))

	if len(comments) == 0 {
		return result
	}

	// Build thread views for current level
	threadViews := make([]*ThreadViewComment, 0, len(comments))
	commentsByURI := make(map[string]*ThreadViewComment)
	parentsWithReplies := make([]string, 0)

	for _, comment := range comments {
		// Skip deleted comments (soft-deleted records)
		if comment.DeletedAt != nil {
			continue
		}

		// Build the comment view with author info and stats
		commentView := s.buildCommentView(comment, viewerDID)

		threadView := &ThreadViewComment{
			Comment: commentView,
			Replies: nil,
			HasMore: comment.ReplyCount > 0 && remainingDepth == 0,
		}

		threadViews = append(threadViews, threadView)
		commentsByURI[comment.URI] = threadView

		// Collect parent URIs that have replies and depth remaining
		if remainingDepth > 0 && comment.ReplyCount > 0 {
			parentsWithReplies = append(parentsWithReplies, comment.URI)
		}
	}

	// Batch load all replies for this level in a single query
	if len(parentsWithReplies) > 0 {
		const repliesPerParent = 5 // Load top 5 replies per comment

		repliesByParent, err := s.commentRepo.ListByParentsBatch(
			ctx,
			parentsWithReplies,
			sort,
			repliesPerParent,
		)

		// Process replies if batch query succeeded
		if err == nil {
			// Group child comments by parent for recursive processing
			for parentURI, replies := range repliesByParent {
				threadView := commentsByURI[parentURI]
				if threadView != nil && len(replies) > 0 {
					// Recursively build views for child comments
					threadView.Replies = s.buildThreadViews(
						ctx,
						replies,
						remainingDepth-1,
						sort,
						viewerDID,
					)

					// Update HasMore based on actual reply count vs loaded count
					// Get the original comment to check reply count
					for _, comment := range comments {
						if comment.URI == parentURI {
							threadView.HasMore = comment.ReplyCount > len(replies)
							break
						}
					}
				}
			}
		}
	}

	return threadViews
}

// buildCommentView converts a Comment entity to a CommentView with full metadata
// Constructs author view, stats, and references to parent post/comment
func (s *commentService) buildCommentView(comment *Comment, viewerDID *string) *CommentView {
	// Build author view from comment data
	// CommenterHandle is hydrated by ListByParentWithHotRank via JOIN
	authorView := &posts.AuthorView{
		DID:    comment.CommenterDID,
		Handle: comment.CommenterHandle,
		// TODO: Add DisplayName, Avatar, Reputation when user service is integrated (Phase 2B)
	}

	// Build aggregated statistics
	stats := &CommentStats{
		Upvotes:    comment.UpvoteCount,
		Downvotes:  comment.DownvoteCount,
		Score:      comment.Score,
		ReplyCount: comment.ReplyCount,
	}

	// Build reference to parent post (always present)
	postRef := &CommentRef{
		URI: comment.RootURI,
		CID: comment.RootCID,
	}

	// Build reference to parent comment (only if nested)
	// Top-level comments have ParentURI == RootURI (both point to the post)
	var parentRef *CommentRef
	if comment.ParentURI != comment.RootURI {
		parentRef = &CommentRef{
			URI: comment.ParentURI,
			CID: comment.ParentCID,
		}
	}

	// Build viewer state (stubbed for now - Phase 2B)
	// Future: Fetch viewer's vote state from GetVoteStateForComments
	var viewer *CommentViewerState
	if viewerDID != nil {
		// TODO: Query voter state
		// voteState, err := s.commentRepo.GetVoteStateForComments(ctx, *viewerDID, []string{comment.URI})
		// For now, return empty viewer state to indicate authenticated request
		viewer = &CommentViewerState{
			Vote:    nil,
			VoteURI: nil,
		}
	}

	// Build minimal comment record to satisfy lexicon contract
	// The record field is required by social.coves.community.comment.defs#commentView
	commentRecord := s.buildCommentRecord(comment)

	return &CommentView{
		URI:       comment.URI,
		CID:       comment.CID,
		Author:    authorView,
		Record:    commentRecord,
		Post:      postRef,
		Parent:    parentRef,
		Content:   comment.Content,
		CreatedAt: comment.CreatedAt.Format(time.RFC3339),
		IndexedAt: comment.IndexedAt.Format(time.RFC3339),
		Stats:     stats,
		Viewer:    viewer,
	}
}

// buildCommentRecord constructs a minimal CommentRecord from a Comment entity
// Satisfies the lexicon requirement that commentView.record is a required field
// TODO (Phase 2C): Unmarshal JSON fields (embed, facets, labels) for complete record
func (s *commentService) buildCommentRecord(comment *Comment) *CommentRecord {
	record := &CommentRecord{
		Type: "social.coves.feed.comment",
		Reply: ReplyRef{
			Root: StrongRef{
				URI: comment.RootURI,
				CID: comment.RootCID,
			},
			Parent: StrongRef{
				URI: comment.ParentURI,
				CID: comment.ParentCID,
			},
		},
		Content:   comment.Content,
		CreatedAt: comment.CreatedAt.Format(time.RFC3339),
		Langs:     comment.Langs,
	}

	// TODO (Phase 2C): Parse JSON fields from database for complete record:
	// - Unmarshal comment.Embed (*string) → record.Embed (map[string]interface{})
	// - Unmarshal comment.ContentFacets (*string) → record.Facets ([]interface{})
	// - Unmarshal comment.ContentLabels (*string) → record.Labels (*SelfLabels)
	// These fields are stored as JSONB in the database and need proper deserialization

	return record
}

// buildPostView converts a Post entity to a PostView for the comment response
// Hydrates author handle and community name per lexicon requirements
func (s *commentService) buildPostView(ctx context.Context, post *posts.Post, viewerDID *string) *posts.PostView {
	// Build author view - fetch user to get handle (required by lexicon)
	// The lexicon marks authorView.handle with format:"handle", so DIDs are invalid
	authorHandle := post.AuthorDID // Fallback if user not found
	if user, err := s.userRepo.GetByDID(ctx, post.AuthorDID); err == nil {
		authorHandle = user.Handle
	} else {
		// Log warning but don't fail the entire request
		log.Printf("Warning: Failed to fetch user for post author %s: %v", post.AuthorDID, err)
	}

	authorView := &posts.AuthorView{
		DID:    post.AuthorDID,
		Handle: authorHandle,
		// TODO (Phase 2C): Add DisplayName, Avatar, Reputation from user profile
	}

	// Build community reference - fetch community to get name (required by lexicon)
	// The lexicon marks communityRef.name as required, so DIDs are insufficient
	communityName := post.CommunityDID // Fallback if community not found
	if community, err := s.communityRepo.GetByDID(ctx, post.CommunityDID); err == nil {
		communityName = community.Handle // Use handle as display name
		// TODO (Phase 2C): Use community.DisplayName or community.Name if available
	} else {
		// Log warning but don't fail the entire request
		log.Printf("Warning: Failed to fetch community for post %s: %v", post.CommunityDID, err)
	}

	communityRef := &posts.CommunityRef{
		DID:  post.CommunityDID,
		Name: communityName,
		// TODO (Phase 2C): Add Avatar from community profile
	}

	// Build aggregated statistics
	stats := &posts.PostStats{
		Upvotes:      post.UpvoteCount,
		Downvotes:    post.DownvoteCount,
		Score:        post.Score,
		CommentCount: post.CommentCount,
	}

	// Build viewer state if authenticated
	var viewer *posts.ViewerState
	if viewerDID != nil {
		// TODO (Phase 2B): Query viewer's vote state
		viewer = &posts.ViewerState{
			Vote:    nil,
			VoteURI: nil,
			Saved:   false,
		}
	}

	// Build minimal post record to satisfy lexicon contract
	// The record field is required by social.coves.community.post.get#postView
	postRecord := s.buildPostRecord(post)

	return &posts.PostView{
		URI:       post.URI,
		CID:       post.CID,
		RKey:      post.RKey,
		Author:    authorView,
		Record:    postRecord,
		Community: communityRef,
		Title:     post.Title,
		Text:      post.Content,
		CreatedAt: post.CreatedAt,
		IndexedAt: post.IndexedAt,
		EditedAt:  post.EditedAt,
		Stats:     stats,
		Viewer:    viewer,
	}
}

// buildPostRecord constructs a minimal PostRecord from a Post entity
// Satisfies the lexicon requirement that postView.record is a required field
// TODO (Phase 2C): Unmarshal JSON fields (embed, facets, labels) for complete record
func (s *commentService) buildPostRecord(post *posts.Post) *posts.PostRecord {
	record := &posts.PostRecord{
		Type:      "social.coves.community.post",
		Community: post.CommunityDID,
		Author:    post.AuthorDID,
		CreatedAt: post.CreatedAt.Format(time.RFC3339),
		Title:     post.Title,
		Content:   post.Content,
	}

	// TODO (Phase 2C): Parse JSON fields from database for complete record:
	// - Unmarshal post.Embed (*string) → record.Embed (map[string]interface{})
	// - Unmarshal post.ContentFacets (*string) → record.Facets ([]interface{})
	// - Unmarshal post.ContentLabels (*string) → record.Labels (*SelfLabels)
	// These fields are stored as JSONB in the database and need proper deserialization

	return record
}

// validateGetCommentsRequest validates and normalizes request parameters
// Applies default values and enforces bounds per API specification
func validateGetCommentsRequest(req *GetCommentsRequest) error {
	if req == nil {
		return errors.New("request cannot be nil")
	}

	// Validate PostURI is present and well-formed
	if req.PostURI == "" {
		return errors.New("post URI is required")
	}

	if !strings.HasPrefix(req.PostURI, "at://") {
		return errors.New("invalid AT-URI format: must start with 'at://'")
	}

	// Apply depth defaults and bounds (0-100, default 10)
	if req.Depth < 0 {
		req.Depth = 10
	}
	if req.Depth > 100 {
		req.Depth = 100
	}

	// Apply limit defaults and bounds (1-100, default 50)
	if req.Limit <= 0 {
		req.Limit = 50
	}
	if req.Limit > 100 {
		req.Limit = 100
	}

	// Apply sort default and validate
	if req.Sort == "" {
		req.Sort = "hot"
	}

	validSorts := map[string]bool{
		"hot": true,
		"top": true,
		"new": true,
	}
	if !validSorts[req.Sort] {
		return fmt.Errorf("invalid sort: must be one of [hot, top, new], got '%s'", req.Sort)
	}

	// Validate timeframe (only applies to "top" sort)
	if req.Timeframe != "" {
		validTimeframes := map[string]bool{
			"hour":  true,
			"day":   true,
			"week":  true,
			"month": true,
			"year":  true,
			"all":   true,
		}
		if !validTimeframes[req.Timeframe] {
			return fmt.Errorf("invalid timeframe: must be one of [hour, day, week, month, year, all], got '%s'", req.Timeframe)
		}
	}

	return nil
}
