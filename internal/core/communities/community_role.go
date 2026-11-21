package communities

import (
	"context"
	"sort"
	"time"
)

// RoleCollection is the NSID of a community's role record.
const RoleCollection = "social.coves.community.moderator"

// Roles a community role record can carry.
const (
	RoleOwner     = "owner"
	RoleModerator = "moderator"
)

// CommunityRole is one indexed social.coves.community.moderator record.
type CommunityRole struct {
	CommunityDID    string
	SubjectDID      string
	Role            string
	RecordKey       string
	RecordCID       string
	RecordCreatedAt time.Time
	IndexedAt       time.Time
}

// Roster is a community's effective owner and moderators.
type Roster struct {
	Owner      *CommunityRole
	Moderators []CommunityRole
}

// RoleReader reads the community role index. Reads serve roles only in
// communities this instance hosts; role records of any other indexed community
// stay indexed but are not served.
type RoleReader interface {
	ListCommunityRoles(ctx context.Context, communityDID string) ([]CommunityRole, error)
	ListRolesBySubject(ctx context.Context, subjectDID string) ([]CommunityRole, error)
}

// RoleRepository maintains the community role index.
type RoleRepository interface {
	RoleReader
	UpsertCommunityRole(ctx context.Context, role CommunityRole) error
	DeleteCommunityRole(ctx context.Context, communityDID, recordKey string) error
	// UpsertCommunityRoleAtRev indexes a role record this instance wrote at
	// commit rev, under the same per-record rev gate the firehose uses. It
	// returns false, writing nothing, when that rev or a newer one was already
	// handled for the record.
	UpsertCommunityRoleAtRev(ctx context.Context, role CommunityRole, rev string) (bool, error)
	// ApplyRoleRecordEvent applies one firehose role commit in one
	// transaction: rev-gate claim, index change, roster version, and the
	// matching open intent's completion or an actorless action. It returns
	// false, writing nothing, when the gate rejects the event's rev.
	ApplyRoleRecordEvent(ctx context.Context, event RoleRecordEvent) (bool, error)
}

// RoleRecordEvent is one firehose commit of a role record. Op is the commit
// operation ("create", "update" or "delete"); Role is nil when the operation
// is a delete or a create/update that is permanently invalid and clears the
// record key's role.
type RoleRecordEvent struct {
	CommunityDID string
	RKey         string
	Rev          string
	CID          string
	Op           string
	Role         *CommunityRole
}

// RoleRecordKey is the deterministic record key of a role record on
// subjectDID: the same digest as BanRecordKey, so a community holds at most one
// role record per subject.
func RoleRecordKey(subjectDID string) string {
	return subjectRecordKey(subjectDID)
}

// EffectiveRoster resolves indexed role rows into a community's roster. Among
// the owner rows, the one with the latest RecordCreatedAt is the owner; equal
// createdAt is broken by the lexically greatest record key, so the result does
// not depend on delivery order. Every other owner row reads as a moderator.
// Rows with any other role are dropped. Moderators are ordered by record key.
func EffectiveRoster(rows []CommunityRole) Roster {
	var roster Roster
	var owners []CommunityRole
	for _, row := range rows {
		switch row.Role {
		case RoleOwner:
			owners = append(owners, row)
		case RoleModerator:
			roster.Moderators = append(roster.Moderators, row)
		}
	}
	for i := range owners {
		if roster.Owner == nil || ownerOutranks(owners[i], *roster.Owner) {
			roster.Owner = &owners[i]
		}
	}
	for _, owner := range owners {
		if owner.RecordKey == roster.Owner.RecordKey {
			continue
		}
		owner.Role = RoleModerator
		roster.Moderators = append(roster.Moderators, owner)
	}
	sort.Slice(roster.Moderators, func(i, j int) bool {
		return roster.Moderators[i].RecordKey < roster.Moderators[j].RecordKey
	})
	return roster
}

// ownerOutranks reports whether owner record a takes the owner role from b.
func ownerOutranks(a, b CommunityRole) bool {
	if !a.RecordCreatedAt.Equal(b.RecordCreatedAt) {
		return a.RecordCreatedAt.After(b.RecordCreatedAt)
	}
	return a.RecordKey > b.RecordKey
}

// ReadRoster reads a community's effective roster.
func ReadRoster(ctx context.Context, reader RoleReader, communityDID string) (Roster, error) {
	rows, err := reader.ListCommunityRoles(ctx, communityDID)
	if err != nil {
		return Roster{}, err
	}
	return EffectiveRoster(rows), nil
}
