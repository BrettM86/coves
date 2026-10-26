//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type postBlobRow struct {
	owner  string
	cid    string
	kind   string
	active bool
}

func newPostBlobFixture(t *testing.T, db *sql.DB) pv2Fixture {
	t.Helper()
	f := newPV2Fixture(t, db)
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		f.users, db,
		WithAdmissions(f.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostBlobReferences(postgres.NewBlobReferenceRepository(db)),
	)
	return f
}

func postBlob(cid string) map[string]interface{} {
	return map[string]interface{}{
		"$type": "blob", "ref": map[string]interface{}{"$link": cid},
		"mimeType": "image/jpeg", "size": 10,
	}
}

func postBlobImages(cids ...string) map[string]interface{} {
	images := make([]interface{}, 0, len(cids))
	for _, cid := range cids {
		images = append(images, map[string]interface{}{"image": postBlob(cid), "alt": ""})
	}
	return map[string]interface{}{"$type": "social.coves.embed.images", "images": images}
}

func postBlobRecord(community string, embed map[string]interface{}) map[string]interface{} {
	record := pv2Record(community, "post with media", "body")
	if embed != nil {
		record["embed"] = embed
	}
	return record
}

func readPostBlobRows(t *testing.T, db *sql.DB, uri string) []postBlobRow {
	t.Helper()
	rows, err := db.Query(`SELECT owner_did, blob_cid, kind, active FROM blob_references WHERE subject_uri = $1`, uri)
	require.NoError(t, err)
	defer rows.Close()
	var found []postBlobRow
	for rows.Next() {
		var row postBlobRow
		require.NoError(t, rows.Scan(&row.owner, &row.cid, &row.kind, &row.active))
		found = append(found, row)
	}
	require.NoError(t, rows.Err())
	return found
}

func activePostBlobRows(rows []postBlobRow) []postBlobRow {
	var active []postBlobRow
	for _, row := range rows {
		if row.active {
			active = append(active, row)
		}
	}
	return active
}

func requireUniquePostBlobRows(t *testing.T, rows []postBlobRow) {
	t.Helper()
	seen := make(map[[3]string]bool)
	for _, row := range rows {
		key := [3]string{row.owner, row.cid, row.kind} // subject_uri is fixed by the reader
		require.False(t, seen[key], "duplicate reference for owner, CID, subject, kind: %v", key)
		seen[key] = true
	}
}

func createPostBlob(t *testing.T, f pv2Fixture, author, rkey, rev string, timeUS int64, embed map[string]interface{}) string {
	t.Helper()
	uri := pv2URI(author, rkey)
	require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
		author, "create", rkey, rev, "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", timeUS,
		postBlobRecord(pv2Community, embed),
	)))
	_, _, _, _, deletedAt := readPV2Post(t, f.db, uri)
	require.Nil(t, deletedAt)
	return uri
}

func TestPostV2BlobReferences_CreateImages(t *testing.T) {
	db := testkit.DB(t)
	f := newPostBlobFixture(t, db)
	uri := createPostBlob(t, f, pv2Author, "blobimages", testkit.TID(), time.Now().UnixMicro(), postBlobImages(
		"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
		"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si",
		"bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q",
	))
	require.ElementsMatch(t, []postBlobRow{
		{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
		{"did:plc:pv2author", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image", true},
		{"did:plc:pv2author", "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "post_image", true},
	}, readPostBlobRows(t, db, uri))
}

func TestPostV2BlobReferences_CreateExternalAndVideo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		embed map[string]interface{}
		want  []postBlobRow
	}{
		{
			name: "external thumbnail and gallery",
			embed: map[string]interface{}{"$type": "social.coves.embed.external", "external": map[string]interface{}{
				"uri":    "https://example.invalid/article",
				"thumb":  postBlob("bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"),
				"images": []interface{}{map[string]interface{}{"image": postBlob("bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si")}},
			}},
			want: []postBlobRow{
				{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "external_thumb", true},
				{"did:plc:pv2author", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image", true},
			},
		},
		{
			name: "video thumbnail excludes video blob",
			embed: map[string]interface{}{
				"$type":     "social.coves.embed.video",
				"video":     postBlob("bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"),
				"thumbnail": postBlob("bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"),
			},
			want: []postBlobRow{
				{"did:plc:pv2author", "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "video_thumb", true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			f := newPostBlobFixture(t, db)
			uri := createPostBlob(t, f, pv2Author, "blobmedia", testkit.TID(), time.Now().UnixMicro(), tc.embed)
			require.ElementsMatch(t, tc.want, readPostBlobRows(t, db, uri))
		})
	}
}

func seedPostBlobNeighbors(t *testing.T, f pv2Fixture, base int64, revs []string) (string, string) {
	t.Helper()
	insertBridgedUser(t, f.db, pv2Other, "pv2otherauthor.test")
	f.users.users[pv2Other] = &users.User{DID: pv2Other, Handle: "pv2otherauthor.test"}
	sibling := createPostBlob(t, f, pv2Author, "blobsibling", revs[0], base, postBlobImages(
		"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"))
	other := createPostBlob(t, f, pv2Other, "blobother", revs[1], base+1, postBlobImages(
		"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"))
	return sibling, other
}

func requirePostBlobNeighbors(t *testing.T, db *sql.DB, sibling, other string) {
	t.Helper()
	require.ElementsMatch(t, []postBlobRow{{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true}}, readPostBlobRows(t, db, sibling))
	require.ElementsMatch(t, []postBlobRow{{"did:plc:pv2otherauthor", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true}}, readPostBlobRows(t, db, other))
}

func TestPostV2BlobReferences_EditReplacesSubjectOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dropEmbed bool
	}{
		{name: "replace and reactivate"},
		{name: "drop embed", dropEmbed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			f := newPostBlobFixture(t, db)
			base := time.Now().UnixMicro()
			revs := increasingTIDs(t, 5)
			uri := createPostBlob(t, f, pv2Author, "blobedit", revs[2], base+2, postBlobImages(
				"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
				"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"))
			sibling, other := seedPostBlobNeighbors(t, f, base, revs)
			require.ElementsMatch(t, []postBlobRow{
				{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
				{"did:plc:pv2author", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image", true},
			}, readPostBlobRows(t, db, uri))
			requirePostBlobNeighbors(t, db, sibling, other)

			var embed map[string]interface{}
			if !tc.dropEmbed {
				embed = postBlobImages("bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q")
			}
			require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
				pv2Author, "update", "blobedit", revs[3], "bafyreipv2blobedit2", base+3,
				postBlobRecord(pv2Community, embed),
			)))
			_, _, storedCID, _, _ := readPV2Post(t, db, uri)
			require.Equal(t, "bafyreipv2blobedit2", storedCID)
			if tc.dropEmbed {
				require.Empty(t, activePostBlobRows(readPostBlobRows(t, db, uri)))
			} else {
				require.ElementsMatch(t, []postBlobRow{
					{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
					{"did:plc:pv2author", "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "post_image", true},
				}, activePostBlobRows(readPostBlobRows(t, db, uri)))
			}
			requirePostBlobNeighbors(t, db, sibling, other)
			if tc.dropEmbed {
				return
			}

			require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
				pv2Author, "update", "blobedit", revs[4], "bafyreipv2blobedit3", base+4,
				postBlobRecord(pv2Community, postBlobImages(
					"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
					"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si")),
			)))
			_, _, storedCID, _, _ = readPV2Post(t, db, uri)
			require.Equal(t, "bafyreipv2blobedit3", storedCID)
			rows := readPostBlobRows(t, db, uri)
			require.ElementsMatch(t, []postBlobRow{
				{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
				{"did:plc:pv2author", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image", true},
			}, activePostBlobRows(rows))
			requireUniquePostBlobRows(t, rows)
			requirePostBlobNeighbors(t, db, sibling, other)
		})
	}
}

func TestPostV2BlobReferences_DeleteDeactivatesSubjectOnly(t *testing.T) {
	db := testkit.DB(t)
	f := newPostBlobFixture(t, db)
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 4)
	uri := createPostBlob(t, f, pv2Author, "blobdelete", revs[2], base+2, postBlobImages(
		"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
		"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"))
	sibling, other := seedPostBlobNeighbors(t, f, base, revs)
	require.ElementsMatch(t, []postBlobRow{
		{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
		{"did:plc:pv2author", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image", true},
	}, readPostBlobRows(t, db, uri))
	requirePostBlobNeighbors(t, db, sibling, other)
	require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
		pv2Author, "delete", "blobdelete", revs[3], "", base+3, nil,
	)))
	_, _, _, _, deletedAt := readPV2Post(t, db, uri)
	require.NotNil(t, deletedAt)
	require.ElementsMatch(t, []postBlobRow{
		{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", false},
		{"did:plc:pv2author", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image", false},
	}, readPostBlobRows(t, db, uri))
	requirePostBlobNeighbors(t, db, sibling, other)
}

func TestPostV2BlobReferences_SkippedEventsDoNotChangeReferences(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, f pv2Fixture)
	}{
		{
			name: "unknown community create",
			run: func(t *testing.T, f pv2Fixture) {
				uri := pv2URI(pv2Author, "blobunknowncommunity")
				require.Error(t, f.consumer.HandleEvent(context.Background(), pv2Event(
					pv2Author, "create", "blobunknowncommunity", testkit.TID(), "bafyreipv2blobunknown", time.Now().UnixMicro(),
					postBlobRecord("did:plc:pv2ghostcommunity", postBlobImages("bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm")),
				)))
				require.Zero(t, countRows(t, f.db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
				require.Empty(t, readPostBlobRows(t, f.db, uri))
			},
		},
		{
			name: "erased author create",
			run: func(t *testing.T, f pv2Fixture) {
				markAccountDeleted(t, f.db, pv2Other)
				uri := pv2URI(pv2Other, "bloberasedcreate")
				require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
					pv2Other, "create", "bloberasedcreate", testkit.TID(), "bafyreipv2bloberased", time.Now().UnixMicro(),
					postBlobRecord(pv2Community, postBlobImages("bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm")),
				)))
				require.Zero(t, countRows(t, f.db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
				require.Empty(t, readPostBlobRows(t, f.db, uri))
			},
		},
		{
			name: "stale revision edit with newer event time",
			run: func(t *testing.T, f pv2Fixture) {
				base := time.Now().UnixMicro()
				revs := increasingTIDs(t, 2)
				uri := createPostBlob(t, f, pv2Author, "blobstaleedit", revs[1], base, postBlobImages(
					"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"))
				original := []postBlobRow{{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true}}
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
				require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
					pv2Author, "update", "blobstaleedit", revs[0], "bafyreipv2staleblobedit", base+1_000_000,
					postBlobRecord(pv2Community, postBlobImages("bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si")),
				)))
				_, _, cid, _, deletedAt := readPV2Post(t, f.db, uri)
				require.Equal(t, "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", cid)
				require.Nil(t, deletedAt)
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
			},
		},
		{
			name: "community changing edit",
			run: func(t *testing.T, f pv2Fixture) {
				insertBridgedCommunity(t, f.db, "did:plc:pv2blobsecondcommunity", "pv2blobsecond.test", pv2Author)
				base := time.Now().UnixMicro()
				revs := increasingTIDs(t, 2)
				uri := createPostBlob(t, f, pv2Author, "blobretarget", revs[0], base, postBlobImages(
					"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"))
				original := []postBlobRow{{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true}}
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
				require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
					pv2Author, "update", "blobretarget", revs[1], "bafyreipv2blobretarget", base+1_000_000,
					postBlobRecord("did:plc:pv2blobsecondcommunity", postBlobImages("bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si")),
				)))
				_, community, cid, _, _ := readPV2Post(t, f.db, uri)
				require.Equal(t, pv2Community, community)
				require.Equal(t, "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", cid)
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
			},
		},
		{
			name: "edit after author delete",
			run: func(t *testing.T, f pv2Fixture) {
				base := time.Now().UnixMicro()
				revs := increasingTIDs(t, 3)
				uri := createPostBlob(t, f, pv2Author, "bloberasededit", revs[0], base, postBlobImages(
					"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"))
				original := []postBlobRow{{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true}}
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
				require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
					pv2Author, "delete", "bloberasededit", revs[1], "", base+1_000_000, nil,
				)))
				_, _, _, _, deletedAt := readPV2Post(t, f.db, uri)
				require.NotNil(t, deletedAt)
				inactive := []postBlobRow{{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", false}}
				require.ElementsMatch(t, inactive, readPostBlobRows(t, f.db, uri))
				require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
					pv2Author, "update", "bloberasededit", revs[2], "bafyreipv2bloberasededit", base+2_000_000,
					postBlobRecord(pv2Community, postBlobImages("bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si")),
				)))
				_, _, _, _, deletedAt = readPV2Post(t, f.db, uri)
				require.NotNil(t, deletedAt)
				require.ElementsMatch(t, inactive, readPostBlobRows(t, f.db, uri))
			},
		},
		{
			name: "stale revision delete with newer event time",
			run: func(t *testing.T, f pv2Fixture) {
				base := time.Now().UnixMicro()
				revs := increasingTIDs(t, 2)
				uri := createPostBlob(t, f, pv2Author, "blobstaledelete", revs[1], base, postBlobImages(
					"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"))
				original := []postBlobRow{{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true}}
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
				require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
					pv2Author, "delete", "blobstaledelete", revs[0], "", base+1_000_000, nil,
				)))
				_, _, _, _, deletedAt := readPV2Post(t, f.db, uri)
				require.Nil(t, deletedAt)
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
			},
		},
		{
			// The ON CONFLICT DO NOTHING branch of the create path commits its
			// transaction while discarding the incoming content, so a newer-rev
			// create for an existing row must not write the discarded embed's
			// references or deactivate the indexed post's.
			name: "newer revision create for an existing row",
			run: func(t *testing.T, f pv2Fixture) {
				base := time.Now().UnixMicro()
				revs := increasingTIDs(t, 2)
				uri := createPostBlob(t, f, pv2Author, "blobexistingcreate", revs[0], base, postBlobImages(
					"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
					"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"))
				original := []postBlobRow{
					{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
					{"did:plc:pv2author", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image", true},
				}
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
				embed, err := json.Marshal(postBlobImages("bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"))
				require.NoError(t, err)
				embedJSON := string(embed)
				applied, err := f.consumer.indexPostIfRevWins(context.Background(), &posts.Post{
					URI: uri, CID: "bafyreipv2blobexistingcreate", RKey: "blobexistingcreate", AuthorDID: pv2Author,
					CommunityDID: pv2Community, Embed: &embedJSON,
					CreatedAt: time.Now(), IndexedAt: time.Now(),
				}, revs[1])
				require.NoError(t, err)
				require.False(t, applied, "an existing row must keep its content")
				_, _, cid, _, deletedAt := readPV2Post(t, f.db, uri)
				require.Equal(t, "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", cid)
				require.Nil(t, deletedAt)
				require.ElementsMatch(t, original, readPostBlobRows(t, f.db, uri))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			f := newPostBlobFixture(t, db)
			tc.run(t, f)
		})
	}
}

func postBlobExternalImages(cids ...string) map[string]interface{} {
	images := make([]interface{}, 0, len(cids))
	for _, cid := range cids {
		images = append(images, map[string]interface{}{"image": postBlob(cid), "alt": ""})
	}
	return map[string]interface{}{"$type": "social.coves.embed.external", "external": map[string]interface{}{
		"uri": "https://example.invalid/gallery", "images": images,
	}}
}

func TestPostV2BlobReferences_RejectsMoreThanEightImages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		embed func(cids ...string) map[string]interface{}
	}{
		{name: "images embed", embed: postBlobImages},
		{name: "external embed gallery", embed: postBlobExternalImages},
	} {
		t.Run(tc.name+"/create with nine images", func(t *testing.T) {
			db := testkit.DB(t)
			f := newPostBlobFixture(t, db)
			uri := pv2URI(pv2Author, "blobninecreate")
			err := f.consumer.HandleEvent(context.Background(), pv2Event(
				pv2Author, "create", "blobninecreate", testkit.TID(), "bafyreipv2blobninecreate", time.Now().UnixMicro(),
				postBlobRecord(pv2Community, tc.embed(embedImageLimitCIDs(t, tc.name+" create", 9)...)),
			))
			require.True(t, errors.Is(err, ErrPermanentEvent), "expected permanent rejection, got %v", err)
			require.ErrorContains(t, err, "maximum of 8")
			require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
			require.Empty(t, readPostBlobRows(t, db, uri))
		})

		t.Run(tc.name+"/update with nine images", func(t *testing.T) {
			db := testkit.DB(t)
			f := newPostBlobFixture(t, db)
			base := time.Now().UnixMicro()
			revs := increasingTIDs(t, 2)
			uri := createPostBlob(t, f, pv2Author, "blobnineupdate", revs[0], base, postBlobImages(
				"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"))
			original := []postBlobRow{{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true}}
			require.ElementsMatch(t, original, readPostBlobRows(t, db, uri))
			err := f.consumer.HandleEvent(context.Background(), pv2Event(
				pv2Author, "update", "blobnineupdate", revs[1], "bafyreipv2blobnineupdate", base+1_000_000,
				postBlobRecord(pv2Community, tc.embed(embedImageLimitCIDs(t, tc.name+" update", 9)...)),
			))
			require.True(t, errors.Is(err, ErrPermanentEvent), "expected permanent rejection, got %v", err)
			require.ErrorContains(t, err, "maximum of 8")
			_, _, storedCID, _, deletedAt := readPV2Post(t, db, uri)
			require.Equal(t, "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", storedCID)
			require.Nil(t, deletedAt)
			require.ElementsMatch(t, original, readPostBlobRows(t, db, uri))
		})

		t.Run(tc.name+"/create with exactly eight images", func(t *testing.T) {
			db := testkit.DB(t)
			f := newPostBlobFixture(t, db)
			images := embedImageLimitCIDs(t, tc.name+" boundary", 8)
			uri := createPostBlob(t, f, pv2Author, "blobeight", testkit.TID(), time.Now().UnixMicro(), tc.embed(images...))
			want := make([]postBlobRow, 0, len(images))
			for _, image := range images {
				want = append(want, postBlobRow{"did:plc:pv2author", image, "post_image", true})
			}
			require.ElementsMatch(t, want, readPostBlobRows(t, db, uri))
		})
	}
}
