//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A community names its owner and moderators with social.coves.community.moderator
// records in its own repo, one per subject at the subject's derived record key.
// The communities consumer indexes them under the rev gate; the index is read
// back through the repository and communities.ReadRoster.

const (
	rosterCommunityA = "did:plc:rostercommunitya"
	rosterCommunityB = "did:plc:rostercommunityb"
	rosterFounder    = "did:plc:rosterfounder"

	rosterSubject1 = "did:plc:rostersubject1"
	rosterSubject2 = "did:plc:rostersubject2"
	rosterAdmin    = "did:plc:rosteradmin1"
	rosterEarly    = "did:plc:rosterearly"
	rosterLate     = "did:plc:rosterlate"
	rosterTieA     = "did:plc:rosterownertiea"
	rosterTieB     = "did:plc:rosterownertieb"

	// Record keys computed outside the code under test: lowercase unpadded
	// base32 of SHA-256 over the subject DID.
	rosterSubject1Key = "zc2jud7dj5x4pzvu56sihs4mckc7l6s3wv6ojrbv6ed6szikisnq"
	rosterSubject2Key = "lk4ulvb25z3owhmahunggqgfc4aiv7a3cbmrrcuwlfdva7x5uiwa"
	rosterAdminKey    = "zwt3sgadooziywb22ettzwurplpbyzd3if26b3stllystrijapza"
	// The later owner deliberately has the lexically SMALLER key, so the
	// createdAt rule and the tie rule cannot be confused.
	rosterEarlyKey = "kpe2k5mtzbbqhivnoao4kqolwwqfxxld5njrikp65x6b2zptqpwq"
	rosterLateKey  = "2hhole3lgwjrzkzyktgf2qrhh7sub6ubl3pbdueknthwpnmcoc5a"
	// rosterTieBKey ("bf...") is lexically greater than rosterTieAKey ("b4...").
	rosterTieAKey = "b4jkmjie2rhtnwfw2unhnuwaidv76rrq3egybwtt3cr7c7izoz4q"
	rosterTieBKey = "bfxwxfke3uws2kwcmxra4azlva5h3hnmrt6rgf2gait6zgs5g4pq"

	rosterTime1 = "2026-11-01T10:00:00Z"
	rosterTime2 = "2026-11-02T10:00:00Z"
)

var (
	rosterT1 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	rosterT2 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
)

// rosterStep is one role commit delivered through the communities consumer.
type rosterStep struct {
	repo      string
	op        string // create, update, delete
	subject   string
	rkey      string
	role      string
	createdAt string
	rev       string
	cid       string
}

func rosterCommitEvent(s rosterStep, timeUS int64) *JetstreamEvent {
	if s.op == "delete" {
		return revCommitEvent(s.repo, communities.RoleCollection, "delete", s.rkey, s.rev, "", timeUS, nil)
	}
	record := map[string]interface{}{
		"$type":     communities.RoleCollection,
		"subject":   s.subject,
		"role":      s.role,
		"createdAt": s.createdAt,
	}
	return revCommitEvent(s.repo, communities.RoleCollection, s.op, s.rkey, s.rev, s.cid, timeUS, record)
}

// newRosterConsumer wires the rev-gated communities consumer with the role
// index over db, with communities A and B indexed.
func newRosterConsumer(t *testing.T, db *sql.DB) (*CommunityEventConsumer, *postgres.CommunityRoleRepository) {
	t.Helper()
	insertBridgedUser(t, db, rosterFounder, "rosterfounder.test")
	insertBridgedCommunity(t, db, rosterCommunityA, "rostercommunitya.test", rosterFounder)
	insertBridgedCommunity(t, db, rosterCommunityB, "rostercommunityb.test", rosterFounder)
	// The role index serves reads only for communities this instance hosts,
	// marked by a stored PDS refresh token.
	_, err := db.Exec(`UPDATE communities SET pds_refresh_token_encrypted = $1 WHERE did = ANY($2)`,
		[]byte("hosted"), pq.Array([]string{rosterCommunityA, rosterCommunityB}))
	require.NoError(t, err)
	roles := postgres.NewCommunityRoleRepository(db)
	consumer := NewCommunityEventConsumer(postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), "did:web:test.local", true, nil,
		WithCommunityRevGate(NewRevGate(db)), WithCommunityRoster(roles))
	return consumer, roles
}

func deliverRosterSteps(t *testing.T, consumer *CommunityEventConsumer, steps []rosterStep) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UnixMicro()
	for i, step := range steps {
		err := consumer.HandleEvent(ctx, rosterCommitEvent(step, base+int64(i)*1_000_000))
		require.NoErrorf(t, err, "step %d (%s %s)", i, step.op, step.rkey)
	}
}

// normalizeRole drops the index time (asserted present) and pins createdAt to UTC.
func normalizeRole(t *testing.T, role communities.CommunityRole) communities.CommunityRole {
	t.Helper()
	assert.False(t, role.IndexedAt.IsZero(), "an indexed role carries its index time")
	role.IndexedAt = time.Time{}
	role.RecordCreatedAt = role.RecordCreatedAt.UTC()
	return role
}

func normalizeRoles(t *testing.T, roles []communities.CommunityRole) []communities.CommunityRole {
	t.Helper()
	out := make([]communities.CommunityRole, 0, len(roles))
	for _, role := range roles {
		out = append(out, normalizeRole(t, role))
	}
	return out
}

func TestCommunityRoleIngestion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		steps []rosterStep
		// want is ListCommunityRoles per community after every step ran.
		want map[string][]communities.CommunityRole
	}{
		{
			name: "a create at the subject's record key indexes the subject with its role",
			steps: []rosterStep{
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revA, cid: "bafyreirostera",
				},
			},
			want: map[string][]communities.CommunityRole{
				rosterCommunityA: {{
					CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rostersubject1", Role: "moderator",
					RecordKey: rosterSubject1Key, RecordCID: "bafyreirostera", RecordCreatedAt: rosterT1,
				}},
			},
		},
		{
			name: "b an update from moderator to owner changes the role in the one row",
			steps: []rosterStep{
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revA, cid: "bafyreirosterb1",
				},
				{
					repo: rosterCommunityA, op: "update", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "owner", createdAt: rosterTime2, rev: revB, cid: "bafyreirosterb2",
				},
			},
			want: map[string][]communities.CommunityRole{
				rosterCommunityA: {{
					CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rostersubject1", Role: "owner",
					RecordKey: rosterSubject1Key, RecordCID: "bafyreirosterb2", RecordCreatedAt: rosterT2,
				}},
			},
		},
		{
			name: "c deleting the record removes its row and only its row",
			steps: []rosterStep{
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revA, cid: "bafyreirosterc",
				},
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject2, rkey: rosterSubject2Key,
					role: "moderator", createdAt: rosterTime2, rev: revA, cid: "bafyreirostersibling",
				},
				{repo: rosterCommunityA, op: "delete", rkey: rosterSubject1Key, rev: revB},
			},
			want: map[string][]communities.CommunityRole{
				rosterCommunityA: {{
					CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rostersubject2", Role: "moderator",
					RecordKey: rosterSubject2Key, RecordCID: "bafyreirostersibling", RecordCreatedAt: rosterT2,
				}},
			},
		},
		{
			name: "d a stale create replayed after the delete stays deleted, a sibling row stays",
			steps: []rosterStep{
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revA, cid: "bafyreirosterd",
				},
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject2, rkey: rosterSubject2Key,
					role: "moderator", createdAt: rosterTime2, rev: revA, cid: "bafyreirostersibling",
				},
				{repo: rosterCommunityA, op: "delete", rkey: rosterSubject1Key, rev: revB},
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revA, cid: "bafyreirosterd",
				},
			},
			want: map[string][]communities.CommunityRole{
				rosterCommunityA: {{
					CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rostersubject2", Role: "moderator",
					RecordKey: rosterSubject2Key, RecordCID: "bafyreirostersibling", RecordCreatedAt: rosterT2,
				}},
			},
		},
		{
			name: "d2 a create older than an earlier-delivered delete is not indexed",
			steps: []rosterStep{
				{repo: rosterCommunityA, op: "delete", rkey: rosterSubject1Key, rev: revB},
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revA, cid: "bafyreirosterd2",
				},
			},
			want: map[string][]communities.CommunityRole{rosterCommunityA: nil},
		},
		{
			name: "e deleting one record leaves the subject's row in another community and another subject's row",
			steps: []rosterStep{
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revA, cid: "bafyreirostere1",
				},
				{
					repo: rosterCommunityB, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revA, cid: "bafyreirostere2",
				},
				{
					repo: rosterCommunityA, op: "create", subject: rosterSubject2, rkey: rosterSubject2Key,
					role: "moderator", createdAt: rosterTime2, rev: revB, cid: "bafyreirostere3",
				},
				{repo: rosterCommunityA, op: "delete", rkey: rosterSubject1Key, rev: revC},
			},
			want: map[string][]communities.CommunityRole{
				rosterCommunityA: {{
					CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rostersubject2", Role: "moderator",
					RecordKey: rosterSubject2Key, RecordCID: "bafyreirostere3", RecordCreatedAt: rosterT2,
				}},
				rosterCommunityB: {{
					CommunityDID: "did:plc:rostercommunityb", SubjectDID: "did:plc:rostersubject1", Role: "moderator",
					RecordKey: rosterSubject1Key, RecordCID: "bafyreirostere2", RecordCreatedAt: rosterT1,
				}},
			},
		},
		{
			name: "f a record with an unknown role is stored as written",
			steps: []rosterStep{
				{
					repo: rosterCommunityA, op: "create", subject: rosterAdmin, rkey: rosterAdminKey,
					role: "admin", createdAt: rosterTime1, rev: revA, cid: "bafyreirosterf",
				},
			},
			want: map[string][]communities.CommunityRole{
				rosterCommunityA: {{
					CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rosteradmin1", Role: "admin",
					RecordKey: rosterAdminKey, RecordCID: "bafyreirosterf", RecordCreatedAt: rosterT1,
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			consumer, roles := newRosterConsumer(t, db)
			deliverRosterSteps(t, consumer, tt.steps)

			for community, want := range tt.want {
				got, err := roles.ListCommunityRoles(context.Background(), community)
				require.NoError(t, err)
				assert.ElementsMatchf(t, want, normalizeRoles(t, got), "roles indexed in %s", community)
			}
		})
	}
}

func TestCommunityRoleOwnerRule(t *testing.T) {
	t.Parallel()

	early := rosterStep{
		repo: rosterCommunityA, op: "create", subject: rosterEarly, rkey: rosterEarlyKey,
		role: "owner", createdAt: rosterTime1, rev: revA, cid: "bafyreirosterearly",
	}
	late := rosterStep{
		repo: rosterCommunityA, op: "create", subject: rosterLate, rkey: rosterLateKey,
		role: "owner", createdAt: rosterTime2, rev: revA, cid: "bafyreirosterlate",
	}
	tieA := rosterStep{
		repo: rosterCommunityA, op: "create", subject: rosterTieA, rkey: rosterTieAKey,
		role: "owner", createdAt: rosterTime1, rev: revA, cid: "bafyreirostertiea",
	}
	tieB := rosterStep{
		repo: rosterCommunityA, op: "create", subject: rosterTieB, rkey: rosterTieBKey,
		role: "owner", createdAt: rosterTime1, rev: revA, cid: "bafyreirostertieb",
	}
	admin := rosterStep{
		repo: rosterCommunityA, op: "create", subject: rosterAdmin, rkey: rosterAdminKey,
		role: "admin", createdAt: rosterTime2, rev: revA, cid: "bafyreirosteradmin",
	}

	lateOwner := communities.CommunityRole{
		CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rosterlate", Role: "owner",
		RecordKey: rosterLateKey, RecordCID: "bafyreirosterlate", RecordCreatedAt: rosterT2,
	}
	earlyAsModerator := communities.CommunityRole{
		CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rosterearly", Role: "moderator",
		RecordKey: rosterEarlyKey, RecordCID: "bafyreirosterearly", RecordCreatedAt: rosterT1,
	}
	tieBOwner := communities.CommunityRole{
		CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rosterownertieb", Role: "owner",
		RecordKey: rosterTieBKey, RecordCID: "bafyreirostertieb", RecordCreatedAt: rosterT1,
	}
	tieAAsModerator := communities.CommunityRole{
		CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rosterownertiea", Role: "moderator",
		RecordKey: rosterTieAKey, RecordCID: "bafyreirostertiea", RecordCreatedAt: rosterT1,
	}

	tests := []struct {
		name           string
		steps          []rosterStep
		wantOwner      communities.CommunityRole
		wantModerators []communities.CommunityRole
	}{
		{
			"later createdAt wins, earlier owner delivered first",
			[]rosterStep{early, late, admin},
			lateOwner,
			[]communities.CommunityRole{earlyAsModerator},
		},
		{
			"later createdAt wins, later owner delivered first",
			[]rosterStep{admin, late, early},
			lateOwner,
			[]communities.CommunityRole{earlyAsModerator},
		},
		{
			"equal createdAt, greatest record key wins, smaller key delivered first",
			[]rosterStep{tieA, tieB, admin},
			tieBOwner,
			[]communities.CommunityRole{tieAAsModerator},
		},
		{
			"equal createdAt, greatest record key wins, greater key delivered first",
			[]rosterStep{tieB, admin, tieA},
			tieBOwner,
			[]communities.CommunityRole{tieAAsModerator},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			consumer, roles := newRosterConsumer(t, db)
			deliverRosterSteps(t, consumer, tt.steps)

			roster, err := communities.ReadRoster(context.Background(), roles, rosterCommunityA)
			require.NoError(t, err)
			require.NotNil(t, roster.Owner, "the community must have an owner")
			assert.Equal(t, tt.wantOwner, normalizeRole(t, *roster.Owner))
			// The admin record is neither the owner nor a moderator.
			assert.ElementsMatch(t, tt.wantModerators, normalizeRoles(t, roster.Moderators))
		})
	}
}

// An update that fails validation permanently is refused, and it also removes
// the role the record held before: the record no longer grants anything, so a
// rebuilt index would hold no row for it. The removal is rev-gated, so an
// invalid event older than the indexed record leaves the record's role alone.
func TestCommunityRoleInvalidUpdateRemovesRole(t *testing.T) {
	t.Parallel()

	validCreate := func(rev string) rosterStep {
		return rosterStep{
			repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
			role: "moderator", createdAt: rosterTime1, rev: rev, cid: "bafyreirostervalid",
		}
	}
	invalidUpdate := func(rev string) map[string]rosterStep {
		base := rosterStep{
			repo: rosterCommunityA, op: "update", subject: rosterSubject1, rkey: rosterSubject1Key,
			role: "owner", createdAt: rosterTime2, rev: rev, cid: "bafyreirosterinvalid",
		}
		changedSubject, badCreatedAt, emptyRole, tooLongRole, nonDIDSubject := base, base, base, base, base
		changedSubject.subject = rosterSubject2
		badCreatedAt.createdAt = "not-a-datetime"
		emptyRole.role = ""
		tooLongRole.role = strings.Repeat("r", maxRoleBytes+1)
		nonDIDSubject.subject = "not-a-did"
		return map[string]rosterStep{
			"changed subject": changedSubject,
			"bad createdAt":   badCreatedAt,
			"empty role":      emptyRole,
			"too-long role":   tooLongRole,
			"non-DID subject": nonDIDSubject,
		}
	}
	keptRole := communities.CommunityRole{
		CommunityDID: "did:plc:rostercommunitya", SubjectDID: "did:plc:rostersubject1", Role: "moderator",
		RecordKey: rosterSubject1Key, RecordCID: "bafyreirostervalid", RecordCreatedAt: rosterT1,
	}

	for shape := range invalidUpdate(revB) {
		t.Run(shape+" at a newer rev removes the role", func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			consumer, roles := newRosterConsumer(t, db)
			ctx := context.Background()
			base := time.Now().UnixMicro()

			deliverRosterSteps(t, consumer, []rosterStep{validCreate(revA)})
			err := consumer.HandleEvent(ctx, rosterCommitEvent(invalidUpdate(revB)[shape], base+1_000_000))
			require.Truef(t, errors.Is(err, ErrPermanentEvent), "HandleEvent() error = %v, want ErrPermanentEvent", err)

			got, err := roles.ListCommunityRoles(ctx, rosterCommunityA)
			require.NoError(t, err)
			assert.Empty(t, got, "the invalid update removes the role the record held")

			// The removal advanced the rev gate, so the valid create replayed
			// at its older rev does not bring the role back.
			deliverRosterSteps(t, consumer, []rosterStep{validCreate(revA)})
			got, err = roles.ListCommunityRoles(ctx, rosterCommunityA)
			require.NoError(t, err)
			assert.Empty(t, got, "a stale valid create does not restore the removed role")
		})

		t.Run(shape+" at an older rev leaves the newer role", func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			consumer, roles := newRosterConsumer(t, db)
			ctx := context.Background()
			base := time.Now().UnixMicro()

			deliverRosterSteps(t, consumer, []rosterStep{validCreate(revB)})
			err := consumer.HandleEvent(ctx, rosterCommitEvent(invalidUpdate(revA)[shape], base+1_000_000))
			require.Truef(t, errors.Is(err, ErrPermanentEvent), "HandleEvent() error = %v, want ErrPermanentEvent", err)

			got, err := roles.ListCommunityRoles(ctx, rosterCommunityA)
			require.NoError(t, err)
			assert.ElementsMatch(t, []communities.CommunityRole{keptRole}, normalizeRoles(t, got),
				"a stale invalid update does not remove the newer role")
		})
	}
}
