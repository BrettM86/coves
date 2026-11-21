package communities_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"Coves/internal/core/communities"
)

func TestRoleRecordKey(t *testing.T) {
	// The same digest as BanRecordKey: unpadded lowercase base32 of
	// SHA-256(subjectDID), so a community holds one role record per subject.
	assert.Equal(t, "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",
		communities.RoleRecordKey("did:plc:troublemaker456"))
}

func TestEffectiveRoster_DropsUnknownRoles(t *testing.T) {
	const community = "did:plc:rostercommunity"
	createdAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	owner := communities.CommunityRole{
		CommunityDID: community, SubjectDID: "did:plc:rosterowner", Role: "owner",
		RecordKey: "ownerkey", RecordCID: "bafyreiowner", RecordCreatedAt: createdAt,
	}
	moderator := communities.CommunityRole{
		CommunityDID: community, SubjectDID: "did:plc:rostermoderator", Role: "moderator",
		RecordKey: "moderatorkey", RecordCID: "bafyreimoderator", RecordCreatedAt: createdAt,
	}
	admin := communities.CommunityRole{
		CommunityDID: community, SubjectDID: "did:plc:rosteradmin", Role: "admin",
		RecordKey: "adminkey", RecordCID: "bafyreiadmin", RecordCreatedAt: createdAt,
	}

	roster := communities.EffectiveRoster([]communities.CommunityRole{admin, moderator, owner})

	assert.Equal(t, communities.Roster{
		Owner:      &owner,
		Moderators: []communities.CommunityRole{moderator},
	}, roster)
}
