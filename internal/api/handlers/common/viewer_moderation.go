package common

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
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

// communityViewerReads memoizes one request's community viewer reads: whether
// the viewer may moderate anywhere, the viewer's authority by community DID,
// and each community's effective roster. A viewer who may moderate nowhere
// costs no per-community read. A failed read omits the affected lists (the
// lists are decoration; procedures still authorize) and is logged once at
// ERROR.
type communityViewerReads struct {
	ctx       context.Context
	reader    moderation.CommunityViewer
	viewerDID string
	// mayModerate is nil until read, and false when the read failed.
	mayModerate *bool
	authority   map[string]*moderation.CommunityViewerAuthority // nil entry: the read failed
	rosters     map[string]*communities.Roster                  // nil entry: the read failed
}

// newCommunityViewerReads returns the request's community viewer reads, or
// nil when there is no reader or the viewer is signed out: no community list
// is served then.
func newCommunityViewerReads(r *http.Request, reader moderation.CommunityViewer) *communityViewerReads {
	viewerDID := middleware.GetUserDID(r)
	if reader == nil || viewerDID == "" {
		return nil
	}
	return &communityViewerReads{
		ctx: r.Context(), reader: reader, viewerDID: viewerDID,
		authority: make(map[string]*moderation.CommunityViewerAuthority),
		rosters:   make(map[string]*communities.Roster),
	}
}

// viewerMayModerate reports, reading it once per request, whether the viewer
// may moderate in any community; false when the read failed.
func (c *communityViewerReads) viewerMayModerate() bool {
	if c.mayModerate != nil {
		return *c.mayModerate
	}
	mayModerate, err := c.reader.ViewerMayModerate(c.ctx, c.viewerDID)
	if err != nil {
		slog.Error("failed to read whether the viewer may moderate any community; omitting community moderation actions",
			"error", err)
		mayModerate = false
	}
	c.mayModerate = &mayModerate
	return mayModerate
}

// authorityIn is the viewer's community authority in communityDID, None when
// the community is unknown, the viewer may moderate nowhere, or a read failed.
func (c *communityViewerReads) authorityIn(communityDID string) moderation.CommunityViewerAuthority {
	if communityDID == "" || !c.viewerMayModerate() {
		return moderation.CommunityAuthorityNone
	}
	if cached, ok := c.authority[communityDID]; ok {
		if cached == nil {
			return moderation.CommunityAuthorityNone
		}
		return *cached
	}
	authority, err := c.reader.ViewerAuthority(c.ctx, communityDID, c.viewerDID)
	if err != nil {
		slog.Error("failed to read the viewer's community authority; omitting community moderation actions",
			"community", communityDID, "error", err)
		c.authority[communityDID] = nil
		return moderation.CommunityAuthorityNone
	}
	c.authority[communityDID] = &authority
	return authority
}

// authorHoldsRole reports whether authorDID holds a role in communityDID,
// answered from the community's roster read once per request; ok is false
// when the roster read failed, and the community's list is then omitted.
func (c *communityViewerReads) authorHoldsRole(communityDID, authorDID string) (holds, ok bool) {
	roster, seen := c.rosters[communityDID]
	if !seen {
		read, err := c.reader.Roster(c.ctx, communityDID)
		if err != nil {
			slog.Error("failed to read a community roster; omitting community moderation actions",
				"community", communityDID, "error", err)
		} else {
			roster = &read
		}
		c.rosters[communityDID] = roster
	}
	if roster == nil {
		return false, false
	}
	return moderation.RosterHoldsRole(*roster, authorDID), true
}

// postActions is the community list for a post in communityDID by authorDID
// with the given admission status. The author's role is read only when the
// viewer has authority there.
func (c *communityViewerReads) postActions(communityDID, authorDID, admissionStatus string) []string {
	authority := c.authorityIn(communityDID)
	if authority == moderation.CommunityAuthorityNone {
		return nil
	}
	holds, ok := c.authorHoldsRole(communityDID, authorDID)
	if !ok {
		return nil
	}
	return moderation.CommunityPostViewerActions(authority, admissionStatus, holds)
}

// PopulateCommunityViewerModeration sets viewer.communityModeration on feed
// posts to the community moderation actions the signed-in viewer may take in
// each post's community. It is independent of instance admin status, and other
// viewer fields are preserved. A nil reader serves no community list.
func PopulateCommunityViewerModeration[T FeedPostProvider](r *http.Request, reader moderation.CommunityViewer, feedPosts []T) {
	if reads := newCommunityViewerReads(r, reader); reads != nil {
		populateCommunityPostActions(reads, feedPosts)
	}
}

func populateCommunityPostActions[T FeedPostProvider](reads *communityViewerReads, feedPosts []T) {
	for _, feedPost := range feedPosts {
		post := feedPost.GetPost()
		if post == nil || post.Community == nil || post.Author == nil {
			continue
		}
		if actions := reads.postActions(post.Community.DID, post.Author.DID, post.Status); len(actions) > 0 {
			if post.Viewer == nil {
				post.Viewer = &posts.ViewerState{}
			}
			post.Viewer.CommunityModeration = actions
		}
	}
}

// PopulatePostResultsCommunityViewerModeration sets viewer.communityModeration
// on post.get's results: live posts, instance removal tombstones (from the
// post's admission status in its community) and community removal tombstones.
// A community removal tombstone carries no author, so it offers restore only.
func PopulatePostResultsCommunityViewerModeration(r *http.Request, reader moderation.CommunityViewer, results []*posts.PostResult) {
	reads := newCommunityViewerReads(r, reader)
	if reads == nil {
		return
	}
	for _, result := range results {
		if result == nil {
			continue
		}
		switch {
		case result.Post != nil:
			post := result.Post
			if post.Community == nil || post.Author == nil {
				continue
			}
			if actions := reads.postActions(post.Community.DID, post.Author.DID, post.Status); len(actions) > 0 {
				if post.Viewer == nil {
					post.Viewer = &posts.ViewerState{}
				}
				post.Viewer.CommunityModeration = actions
			}
		case result.Moderated != nil:
			tombstone := result.Moderated
			if tombstone.Community == nil {
				continue
			}
			if actions := reads.postActions(tombstone.Community.DID, tombstone.AuthorDID, tombstone.CommunityStatus); len(actions) > 0 {
				if tombstone.Viewer == nil {
					tombstone.Viewer = &posts.ModeratedPostViewerState{}
				}
				tombstone.Viewer.CommunityModeration = actions
			}
		case result.Removed != nil:
			removed := result.Removed
			authority := reads.authorityIn(removed.CommunityDID)
			// No author: ban-author is not actionable from this tombstone.
			if actions := moderation.CommunityPostViewerActions(authority, string(posts.AdmissionStatusRemoved), true); len(actions) > 0 {
				removed.Viewer = &posts.RemovedPostViewerState{CommunityModeration: actions}
			}
		}
	}
}

// PopulateCommentCommunityViewerModeration sets viewer.communityModeration on
// comment views to the community moderation actions the signed-in viewer may
// take in each comment's community. Callers holding a reply tree pass every
// view in it. A comment counts as community-removed when a removal source is
// its own community's; it counts as deleted only when its author deleted it
// (a community-removed comment is served as a deleted placeholder too).
func PopulateCommentCommunityViewerModeration(r *http.Request, reader moderation.CommunityViewer, views []*comments.CommentView) {
	if reads := newCommunityViewerReads(r, reader); reads != nil {
		reads.populateCommentActions(views)
	}
}

// PopulateThreadCommunityViewerModeration sets viewer.communityModeration on a
// comment thread's views, as PopulateCommentCommunityViewerModeration does,
// and on its header post, as PopulateCommunityViewerModeration does, sharing
// one request's reads between them. A nil header is skipped.
func PopulateThreadCommunityViewerModeration(r *http.Request, reader moderation.CommunityViewer, views []*comments.CommentView, header *posts.PostView) {
	reads := newCommunityViewerReads(r, reader)
	if reads == nil {
		return
	}
	reads.populateCommentActions(views)
	if header != nil {
		populateCommunityPostActions(reads, []*posts.FeedViewPost{{Post: header}})
	}
}

func (c *communityViewerReads) populateCommentActions(views []*comments.CommentView) {
	for _, view := range views {
		if view == nil || view.CommunityDID == "" || view.Author == nil {
			continue
		}
		authority := c.authorityIn(view.CommunityDID)
		if authority == moderation.CommunityAuthorityNone {
			continue
		}
		holds, ok := c.authorHoldsRole(view.CommunityDID, view.Author.DID)
		if !ok {
			continue
		}
		authorDeleted := view.DeletionReason != nil && *view.DeletionReason == comments.DeletionReasonAuthor
		actions := moderation.CommunityCommentViewerActions(authority, communityRemovedComment(view), authorDeleted, holds)
		if len(actions) == 0 {
			continue
		}
		if view.Viewer == nil {
			view.Viewer = &comments.CommentViewerState{}
		}
		view.Viewer.CommunityModeration = actions
	}
}

// communityRemovedComment reports whether the comment's own community removed it.
func communityRemovedComment(view *comments.CommentView) bool {
	if view.Moderation == nil || view.Moderation.State != moderation.ModerationStateRemoved {
		return false
	}
	for _, source := range view.Moderation.Sources {
		if source.Scope.Kind == moderation.ScopeCommunity && source.Scope.CommunityDID == view.CommunityDID {
			return true
		}
	}
	return false
}

// CommunityViewerActionsOn is the community moderation actions the signed-in
// viewer may take on the community itself; nil when signed out or without a
// reader. A failed read is returned, for a single-item read to fail on.
func CommunityViewerActionsOn(r *http.Request, reader moderation.CommunityViewer, communityDID string) ([]string, error) {
	viewerDID := middleware.GetUserDID(r)
	if reader == nil || viewerDID == "" {
		return nil, nil
	}
	authority, err := reader.ViewerAuthority(r.Context(), communityDID, viewerDID)
	if err != nil {
		return nil, fmt.Errorf("reading the viewer's community authority in %s: %w", communityDID, err)
	}
	return moderation.CommunityViewerActions(authority), nil
}
