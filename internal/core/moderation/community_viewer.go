package moderation

import (
	"context"
	"errors"
	"fmt"

	"Coves/internal/core/communities"
)

// CommunityViewer reads a viewer's community authority for the viewer action
// lists.
type CommunityViewer interface {
	// ViewerMayModerate reports whether viewerDID can hold community authority
	// anywhere: it is an instance admin or holds a usable role in a hosted,
	// not-removed community. When false, ViewerAuthority is
	// CommunityAuthorityNone in every community.
	ViewerMayModerate(ctx context.Context, viewerDID string) (bool, error)
	// ViewerAuthority is the viewer's community authority, decided by the
	// rules of the community role authorizer.
	ViewerAuthority(ctx context.Context, communityDID, viewerDID string) (CommunityViewerAuthority, error)
	// Roster is the community's effective roster; RosterHoldsRole answers
	// whether a subject holds a role in it.
	Roster(ctx context.Context, communityDID string) (communities.Roster, error)
}

// RosterHoldsRole reports whether subjectDID holds a role in roster, the rule
// a community ban uses to refuse role holders.
func RosterHoldsRole(roster communities.Roster, subjectDID string) bool {
	return rosterRow(roster, subjectDID) != nil
}

// NewCommunityViewer returns the CommunityViewer over the role authorizer, the
// role index and the instance admin checker the authorizer decides with. It
// panics on a nil argument.
func NewCommunityViewer(authorizer communities.RoleAuthorizer, roles communities.RoleReader, admins communities.InstanceAdminChecker) CommunityViewer {
	if authorizer == nil {
		panic("moderation.NewCommunityViewer: nil role authorizer")
	}
	if roles == nil {
		panic("moderation.NewCommunityViewer: nil role reader")
	}
	if admins == nil {
		panic("moderation.NewCommunityViewer: nil instance admin checker")
	}
	return &communityViewer{authorizer: authorizer, roles: roles, admins: admins}
}

type communityViewer struct {
	authorizer communities.RoleAuthorizer
	roles      communities.RoleReader
	admins     communities.InstanceAdminChecker
}

func (v *communityViewer) ViewerMayModerate(ctx context.Context, viewerDID string) (bool, error) {
	if viewerDID == "" {
		return false, nil
	}
	if v.admins.IsInstanceAdmin(viewerDID) {
		return true, nil
	}
	return v.authorizer.HasUsableRole(ctx, viewerDID)
}

// ViewerAuthority is owner authority for an instance admin in a hosted,
// not-removed community, and otherwise the viewer's usable role there. A
// community not hosted or removed, and a suspended or absent role, is no
// authority; any other error is returned.
func (v *communityViewer) ViewerAuthority(ctx context.Context, communityDID, viewerDID string) (CommunityViewerAuthority, error) {
	if viewerDID == "" {
		return CommunityAuthorityNone, nil
	}
	if v.admins.IsInstanceAdmin(viewerDID) {
		err := v.authorizer.RequireHostedCommunity(ctx, communityDID)
		switch {
		case err == nil:
			return CommunityAuthorityOwner, nil
		case errors.Is(err, communities.ErrUnauthorized), errors.Is(err, communities.ErrCommunityRemoved):
			return CommunityAuthorityNone, nil
		default:
			return CommunityAuthorityNone, err
		}
	}
	role, err := v.authorizer.ViewerRole(ctx, communityDID, viewerDID)
	if err != nil {
		return CommunityAuthorityNone, err
	}
	switch role {
	case communities.RoleOwner:
		return CommunityAuthorityOwner, nil
	case communities.RoleModerator:
		return CommunityAuthorityModerator, nil
	default:
		return CommunityAuthorityNone, nil
	}
}

func (v *communityViewer) Roster(ctx context.Context, communityDID string) (communities.Roster, error) {
	roster, err := communities.ReadRoster(ctx, v.roles, communityDID)
	if err != nil {
		return communities.Roster{}, fmt.Errorf("reading the roster of community %s: %w", communityDID, err)
	}
	return roster, nil
}
