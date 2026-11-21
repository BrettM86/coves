// Package moderationtest holds moderation test doubles shared by handler tests.
package moderationtest

import (
	"context"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
)

// CommunityViewer is a moderation.CommunityViewer answering from fixed maps.
// A pair absent from a map reads as no authority or no role.
type CommunityViewer struct {
	// Authority maps community DID -> viewer DID -> the viewer's authority.
	Authority map[string]map[string]moderation.CommunityViewerAuthority
	// Roles maps community DID -> subject DID -> whether the subject holds a role.
	Roles map[string]map[string]bool
}

// ViewerAuthority returns the configured authority of viewerDID in communityDID.
func (v CommunityViewer) ViewerAuthority(_ context.Context, communityDID, viewerDID string) (moderation.CommunityViewerAuthority, error) {
	return v.Authority[communityDID][viewerDID], nil
}

// ViewerMayModerate reports whether viewerDID has any configured authority.
func (v CommunityViewer) ViewerMayModerate(_ context.Context, viewerDID string) (bool, error) {
	for _, viewers := range v.Authority {
		if viewers[viewerDID] != moderation.CommunityAuthorityNone {
			return true, nil
		}
	}
	return false, nil
}

// Roster is a roster in which every subject configured as holding a role in
// communityDID is a moderator.
func (v CommunityViewer) Roster(_ context.Context, communityDID string) (communities.Roster, error) {
	var roster communities.Roster
	for subjectDID, holds := range v.Roles[communityDID] {
		if holds {
			roster.Moderators = append(roster.Moderators, communities.CommunityRole{
				CommunityDID: communityDID, SubjectDID: subjectDID, Role: communities.RoleModerator,
			})
		}
	}
	return roster, nil
}
