package feed

import (
	"net/http"
	"net/url"

	"Coves/internal/core/feedgen"
)

// FeedAliasHandler serves the old feed endpoints as aliases of getFeed: each
// maps sort to a built-in feed and runs the getFeed path.
type FeedAliasHandler struct {
	getFeed  *GetFeedHandler
	registry *feedgen.Registry
}

// NewFeedAliasHandler returns the alias handler over getFeed.
func NewFeedAliasHandler(getFeed *GetFeedHandler, registry *feedgen.Registry) *FeedAliasHandler {
	return &FeedAliasHandler{getFeed: getFeed, registry: registry}
}

// homeAndCommunitySorts maps getTimeline and getCommunity sorts to built-in
// rkeys.
var homeAndCommunitySorts = map[string]string{"hot": "hot", "new": "new", "top": "top"}

// discoverSorts maps getDiscover sorts to built-in rkeys; scope all ranks Hot
// with discover-hot.
var discoverSorts = map[string]string{"hot": "discover-hot", "new": "new", "top": "top"}

// HandleGetTimeline serves social.coves.feed.getTimeline: the home feed.
func (h *FeedAliasHandler) HandleGetTimeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	viewerDID := requestViewerDID(r)
	if viewerDID == "" {
		writeAuthenticationRequired(w)
		return
	}
	query := r.URL.Query()
	feedURI, ok := h.feedURI(w, query, homeAndCommunitySorts)
	if !ok {
		return
	}
	h.getFeed.serve(w, r, aliasRequest(query, viewerDID, feedgen.Scope{Name: feedgen.ScopeHome}, feedURI))
}

// HandleGetDiscover serves social.coves.feed.getDiscover: the all scope.
func (h *FeedAliasHandler) HandleGetDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	feedURI, ok := h.feedURI(w, query, discoverSorts)
	if !ok {
		return
	}
	h.getFeed.serve(w, r, aliasRequest(query, requestViewerDID(r), feedgen.Scope{Name: feedgen.ScopeAll}, feedURI))
}

// HandleGetCommunity serves social.coves.communityFeed.getCommunity: one
// community's feed. As before it became an alias, sort, limit and timeframe
// are validated before the community is resolved, and the community parameter
// has no length cap.
func (h *FeedAliasHandler) HandleGetCommunity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	feedURI, ok := h.feedURI(w, query, homeAndCommunitySorts)
	if !ok {
		return
	}
	if err := feedgen.ValidatePageParameters(parseLimit(query), query.Get("timeframe")); err != nil {
		handleServiceError(w, err)
		return
	}
	communityDID, ok := h.getFeed.resolveCommunity(w, r, query.Get("community"))
	if !ok {
		return
	}
	scope := feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: communityDID}
	h.getFeed.serve(w, r, aliasRequest(query, requestViewerDID(r), scope, feedURI))
}

// feedURI maps the sort parameter (absent is hot) to its built-in feed URI. On
// an unknown sort it writes 400 InvalidRequest and returns false.
func (h *FeedAliasHandler) feedURI(w http.ResponseWriter, query url.Values, sorts map[string]string) (string, bool) {
	sort := query.Get("sort")
	if sort == "" {
		sort = "hot"
	}
	rkey, ok := sorts[sort]
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "sort must be one of: hot, top, new")
		return "", false
	}
	return h.registry.URI(rkey), true
}

// aliasRequest is the getFeed request for an alias, with limit, cursor and
// timeframe passed through.
func aliasRequest(query url.Values, viewerDID string, scope feedgen.Scope, feedURI string) feedgen.GetFeedRequest {
	return feedgen.GetFeedRequest{
		ViewerDID: viewerDID,
		Scope:     scope,
		Feed:      feedURI,
		Limit:     parseLimit(query),
		Cursor:    query.Get("cursor"),
		Timeframe: query.Get("timeframe"),
	}
}
