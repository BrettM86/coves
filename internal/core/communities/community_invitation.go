package communities

import (
	"context"
	"errors"
	"time"
)

// ErrInvitationPartyErased reports that an invitation was not created because
// its inviter or invitee is an erased account.
var ErrInvitationPartyErased = errors.New("community invitation party is an erased account")

// InvitationLifetime is how long an open invitation can be accepted.
const InvitationLifetime = 14 * 24 * time.Hour

// InvitationState is the stored lifecycle state of an invitation. Lapsing is
// computed at read time and never stored.
type InvitationState string

// The invitation states.
const (
	InvitationOpen       InvitationState = "open"
	InvitationAccepted   InvitationState = "accepted"
	InvitationDeclined   InvitationState = "declined"
	InvitationSuperseded InvitationState = "superseded"
)

// Invitation is an invitation to a community role: a moderator invitation
// (Role RoleModerator) or an ownership offer (Role RoleOwner).
type Invitation struct {
	ID, CommunityDID, InviteeDID, Role, InviterDID string
	// InviterAdmin: the inviter acted as an instance admin.
	InviterAdmin         bool
	State                InvitationState
	CreatedAt, ExpiresAt time.Time // ExpiresAt = CreatedAt + InvitationLifetime
}

// Lapsed reports whether an open invitation is at or past ExpiresAt.
func (i Invitation) Lapsed(now time.Time) bool {
	return i.State == InvitationOpen && !now.Before(i.ExpiresAt)
}

// Pending reports whether the invitation is open and not lapsed at now.
func (i Invitation) Pending(now time.Time) bool {
	return i.State == InvitationOpen && !i.Lapsed(now)
}

// InvitationReader reads pending invitations.
type InvitationReader interface {
	// ListPendingInvitationsForCommunity lists the community's pending
	// invitations (open, not lapsed at now), newest first.
	ListPendingInvitationsForCommunity(ctx context.Context, communityDID string, now time.Time) ([]Invitation, error)
	// ListPendingInvitationsForInvitee lists the invitee's pending
	// invitations, newest first.
	ListPendingInvitationsForInvitee(ctx context.Context, inviteeDID string, now time.Time) ([]Invitation, error)
	// PendingInvitation is the invitee's pending invitation in the community,
	// or nil.
	PendingInvitation(ctx context.Context, communityDID, inviteeDID string, now time.Time) (*Invitation, error)
}
