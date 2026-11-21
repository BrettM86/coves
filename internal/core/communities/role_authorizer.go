package communities

import (
	"context"
	"fmt"
	"time"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/accountstatus"
)

// HostedCommunityChecker reports whether this instance hosts a community.
type HostedCommunityChecker interface {
	IsHostedCommunity(ctx context.Context, communityDID string) (bool, error)
}

// InstanceAdminChecker reports whether a DID is an instance admin.
// moderation.Authority satisfies it.
type InstanceAdminChecker interface {
	IsInstanceAdmin(did string) bool
}

// RoleAuthorizerConfig holds the reads NewRoleAuthorizer decides over.
type RoleAuthorizerConfig struct {
	Hosted        HostedCommunityChecker
	Removals      RemovalReader
	Roles         RoleReader
	AccountBans   accountbans.Reader
	AccountStatus accountstatus.Reader
	Admins        InstanceAdminChecker
	Now           func() time.Time
}

// RoleAuthorization is what an authorized caller was authorized as.
type RoleAuthorization struct {
	Role          string // caller's own usable roster role (RoleOwner/RoleModerator), "" when authorized only as admin
	InstanceAdmin bool   // true when authorized as an instance admin acting for the community (no satisfying role of its own)
}

// RoleAuthorizer decides community role authority.
type RoleAuthorizer interface {
	// required is RoleOwner or RoleModerator; an owner satisfies RoleModerator.
	Authorize(ctx context.Context, communityDID, callerDID, required string) (RoleAuthorization, error)
	// ViewerRole: the viewer's usable role in the community ("" when none, suspended, not hosted, removed). Admin status is not a role.
	ViewerRole(ctx context.Context, communityDID, viewerDID string) (string, error)
	// HasUsableRole: whether the viewer holds a usable role in any hosted, not-removed community.
	HasUsableRole(ctx context.Context, viewerDID string) (bool, error)
	// RequireHostedCommunity refuses a community this instance does not host
	// (ErrUnauthorized) or has removed (ErrCommunityRemoved).
	RequireHostedCommunity(ctx context.Context, communityDID string) error
}

// NewRoleAuthorizer returns the role authorizer over cfg. Every field is
// required.
//
// A role counts only in a community this instance hosts (it stores the
// community's PDS credentials) and has not removed, and only as the effective
// roster of the role index resolves it. An in-force instance account ban or a
// stored PDS takendown/suspended restriction suspends the holder's role without
// touching the record. An instance admin acts with owner and moderator
// authority in any hosted, not-removed community.
func NewRoleAuthorizer(cfg RoleAuthorizerConfig) RoleAuthorizer {
	if cfg.Hosted == nil || cfg.Removals == nil || cfg.Roles == nil ||
		cfg.AccountBans == nil || cfg.AccountStatus == nil || cfg.Admins == nil || cfg.Now == nil {
		panic("communities.NewRoleAuthorizer: every RoleAuthorizerConfig field is required")
	}
	return &roleAuthorizer{cfg: cfg}
}

type roleAuthorizer struct {
	cfg RoleAuthorizerConfig
}

func (a *roleAuthorizer) Authorize(ctx context.Context, communityDID, callerDID, required string) (RoleAuthorization, error) {
	if required != RoleOwner && required != RoleModerator {
		return RoleAuthorization{}, fmt.Errorf("authorizing a community role: unknown required role %q", required)
	}
	if err := a.requireActingCommunity(ctx, communityDID); err != nil {
		return RoleAuthorization{}, err
	}
	role, err := a.usableRole(ctx, communityDID, callerDID)
	if err != nil {
		return RoleAuthorization{}, err
	}
	if role == RoleOwner || (role == RoleModerator && required == RoleModerator) {
		return RoleAuthorization{Role: role}, nil
	}
	if a.cfg.Admins.IsInstanceAdmin(callerDID) {
		return RoleAuthorization{InstanceAdmin: true}, nil
	}
	return RoleAuthorization{}, fmt.Errorf("%w: the caller does not hold the %s role in community %s",
		ErrUnauthorized, required, communityDID)
}

func (a *roleAuthorizer) ViewerRole(ctx context.Context, communityDID, viewerDID string) (string, error) {
	usable, err := a.communityUsable(ctx, communityDID)
	if err != nil || !usable {
		return "", err
	}
	return a.usableRole(ctx, communityDID, viewerDID)
}

func (a *roleAuthorizer) HasUsableRole(ctx context.Context, viewerDID string) (bool, error) {
	rows, err := a.cfg.Roles.ListRolesBySubject(ctx, viewerDID)
	if err != nil {
		return false, fmt.Errorf("listing the roles of %s: %w", viewerDID, err)
	}
	held := false
	for _, row := range rows {
		if row.Role != RoleOwner && row.Role != RoleModerator {
			continue
		}
		usable, err := a.communityUsable(ctx, row.CommunityDID)
		if err != nil {
			return false, err
		}
		if usable {
			held = true
			break
		}
	}
	if !held {
		return false, nil
	}
	suspended, err := a.suspended(ctx, viewerDID)
	if err != nil {
		return false, err
	}
	return !suspended, nil
}

func (a *roleAuthorizer) RequireHostedCommunity(ctx context.Context, communityDID string) error {
	return a.requireActingCommunity(ctx, communityDID)
}

// requireActingCommunity refuses a community this instance does not host
// (ErrUnauthorized) or has removed (ErrCommunityRemoved).
func (a *roleAuthorizer) requireActingCommunity(ctx context.Context, communityDID string) error {
	hosted, removed, err := a.standing(ctx, communityDID)
	if err != nil {
		return err
	}
	if !hosted {
		return fmt.Errorf("%w: community %s is not hosted by this instance", ErrUnauthorized, communityDID)
	}
	if removed {
		return fmt.Errorf("%w: %s", ErrCommunityRemoved, communityDID)
	}
	return nil
}

// communityUsable reports whether roles in communityDID count: it is hosted
// here and not removed.
func (a *roleAuthorizer) communityUsable(ctx context.Context, communityDID string) (bool, error) {
	hosted, removed, err := a.standing(ctx, communityDID)
	return hosted && !removed, err
}

// standing reports whether this instance hosts communityDID and, when it
// does, whether the instance has removed it.
func (a *roleAuthorizer) standing(ctx context.Context, communityDID string) (hosted, removed bool, err error) {
	hosted, err = a.cfg.Hosted.IsHostedCommunity(ctx, communityDID)
	if err != nil {
		return false, false, fmt.Errorf("checking whether community %s is hosted: %w", communityDID, err)
	}
	if !hosted {
		return false, false, nil
	}
	removed, err = a.cfg.Removals.CommunityRemoved(ctx, communityDID)
	if err != nil {
		return false, false, fmt.Errorf("checking whether community %s is removed: %w", communityDID, err)
	}
	return true, removed, nil
}

// usableRole is subjectDID's role in communityDID's effective roster, or ""
// when it holds none or the role is suspended. The community's hosting and
// removal are the caller's to check.
func (a *roleAuthorizer) usableRole(ctx context.Context, communityDID, subjectDID string) (string, error) {
	roster, err := ReadRoster(ctx, a.cfg.Roles, communityDID)
	if err != nil {
		return "", fmt.Errorf("reading the roster of community %s: %w", communityDID, err)
	}
	role := rosterRole(roster, subjectDID)
	if role == "" {
		return "", nil
	}
	suspended, err := a.suspended(ctx, subjectDID)
	if err != nil || suspended {
		return "", err
	}
	return role, nil
}

// rosterRole is subjectDID's role in roster, or "" when it holds none.
func rosterRole(roster Roster, subjectDID string) string {
	if roster.Owner != nil && roster.Owner.SubjectDID == subjectDID {
		return RoleOwner
	}
	for _, moderator := range roster.Moderators {
		if moderator.SubjectDID == subjectDID {
			return RoleModerator
		}
	}
	return ""
}

// suspended reports whether accountDID's roles are suspended: it has an
// in-force instance ban or a stored takendown or suspended PDS restriction.
func (a *roleAuthorizer) suspended(ctx context.Context, accountDID string) (bool, error) {
	ban, err := a.cfg.AccountBans.ActiveBan(ctx, accountDID)
	if err != nil {
		return false, fmt.Errorf("reading the instance ban of %s: %w", accountDID, err)
	}
	if ban.InForce(a.cfg.Now()) {
		return true, nil
	}
	status, restricted, err := a.cfg.AccountStatus.Restriction(ctx, accountDID)
	if err != nil {
		return false, fmt.Errorf("reading the account status of %s: %w", accountDID, err)
	}
	return restricted && (status == accountstatus.Takendown || status == accountstatus.Suspended), nil
}

// WithRoleAuthorization installs the role index and authorizer CreateCommunity
// and UpdateCommunity require.
func WithRoleAuthorization(roles RoleRepository, authorizer RoleAuthorizer) PDSClientOption {
	return func(c *pdsClientConfig) {
		c.roles = roles
		c.roleAuthorizer = authorizer
	}
}
