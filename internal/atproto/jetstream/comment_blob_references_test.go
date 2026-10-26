//go:build integration

package jetstream

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

func newCommentBlobConsumer(db *sql.DB) *CommentEventConsumer {
	return NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentBridgeTrust(bridgeTrustForTests()),
		WithCommentBlobReferences(postgres.NewBlobReferenceRepository(db)))
}

func commentBlobImages(images ...interface{}) map[string]interface{} {
	entries := make([]interface{}, 0, len(images))
	for _, image := range images {
		entries = append(entries, map[string]interface{}{"image": image, "alt": ""})
	}
	return map[string]interface{}{"$type": "social.coves.embed.images", "images": entries}
}

func commentBlobEvent(postURI, postCID, operation, rkey, rev, recordCID string, timeUS int64, embed map[string]interface{}) *JetstreamEvent {
	var record map[string]interface{}
	if operation != "delete" {
		record = revCommentRecord("comment with images", postURI, postCID, postURI, postCID)
		if embed != nil {
			record["embed"] = embed
		}
	}
	return revCommitEvent(bridgedTestCommenter, CommentCollection, operation, rkey, rev, recordCID, timeUS, record)
}

func createCommentBlob(t *testing.T, consumer *CommentEventConsumer, postURI, postCID, rkey, rev, recordCID string, timeUS int64, embed map[string]interface{}) string {
	t.Helper()
	require.NoError(t, consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, "create", rkey, rev, recordCID, timeUS, embed)))
	return "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + rkey
}

func TestCommentBlobReferences_CreateImages(t *testing.T) {
	for _, test := range []struct {
		name     string
		recreate bool
	}{
		{name: "fresh insert with legacy blob"},
		{name: "author-deleted same-rkey re-create", recreate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			postURI, postCID := setupCommentThread(t, db)
			consumer := newCommentBlobConsumer(db)
			revs := increasingTIDs(t, 3)
			base := time.Now().UnixMicro()
			rkey := testkit.TID()
			uri := createCommentBlob(t, consumer, postURI, postCID, rkey, revs[0], "bafcommentblobcreate", base,
				commentBlobImages(
					postBlob("bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"),
					map[string]interface{}{"cid": "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "mimeType": "image/png"},
				))
			if !test.recreate {
				require.ElementsMatch(t, []postBlobRow{
					{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", true},
					{bridgedTestCommenter, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image", true},
				}, readPostBlobRows(t, db, uri))
				return
			}

			require.NoError(t, consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, "delete", rkey, revs[1], "", base+1_000_000, nil)))
			var deletedAt *time.Time
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT deleted_at FROM comments WHERE uri = $1`, uri).Scan(&deletedAt))
			require.NotNil(t, deletedAt)
			require.NoError(t, consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, "create", rkey, revs[2], "bafcommentblobrecreated", base+2_000_000,
				postBlobImages(
					"bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q",
					"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku",
				))))
			var storedCID string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT cid, deleted_at FROM comments WHERE uri = $1`, uri).Scan(&storedCID, &deletedAt))
			require.Equal(t, "bafcommentblobrecreated", storedCID)
			require.Nil(t, deletedAt)
			require.ElementsMatch(t, []postBlobRow{
				{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", false},
				{bridgedTestCommenter, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image", false},
				{bridgedTestCommenter, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "comment_image", true},
				{bridgedTestCommenter, "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku", "comment_image", true},
			}, readPostBlobRows(t, db, uri))
		})
	}
}

func TestCommentBlobReferences_QuotePostHasNoReferences(t *testing.T) {
	db := testkit.DB(t)
	postURI, postCID := setupCommentThread(t, db)
	consumer := newCommentBlobConsumer(db)
	uri := createCommentBlob(t, consumer, postURI, postCID, testkit.TID(), testkit.TID(), "bafcommentblobquote", time.Now().UnixMicro(),
		map[string]interface{}{"$type": "social.coves.embed.post", "post": map[string]interface{}{"uri": postURI, "cid": postCID}})
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, uri))
	require.Empty(t, readPostBlobRows(t, db, uri))
}

func TestCommentBlobReferences_EditReplacesSubjectOnly(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		dropEmbed bool
	}{
		{name: "update replaces images", operation: "update"},
		{name: "active same-rkey re-create replaces images", operation: "create"},
		{name: "update drops embed", operation: "update", dropEmbed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			postURI, postCID := setupCommentThread(t, db)
			consumer := newCommentBlobConsumer(db)
			revs := increasingTIDs(t, 3)
			base := time.Now().UnixMicro()
			const imageA = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
			const imageB = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
			const imageC = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
			rkey := testkit.TID()
			uri := createCommentBlob(t, consumer, postURI, postCID, rkey, revs[0], "bafcommentbloboriginal", base, postBlobImages(imageA, imageB))
			sibling := createCommentBlob(t, consumer, postURI, postCID, testkit.TID(), revs[1], "bafcommentblobsibling", base+1_000_000, postBlobImages(imageA))
			var replacement map[string]interface{}
			if !test.dropEmbed {
				replacement = postBlobImages(imageA, imageC)
			}
			require.NoError(t, consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, test.operation, rkey, revs[2], "bafcommentblobedited", base+2_000_000, replacement)))
			var storedCID string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT cid FROM comments WHERE uri = $1`, uri).Scan(&storedCID))
			require.Equal(t, "bafcommentblobedited", storedCID)
			if test.dropEmbed {
				require.ElementsMatch(t, []postBlobRow{
					{bridgedTestCommenter, imageA, "comment_image", false},
					{bridgedTestCommenter, imageB, "comment_image", false},
				}, readPostBlobRows(t, db, uri))
				require.Empty(t, activePostBlobRows(readPostBlobRows(t, db, uri)))
			} else {
				require.ElementsMatch(t, []postBlobRow{
					{bridgedTestCommenter, imageA, "comment_image", true},
					{bridgedTestCommenter, imageB, "comment_image", false},
					{bridgedTestCommenter, imageC, "comment_image", true},
				}, readPostBlobRows(t, db, uri))
			}
			require.ElementsMatch(t, []postBlobRow{
				{bridgedTestCommenter, imageA, "comment_image", true},
			}, readPostBlobRows(t, db, sibling))
		})
	}
}

const (
	commentImageA = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	commentImageB = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	commentImageC = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
	commentImageD = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
)

func commentBlobActiveRowsAB() []postBlobRow {
	return []postBlobRow{
		{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", true},
		{bridgedTestCommenter, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image", true},
	}
}

func commentBlobInactiveRowsAB() []postBlobRow {
	return []postBlobRow{
		{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", false},
		{bridgedTestCommenter, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image", false},
	}
}

type commentBlobState struct {
	cid       string
	embed     sql.NullString
	deletedAt *time.Time
}

func readCommentBlobState(t *testing.T, db *sql.DB, uri string) commentBlobState {
	t.Helper()
	var state commentBlobState
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT cid, embed::text, deleted_at FROM comments WHERE uri = $1`, uri).
		Scan(&state.cid, &state.embed, &state.deletedAt))
	return state
}

func TestCommentBlobReferences_DeleteDeactivatesSubjectOnly(t *testing.T) {
	db := testkit.DB(t)
	postURI, postCID := setupCommentThread(t, db)
	consumer := newCommentBlobConsumer(db)
	revs := increasingTIDs(t, 3)
	base := time.Now().UnixMicro()
	rkey := testkit.TID()
	uri := createCommentBlob(t, consumer, postURI, postCID, rkey, revs[0], "bafcommentbloboriginal", base,
		postBlobImages(commentImageA, commentImageB))
	sibling := createCommentBlob(t, consumer, postURI, postCID, testkit.TID(), revs[1], "bafcommentblobsibling", base+1_000_000,
		postBlobImages(commentImageA))
	require.ElementsMatch(t, commentBlobActiveRowsAB(), readPostBlobRows(t, db, uri))
	require.ElementsMatch(t, []postBlobRow{{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", true}}, readPostBlobRows(t, db, sibling))
	require.NoError(t, consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, "delete", rkey, revs[2], "", base+2_000_000, nil)))
	require.NotNil(t, readCommentBlobState(t, db, uri).deletedAt)
	require.ElementsMatch(t, commentBlobInactiveRowsAB(), readPostBlobRows(t, db, uri))
	require.ElementsMatch(t, []postBlobRow{{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", true}}, readPostBlobRows(t, db, sibling))
}

func TestCommentBlobReferences_SkippedEventsPreserveReferences(t *testing.T) {
	for _, test := range []struct {
		name          string
		operation     string
		olderRev      bool
		olderTime     bool
		deleteFirst   bool
		invalidRoot   bool
		changedParent bool
		newRkey       bool
		permanent     bool
	}{
		{name: "invalid create on new rkey", operation: "create", newRkey: true, invalidRoot: true, permanent: true},
		{name: "thread-changing update", operation: "update", changedParent: true, permanent: true},
		{name: "older rev update with newer event time", operation: "update", olderRev: true},
		{name: "newer rev update with older event time", operation: "update", olderTime: true},
		{name: "update after author delete", operation: "update", deleteFirst: true},
		{name: "active re-create with changed parent", operation: "create", changedParent: true},
		{name: "stale delete", operation: "delete", olderRev: true},
		{name: "stale create after author delete", operation: "create", olderRev: true, deleteFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			postURI, postCID := setupCommentThread(t, db)
			consumer := newCommentBlobConsumer(db)
			revs := increasingTIDs(t, 4)
			base := time.Now().UnixMicro()
			rkey := testkit.TID()
			uri := createCommentBlob(t, consumer, postURI, postCID, rkey, revs[1], "bafcommentbloboriginal", base,
				postBlobImages(commentImageA, commentImageB))
			require.ElementsMatch(t, commentBlobActiveRowsAB(), readPostBlobRows(t, db, uri))
			if test.deleteFirst {
				require.NoError(t, consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, "delete", rkey, revs[2], "", base+1_000_000, nil)))
				require.NotNil(t, readCommentBlobState(t, db, uri).deletedAt)
				require.ElementsMatch(t, commentBlobInactiveRowsAB(), readPostBlobRows(t, db, uri))
			}
			before := readCommentBlobState(t, db, uri)
			wanted := commentBlobActiveRowsAB()
			if test.deleteFirst {
				wanted = commentBlobInactiveRowsAB()
			}
			eventRkey := rkey
			if test.newRkey {
				eventRkey = testkit.TID()
			}
			rev := revs[3]
			if test.olderRev {
				rev = revs[0]
			}
			eventTime := base + 2_000_000
			if test.olderTime {
				eventTime = base
			}
			event := commentBlobEvent(postURI, postCID, test.operation, eventRkey, rev, "bafcommentblobincoming", eventTime,
				postBlobImages(commentImageC))
			if test.invalidRoot {
				event.Commit.Record["reply"].(map[string]interface{})["root"].(map[string]interface{})["cid"] = ""
			}
			if test.changedParent {
				parentRkey := testkit.TID()
				parentURI := createCommentBlob(t, consumer, postURI, postCID, parentRkey, revs[0], "bafcommentblobparent", base-1_000_000, nil)
				event.Commit.Record["reply"].(map[string]interface{})["parent"] = map[string]interface{}{
					"uri": parentURI, "cid": "bafcommentblobparent",
				}
			}
			err := consumer.HandleEvent(t.Context(), event)
			if test.permanent {
				require.True(t, errors.Is(err, ErrPermanentEvent), "expected permanent rejection, got %v", err)
			} else {
				require.NoError(t, err)
			}
			after := readCommentBlobState(t, db, uri)
			require.Equal(t, before.cid, after.cid)
			require.Equal(t, before.deletedAt, after.deletedAt)
			require.ElementsMatch(t, wanted, readPostBlobRows(t, db, uri))
			if test.newRkey {
				newURI := "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + eventRkey
				require.Zero(t, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, newURI))
				require.Empty(t, readPostBlobRows(t, db, newURI))
			}
		})
	}
}

func TestCommentBlobReferences_FailedWriteRollsBackCommentReferencesAndRevision(t *testing.T) {
	for _, test := range []struct {
		name        string
		operation   string
		deleteFirst bool
		initial     bool
		images      map[string]interface{}
		want        []postBlobRow
	}{
		{name: "fresh create", operation: "create", images: postBlobImages(commentImageA, commentImageB), want: commentBlobActiveRowsAB()},
		{
			name: "re-create after author delete", operation: "create", initial: true, deleteFirst: true,
			images: postBlobImages(commentImageC, commentImageD), want: []postBlobRow{
				{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", false},
				{bridgedTestCommenter, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image", false},
				{bridgedTestCommenter, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "comment_image", true},
				{bridgedTestCommenter, "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku", "comment_image", true},
			},
		},
		{name: "active same-rkey re-create", operation: "create", initial: true, images: postBlobImages(commentImageA, commentImageC), want: []postBlobRow{
			{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", true},
			{bridgedTestCommenter, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image", false},
			{bridgedTestCommenter, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "comment_image", true},
		}},
		{name: "update", operation: "update", initial: true, images: postBlobImages(commentImageA, commentImageC), want: []postBlobRow{
			{bridgedTestCommenter, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", true},
			{bridgedTestCommenter, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image", false},
			{bridgedTestCommenter, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "comment_image", true},
		}},
		{name: "delete", operation: "delete", initial: true, want: commentBlobInactiveRowsAB()},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			postURI, postCID := setupCommentThread(t, db)
			references := &failingPostBlobReferences{Repository: postgres.NewBlobReferenceRepository(db)}
			consumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db, WithCommentBlobReferences(references))
			revs := increasingTIDs(t, 3)
			base := time.Now().UnixMicro()
			rkey := testkit.TID()
			uri := "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + rkey
			if test.initial {
				createCommentBlob(t, consumer, postURI, postCID, rkey, revs[0], "bafcommentbloboriginal", base,
					postBlobImages(commentImageA, commentImageB))
				require.ElementsMatch(t, commentBlobActiveRowsAB(), readPostBlobRows(t, db, uri))
			}
			if test.deleteFirst {
				require.NoError(t, consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, "delete", rkey, revs[1], "", base+1_000_000, nil)))
				require.ElementsMatch(t, commentBlobInactiveRowsAB(), readPostBlobRows(t, db, uri))
			}
			var before commentBlobState
			if test.initial {
				before = readCommentBlobState(t, db, uri)
			}
			previousRows := readPostBlobRows(t, db, uri)
			rev := revs[1]
			if test.deleteFirst {
				rev = revs[2]
			}
			if !test.initial {
				rev = revs[0]
			}
			event := commentBlobEvent(postURI, postCID, test.operation, rkey, rev, "bafcommentblobedited", base+2_000_000, test.images)
			references.fail = true
			err := consumer.HandleEvent(t.Context(), event)
			require.ErrorIs(t, err, errPostBlobReferenceWrite)
			if test.initial {
				require.Equal(t, before, readCommentBlobState(t, db, uri))
			} else {
				require.Zero(t, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, uri))
			}
			require.ElementsMatch(t, previousRows, readPostBlobRows(t, db, uri))
			references.fail = false
			require.NoError(t, consumer.HandleEvent(t.Context(), event))
			after := readCommentBlobState(t, db, uri)
			if test.operation == "delete" {
				require.Equal(t, "bafcommentbloboriginal", after.cid)
				require.NotNil(t, after.deletedAt)
			} else {
				require.Equal(t, "bafcommentblobedited", after.cid)
				require.Nil(t, after.deletedAt)
				require.True(t, after.embed.Valid)
			}
			require.ElementsMatch(t, test.want, readPostBlobRows(t, db, uri))
		})
	}
}

func TestCommentBlobReferences_InstanceRemovalKeepsReferencesActiveThroughEdit(t *testing.T) {
	h := newEditMediaHarness(t, true)
	imageA := editMediaCID("comment edit image A")
	imageB := editMediaCID("comment edit image B")
	imageC := editMediaCID("comment edit image C")
	h.pds.know(pv2Author, imageA, imageB, imageC)
	revs := increasingTIDs(t, 2)
	started := time.Now().Add(-time.Minute)
	postRkey := testkit.TID()
	root := moderation.StrongRef{URI: pv2URI(pv2Author, postRkey), CID: editMediaCID("comment edit post record")}
	require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", postRkey, revs[0], root.CID,
		started.UnixMicro(), postModerationRecord(imageA))))
	rkey := testkit.TID()
	uri := "at://" + pv2Author + "/" + CommentCollection + "/" + rkey
	const initialCID = "bafcommentblobremovaloriginal"
	require.NoError(t, h.comments.HandleEvent(t.Context(), editMediaCommentEvent(root, "create", rkey, revs[0], initialCID, started, imageA, imageB)))
	h.remove(t, moderation.StrongRef{URI: uri, CID: initialCID}, "v0")
	require.Equal(t, 1, countRows(t, h.db, `SELECT count(*) FROM moderation_decisions
		WHERE subject_uri = $1 AND scope_kind = 'instance' AND kind = 'removal' AND active`, uri))
	require.ElementsMatch(t, []postBlobRow{
		{pv2Author, imageA, "comment_image", true},
		{pv2Author, imageB, "comment_image", true},
	}, readPostBlobRows(t, h.db, uri))
	const editedCID = "bafcommentblobremovaledited"
	require.NoError(t, h.comments.HandleEvent(t.Context(), editMediaCommentEvent(root, "update", rkey, revs[1], editedCID,
		started.Add(time.Second), imageA, imageC)))
	require.Equal(t, editedCID, readCommentBlobState(t, h.db, uri).cid)
	require.Equal(t, 1, countRows(t, h.db, `SELECT count(*) FROM moderation_decisions
		WHERE subject_uri = $1 AND scope_kind = 'instance' AND kind = 'removal' AND active`, uri))
	require.ElementsMatch(t, []postBlobRow{
		{pv2Author, imageA, "comment_image", true},
		{pv2Author, imageB, "comment_image", false},
		{pv2Author, imageC, "comment_image", true},
	}, readPostBlobRows(t, h.db, uri))
}

// embedImageLimitCIDs returns count distinct sha2-256 raw CIDs, the shape every
// atproto blob CID has, so the extractor accepts each one.
func embedImageLimitCIDs(t *testing.T, label string, count int) []string {
	t.Helper()
	cids := make([]string, 0, count)
	for index := 0; index < count; index++ {
		digest, err := multihash.Sum([]byte(fmt.Sprintf("%s image %d", label, index)), multihash.SHA2_256, -1)
		require.NoError(t, err)
		cids = append(cids, cid.NewCidV1(cid.Raw, digest).String())
	}
	return cids
}

func TestCommentBlobReferences_RejectsMoreThanEightImages(t *testing.T) {
	t.Run("create with nine images", func(t *testing.T) {
		db := testkit.DB(t)
		postURI, postCID := setupCommentThread(t, db)
		consumer := newCommentBlobConsumer(db)
		rkey := testkit.TID()
		uri := "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + rkey
		err := consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, "create", rkey, testkit.TID(), "bafcommentblobnine",
			time.Now().UnixMicro(), postBlobImages(embedImageLimitCIDs(t, "comment create", 9)...)))
		require.True(t, errors.Is(err, ErrPermanentEvent), "expected permanent rejection, got %v", err)
		require.ErrorContains(t, err, "maximum of 8")
		require.Zero(t, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, uri))
		require.Empty(t, readPostBlobRows(t, db, uri))
	})

	t.Run("update with nine images", func(t *testing.T) {
		db := testkit.DB(t)
		postURI, postCID := setupCommentThread(t, db)
		consumer := newCommentBlobConsumer(db)
		revs := increasingTIDs(t, 2)
		base := time.Now().UnixMicro()
		rkey := testkit.TID()
		uri := createCommentBlob(t, consumer, postURI, postCID, rkey, revs[0], "bafcommentbloboriginal", base,
			postBlobImages(commentImageA, commentImageB))
		before := readCommentBlobState(t, db, uri)
		err := consumer.HandleEvent(t.Context(), commentBlobEvent(postURI, postCID, "update", rkey, revs[1], "bafcommentblobnine",
			base+1_000_000, postBlobImages(embedImageLimitCIDs(t, "comment update", 9)...)))
		require.True(t, errors.Is(err, ErrPermanentEvent), "expected permanent rejection, got %v", err)
		require.ErrorContains(t, err, "maximum of 8")
		require.Equal(t, before, readCommentBlobState(t, db, uri))
		require.ElementsMatch(t, commentBlobActiveRowsAB(), readPostBlobRows(t, db, uri))
	})

	t.Run("create with exactly eight images", func(t *testing.T) {
		db := testkit.DB(t)
		postURI, postCID := setupCommentThread(t, db)
		consumer := newCommentBlobConsumer(db)
		images := embedImageLimitCIDs(t, "comment boundary", 8)
		uri := createCommentBlob(t, consumer, postURI, postCID, testkit.TID(), testkit.TID(), "bafcommentblobeight",
			time.Now().UnixMicro(), postBlobImages(images...))
		want := make([]postBlobRow, 0, len(images))
		for _, image := range images {
			want = append(want, postBlobRow{bridgedTestCommenter, image, "comment_image", true})
		}
		require.ElementsMatch(t, want, readPostBlobRows(t, db, uri))
	})
}
