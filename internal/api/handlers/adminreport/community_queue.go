package adminreport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/adminreports"
	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
)

// CommunityQueueService serves a community's report queue.
// *adminreports.CommunityQueueService satisfies it.
type CommunityQueueService interface {
	ListQueue(ctx context.Context, req adminreports.ListCommunityQueueRequest) (adminreports.CommunityQueuePage, error)
	Dismiss(ctx context.Context, req adminreports.DismissCommunityReportsRequest) (adminreports.DismissResult, error)
}

// ReportSubjectViewer hydrates reported subjects keyed by URI: a post as its
// GetPosts result (its view, or the state that keeps the viewer from seeing
// it), a comment as its view, or in blocked when the viewer blocked its
// author. A URI with nothing to serve is absent.
type ReportSubjectViewer interface {
	PostResults(ctx context.Context, viewerDID string, uris []string) (map[string]*posts.PostResult, error)
	CommentViews(ctx context.Context, viewerDID string, uris []string) (views map[string]*comments.CommentView, blocked map[string]bool, err error)
}

// ReportPostGetter batch-fetches post views. posts.Service satisfies it.
type ReportPostGetter interface {
	GetPosts(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error)
}

// ReportCommentGetter batch-fetches comment views.
type ReportCommentGetter interface {
	GetCommentViewsByURIs(ctx context.Context, uris []string, viewerDID string) (views map[string]*comments.CommentView, blocked map[string]bool, err error)
}

type reportSubjectViewer struct {
	posts    ReportPostGetter
	comments ReportCommentGetter
}

// NewReportSubjectViewer builds a ReportSubjectViewer over the post and
// comment services.
func NewReportSubjectViewer(postService ReportPostGetter, commentService ReportCommentGetter) ReportSubjectViewer {
	return &reportSubjectViewer{posts: postService, comments: commentService}
}

// maxPostsPerGetPosts is the most URIs one posts.GetPosts call accepts.
const maxPostsPerGetPosts = 25

// PostResults hydrates the post URIs in batches GetPosts accepts, keeping
// each URI's result whether it is a view or a content-free state.
func (v *reportSubjectViewer) PostResults(ctx context.Context, viewerDID string, uris []string) (map[string]*posts.PostResult, error) {
	byURI := make(map[string]*posts.PostResult, len(uris))
	for start := 0; start < len(uris); start += maxPostsPerGetPosts {
		end := min(start+maxPostsPerGetPosts, len(uris))
		results, err := v.posts.GetPosts(ctx, posts.GetPostsRequest{URIs: uris[start:end], ViewerDID: viewerDID})
		if err != nil {
			return nil, fmt.Errorf("hydrating reported posts: %w", err)
		}
		if len(results) != end-start {
			return nil, fmt.Errorf("hydrating reported posts: %d results for %d posts", len(results), end-start)
		}
		for i, result := range results {
			if result != nil {
				byURI[uris[start+i]] = result
			}
		}
	}
	return byURI, nil
}

// CommentViews hydrates the comment URIs; a URI with no indexed comment is
// absent, and one whose author the viewer blocked is in blocked instead.
func (v *reportSubjectViewer) CommentViews(ctx context.Context, viewerDID string, uris []string) (map[string]*comments.CommentView, map[string]bool, error) {
	if len(uris) == 0 {
		return map[string]*comments.CommentView{}, map[string]bool{}, nil
	}
	views, blocked, err := v.comments.GetCommentViewsByURIs(ctx, uris, viewerDID)
	if err != nil {
		return nil, nil, fmt.Errorf("hydrating reported comments: %w", err)
	}
	return views, blocked, nil
}

// ListCommunityReportsHandler serves social.coves.community.listReports.
type ListCommunityReportsHandler struct {
	service CommunityQueueService
	viewer  ReportSubjectViewer
}

// NewListCommunityReportsHandler builds the listReports handler.
func NewListCommunityReportsHandler(service CommunityQueueService, viewer ReportSubjectViewer) *ListCommunityReportsHandler {
	return &ListCommunityReportsHandler{service: service, viewer: viewer}
}

type reportQueueEntryOutput struct {
	Subject         string                `json:"subject"`
	SubjectType     string                `json:"subjectType"`
	Post            *posts.PostView       `json:"post,omitempty"`
	Comment         *comments.CommentView `json:"comment,omitempty"`
	PostState       string                `json:"postState,omitempty"`
	CommentState    string                `json:"commentState,omitempty"`
	Reasons         []string              `json:"reasons"`
	ReportCount     int                   `json:"reportCount"`
	FirstReportedAt string                `json:"firstReportedAt"`
	LastReportedAt  string                `json:"lastReportedAt"`
}

type listReportsOutput struct {
	Reports []reportQueueEntryOutput `json:"reports"`
	Cursor  string                   `json:"cursor,omitempty"`
}

// HandleListReports answers the listReports query for the authenticated
// caller. Each entry carries the subject's current view when it hydrates and
// never names a reporter.
func (h *ListCommunityReportsHandler) HandleListReports(w http.ResponseWriter, r *http.Request) {
	callerDID := middleware.GetUserDID(r)
	if callerDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	query := r.URL.Query()
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "limit must be an integer")
			return
		}
		limit = parsed
	}
	page, err := h.service.ListQueue(r.Context(), adminreports.ListCommunityQueueRequest{
		CommunityDID: query.Get("community"),
		CallerDID:    callerDID,
		Limit:        limit,
		Cursor:       query.Get("cursor"),
	})
	if err != nil {
		writeCommunityQueueError(w, err)
		return
	}

	var postURIs, commentURIs []string
	for _, entry := range page.Entries {
		switch entry.SubjectType {
		case adminreports.TargetTypePost:
			postURIs = append(postURIs, entry.SubjectURI)
		case adminreports.TargetTypeComment:
			commentURIs = append(commentURIs, entry.SubjectURI)
		}
	}
	postResults := map[string]*posts.PostResult{}
	if len(postURIs) > 0 {
		if postResults, err = h.viewer.PostResults(r.Context(), callerDID, postURIs); err != nil {
			writeCommunityQueueError(w, err)
			return
		}
	}
	commentViews := map[string]*comments.CommentView{}
	commentBlocked := map[string]bool{}
	if len(commentURIs) > 0 {
		if commentViews, commentBlocked, err = h.viewer.CommentViews(r.Context(), callerDID, commentURIs); err != nil {
			writeCommunityQueueError(w, err)
			return
		}
	}

	output := listReportsOutput{Reports: make([]reportQueueEntryOutput, 0, len(page.Entries)), Cursor: page.Cursor}
	for _, entry := range page.Entries {
		reasons := make([]string, 0, len(entry.Reasons))
		for _, reason := range entry.Reasons {
			reasons = append(reasons, string(reason))
		}
		item := reportQueueEntryOutput{
			Subject:         entry.SubjectURI,
			SubjectType:     string(entry.SubjectType),
			Reasons:         reasons,
			ReportCount:     entry.ReportCount,
			FirstReportedAt: entry.FirstReportedAt.UTC().Format(time.RFC3339Nano),
			LastReportedAt:  entry.LastReportedAt.UTC().Format(time.RFC3339Nano),
		}
		switch entry.SubjectType {
		case adminreports.TargetTypePost:
			item.Post, item.PostState = servedPost(postResults[entry.SubjectURI])
		case adminreports.TargetTypeComment:
			if commentBlocked[entry.SubjectURI] {
				item.CommentState = reportCommentStateBlocked
			} else {
				item.Comment = commentViews[entry.SubjectURI]
			}
		}
		output.Reports = append(output.Reports, item)
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

// Post states an entry serves for a reported post the caller's view cannot
// show (listReports#reportQueueEntry postState).
const (
	reportPostStateBlocked   = "blocked"
	reportPostStateRemoved   = "removed"
	reportPostStateModerated = "moderated"
	reportPostStateNotFound  = "notFound"
)

// reportCommentStateBlocked is the state an entry serves, in place of the
// comment, for a reported comment whose author the caller blocked
// (listReports#reportQueueEntry commentState).
const reportCommentStateBlocked = "blocked"

// servedPost is what an entry serves for a reported post's GetPosts result:
// its view, or, when the caller cannot see it, the state that hides it. A
// post with no result is served as not found.
func servedPost(result *posts.PostResult) (*posts.PostView, string) {
	switch {
	case result == nil:
		return nil, reportPostStateNotFound
	case result.Post != nil:
		return result.Post, ""
	case result.Blocked != nil:
		return nil, reportPostStateBlocked
	case result.Removed != nil:
		return nil, reportPostStateRemoved
	case result.Moderated != nil:
		return nil, reportPostStateModerated
	default:
		return nil, reportPostStateNotFound
	}
}

// DismissCommunityReportsHandler serves social.coves.community.dismissReports.
type DismissCommunityReportsHandler struct {
	service CommunityQueueService
}

// NewDismissCommunityReportsHandler builds the dismissReports handler.
func NewDismissCommunityReportsHandler(service CommunityQueueService) *DismissCommunityReportsHandler {
	return &DismissCommunityReportsHandler{service: service}
}

type dismissReportsInput struct {
	Community      string `json:"community"`
	Subject        string `json:"subject"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type dismissReportsOutput struct {
	DismissedCount int `json:"dismissedCount"`
}

// HandleDismissReports answers the dismissReports procedure as the
// authenticated caller.
func (h *DismissCommunityReportsHandler) HandleDismissReports(w http.ResponseWriter, r *http.Request) {
	callerDID := middleware.GetUserDID(r)
	if callerDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	var input dismissReportsInput
	if !xrpc.DecodeJSON(w, r, reqbody.LimitSmall, &input) {
		return
	}
	result, err := h.service.Dismiss(r.Context(), adminreports.DismissCommunityReportsRequest{
		CommunityDID:   input.Community,
		CallerDID:      callerDID,
		SubjectURI:     input.Subject,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		writeCommunityQueueError(w, err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, dismissReportsOutput{DismissedCount: result.DismissedCount})
}

func writeCommunityQueueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, adminreports.ErrCommunityQueueForbidden):
		xrpc.WriteError(w, http.StatusForbidden, "Forbidden", "Not a moderator of the community, or the community is not hosted here or is removed")
	case errors.Is(err, adminreports.ErrInvalidQueueRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid community, subject, idempotencyKey, limit or cursor")
	case errors.Is(err, adminreports.ErrIdempotencyConflict):
		xrpc.WriteError(w, http.StatusConflict, "IdempotencyConflict", "The idempotency key was already used for a different subject")
	case errors.Is(err, context.Canceled):
		xrpc.WriteError(w, http.StatusBadRequest, "RequestCanceled", "Request was canceled")
	case errors.Is(err, context.DeadlineExceeded):
		slog.Error("community report queue timed out", "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "The report queue is temporarily unavailable")
	default:
		slog.Error("unexpected community report queue failure", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
