package jetstream

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
)

const (
	roleTestCommunity = "did:plc:rolecommunity1"
	roleTestSubject   = "did:plc:troublemaker456"
	// RoleRecordKey("did:plc:troublemaker456")
	roleTestSubjectKey = "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa"
	// RoleRecordKey("not-a-did")
	roleTestNotADIDKey = "m27h5oluvhmp4zjtut5aijcunlxanoslceoj4bbc4c77ayhs67ya"
	// RoleRecordKey("did:plc:rostersubject1")
	roleTestOtherSubjectKey = "zc2jud7dj5x4pzvu56sihs4mckc7l6s3wv6ojrbv6ed6szikisnq"
)

// recordingRoleStore is an in-memory communities.RoleRepository that keeps
// every write it receives, so a test can assert that nothing was stored.
type recordingRoleStore struct {
	upserted []communities.CommunityRole
	deleted  []string
}

func (s *recordingRoleStore) ListCommunityRoles(context.Context, string) ([]communities.CommunityRole, error) {
	return s.upserted, nil
}

func (s *recordingRoleStore) ListRolesBySubject(context.Context, string) ([]communities.CommunityRole, error) {
	return nil, nil
}

func (s *recordingRoleStore) UpsertCommunityRole(_ context.Context, role communities.CommunityRole) error {
	s.upserted = append(s.upserted, role)
	return nil
}

func (s *recordingRoleStore) DeleteCommunityRole(_ context.Context, communityDID, recordKey string) error {
	s.deleted = append(s.deleted, communityDID+"/"+recordKey)
	return nil
}

func (s *recordingRoleStore) UpsertCommunityRoleAtRev(_ context.Context, role communities.CommunityRole, _ string) (bool, error) {
	s.upserted = append(s.upserted, role)
	return true, nil
}

func (s *recordingRoleStore) ApplyRoleRecordEvent(_ context.Context, event communities.RoleRecordEvent) (bool, error) {
	if event.Role != nil {
		s.upserted = append(s.upserted, *event.Role)
	} else {
		s.deleted = append(s.deleted, event.CommunityDID+"/"+event.RKey)
	}
	return true, nil
}

func roleCommitEvent(repoDID, rkey string, record map[string]interface{}) *JetstreamEvent {
	return &JetstreamEvent{
		Did:  repoDID,
		Kind: "commit",
		Commit: &CommitEvent{
			Rev:        "3lrolerev0001",
			Operation:  "create",
			Collection: "social.coves.community.moderator",
			RKey:       rkey,
			CID:        "bafyreirolerecord",
			Record:     record,
		},
	}
}

func roleRecord(subject, role, createdAt string) map[string]interface{} {
	return map[string]interface{}{
		"$type":     "social.coves.community.moderator",
		"subject":   subject,
		"role":      role,
		"createdAt": createdAt,
	}
}

func TestCommunityRole_RepoNotAnIndexedCommunityIsUnresolved(t *testing.T) {
	store := &recordingRoleStore{}
	consumer := NewCommunityEventConsumer(newOriginRepo(), originTestInstance, true, nil, WithCommunityRoster(store))

	err := consumer.HandleEvent(context.Background(),
		roleCommitEvent(roleTestCommunity, roleTestSubjectKey, roleRecord(roleTestSubject, "moderator", "2026-11-01T10:00:00Z")))

	require.ErrorIs(t, err, ErrUnresolvedReference)
	assert.Empty(t, store.upserted)
	assert.Empty(t, store.deleted)
}

func TestCommunityRole_InvalidRecordsArePermanent(t *testing.T) {
	missingSubject := roleRecord(roleTestSubject, "moderator", "2026-11-01T10:00:00Z")
	delete(missingSubject, "subject")

	tests := []struct {
		name   string
		rkey   string
		record map[string]interface{}
	}{
		{"record key is not the subject's", roleTestOtherSubjectKey, roleRecord(roleTestSubject, "moderator", "2026-11-01T10:00:00Z")},
		{"role of 65 bytes", roleTestSubjectKey, roleRecord(roleTestSubject, strings.Repeat("r", 65), "2026-11-01T10:00:00Z")},
		{"empty role", roleTestSubjectKey, roleRecord(roleTestSubject, "", "2026-11-01T10:00:00Z")},
		{"malformed createdAt", roleTestSubjectKey, roleRecord(roleTestSubject, "moderator", "yesterday")},
		{"missing subject", roleTestSubjectKey, missingSubject},
		{"subject is not a DID", roleTestNotADIDKey, roleRecord("not-a-did", "moderator", "2026-11-01T10:00:00Z")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newOriginRepo()
			repo.byDID[roleTestCommunity] = &communities.Community{DID: roleTestCommunity}
			store := &recordingRoleStore{}
			consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil, WithCommunityRoster(store))

			err := consumer.HandleEvent(context.Background(), roleCommitEvent(roleTestCommunity, tt.rkey, tt.record))

			require.ErrorIs(t, err, ErrPermanentEvent)
			assert.Empty(t, store.upserted)
			// The invalid record removes whatever role its record key held.
			assert.Equal(t, []string{roleTestCommunity + "/" + tt.rkey}, store.deleted)
		})
	}
}
