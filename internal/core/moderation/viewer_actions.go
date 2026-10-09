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
