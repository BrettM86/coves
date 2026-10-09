package common

import (
	"log"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/votes"
)

// PopulateCommentViewerVoteState overlays cached votes on indexed comment views.
// Indexed state is preserved when the cache is unavailable; confirmed absence
// clears both the selected direction and the old vote record URI. Other viewer
// fields are preserved; a viewer state left empty is omitted.
func PopulateCommentViewerVoteState(r *http.Request, voteService votes.Service, views []*comments.CommentView) {
	if voteService == nil || len(views) == 0 {
		return
	}
	session := middleware.GetOAuthSession(r)
	viewerDID := middleware.GetUserDID(r)
	if session == nil || viewerDID == "" {
		return
	}

	subjects := make([]string, 0, len(views))
	for _, view := range views {
		if view != nil {
			subjects = append(subjects, view.URI)
		}
	}
	if len(subjects) == 0 {
		return
	}
	if err := voteService.EnsureCachePopulated(r.Context(), session); err != nil {
		log.Printf("Warning: failed to populate comment vote cache: %v", err)
		return
	}
	viewerVotes := voteService.GetViewerVotesForSubjects(viewerDID, subjects)
	if viewerVotes == nil {
		return
	}
	for _, view := range views {
		if view == nil {
			continue
		}
		vote := viewerVotes[view.URI]
		if vote == nil {
			if view.Viewer != nil {
				view.Viewer.Vote = nil
				view.Viewer.VoteURI = nil
				if view.Viewer.IsEmpty() {
					view.Viewer = nil
				}
			}
			continue
		}
		if view.Viewer == nil {
			view.Viewer = &comments.CommentViewerState{}
		}
		view.Viewer.Vote = &vote.Direction
		view.Viewer.VoteURI = &vote.URI
	}
}
