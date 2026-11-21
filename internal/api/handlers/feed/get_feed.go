// Package feed serves social.coves.feed.getFeed.
package feed

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"Coves/internal/api/handlers/common"
	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/communities"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"
)

const (
	// defaultLimit is the page size when limit is absent or not a number.
	defaultLimit = 15
	// maxCommunityLength is the lexicon maxLength of the community parameter.
	maxCommunityLength = 320
)

// CommunityResolver resolves a community handle or DID to its DID.
type CommunityResolver interface {
	ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error)
}

// GetFeedHandler serves GET /xrpc/social.coves.feed.getFeed.
type GetFeedHandler struct {
	service        feedgen.Service
	resolver       CommunityResolver
	voteService    votes.Service
	blueskyService blueskypost.Service
	authority      moderation.Authority
	instanceDID    string
	communities    moderation.CommunityViewer
}

// NewGetFeedHandler creates a getFeed handler.
func NewGetFeedHandler(service feedgen.Service, resolver CommunityResolver, voteService votes.Service, blueskyService blueskypost.Service) *GetFeedHandler {
	if blueskyService == nil {
		log.Printf("[FEED-HANDLER] WARNING: blueskyService is nil - Bluesky post embeds will not be resolved")
	}
	return &GetFeedHandler{
		service:        service,
		resolver:       resolver,
		voteService:    voteService,
		blueskyService: blueskyService,
	}
}

// WithViewerModeration makes the handler report, in viewer.moderation, the
// moderation actions an instance admin viewer may take on each post.
func (h *GetFeedHandler) WithViewerModeration(authority moderation.Authority, instanceDID string) *GetFeedHandler {
	if authority == nil || instanceDID == "" {
		panic("feed.GetFeedHandler.WithViewerModeration: authority and instanceDID are required")
	}
	h.authority = authority
	h.instanceDID = instanceDID
	return h
}

// WithCommunityViewerModeration makes the handler report, in
// viewer.communityModeration, the community moderation actions the viewer may
// take on each post.
func (h *GetFeedHandler) WithCommunityViewerModeration(viewer moderation.CommunityViewer) *GetFeedHandler {
	if viewer == nil {
		panic("feed.GetFeedHandler.WithCommunityViewerModeration: viewer is required")
	}
	h.communities = viewer
	return h
}

// HandleGetFeed serves one page of a feed in a scope.
// GET /xrpc/social.coves.feed.getFeed?scope=home|community&community=...&feed=...&limit=15&cursor=...&timeframe=day
func (h *GetFeedHandler) HandleGetFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	scopeName := query.Get("scope")
	if scopeName == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "scope is required")
		return
	}

	viewerDID := requestViewerDID(r)
	if scopeName == feedgen.ScopeHome && viewerDID == "" {
		writeAuthenticationRequired(w)
		return
	}

	scope := feedgen.Scope{Name: scopeName}
	if scopeName == feedgen.ScopeCommunity {
		identifier := query.Get("community")
		if len(identifier) > maxCommunityLength {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "community is too long")
			return
		}
		communityDID, ok := h.resolveCommunity(w, r, identifier)
		if !ok {
			return
		}
		scope.CommunityDID = communityDID
	}

	h.serve(w, r, feedgen.GetFeedRequest{
		ViewerDID: viewerDID,
		Scope:     scope,
		Feed:      query.Get("feed"),
		Limit:     parseLimit(query),
		Cursor:    query.Get("cursor"),
		Timeframe: query.Get("timeframe"),
	})
}

// serve runs one getFeed request and writes the page with the viewer's vote
// state, moderation actions and resolved embeds.
func (h *GetFeedHandler) serve(w http.ResponseWriter, r *http.Request, req feedgen.GetFeedRequest) {
	// The cursor is not length-checked: the server signs it, and an envelope
	// around a long post URI may exceed the lexicon maxLength yet must page.
	response, err := h.service.GetFeed(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	common.PopulateViewerVoteState(r.Context(), r, h.voteService, response.Feed)
	common.PopulateViewerModeration(r, h.authority, h.instanceDID, response.Feed)
	common.PopulateCommunityViewerModeration(r, h.communities, response.Feed)
	for _, item := range response.Feed {
		if item.Post != nil {
			posts.TransformBlobRefsToURLs(item.Post)
			posts.TransformPostEmbeds(r.Context(), item.Post, h.blueskyService)
		}
	}

	xrpc.WriteJSON(w, http.StatusOK, response)
}

// requestViewerDID is the signed-in viewer's DID, or "" when the request has
// no DID viewer.
func requestViewerDID(r *http.Request) string {
	viewerDID := middleware.GetUserDID(r)
	if !strings.HasPrefix(viewerDID, "did:") {
		return ""
	}
	return viewerDID
}

// writeAuthenticationRequired answers a home-feed request with no DID viewer.
func writeAuthenticationRequired(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, "AuthenticationRequired", "User must be authenticated to view the home feed")
}

// parseLimit is the limit parameter; absent or not a number is defaultLimit.
func parseLimit(query url.Values) int {
	if parsed, err := strconv.Atoi(query.Get("limit")); err == nil {
		return parsed
	}
	return defaultLimit
}

// resolveCommunity resolves the community parameter of a community-scope
// request. On failure it writes the error response and returns false.
func (h *GetFeedHandler) resolveCommunity(w http.ResponseWriter, r *http.Request, identifier string) (string, bool) {
	if identifier == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required for scope community")
		return "", false
	}
	communityDID, err := h.resolver.ResolveCommunityIdentifier(r.Context(), identifier)
	if err != nil {
		switch {
		case communities.IsNotFound(err):
			writeError(w, http.StatusNotFound, "CommunityNotFound", "Community not found")
		case communities.IsValidationError(err):
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid community identifier")
		case errors.Is(err, communities.ErrAmbiguousCommunity):
			// The shared mapper's 409 AmbiguousCommunity, as getCommunity answers.
			handleServiceError(w, err)
		default:
			log.Printf("ERROR: feed failed to resolve community identifier: %v", err)
			writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to resolve community")
		}
		return "", false
	}
	return communityDID, true
}
