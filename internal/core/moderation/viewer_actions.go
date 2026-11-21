package moderation

import (
	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
)

// PostViewerActions lists the moderation actions the viewer may take on a post,
// using the modlog action tokens. A post removed by this instance offers only
// restore; a post removed solely by another authority offers nothing. A live
// post offers remove plus either label or, when this instance already applied
// the nsfw label, retract-label. Callers must already have established that
// the viewer is an instance admin.
func PostViewerActions(instanceDID string, m *posts.ModerationView) []string {
	if m != nil && m.State == ModerationStateRemoved {
		if hasInstancePostSource(instanceDID, m.Sources) {
			return []string{ActionRestore}
		}
		return nil
	}
	if m != nil {
		for _, label := range m.ContentLabels {
			if label.Value == LabelNSFW && hasInstancePostSource(instanceDID, label.Sources) {
				return []string{ActionRemove, ActionRetractLabel}
			}
		}
	}
	return []string{ActionRemove, ActionLabel}
}

// CommentViewerActions lists the moderation actions the viewer may take on a
// comment. Comments cannot be labeled. A comment removed by this instance
// offers restore, even when its author also deleted it; any other deleted or
// removed placeholder offers nothing; a live comment offers remove. Callers
// must already have established that the viewer is an instance admin.
func CommentViewerActions(instanceDID string, view *comments.CommentView) []string {
	if view == nil {
		return nil
	}
	if m := view.Moderation; m != nil && m.State == ModerationStateRemoved {
		for _, source := range m.Sources {
			if isThisInstanceSource(instanceDID, source.AuthorityDID, source.Scope.Kind) {
				return []string{ActionRestore}
			}
		}
		return nil
	}
	if view.IsDeleted {
		return nil
	}
	return []string{ActionRemove}
}

func hasInstancePostSource(instanceDID string, sources []posts.ModerationSourceView) bool {
	for _, source := range sources {
		if isThisInstanceSource(instanceDID, source.AuthorityDID, source.Scope.Kind) {
			return true
		}
	}
	return false
}

// isThisInstanceSource reports whether a moderation source is this instance
// acting at instance scope.
func isThisInstanceSource(instanceDID, authorityDID, scopeKind string) bool {
	return authorityDID == instanceDID && scopeKind == ScopeInstance
}

// CommunityViewerAuthority is the community authority a viewer holds in one
// community.
type CommunityViewerAuthority string

const (
	CommunityAuthorityNone      CommunityViewerAuthority = ""
	CommunityAuthorityModerator CommunityViewerAuthority = "moderator"
	// CommunityAuthorityOwner is the owner, or an instance admin acting for
	// the community.
	CommunityAuthorityOwner CommunityViewerAuthority = "owner"
)

// Community viewer action tokens with no modlog name.
const (
	ActionBanAuthor    = "ban-author"
	ActionBanAccount   = "ban-account"
	ActionUnbanAccount = "unban-account"
)

// CommunityPostViewerActions lists the community moderation actions the viewer
// may take on a post in a community.
//
// A viewer without community authority gets nothing. A post whose admission
// is accepted, pending or pending reacceptance offers remove; a community-
// removed post offers restore; any other status (rejected, or no admission
// row) offers neither. ban-author follows whatever the admission status,
// because a community ban does not depend on the post, unless the author
// holds a role in the community, which a community ban refuses.
func CommunityPostViewerActions(authority CommunityViewerAuthority, admissionStatus string, authorHoldsRole bool) []string {
	if authority == CommunityAuthorityNone {
		return nil
	}
	var actions []string
	switch admissionStatus {
	case "accepted", "pending", "pending_reacceptance":
		actions = []string{ActionRemove}
	case "removed":
		actions = []string{ActionRestore}
	}
	if !authorHoldsRole {
		actions = append(actions, ActionBanAuthor)
	}
	return actions
}

// CommunityCommentViewerActions lists the community moderation actions the
// viewer may take on a comment in a community.
//
// communityRemoved is whether the community removed the comment;
// authorDeleted is whether its author deleted it, which offers nothing (a
// community-removed comment is served as a deleted placeholder but is not
// author-deleted). ban-author follows unless the author holds a role in the
// community.
func CommunityCommentViewerActions(authority CommunityViewerAuthority, communityRemoved, authorDeleted, authorHoldsRole bool) []string {
	if authority == CommunityAuthorityNone || authorDeleted {
		return nil
	}
	actions := []string{ActionRemove}
	if communityRemoved {
		actions = []string{ActionRestore}
	}
	if !authorHoldsRole {
		actions = append(actions, ActionBanAuthor)
	}
	return actions
}

// CommunityViewerActions lists the community moderation actions the viewer may
// take on the community itself.
// A moderator may ban and unban accounts; the owner (or an instance admin
// acting for the community) may also grant and remove roles and transfer
// ownership.
func CommunityViewerActions(authority CommunityViewerAuthority) []string {
	switch authority {
	case CommunityAuthorityModerator:
		return []string{ActionBanAccount, ActionUnbanAccount}
	case CommunityAuthorityOwner:
		return []string{ActionBanAccount, ActionUnbanAccount, ActionGrantRole, ActionRemoveRole, ActionTransferOwnership}
	default:
		return nil
	}
}
