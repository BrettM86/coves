//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/embedblobs"
	"Coves/internal/core/blobreferences"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

var errPostBlobReferenceWrite = errors.New("reference write failed after delegation")

type failingPostBlobReferences struct {
	blobreferences.Repository
	fail bool
}

func (r *failingPostBlobReferences) ReplaceSubjectTx(ctx context.Context, tx *sql.Tx, ownerDID, subjectURI string, references []embedblobs.Reference) error {
	if err := r.Repository.ReplaceSubjectTx(ctx, tx, ownerDID, subjectURI, references); err != nil {
		return err
	}
	if r.fail {
		return errPostBlobReferenceWrite
	}
	return nil
}

func (r *failingPostBlobReferences) DeactivateSubjectTx(ctx context.Context, tx *sql.Tx, subjectURI string) error {
	if err := r.Repository.DeactivateSubjectTx(ctx, tx, subjectURI); err != nil {
		return err
	}
	if r.fail {
		return errPostBlobReferenceWrite
	}
	return nil
}

func TestPostV2BlobReferences_FailedWriteRollsBackPostReferencesAndRevision(t *testing.T) {
	const (
		imageA     = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
		imageB     = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
		imageNew   = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
		initialCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
		updatedCID = "bafyreipv2blobedit2"
	)
	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			db := testkit.DB(t)
			f := newPV2Fixture(t, db)
			references := &failingPostBlobReferences{Repository: postgres.NewBlobReferenceRepository(db)}
			f.consumer = NewPostEventConsumer(
				postgres.NewPostRepository(db), postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
				f.users, db, WithAdmissions(f.admissions),
				WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)), WithPostBlobReferences(references),
			)
			rkey := "blobfailed" + operation
			uri := pv2URI(pv2Author, rkey)
			revs := increasingTIDs(t, 2)
			base := time.Now().UnixMicro()
			original := []postBlobRow{
				{pv2Author, imageA, "post_image", true},
				{pv2Author, imageB, "post_image", true},
			}
			if operation != "create" {
				createPostBlob(t, f, pv2Author, rkey, revs[0], base, postBlobImages(imageA, imageB))
			}
			var originalEmbed string
			if operation == "update" {
				require.NoError(t, db.QueryRow(`SELECT embed::text FROM posts WHERE uri = $1`, uri).Scan(&originalEmbed))
			}
			var event *JetstreamEvent
			switch operation {
			case "create":
				event = pv2Event(pv2Author, "create", rkey, revs[0], initialCID, base,
					postBlobRecord(pv2Community, postBlobImages(imageA, imageB)))
			case "update":
				event = pv2Event(pv2Author, "update", rkey, revs[1], updatedCID, base+1_000_000,
					postBlobRecord(pv2Community, postBlobImages(imageA, imageNew)))
			case "delete":
				event = pv2Event(pv2Author, "delete", rkey, revs[1], "", base+1_000_000, nil)
			}
			references.fail = true
			err := f.consumer.HandleEvent(context.Background(), event)
			require.Error(t, err, "reference failure must abort the %s event", operation)
			require.ErrorIs(t, err, errPostBlobReferenceWrite)
			if operation == "create" {
				require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
				require.Empty(t, readPostBlobRows(t, db, uri))
			} else {
				_, _, cid, _, deletedAt := readPV2Post(t, db, uri)
				require.Equal(t, initialCID, cid)
				require.Nil(t, deletedAt)
				if operation == "update" {
					var embed string
					require.NoError(t, db.QueryRow(`SELECT embed::text FROM posts WHERE uri = $1`, uri).Scan(&embed))
					require.Equal(t, originalEmbed, embed)
				}
				require.ElementsMatch(t, original, readPostBlobRows(t, db, uri))
			}

			references.fail = false
			require.NoError(t, f.consumer.HandleEvent(context.Background(), event), "identical %s event must retry after rollback", operation)
			_, _, cid, _, deletedAt := readPV2Post(t, db, uri)
			switch operation {
			case "create":
				require.Equal(t, initialCID, cid)
				require.Nil(t, deletedAt)
				require.ElementsMatch(t, original, activePostBlobRows(readPostBlobRows(t, db, uri)))
			case "update":
				require.Equal(t, updatedCID, cid)
				require.Nil(t, deletedAt)
				require.ElementsMatch(t, []postBlobRow{
					{pv2Author, imageA, "post_image", true},
					{pv2Author, imageNew, "post_image", true},
				}, activePostBlobRows(readPostBlobRows(t, db, uri)))
			case "delete":
				require.NotNil(t, deletedAt)
				require.ElementsMatch(t, []postBlobRow{
					{pv2Author, imageA, "post_image", false},
					{pv2Author, imageB, "post_image", false},
				}, readPostBlobRows(t, db, uri))
			}
		})
	}
}

func TestPostV2BlobReferences_InstanceRemovalKeepsReferencesActiveThroughEdit(t *testing.T) {
	h := newEditMediaHarness(t, true)
	revs := increasingTIDs(t, 2)
	rkey := testkit.TID()
	uri := pv2URI(pv2Author, rkey)
	base := time.Now().Add(-time.Minute).UnixMicro()
	const initialCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
	require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", rkey, revs[0], initialCID,
		base, postModerationRecord(postModerationCIDOne, postModerationCIDTwo))))
	h.remove(t, moderation.StrongRef{URI: uri, CID: initialCID}, "v0")
	require.Equal(t, 1, countRows(t, h.db, `SELECT count(*) FROM moderation_decisions
		WHERE subject_uri = $1 AND scope_kind = 'instance' AND kind = 'removal' AND active`, uri))
	require.ElementsMatch(t, []postBlobRow{
		{pv2Author, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
		{pv2Author, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "post_image", true},
	}, readPostBlobRows(t, h.db, uri))

	const editedCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "update", rkey, revs[1], editedCID,
		base+1_000_000, postModerationRecord(postModerationCIDOne, postModerationCIDThree))))
	_, _, cid, _, deletedAt := readPV2Post(t, h.db, uri)
	require.Equal(t, editedCID, cid)
	require.Nil(t, deletedAt)
	require.Equal(t, 1, countRows(t, h.db, `SELECT count(*) FROM moderation_decisions
		WHERE subject_uri = $1 AND scope_kind = 'instance' AND kind = 'removal' AND active`, uri))
	require.ElementsMatch(t, []postBlobRow{
		{pv2Author, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
		{pv2Author, "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku", "post_image", true},
	}, activePostBlobRows(readPostBlobRows(t, h.db, uri)))
}
