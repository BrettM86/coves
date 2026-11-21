package communities_test

import (
	"context"
	"sync"
	"time"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/communities"
)

// In-memory readers for the role authorizer, shared by the authorizer rules
// and the service tests that wire the real authorizer. Untagged, so the
// integration build may use them too.

// fakeRoleIndex is a communities.RoleRepository over memory. Unlike the
// Postgres repository it serves rows of every community, hosted or not, so a
// test can prove the authorizer itself refuses a role in a community this
// instance does not host.
type fakeRoleIndex struct {
	mu   sync.Mutex
	rows []communities.CommunityRole
}

func newFakeRoleIndex(rows ...communities.CommunityRole) *fakeRoleIndex {
	return &fakeRoleIndex{rows: append([]communities.CommunityRole(nil), rows...)}
}

func (f *fakeRoleIndex) ListCommunityRoles(_ context.Context, communityDID string) ([]communities.CommunityRole, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []communities.CommunityRole
	for _, row := range f.rows {
		if row.CommunityDID == communityDID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeRoleIndex) ListRolesBySubject(_ context.Context, subjectDID string) ([]communities.CommunityRole, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []communities.CommunityRole
	for _, row := range f.rows {
		if row.SubjectDID == subjectDID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeRoleIndex) UpsertCommunityRole(_ context.Context, role communities.CommunityRole) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, row := range f.rows {
		if row.CommunityDID == role.CommunityDID && row.RecordKey == role.RecordKey {
			f.rows[i] = role
			return nil
		}
	}
	f.rows = append(f.rows, role)
	return nil
}

func (f *fakeRoleIndex) UpsertCommunityRoleAtRev(ctx context.Context, role communities.CommunityRole, _ string) (bool, error) {
	return true, f.UpsertCommunityRole(ctx, role)
}

func (f *fakeRoleIndex) ApplyRoleRecordEvent(ctx context.Context, event communities.RoleRecordEvent) (bool, error) {
	if event.Role != nil {
		return true, f.UpsertCommunityRole(ctx, *event.Role)
	}
	return true, f.DeleteCommunityRole(ctx, event.CommunityDID, event.RKey)
}

func (f *fakeRoleIndex) DeleteCommunityRole(_ context.Context, communityDID, recordKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[:0]
	for _, row := range f.rows {
		if row.CommunityDID != communityDID || row.RecordKey != recordKey {
			kept = append(kept, row)
		}
	}
	f.rows = kept
	return nil
}

func (f *fakeRoleIndex) snapshot() []communities.CommunityRole {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]communities.CommunityRole(nil), f.rows...)
}

type fakeHostedCommunities map[string]bool

func (f fakeHostedCommunities) IsHostedCommunity(_ context.Context, communityDID string) (bool, error) {
	return f[communityDID], nil
}

type fakeCommunityRemovals map[string]bool

func (f fakeCommunityRemovals) CommunityRemoved(_ context.Context, communityDID string) (bool, error) {
	return f[communityDID], nil
}

type fakeAccountBans map[string]*accountbans.Ban

func (f fakeAccountBans) ActiveBan(_ context.Context, accountDID string) (*accountbans.Ban, error) {
	return f[accountDID], nil
}

type fakeAccountStatuses map[string]accountstatus.Status

func (f fakeAccountStatuses) Restriction(_ context.Context, did string) (accountstatus.Status, bool, error) {
	status, ok := f[did]
	return status, ok, nil
}

type fakeInstanceAdmins map[string]bool

func (f fakeInstanceAdmins) IsInstanceAdmin(did string) bool { return f[did] }

// roleAuthorizationFixture is the real authorizer over in-memory readers.
type roleAuthorizationFixture struct {
	roles    *fakeRoleIndex
	hosted   fakeHostedCommunities
	removals fakeCommunityRemovals
	bans     fakeAccountBans
	statuses fakeAccountStatuses
	admins   fakeInstanceAdmins
	now      time.Time
}

func newRoleAuthorizationFixture(rows ...communities.CommunityRole) *roleAuthorizationFixture {
	return &roleAuthorizationFixture{
		roles:    newFakeRoleIndex(rows...),
		hosted:   fakeHostedCommunities{},
		removals: fakeCommunityRemovals{},
		bans:     fakeAccountBans{},
		statuses: fakeAccountStatuses{},
		admins:   fakeInstanceAdmins{},
		now:      time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC),
	}
}

func (f *roleAuthorizationFixture) authorizer() communities.RoleAuthorizer {
	return communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted:        f.hosted,
		Removals:      f.removals,
		Roles:         f.roles,
		AccountBans:   f.bans,
		AccountStatus: f.statuses,
		Admins:        f.admins,
		Now:           func() time.Time { return f.now },
	})
}

// option is the service wiring under test: the fixture's role index and the
// real authorizer over it.
func (f *roleAuthorizationFixture) option() communities.PDSClientOption {
	return communities.WithRoleAuthorization(f.roles, f.authorizer())
}

// ownerRow is an owner role record for subjectDID in communityDID.
func ownerRow(communityDID, subjectDID string, createdAt time.Time) communities.CommunityRole {
	return communities.CommunityRole{
		CommunityDID: communityDID, SubjectDID: subjectDID, Role: communities.RoleOwner,
		RecordKey: communities.RoleRecordKey(subjectDID), RecordCID: "bafyowner" + subjectDID[len("did:plc:"):],
		RecordCreatedAt: createdAt, IndexedAt: createdAt,
	}
}

// moderatorRow is a moderator role record for subjectDID in communityDID.
func moderatorRow(communityDID, subjectDID string, createdAt time.Time) communities.CommunityRole {
	row := ownerRow(communityDID, subjectDID, createdAt)
	row.Role = communities.RoleModerator
	row.RecordCID = "bafymoderator" + subjectDID[len("did:plc:"):]
	return row
}
