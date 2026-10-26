//go:build integration

package postgres

import (
	"context"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestBlobReferenceRepository_HasActiveReference(t *testing.T) {
	const (
		requestedCID = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
		otherCID     = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	)
	type referenceRow struct {
		otherOwner         bool
		cid, subject, kind string
		active             bool
	}
	for _, test := range []struct {
		name string
		rows []referenceRow
		want bool
	}{
		{name: "active post image", rows: []referenceRow{{cid: requestedCID, subject: "post", kind: "post_image", active: true}}, want: true},
		{name: "active avatar", rows: []referenceRow{{cid: requestedCID, subject: "profile", kind: "avatar", active: true}}, want: true},
		{name: "active banner", rows: []referenceRow{{cid: requestedCID, subject: "profile", kind: "banner", active: true}}, want: true},
		{name: "only inactive subjects", rows: []referenceRow{
			{cid: requestedCID, subject: "one", kind: "post_image"},
			{cid: requestedCID, subject: "two", kind: "post_image"},
		}},
		{name: "one active among inactive", rows: []referenceRow{
			{cid: requestedCID, subject: "one", kind: "post_image"},
			{cid: requestedCID, subject: "two", kind: "post_image", active: true},
		}, want: true},
		{name: "active for another owner", rows: []referenceRow{{otherOwner: true, cid: requestedCID, subject: "post", kind: "post_image", active: true}}},
		{name: "active for another CID", rows: []referenceRow{{cid: otherCID, subject: "post", kind: "post_image", active: true}}},
		{name: "no rows"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			owner := "did:plc:" + testkit.UniqueID(t)
			otherOwner := "did:plc:" + testkit.UniqueID(t)
			for _, row := range test.rows {
				rowOwner := owner
				if row.otherOwner {
					rowOwner = otherOwner
				}
				_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
					VALUES ($1, $2, $3, $4, $5)`, rowOwner, row.cid,
					"at://"+rowOwner+"/social.coves.community.postv2/"+row.subject, row.kind, row.active)
				require.NoError(t, err)
			}
			active, err := NewBlobReferenceRepository(db).HasActiveReference(context.Background(), owner, requestedCID)
			require.NoError(t, err)
			require.Equal(t, test.want, active)
		})
	}
}
