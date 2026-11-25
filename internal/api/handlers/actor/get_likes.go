package actor

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"Coves/internal/api/handlers/common"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
)

// GetLikesHandler handles social.coves.actor.getLikes.
type GetLikesHandler struct {
	commentService comments.Service
	userService    users.UserService
	voteService    votes.Service
	blueskyService blueskypost.Service
	authority      moderation.Authority
	instanceDID    string
	communities    moderation.CommunityViewer
}

// NewGetLikesHandler creates a new actor likes handler.
func NewGetLikesHandler(
	commentService comments.Service,
	userService users.UserService,
	voteService votes.Service,
	blueskyService blueskypost.Service,
) *GetLikesHandler {
	return &GetLikesHandler{
		commentService: commentService,
		userService:    userService,
		voteService:    voteService,
		blueskyService: blueskyService,
	}
}

// WithViewerModeration makes the handler report, in viewer.moderation, the
// moderation actions an instance admin viewer may take on each post.
func (h *GetLikesHandler) WithViewerModeration(authority moderation.Authority, instanceDID string) *GetLikesHandler {
	if authority == nil || instanceDID == "" {
		panic("actor.GetLikesHandler.WithViewerModeration: authority and instanceDID are required")
	}
	h.authority = authority
	h.instanceDID = instanceDID
	return h
}

// WithCommunityViewerModeration makes the handler report, in
// viewer.communityModeration, the community moderation actions the viewer may
// take on each post.
func (h *GetLikesHandler) WithCommunityViewerModeration(viewer moderation.CommunityViewer) *GetLikesHandler {
	if viewer == nil {
		panic("actor.GetLikesHandler.WithCommunityViewerModeration: viewer is required")
	}
	h.communities = viewer
	return h
}

// HandleGetLikes lists the posts and comments the viewer upvoted, narrowed by
// the optional filter (all, posts or comments; default all). Only the actor themselves
// may list their likes: an anonymous request is 401 and a request for another
// actor is 403.
// GET /xrpc/social.coves.actor.getLikes?actor={did_or_handle}&filter=all&limit=50&cursor=...
func (h *GetLikesHandler) HandleGetLikes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthenticationRequired", "Authentication required")
		return
	}

	req, err := h.parseRequest(r)
	if err != nil {
		writeActorRequestError(w, err)
		return
	}
	if req.ActorDID != viewerDID {
		writeError(w, http.StatusForbidden, "NotAllowed", "Likes are visible only to their owner")
		return
	}
	req.ViewerDID = viewerDID

	response, err := h.commentService.GetActorLikes(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	common.PopulateViewerVoteState(r.Context(), r, h.voteService, response.Feed)
	common.PopulateViewerModeration(r, h.authority, h.instanceDID, response.Feed)
	common.PopulateCommunityViewerModeration(r, h.communities, response.Feed)

	commentViews := make([]*comments.CommentView, 0, len(response.Feed))
	// A post can back more than one item (a liked post and a liked comment in
	// its thread share one view), and the transforms rewrite the view in place.
	transformed := make(map[*posts.PostView]bool, len(response.Feed))
	items := make([]likedItemView, 0, len(response.Feed))
	for _, item := range response.Feed {
		if item.Post != nil && !transformed[item.Post] {
			transformed[item.Post] = true
			posts.TransformBlobRefsToURLs(item.Post)
			posts.TransformPostEmbeds(r.Context(), item.Post, h.blueskyService)
		}
		view := likedItemView{Type: likedPostType, Post: item.Post}
		if item.Comment != nil {
			commentViews = append(commentViews, item.Comment)
			view = likedItemView{Type: likedCommentType, Comment: item.Comment, Post: item.Post}
		}
		items = append(items, view)
	}
	common.PopulateCommentViewerVoteState(r, h.voteService, commentViews)
	common.PopulateCommentViewerModeration(r, h.authority, h.instanceDID, commentViews)
	common.PopulateCommentCommunityViewerModeration(r, h.communities, commentViews)

	// Pre-encode response to buffer before writing headers so an encoding
	// failure can still be reported as an error.
	responseBytes, err := json.Marshal(likesResponseView{Feed: items, Cursor: response.Cursor})
	if err != nil {
		log.Printf("ERROR: Failed to encode actor likes response: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to encode response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(responseBytes); err != nil {
		log.Printf("ERROR: Failed to write actor likes response: %v", err)
	}
}

// parseRequest parses query parameters into GetActorLikesRequest.
func (h *GetLikesHandler) parseRequest(r *http.Request) (*comments.GetActorLikesRequest, error) {
	req := &comments.GetActorLikesRequest{}

	actor := r.URL.Query().Get("actor")
	if actor == "" {
		return nil, posts.NewValidationError("actor", "actor parameter is required")
	}
	// Same bound as getPosts: DIDs and handles never approach this length.
	const maxActorLength = 2048
	if len(actor) > maxActorLength {
		return nil, posts.NewValidationError("actor", "actor parameter exceeds maximum length")
	}

	actorDID, err := resolveActorDID(r.Context(), h.userService, actor)
	if err != nil {
		return nil, err
	}
	req.ActorDID = actorDID

	switch filter := comments.LikesFilter(r.URL.Query().Get("filter")); filter {
	case "", comments.LikesFilterAll:
		req.Filter = comments.LikesFilterAll
	case comments.LikesFilterPosts, comments.LikesFilterComments:
		req.Filter = filter
	default:
		return nil, posts.NewValidationError("filter", "filter must be one of all, posts, comments")
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil {
			return nil, posts.NewValidationError("limit", "limit must be a valid integer")
		}
		req.Limit = limit
	}

	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		req.Cursor = &cursor
	}

	return req, nil
}

// The social.coves.actor.getLikes feed item union members.
const (
	likedPostType    = "social.coves.actor.getLikes#likedPost"
	likedCommentType = "social.coves.actor.getLikes#likedComment"
)

// likesResponseView is the social.coves.actor.getLikes output.
type likesResponseView struct {
	Feed   []likedItemView `json:"feed"`
	Cursor *string         `json:"cursor,omitempty"`
}

// likedItemView is one getLikes feed item: a #likedPost (Post only) or a
// #likedComment (Comment, with Post its thread's root post).
type likedItemView struct {
	Type    string                `json:"$type"`
	Comment *comments.CommentView `json:"comment,omitempty"`
	Post    *posts.PostView       `json:"post"`
}
