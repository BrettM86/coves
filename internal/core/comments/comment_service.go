package comments

import (
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/rivo/uniseg"

	oauthclient "Coves/internal/atproto/oauth"
	"Coves/internal/atproto/pds"
)

const (
	// DefaultRepliesPerParent defines how many nested replies to load per parent comment
	// This balances UX (showing enough context) with performance (limiting query size)
	// Can be made configurable via constructor if needed in the future
	DefaultRepliesPerParent = 5

	// commentCollection is the AT Protocol collection for comment records
	commentCollection = "social.coves.community.comment"

	// maxCommentGraphemes is the maximum length for comment content in graphemes
	maxCommentGraphemes = 10000
)

// PDSClientFactory creates PDS clients from session data.
// Used to allow injection of different auth mechanisms (OAuth for production, password for tests).
type PDSClientFactory func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error)

// Service defines the business logic interface for comment operations
// Orchestrates repository calls and builds view models for API responses
type Service interface {
	// GetComments retrieves and builds a threaded comment tree for a post
	// Supports hot, top, and new sorting with configurable depth and pagination
	GetComments(ctx context.Context, req *GetCommentsRequest) (*GetCommentsResponse, error)

	// GetActorComments retrieves comments by a user for their profile page
	// Supports optional community filtering and cursor-based pagination
	GetActorComments(ctx context.Context, req *GetActorCommentsRequest) (*GetActorCommentsResponse, error)

	// CreateComment creates a new comment or reply
	CreateComment(ctx context.Context, session *oauth.ClientSessionData, req CreateCommentRequest) (*CreateCommentResponse, error)

	// UpdateComment updates an existing comment's content
	UpdateComment(ctx context.Context, session *oauth.ClientSessionData, req UpdateCommentRequest) (*UpdateCommentResponse, error)

	// DeleteComment soft-deletes a comment
	DeleteComment(ctx context.Context, session *oauth.ClientSessionData, req DeleteCommentRequest) error
}

// GetCommentsRequest defines the parameters for fetching comments
type GetCommentsRequest struct {
	Cursor    *string
	ViewerDID *string
	PostURI   string
	Sort      string
	Timeframe string
	Depth     int
	Limit     int
}

// commentService implements the Service interface
// Coordinates between repository layer and view model construction
type commentService struct {
	commentRepo      Repository                // Comment data access
	userRepo         users.UserRepository      // User lookup for author hydration
	postRepo         posts.Repository          // Post lookup for building post views
	communityRepo    communities.Repository    // Community lookup for community hydration
	oauthClient      *oauthclient.OAuthClient  // OAuth client for PDS authentication
	oauthStore       oauth.ClientAuthStore     // OAuth session store
	logger           *slog.Logger              // Structured logger
	pdsClientFactory PDSClientFactory          // Optional, for testing. If nil, uses OAuth.
}

// NewCommentService creates a new comment service instance
// All repositories are required for proper view construction per lexicon requirements
func NewCommentService(
	commentRepo Repository,
	userRepo users.UserRepository,
	postRepo posts.Repository,
	communityRepo communities.Repository,
	oauthClient *oauthclient.OAuthClient,
	oauthStore oauth.ClientAuthStore,
	logger *slog.Logger,
) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &commentService{
		commentRepo:   commentRepo,
		userRepo:      userRepo,
		postRepo:      postRepo,
		communityRepo: communityRepo,
		oauthClient:   oauthClient,
		oauthStore:    oauthStore,
		logger:        logger,
	}
}

// NewCommentServiceWithPDSFactory creates a comment service with a custom PDS client factory.
// This is primarily for testing with password-based authentication.
func NewCommentServiceWithPDSFactory(
	commentRepo Repository,
	userRepo users.UserRepository,
	postRepo posts.Repository,
	communityRepo communities.Repository,
	logger *slog.Logger,
	factory PDSClientFactory,
) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &commentService{
		commentRepo:      commentRepo,
		userRepo:         userRepo,
		postRepo:         postRepo,
		communityRepo:    communityRepo,
		logger:           logger,
		pdsClientFactory: factory,
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

	// Batch fetch vote states for all comments at this level (Phase 2B)
	var voteStates map[string]interface{}
	if viewerDID != nil {
		commentURIs := make([]string, 0, len(comments))
		for _, comment := range comments {
			if comment.DeletedAt == nil {
				commentURIs = append(commentURIs, comment.URI)
			}
		}

		if len(commentURIs) > 0 {
			var err error
			voteStates, err = s.commentRepo.GetVoteStateForComments(ctx, *viewerDID, commentURIs)
			if err != nil {
				// Log error but don't fail the request - vote state is optional
				log.Printf("Warning: Failed to fetch vote states for comments: %v", err)
			}
		}
	}

	// Batch fetch user data for all comment authors (Phase 2C)
	// Collect unique author DIDs to prevent duplicate queries
	authorDIDs := make([]string, 0, len(comments))
	seenDIDs := make(map[string]bool)
	for _, comment := range comments {
		if comment.DeletedAt == nil && !seenDIDs[comment.CommenterDID] {
			authorDIDs = append(authorDIDs, comment.CommenterDID)
			seenDIDs[comment.CommenterDID] = true
		}
	}

	// Fetch all users in one query to avoid N+1 problem
	var usersByDID map[string]*users.User
	if len(authorDIDs) > 0 {
		var err error
		usersByDID, err = s.userRepo.GetByDIDs(ctx, authorDIDs)
		if err != nil {
			// Log error but don't fail the request - user data is optional
			log.Printf("Warning: Failed to batch fetch users for comment authors: %v", err)
			usersByDID = make(map[string]*users.User)
		}
	} else {
		usersByDID = make(map[string]*users.User)
	}

	// Build thread views for current level
	threadViews := make([]*ThreadViewComment, 0, len(comments))
	commentsByURI := make(map[string]*ThreadViewComment)
	parentsWithReplies := make([]string, 0)

	for _, comment := range comments {
		var commentView *CommentView

		// Build appropriate view based on deletion status
		if comment.DeletedAt != nil {
			// Deleted comment - build placeholder view to preserve thread structure
			commentView = s.buildDeletedCommentView(comment)
		} else {
			// Active comment - build full view with author info and stats
			commentView = s.buildCommentView(comment, viewerDID, voteStates, usersByDID)
		}

		threadView := &ThreadViewComment{
			Comment: commentView,
			Replies: nil,
			HasMore: comment.ReplyCount > 0 && remainingDepth == 0,
		}

		threadViews = append(threadViews, threadView)
		commentsByURI[comment.URI] = threadView

		// Collect parent URIs that have replies and depth remaining
		// Include deleted comments so their children are still loaded
		if remainingDepth > 0 && comment.ReplyCount > 0 {
			parentsWithReplies = append(parentsWithReplies, comment.URI)
		}
	}

	// Batch load all replies for this level in a single query
	if len(parentsWithReplies) > 0 {
		repliesByParent, err := s.commentRepo.ListByParentsBatch(
			ctx,
			parentsWithReplies,
			sort,
			DefaultRepliesPerParent,
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
// voteStates map contains viewer's vote state for comments (from GetVoteStateForComments)
// usersByDID map contains pre-loaded user data for batch author hydration (Phase 2C)
func (s *commentService) buildCommentView(
	comment *Comment,
	viewerDID *string,
	voteStates map[string]interface{},
	usersByDID map[string]*users.User,
) *CommentView {
	// Build author view from comment data with full user hydration (Phase 2C)
	// CommenterHandle is hydrated by ListByParentWithHotRank via JOIN (fallback)
	// Prefer handle from usersByDID map for consistency
	authorHandle := comment.CommenterHandle
	if user, found := usersByDID[comment.CommenterDID]; found {
		authorHandle = user.Handle
	}

	authorView := &posts.AuthorView{
		DID:    comment.CommenterDID,
		Handle: authorHandle,
		// DisplayName, Avatar, Reputation will be populated when user profile schema is extended
		// Currently User model only has DID, Handle, PDSURL fields
		DisplayName: nil,
		Avatar:      nil,
		Reputation:  nil,
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

	// Build viewer state - populate from vote states map (Phase 2B)
	var viewer *CommentViewerState
	if viewerDID != nil {
		viewer = &CommentViewerState{
			Vote:    nil,
			VoteURI: nil,
		}

		// Check if viewer has voted on this comment
		if voteStates != nil {
			if voteData, ok := voteStates[comment.URI]; ok {
				voteMap, isMap := voteData.(map[string]interface{})
				if isMap {
					// Extract vote direction and URI
					// Create copies before taking addresses to avoid pointer to loop variable issues
					if direction, hasDirection := voteMap["direction"].(string); hasDirection {
						directionCopy := direction
						viewer.Vote = &directionCopy
					}
					if voteURI, hasVoteURI := voteMap["uri"].(string); hasVoteURI {
						voteURICopy := voteURI
						viewer.VoteURI = &voteURICopy
					}
				}
			}
		}
	}

	// Build minimal comment record to satisfy lexicon contract
	// The record field is required by social.coves.community.comment.defs#commentView
	commentRecord := s.buildCommentRecord(comment)

	// Deserialize contentFacets from JSONB (Phase 2C)
	// Parse facets from database JSON string to populate contentFacets field
	var contentFacets []interface{}
	if comment.ContentFacets != nil && *comment.ContentFacets != "" {
		if err := json.Unmarshal([]byte(*comment.ContentFacets), &contentFacets); err != nil {
			// Log error but don't fail request - facets are optional
			log.Printf("Warning: Failed to unmarshal content facets for comment %s: %v", comment.URI, err)
		}
	}

	// Deserialize embed from JSONB (Phase 2C)
	// Parse embed from database JSON string to populate embed field
	var embed interface{}
	if comment.Embed != nil && *comment.Embed != "" {
		var embedMap map[string]interface{}
		if err := json.Unmarshal([]byte(*comment.Embed), &embedMap); err != nil {
			// Log error but don't fail request - embed is optional
			log.Printf("Warning: Failed to unmarshal embed for comment %s: %v", comment.URI, err)
		} else {
			embed = embedMap
		}
	}

	return &CommentView{
		URI:           comment.URI,
		CID:           comment.CID,
		Author:        authorView,
		Record:        commentRecord,
		Post:          postRef,
		Parent:        parentRef,
		Content:       comment.Content,
		ContentFacets: contentFacets,
		Embed:         embed,
		CreatedAt:     comment.CreatedAt.Format(time.RFC3339),
		IndexedAt:     comment.IndexedAt.Format(time.RFC3339),
		Stats:         stats,
		Viewer:        viewer,
	}
}

// buildDeletedCommentView creates a placeholder view for a deleted comment
// Preserves threading structure while hiding content
// Shows as "[deleted]" in the UI with minimal metadata
func (s *commentService) buildDeletedCommentView(comment *Comment) *CommentView {
	// Build minimal author view - just DID for attribution
	// Frontend will display "[deleted]" or "[deleted by @user]" based on deletion_reason
	authorView := &posts.AuthorView{
		DID:         comment.CommenterDID,
		Handle:      "", // Empty - frontend handles display
		DisplayName: nil,
		Avatar:      nil,
		Reputation:  nil,
	}

	// Build minimal stats - preserve reply count for threading indication
	stats := &CommentStats{
		Upvotes:    0,
		Downvotes:  0,
		Score:      0,
		ReplyCount: comment.ReplyCount, // Keep this to show threading
	}

	// Build reference to parent post (always present)
	postRef := &CommentRef{
		URI: comment.RootURI,
		CID: comment.RootCID,
	}

	// Build reference to parent comment (only if nested)
	var parentRef *CommentRef
	if comment.ParentURI != comment.RootURI {
		parentRef = &CommentRef{
			URI: comment.ParentURI,
			CID: comment.ParentCID,
		}
	}

	// Format deletion timestamp for frontend
	var deletedAtStr *string
	if comment.DeletedAt != nil {
		ts := comment.DeletedAt.Format(time.RFC3339)
		deletedAtStr = &ts
	}

	return &CommentView{
		URI:            comment.URI,
		CID:            comment.CID,
		Author:         authorView,
		Record:         nil, // No record for deleted comments
		Post:           postRef,
		Parent:         parentRef,
		Content:        "", // Blanked content
		ContentFacets:  nil,
		Embed:          nil,
		CreatedAt:      comment.CreatedAt.Format(time.RFC3339),
		IndexedAt:      comment.IndexedAt.Format(time.RFC3339),
		Stats:          stats,
		Viewer:         nil, // No viewer state for deleted comments
		IsDeleted:      true,
		DeletionReason: comment.DeletionReason,
		DeletedAt:      deletedAtStr,
	}
}

// buildCommentRecord constructs a complete CommentRecord from a Comment entity
// Satisfies the lexicon requirement that commentView.record is a required field
// Deserializes JSONB fields (embed, facets, labels) for complete record (Phase 2C)
func (s *commentService) buildCommentRecord(comment *Comment) *CommentRecord {
	record := &CommentRecord{
		Type: "social.coves.community.comment",
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

	// Deserialize facets from JSONB (Phase 2C)
	if comment.ContentFacets != nil && *comment.ContentFacets != "" {
		var facets []interface{}
		if err := json.Unmarshal([]byte(*comment.ContentFacets), &facets); err != nil {
			// Log error but don't fail request - facets are optional
			log.Printf("Warning: Failed to unmarshal facets for record %s: %v", comment.URI, err)
		} else {
			record.Facets = facets
		}
	}

	// Deserialize embed from JSONB (Phase 2C)
	if comment.Embed != nil && *comment.Embed != "" {
		var embed map[string]interface{}
		if err := json.Unmarshal([]byte(*comment.Embed), &embed); err != nil {
			// Log error but don't fail request - embed is optional
			log.Printf("Warning: Failed to unmarshal embed for record %s: %v", comment.URI, err)
		} else {
			record.Embed = embed
		}
	}

	// Deserialize labels from JSONB (Phase 2C)
	if comment.ContentLabels != nil && *comment.ContentLabels != "" {
		var labels SelfLabels
		if err := json.Unmarshal([]byte(*comment.ContentLabels), &labels); err != nil {
			// Log error but don't fail request - labels are optional
			log.Printf("Warning: Failed to unmarshal labels for record %s: %v", comment.URI, err)
		} else {
			record.Labels = &labels
		}
	}

	return record
}

// getPDSClient creates a PDS client from an OAuth session.
// If a custom factory was provided (for testing), uses that.
// Otherwise, uses DPoP authentication via indigo's APIClient for proper OAuth token handling.
func (s *commentService) getPDSClient(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
	// Use custom factory if provided (e.g., for testing with password auth)
	if s.pdsClientFactory != nil {
		return s.pdsClientFactory(ctx, session)
	}

	// Production path: use OAuth with DPoP
	if s.oauthClient == nil || s.oauthClient.ClientApp == nil {
		return nil, fmt.Errorf("OAuth client not configured")
	}

	client, err := pds.NewFromOAuthSession(ctx, s.oauthClient.ClientApp, session)
	if err != nil {
		return nil, fmt.Errorf("failed to create PDS client: %w", err)
	}

	return client, nil
}

// CreateComment creates a new comment on a post or reply to another comment
func (s *commentService) CreateComment(ctx context.Context, session *oauth.ClientSessionData, req CreateCommentRequest) (*CreateCommentResponse, error) {
	// Validate content not empty
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return nil, ErrContentEmpty
	}

	// Validate content length (max 10000 graphemes)
	if uniseg.GraphemeClusterCount(content) > maxCommentGraphemes {
		return nil, ErrContentTooLong
	}

	// Validate reply references
	if err := validateReplyRef(req.Reply); err != nil {
		return nil, err
	}

	// Create PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		s.logger.Error("failed to create PDS client",
			"error", err,
			"commenter", session.AccountDID)
		return nil, fmt.Errorf("failed to create PDS client: %w", err)
	}

	// Generate TID for the record key
	tid := syntax.NewTIDNow(0)

	// Build comment record following the lexicon schema
	record := CommentRecord{
		Type:      commentCollection,
		Reply:     req.Reply,
		Content:   content,
		Facets:    req.Facets,
		Embed:     req.Embed,
		Langs:     req.Langs,
		Labels:    req.Labels,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Create the comment record on the user's PDS
	uri, cid, err := pdsClient.CreateRecord(ctx, commentCollection, tid.String(), record)
	if err != nil {
		s.logger.Error("failed to create comment on PDS",
			"error", err,
			"commenter", session.AccountDID,
			"root", req.Reply.Root.URI,
			"parent", req.Reply.Parent.URI)
		if pds.IsAuthError(err) {
			return nil, ErrNotAuthorized
		}
		return nil, fmt.Errorf("failed to create comment: %w", err)
	}

	s.logger.Info("comment created",
		"commenter", session.AccountDID,
		"uri", uri,
		"cid", cid,
		"root", req.Reply.Root.URI,
		"parent", req.Reply.Parent.URI)

	return &CreateCommentResponse{
		URI: uri,
		CID: cid,
	}, nil
}

// UpdateComment updates an existing comment's content
func (s *commentService) UpdateComment(ctx context.Context, session *oauth.ClientSessionData, req UpdateCommentRequest) (*UpdateCommentResponse, error) {
	// Validate URI format
	if req.URI == "" {
		return nil, ErrCommentNotFound
	}
	if !strings.HasPrefix(req.URI, "at://") {
		return nil, ErrCommentNotFound
	}

	// Extract DID and rkey from URI (at://did/collection/rkey)
	parts := strings.Split(req.URI, "/")
	if len(parts) < 5 || parts[3] != commentCollection {
		return nil, ErrCommentNotFound
	}
	did := parts[2]
	rkey := parts[4]

	// Verify ownership: URI must belong to the authenticated user
	if did != session.AccountDID.String() {
		return nil, ErrNotAuthorized
	}

	// Validate new content
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return nil, ErrContentEmpty
	}

	// Validate content length (max 10000 graphemes)
	if uniseg.GraphemeClusterCount(content) > maxCommentGraphemes {
		return nil, ErrContentTooLong
	}

	// Create PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		s.logger.Error("failed to create PDS client",
			"error", err,
			"commenter", session.AccountDID)
		return nil, fmt.Errorf("failed to create PDS client: %w", err)
	}

	// Fetch existing record from PDS to get the reply refs (immutable)
	existingRecord, err := pdsClient.GetRecord(ctx, commentCollection, rkey)
	if err != nil {
		s.logger.Error("failed to fetch existing comment from PDS",
			"error", err,
			"uri", req.URI,
			"rkey", rkey)
		if pds.IsAuthError(err) {
			return nil, ErrNotAuthorized
		}
		if errors.Is(err, pds.ErrNotFound) {
			return nil, ErrCommentNotFound
		}
		return nil, fmt.Errorf("failed to fetch existing comment: %w", err)
	}

	// Extract reply refs from existing record (must be preserved)
	replyData, ok := existingRecord.Value["reply"].(map[string]interface{})
	if !ok {
		s.logger.Error("invalid reply structure in existing comment",
			"uri", req.URI)
		return nil, fmt.Errorf("invalid existing comment structure")
	}

	// Parse reply refs
	var reply ReplyRef
	replyJSON, err := json.Marshal(replyData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal reply data: %w", err)
	}
	if err := json.Unmarshal(replyJSON, &reply); err != nil {
		return nil, fmt.Errorf("failed to unmarshal reply data: %w", err)
	}

	// Extract original createdAt timestamp (immutable)
	createdAt, _ := existingRecord.Value["createdAt"].(string)
	if createdAt == "" {
		createdAt = time.Now().UTC().Format(time.RFC3339)
	}

	// Build updated comment record
	updatedRecord := CommentRecord{
		Type:      commentCollection,
		Reply:     reply, // Preserve original reply refs
		Content:   content,
		Facets:    req.Facets,
		Embed:     req.Embed,
		Langs:     req.Langs,
		Labels:    req.Labels,
		CreatedAt: createdAt, // Preserve original timestamp
	}

	// Update the record on PDS with optimistic locking via swapRecord CID
	uri, cid, err := pdsClient.PutRecord(ctx, commentCollection, rkey, updatedRecord, existingRecord.CID)
	if err != nil {
		s.logger.Error("failed to update comment on PDS",
			"error", err,
			"uri", req.URI,
			"rkey", rkey)
		if pds.IsAuthError(err) {
			return nil, ErrNotAuthorized
		}
		if errors.Is(err, pds.ErrConflict) {
			return nil, ErrConcurrentModification
		}
		return nil, fmt.Errorf("failed to update comment: %w", err)
	}

	s.logger.Info("comment updated",
		"commenter", session.AccountDID,
		"uri", uri,
		"new_cid", cid,
		"old_cid", existingRecord.CID)

	return &UpdateCommentResponse{
		URI: uri,
		CID: cid,
	}, nil
}

// DeleteComment soft-deletes a comment by removing it from the user's PDS
func (s *commentService) DeleteComment(ctx context.Context, session *oauth.ClientSessionData, req DeleteCommentRequest) error {
	// Validate URI format
	if req.URI == "" {
		return ErrCommentNotFound
	}
	if !strings.HasPrefix(req.URI, "at://") {
		return ErrCommentNotFound
	}

	// Extract DID and rkey from URI (at://did/collection/rkey)
	parts := strings.Split(req.URI, "/")
	if len(parts) < 5 || parts[3] != commentCollection {
		return ErrCommentNotFound
	}
	did := parts[2]
	rkey := parts[4]

	// Verify ownership: URI must belong to the authenticated user
	if did != session.AccountDID.String() {
		return ErrNotAuthorized
	}

	// Create PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		s.logger.Error("failed to create PDS client",
			"error", err,
			"commenter", session.AccountDID)
		return fmt.Errorf("failed to create PDS client: %w", err)
	}

	// Verify comment exists on PDS before deleting
	_, err = pdsClient.GetRecord(ctx, commentCollection, rkey)
	if err != nil {
		s.logger.Error("failed to verify comment exists on PDS",
			"error", err,
			"uri", req.URI,
			"rkey", rkey)
		if pds.IsAuthError(err) {
			return ErrNotAuthorized
		}
		if errors.Is(err, pds.ErrNotFound) {
			return ErrCommentNotFound
		}
		return fmt.Errorf("failed to verify comment: %w", err)
	}

	// Delete the comment record from user's PDS
	if err := pdsClient.DeleteRecord(ctx, commentCollection, rkey); err != nil {
		s.logger.Error("failed to delete comment on PDS",
			"error", err,
			"uri", req.URI,
			"rkey", rkey)
		if pds.IsAuthError(err) {
			return ErrNotAuthorized
		}
		return fmt.Errorf("failed to delete comment: %w", err)
	}

	s.logger.Info("comment deleted",
		"commenter", session.AccountDID,
		"uri", req.URI)

	return nil
}

// validateReplyRef validates that reply references are well-formed
func validateReplyRef(reply ReplyRef) error {
	// Validate root reference
	if reply.Root.URI == "" {
		return ErrInvalidReply
	}
	if !strings.HasPrefix(reply.Root.URI, "at://") {
		return ErrInvalidReply
	}
	if reply.Root.CID == "" {
		return ErrInvalidReply
	}

	// Validate parent reference
	if reply.Parent.URI == "" {
		return ErrInvalidReply
	}
	if !strings.HasPrefix(reply.Parent.URI, "at://") {
		return ErrInvalidReply
	}
	if reply.Parent.CID == "" {
		return ErrInvalidReply
	}

	return nil
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
		// DisplayName, Avatar, Reputation will be populated when user profile schema is extended
		// Currently User model only has DID, Handle, PDSURL fields
		DisplayName: nil,
		Avatar:      nil,
		Reputation:  nil,
	}

	// Build community reference - fetch community to get name and avatar (required by lexicon)
	// The lexicon marks communityRef.name and handle as required, so DIDs alone are insufficient
	// DATA INTEGRITY: Community should always exist for posts. If missing, it indicates orphaned data.
	community, err := s.communityRepo.GetByDID(ctx, post.CommunityDID)
	if err != nil {
		// This indicates a data integrity issue: post references non-existent community
		// Log as ERROR (not warning) since this should never happen in normal operation
		log.Printf("ERROR: Data integrity issue - post %s references non-existent community %s: %v",
			post.URI, post.CommunityDID, err)
		// Use DID as fallback for both handle and name to prevent breaking the API
		// This allows the response to be returned while surfacing the integrity issue in logs
		community = &communities.Community{
			DID:    post.CommunityDID,
			Handle: post.CommunityDID, // Fallback: use DID as handle
			Name:   post.CommunityDID, // Fallback: use DID as name
		}
	}

	// Capture handle for communityRef (required by lexicon)
	communityHandle := community.Handle

	// Determine display name: prefer DisplayName, fall back to Name, then Handle
	var communityName string
	if community.DisplayName != "" {
		communityName = community.DisplayName
	} else if community.Name != "" {
		communityName = community.Name
	} else {
		communityName = community.Handle
	}

	// Build avatar URL from CID if available
	// Avatar is stored as blob in community's repository
	// Format: https://{pds}/xrpc/com.atproto.sync.getBlob?did={community_did}&cid={avatar_cid}
	var avatarURL *string
	if community.AvatarCID != "" && community.PDSURL != "" {
		// Validate HTTPS for security (prevent mixed content warnings, MitM attacks)
		if !strings.HasPrefix(community.PDSURL, "https://") {
			log.Printf("Warning: Skipping non-HTTPS PDS URL for community %s", community.DID)
		} else if !strings.HasPrefix(community.AvatarCID, "baf") {
			// Validate CID format (IPFS CIDs start with "baf" for CIDv1 base32)
			log.Printf("Warning: Invalid CID format for community %s", community.DID)
		} else {
			// Use proper URL escaping to prevent injection attacks
			avatarURLString := fmt.Sprintf("%s/xrpc/com.atproto.sync.getBlob?did=%s&cid=%s",
				strings.TrimSuffix(community.PDSURL, "/"),
				url.QueryEscape(community.DID),
				url.QueryEscape(community.AvatarCID))
			avatarURL = &avatarURLString
		}
	}

	communityRef := &posts.CommunityRef{
		DID:    post.CommunityDID,
		Handle: communityHandle,
		Name:   communityName,
		Avatar: avatarURL,
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

// GetActorComments retrieves comments by a user for their profile page
// Supports optional community filtering and cursor-based pagination
// Algorithm:
// 1. Validate and normalize request parameters (limit bounds)
// 2. Resolve community identifier to DID if provided
// 3. Fetch comments from repository with cursor-based pagination
// 4. Build CommentView for each comment with author info and stats
// 5. Return response with pagination cursor
func (s *commentService) GetActorComments(ctx context.Context, req *GetActorCommentsRequest) (*GetActorCommentsResponse, error) {
	// 1. Validate and normalize request
	if err := validateGetActorCommentsRequest(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// Add timeout to prevent runaway queries
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 2. Resolve community identifier to DID if provided
	var communityDID *string
	if req.Community != "" {
		// Check if it's already a DID
		if strings.HasPrefix(req.Community, "did:") {
			communityDID = &req.Community
		} else {
			// It's a handle - resolve to DID via community repository
			community, err := s.communityRepo.GetByHandle(ctx, req.Community)
			if err != nil {
				// If community not found, return empty results rather than error
				// This matches behavior of other endpoints
				if errors.Is(err, communities.ErrCommunityNotFound) {
					return &GetActorCommentsResponse{
						Comments: []*CommentView{},
						Cursor:   nil,
					}, nil
				}
				return nil, fmt.Errorf("failed to resolve community: %w", err)
			}
			communityDID = &community.DID
		}
	}

	// 3. Fetch comments from repository
	repoReq := ListByCommenterRequest{
		CommenterDID: req.ActorDID,
		CommunityDID: communityDID,
		Limit:        req.Limit,
		Cursor:       req.Cursor,
	}

	dbComments, nextCursor, err := s.commentRepo.ListByCommenterWithCursor(ctx, repoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch comments: %w", err)
	}

	// 4. Build CommentViews for each comment
	// Batch fetch vote states if viewer is authenticated
	var voteStates map[string]interface{}
	if req.ViewerDID != nil && len(dbComments) > 0 {
		commentURIs := make([]string, 0, len(dbComments))
		for _, comment := range dbComments {
			commentURIs = append(commentURIs, comment.URI)
		}

		var err error
		voteStates, err = s.commentRepo.GetVoteStateForComments(ctx, *req.ViewerDID, commentURIs)
		if err != nil {
			// Log error but don't fail the request - vote state is optional
			log.Printf("Warning: Failed to fetch vote states for actor comments: %v", err)
		}
	}

	// Batch fetch user data for comment authors (should all be the same user, but handle consistently)
	usersByDID := make(map[string]*users.User)
	if len(dbComments) > 0 {
		// For actor comments, all comments are by the same user
		// But we still use the batch pattern for consistency with other methods
		user, err := s.userRepo.GetByDID(ctx, req.ActorDID)
		if err != nil {
			// Log error but don't fail request - user data is optional
			log.Printf("Warning: Failed to fetch user for actor %s: %v", req.ActorDID, err)
		} else if user != nil {
			usersByDID[user.DID] = user
		}
	}

	// Build comment views
	commentViews := make([]*CommentView, 0, len(dbComments))
	for _, comment := range dbComments {
		commentView := s.buildCommentView(comment, req.ViewerDID, voteStates, usersByDID)
		commentViews = append(commentViews, commentView)
	}

	// 5. Return response with comments and cursor
	return &GetActorCommentsResponse{
		Comments: commentViews,
		Cursor:   nextCursor,
	}, nil
}

// validateGetActorCommentsRequest validates and normalizes request parameters
// Applies default values and enforces bounds per API specification
func validateGetActorCommentsRequest(req *GetActorCommentsRequest) error {
	if req == nil {
		return errors.New("request cannot be nil")
	}

	// ActorDID is required
	if req.ActorDID == "" {
		return errors.New("actor DID is required")
	}

	// Validate DID format
	if !strings.HasPrefix(req.ActorDID, "did:") {
		return errors.New("invalid actor DID format")
	}

	// Apply limit defaults and bounds (1-100, default 50)
	if req.Limit <= 0 {
		req.Limit = 50
	}
	if req.Limit > 100 {
		req.Limit = 100
	}

	return nil
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
