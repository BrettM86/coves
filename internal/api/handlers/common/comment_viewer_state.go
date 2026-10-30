package common

import (
	"log"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/votes"
)

// PopulateCommentViewerVoteState resolves the viewer's votes on every comment
// view in one ViewerVotes call. A comment the viewer voted on gets vote and
// voteUri; one without a vote gets neither. Other viewer fields are preserved;
// a viewer state left empty is omitted. If the lookup fails, the error is
// logged and no comment gets vote state: a failed lookup must not render as
// "not voted".
func PopulateCommentViewerVoteState(r *http.Request, voteService votes.Service, views []*comments.CommentView) {
	if voteService == nil || len(views) == 0 {
		return
	}
	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
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

	viewerVotes, err := voteService.ViewerVotes(r.Context(), viewerDID, subjects)
	if err != nil {
		log.Printf("Warning: failed to resolve comment viewer votes for user %s (%d comments): %v",
			viewerDID, len(subjects), err)
		viewerVotes = nil
	}
	for _, view := range views {
		if view == nil {
			continue
		}
		if vote, exists := viewerVotes[view.URI]; exists {
			if view.Viewer == nil {
				view.Viewer = &comments.CommentViewerState{}
			}
			view.Viewer.Vote = &vote.Direction
			view.Viewer.VoteURI = &vote.URI
			continue
		}
		if view.Viewer != nil && view.Viewer.IsEmpty() {
			view.Viewer = nil
		}
	}
}
