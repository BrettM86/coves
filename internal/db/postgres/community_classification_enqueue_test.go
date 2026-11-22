//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"Coves/internal/core/classification"
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installClassificationEnqueueFailure makes every later insert into
// classification_jobs raise.
func installClassificationEnqueueFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE FUNCTION test_fail_classification_job_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'test: forced classification enqueue failure';
		END $$`,
		`CREATE TRIGGER test_fail_classification_job_insert
			BEFORE INSERT ON classification_jobs
			FOR EACH ROW EXECUTE FUNCTION test_fail_classification_job_insert()`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "installing the classification enqueue failure trigger")
	}
}

// TestCommunityClassificationEnqueue_FailureRollsBackWrite (T1, community
// rows): the job insert runs in the community write transaction, so a failed
// enqueue fails the write and leaves the community row as it was.
func TestCommunityClassificationEnqueue_FailureRollsBackWrite(t *testing.T) {
	t.Parallel()
	evaluation := classification.Evaluation{
		PolicyVersion: "v1",
		TextModelID:   "@cf/cloudflare/clef-flash",
		ImageModelID:  "@cf/cloudflare/clef",
	}
	seeded := func(did, name string) *communities.Community {
		community := atomicityCommunity(did, name, nil)
		community.Description = "Original description"
		community.RecordCID = "bafyclsrollbacka"
		return community
	}

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, db *sql.DB, repo communities.Repository, did, name string)
	}{
		{
			name: "create",
			run: func(t *testing.T, db *sql.DB, repo communities.Repository, did, name string) {
				installClassificationEnqueueFailure(t, db)

				_, err := repo.Create(context.Background(), seeded(did, name))
				assert.Error(t, err, "a create whose enqueue fails must return an error")

				var rows int
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM communities WHERE did = $1`, did).Scan(&rows))
				assert.Equal(t, 0, rows, "the community row must be absent after the failed create")
			},
		},
		{
			name: "update",
			run: func(t *testing.T, db *sql.DB, repo communities.Repository, did, name string) {
				ctx := context.Background()
				_, err := repo.Create(ctx, seeded(did, name))
				require.NoError(t, err, "fixture: the community must index before the trigger is installed")
				installClassificationEnqueueFailure(t, db)

				stored, err := repo.GetByDID(ctx, did)
				require.NoError(t, err)
				stored.Description = "Edited description"
				stored.RecordCID = "bafyclsrollbackb"
				_, err = repo.Update(ctx, stored)
				assert.Error(t, err, "an update whose enqueue fails must return an error")

				var description, recordCID sql.NullString
				require.NoError(t, db.QueryRow(`SELECT description, record_cid FROM communities WHERE did = $1`, did).
					Scan(&description, &recordCID))
				assert.Equal(t, "Original description", description.String, "description must be unchanged after the failed update")
				assert.Equal(t, "bafyclsrollbacka", recordCID.String, "record_cid must be unchanged after the failed update")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			name := "clsrollback" + testkit.UniqueID(t)
			did := "did:plc:" + name
			repo := NewCommunityRepository(db, credentialciphertest.Fixed(), WithCommunityClassification(evaluation))

			tc.run(t, db, repo, did, name)
		})
	}
}
