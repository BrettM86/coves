//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rejectCommunitySubjectVersionWrites makes every write to
// community_subject_versions in db fail with SQLSTATE P4242.
func rejectCommunitySubjectVersionWrites(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`CREATE FUNCTION reject_community_subject_version() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'reject community subject version write' USING ERRCODE = 'P4242'; END; $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER reject_community_subject_version
		BEFORE INSERT OR UPDATE OR DELETE ON community_subject_versions
		FOR EACH ROW EXECUTE FUNCTION reject_community_subject_version()`)
	require.NoError(t, err, "installing the rejecting trigger on community_subject_versions")
}

// B12: the version advance runs in the transaction that makes the index
// change, so a failed version write rolls the index change back with it.
func TestCommunitySubjectVersions_FailedVersionWriteRollsBackTheIndexChange(t *testing.T) {
	t.Parallel()

	const (
		roleCommunity = "did:plc:csrollbackcommunity"
		roleSubject   = "did:plc:csrollbacksubject"
		roleKey       = "csrollbackrolekey"
		// The owner record's at-URI, whose rev gate the local index call claims.
		roleGateURI = "at://did:plc:csrollbackcommunity/social.coves.community.moderator/csrollbackrolekey"
	)
	role := communities.CommunityRole{
		CommunityDID: roleCommunity, SubjectDID: roleSubject, Role: communities.RoleOwner,
		RecordKey: roleKey, RecordCID: "bafyreicsrollbackrole", RecordCreatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name string
		// prepare seeds what the write needs and returns the write and a check
		// that the index is as it was before the write.
		prepare func(t *testing.T, db *sql.DB) (write func(ctx context.Context) error, unchanged func(t *testing.T))
	}{
		{
			name: "admission acceptance",
			prepare: func(t *testing.T, db *sql.DB) (func(context.Context) error, func(*testing.T)) {
				repo := NewAdmissionRepository(db)
				subject, cid := newWithdrawalSubject(t, db)
				_, err := repo.UpsertPending(context.Background(), posts.UpsertPendingCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: cid,
				})
				require.NoError(t, err)
				before, err := repo.Get(context.Background(), subject.CommunityDID, subject.PostURI)
				require.NoError(t, err)
				uri, rkey := acceptanceRecord(t, subject.CommunityDID)
				return func(ctx context.Context) error {
						_, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
							CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
							AcceptanceURI: uri, AcceptanceRkey: rkey, PinnedCID: cid,
							Watermark: posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
						})
						return err
					}, func(t *testing.T) {
						after, err := repo.Get(context.Background(), subject.CommunityDID, subject.PostURI)
						require.NoError(t, err)
						assert.Equal(t, before, after, "the admission row and its watermark must roll back")
						assert.Equal(t, posts.AdmissionStatusPending, after.Status)
					}
			},
		},
		{
			name: "comment removal record",
			prepare: func(t *testing.T, db *sql.DB) (func(context.Context) error, func(*testing.T)) {
				repo := NewCommunityCommentRemovalRepository(db)
				thread := seedCCRRepoThread(t, db)
				const recordKey = "csrollbackremoval"
				return func(ctx context.Context) error {
						_, err := repo.RecordRemoval(ctx, ccrRepoRemovalFrom(thread.communityDID, recordKey, testkit.TID()))
						return err
					}, func(t *testing.T) {
						var mappings int
						require.NoError(t, db.QueryRow(`
							SELECT COUNT(*) FROM community_comment_removals WHERE repository_did = $1 AND record_key = $2`,
							thread.communityDID, recordKey).Scan(&mappings))
						assert.Zero(t, mappings, "the removal record must not stay indexed")
						assert.Zero(t, ccrRepoActiveDecisions(t, db, thread.communityDID), "no removal decision may stay applied")
						assert.Empty(t, ccrRepoActions(t, db), "no moderation action may stay logged")
					}
			},
		},
		{
			name: "community ban upsert",
			prepare: func(t *testing.T, db *sql.DB) (func(context.Context) error, func(*testing.T)) {
				insertRoleTestCommunity(t, db, roleCommunity)
				repo := NewCommunityBanRepository(db)
				return func(ctx context.Context) error {
						return repo.UpsertCommunityBan(ctx, communities.CommunityBan{
							CommunityDID: roleCommunity, SubjectDID: roleSubject,
							RecordKey: "csrollbackbankey", RecordCID: "bafyreicsrollbackban", Reason: "spam",
						})
					}, func(t *testing.T) {
						_, err := repo.GetCommunityBan(context.Background(), roleCommunity, roleSubject)
						assert.ErrorIs(t, err, communities.ErrCommunityBanNotFound, "the ban must not stay indexed")
					}
			},
		},
		{
			name: "community role upsert",
			prepare: func(t *testing.T, db *sql.DB) (func(context.Context) error, func(*testing.T)) {
				insertRoleTestCommunity(t, db, roleCommunity)
				repo := NewCommunityRoleRepository(db)
				return func(ctx context.Context) error {
						return repo.UpsertCommunityRole(ctx, role)
					}, func(t *testing.T) {
						assert.Zero(t, countIndexedRoles(t, db, roleCommunity), "the role must not stay indexed")
					}
			},
		},
		{
			name: "local community role index at a rev",
			prepare: func(t *testing.T, db *sql.DB) (func(context.Context) error, func(*testing.T)) {
				insertRoleTestCommunity(t, db, roleCommunity)
				repo := NewCommunityRoleRepository(db)
				return func(ctx context.Context) error {
						_, err := repo.UpsertCommunityRoleAtRev(ctx, role, testkit.TID())
						return err
					}, func(t *testing.T) {
						assert.Zero(t, countIndexedRoles(t, db, roleCommunity), "the role must not stay indexed")
						var claims int
						require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM jetstream_record_revs WHERE record_uri = $1`,
							roleGateURI).Scan(&claims))
						assert.Zero(t, claims, "the owner record's rev claim must roll back")
					}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			write, unchanged := tt.prepare(t, db)
			rejectCommunitySubjectVersionWrites(t, db)

			err := write(context.Background())
			require.Error(t, err, "the write must fail when its version write fails")
			var postgresError *pq.Error
			require.Truef(t, errors.As(err, &postgresError), "the write must report the version write failure, got %v", err)
			assert.Equal(t, pqerror.Code("P4242"), postgresError.Code, "the failure must be the rejected version write")
			unchanged(t)
		})
	}
}
