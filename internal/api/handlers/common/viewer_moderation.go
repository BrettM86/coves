package common

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
)

// viewerIsInstanceAdmin reports whether the request's authenticated viewer is
// an instance admin. Signed-out viewers and a nil authority are never admins.
func viewerIsInstanceAdmin(r *http.Request, authority moderation.Authority) bool {
	if authority == nil {
		return false
	}
	viewerDID := middleware.GetUserDID(r)
	return viewerDID != "" && authority.IsInstanceAdmin(viewerDID)
}

// PopulateViewerModeration sets viewer.moderation on feed posts to the
// moderation actions the authenticated viewer may take. Other viewer fields
// are preserved. Posts get no viewer.moderation when the viewer is signed out
// or not an instance admin.
func PopulateViewerModeration[T FeedPostProvider](r *http.Request, authority moderation.Authority, instanceDID string, feedPosts []T) {
	if !viewerIsInstanceAdmin(r, authority) {
		return
	}
	for _, feedPost := range feedPosts {
		post := feedPost.GetPost()
		if post == nil {
			continue
		}
		if actions := moderation.PostViewerActions(instanceDID, post.Moderation); len(actions) > 0 {
			if post.Viewer == nil {
				post.Viewer = &posts.ViewerState{}
			}
			post.Viewer.Moderation = actions
		}
	}
}

// PopulateModeratedPostViewerModeration sets viewer.moderation on removal
// tombstones (post.get's moderatedPost union member) with the same rules and
// viewer gate as PopulateViewerModeration.
func PopulateModeratedPostViewerModeration(r *http.Request, authority moderation.Authority, instanceDID string, tombstones []*posts.ModeratedPost) {
	if !viewerIsInstanceAdmin(r, authority) {
		return
	}
	for _, tombstone := range tombstones {
		if tombstone == nil {
			continue
		}
		if actions := moderation.PostViewerActions(instanceDID, tombstone.Moderation); len(actions) > 0 {
			tombstone.Viewer = &posts.ModeratedPostViewerState{Moderation: actions}
		}
	}
}

// PopulateCommentViewerModeration sets viewer.moderation on comment views to
// the moderation actions the authenticated viewer may take. Callers holding a
// reply tree pass every view in it. Other viewer fields are preserved. Comments
// get no viewer.moderation when the viewer is signed out or not an instance
// admin.
func PopulateCommentViewerModeration(r *http.Request, authority moderation.Authority, instanceDID string, views []*comments.CommentView) {
	if !viewerIsInstanceAdmin(r, authority) {
		return
	}
	for _, view := range views {
		actions := moderation.CommentViewerActions(instanceDID, view)
		if len(actions) == 0 {
			continue
		}
		if view.Viewer == nil {
			view.Viewer = &comments.CommentViewerState{}
		}
		view.Viewer.Moderation = actions
	}
}
