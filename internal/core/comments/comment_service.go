package comments

import (
	"Coves/internal/core/posts"
	"context"
	"errors"
	"fmt"
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
	commentRepo Repository  // Comment data access
	userRepo    interface{} // User lookup (stubbed for now - Phase 2B)
	postRepo    interface{} // Post lookup (stubbed for now - Phase 2B)
}

// NewCommentService creates a new comment service instance
// userRepo and postRepo are interface{} for now to allow incremental implementation
func NewCommentService(commentRepo Repository, userRepo, postRepo interface{}) Service {
	return &commentService{
		commentRepo: commentRepo,
		userRepo:    userRepo,
		postRepo:    postRepo,
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
	// 1. Validate inputs and apply defaults/bounds
	if err := validateGetCommentsRequest(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// 2. Fetch post for context (stubbed for now - just create minimal response)
	// Future: s.fetchPost(ctx, req.PostURI)
	// For now, we'll return nil for Post field per the instructions

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
		Post:     nil, // TODO: Fetch and include PostView (Phase 2B)
		Cursor:   nextCursor,
	}, nil
}

// buildThreadViews recursively constructs threaded comment views with nested replies
// Loads replies iteratively up to the specified depth limit
// Each level fetches a limited number of replies to prevent N+1 query explosions
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

	// Convert each comment to a thread view
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

		// Recursively load replies if depth remains and comment has replies
		if remainingDepth > 0 && comment.ReplyCount > 0 {
			// Load first 5 replies per comment (configurable constant)
			// This prevents excessive nesting while showing conversation flow
			const repliesPerLevel = 5

			replies, _, err := s.commentRepo.ListByParentWithHotRank(
				ctx,
				comment.URI,
				sort,
				"", // No timeframe filter for nested replies
				repliesPerLevel,
				nil, // No cursor for nested replies (top 5 only)
			)

			// Only recurse if we successfully fetched replies
			if err == nil && len(replies) > 0 {
				threadView.Replies = s.buildThreadViews(
					ctx,
					replies,
					remainingDepth-1,
					sort,
					viewerDID,
				)

				// HasMore indicates if there are additional replies beyond what we loaded
				threadView.HasMore = comment.ReplyCount > len(replies)
			}
		}

		result = append(result, threadView)
	}

	return result
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

	return &CommentView{
		URI:       comment.URI,
		CID:       comment.CID,
		Author:    authorView,
		Record:    nil, // TODO: Parse and include original record if needed (Phase 2B)
		Post:      postRef,
		Parent:    parentRef,
		Content:   comment.Content,
		CreatedAt: comment.CreatedAt.Format(time.RFC3339),
		IndexedAt: comment.IndexedAt.Format(time.RFC3339),
		Stats:     stats,
		Viewer:    viewer,
	}
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
