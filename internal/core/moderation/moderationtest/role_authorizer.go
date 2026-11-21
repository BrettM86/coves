package moderationtest

import (
	"context"
	"fmt"
	"sync"

	"Coves/internal/core/communities"
)

// RoleAuthorizer is a communities.RoleAuthorizer and InstanceAdminChecker
// answering from fixed tables, with the rules of the real authorizer, and
// counting every call by method name.
type RoleAuthorizer struct {
	// Hosted lists the communities this instance hosts.
	Hosted map[string]bool
	// Removed lists the hosted communities the instance has removed.
	Removed map[string]bool
	// Roles maps community DID -> viewer DID -> usable role.
	Roles map[string]map[string]string
	// Admins lists the instance admins.
	Admins map[string]bool

	mu    sync.Mutex
	calls map[string]int
}

// Calls is how many times method was called.
func (a *RoleAuthorizer) Calls(method string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[method]
}

func (a *RoleAuthorizer) count(method string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.calls == nil {
		a.calls = make(map[string]int)
	}
	a.calls[method]++
}

func (a *RoleAuthorizer) requireActing(communityDID string) error {
	if !a.Hosted[communityDID] {
		return fmt.Errorf("%w: community %s is not hosted", communities.ErrUnauthorized, communityDID)
	}
	if a.Removed[communityDID] {
		return fmt.Errorf("%w: %s", communities.ErrCommunityRemoved, communityDID)
	}
	return nil
}

// Authorize grants required to its holder, an owner satisfying moderator, and
// to an instance admin in a hosted, not-removed community.
func (a *RoleAuthorizer) Authorize(_ context.Context, communityDID, callerDID, required string) (communities.RoleAuthorization, error) {
	a.count("Authorize")
	if err := a.requireActing(communityDID); err != nil {
		return communities.RoleAuthorization{}, err
	}
	role := a.Roles[communityDID][callerDID]
	if role == communities.RoleOwner || (role == communities.RoleModerator && required == communities.RoleModerator) {
		return communities.RoleAuthorization{Role: role}, nil
	}
	if a.Admins[callerDID] {
		return communities.RoleAuthorization{InstanceAdmin: true}, nil
	}
	return communities.RoleAuthorization{}, fmt.Errorf("%w: no %s role", communities.ErrUnauthorized, required)
}

// ViewerRole is the viewer's role, "" in a community not hosted or removed.
func (a *RoleAuthorizer) ViewerRole(_ context.Context, communityDID, viewerDID string) (string, error) {
	a.count("ViewerRole")
	if a.requireActing(communityDID) != nil {
		return "", nil
	}
	return a.Roles[communityDID][viewerDID], nil
}

// HasUsableRole reports whether the viewer holds a role in any hosted,
// not-removed community.
func (a *RoleAuthorizer) HasUsableRole(_ context.Context, viewerDID string) (bool, error) {
	a.count("HasUsableRole")
	for communityDID, holders := range a.Roles {
		if holders[viewerDID] != "" && a.requireActing(communityDID) == nil {
			return true, nil
		}
	}
	return false, nil
}

// RequireHostedCommunity refuses a community not hosted or removed.
func (a *RoleAuthorizer) RequireHostedCommunity(_ context.Context, communityDID string) error {
	a.count("RequireHostedCommunity")
	return a.requireActing(communityDID)
}

// IsInstanceAdmin reports whether did is listed in Admins.
func (a *RoleAuthorizer) IsInstanceAdmin(did string) bool {
	a.count("IsInstanceAdmin")
	return a.Admins[did]
}

// RoleIndex is a communities.RoleReader serving fixed role rows and counting
// every call by method name.
type RoleIndex struct {
	// Rows maps community DID -> its indexed role rows.
	Rows map[string][]communities.CommunityRole

	mu    sync.Mutex
	calls map[string]int
}

// Calls is how many times method was called.
func (i *RoleIndex) Calls(method string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls[method]
}

func (i *RoleIndex) count(method string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.calls == nil {
		i.calls = make(map[string]int)
	}
	i.calls[method]++
}

// ListCommunityRoles serves the community's rows.
func (i *RoleIndex) ListCommunityRoles(_ context.Context, communityDID string) ([]communities.CommunityRole, error) {
	i.count("ListCommunityRoles")
	return i.Rows[communityDID], nil
}

// ListRolesBySubject serves the subject's rows across communities.
func (i *RoleIndex) ListRolesBySubject(_ context.Context, subjectDID string) ([]communities.CommunityRole, error) {
	i.count("ListRolesBySubject")
	var rows []communities.CommunityRole
	for _, communityRows := range i.Rows {
		for _, row := range communityRows {
			if row.SubjectDID == subjectDID {
				rows = append(rows, row)
			}
		}
	}
	return rows, nil
}
