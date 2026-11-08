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
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// imagesWithAttachment.images is a plain (non-union) ref, so its $type is
// optional and an authored record may omit it. The firehose canonicalizes the
// embed before indexing: the stored gallery carries $type, its images get blob
// references, and a gallery $type naming another schema is a permanent rejection.

const (
	untypedGalleryFirstCID  = "bafkreigqcbqof2mpgbvdm7lueiz5aryqk2gkghzscwcdgil2izgkrm2sma"
	untypedGallerySecondCID = "bafkreiclrl4ktnbj6zgb523ih4yy4e7jpzv34m45so2z5kzufto45xoid4"
)

var untypedGalleryNineCIDs = []string{
	"bafkreigqcbqof2mpgbvdm7lueiz5aryqk2gkghzscwcdgil2izgkrm2sma",
	"bafkreiclrl4ktnbj6zgb523ih4yy4e7jpzv34m45so2z5kzufto45xoid4",
	"bafkreidcbecnkdg3a5bqobmmilbz3mx3l2qoyavpoijbns75fsswqdiwwe",
	"bafkreienbzklj25jy3jklwj2wnemyrbbclh2t6nbi4hvvncoigfrqenmxm",
	"bafkreifwxx3rf7x45ovruuooix6d7nm6y6w26mmohz6bp3ofgaad66gaha",
	"bafkreidwclipxiktw52q52zlmc4rrdfkb3qr2axvp5kwg3xz2zree6jtxa",
	"bafkreifeoznxzcym4z2hxb3ubgsha5xxoqxw3cal2m2zo3dpypfmuouyny",
	"bafkreihs7rffnc7qvlbynqs3i7newxdx37hgqq3n6sewg64cdtjrmrolha",
	"bafkreidfophtdrm4dquax7ya2bwiw23d3xhfwmdluz4ojtrrwbsbtl34yi",
}

// untypedGalleryEmbed builds an imagesWithAttachment embed with a quoted-post
// attachment. galleryType "" omits the gallery's $type key entirely.
func untypedGalleryEmbed(galleryType string, cids ...string) map[string]interface{} {
	gallery := postBlobImages(cids...)
	delete(gallery, "$type")
	if galleryType != "" {
		gallery["$type"] = galleryType
	}
	return map[string]interface{}{
		"$type":  "social.coves.embed.imagesWithAttachment",
		"images": gallery,
		"attachment": map[string]interface{}{
			"$type": "social.coves.embed.post",
			"post": map[string]interface{}{
				"uri": "at://did:plc:quoted/social.coves.community.postv2/3kquoted",
				"cid": "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si",
			},
		},
	}
}

// storedGalleryType reads the gallery $type of the embed stored for uri.
func storedGalleryType(t *testing.T, db *sql.DB, table, uri string) interface{} {
	t.Helper()
	require.Contains(t, []string{"posts", "comments"}, table)
	var raw string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT embed::text FROM `+table+` WHERE uri = $1`, uri).Scan(&raw))
	var embed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(raw), &embed))
	gallery, ok := embed["images"].(map[string]interface{})
	require.Truef(t, ok, "stored embed has no gallery object: %s", raw)
	return gallery["$type"]
}

// B6 + acceptance: an untyped gallery is stored canonical, its images are
// referenced under the author, and the post view projects the gallery.
func TestPostConsumer_UntypedGallery_StoredCanonicalWithReferences(t *testing.T) {
	db := testkit.DB(t)
	fixture := newPostBlobFixture(t, db)
	uri := createPostBlob(t, fixture, pv2Author, "untypedgallery", testkit.TID(), time.Now().UnixMicro(),
		untypedGalleryEmbed("", untypedGalleryFirstCID, untypedGallerySecondCID))

	require.Equal(t, "social.coves.embed.images", storedGalleryType(t, db, "posts", uri), "stored gallery must carry its $type")

	require.ElementsMatch(t, []postBlobRow{
		{"did:plc:pv2author", "bafkreigqcbqof2mpgbvdm7lueiz5aryqk2gkghzscwcdgil2izgkrm2sma", "post_image", true},
		{"did:plc:pv2author", "bafkreiclrl4ktnbj6zgb523ih4yy4e7jpzv34m45so2z5kzufto45xoid4", "post_image", true},
	}, readPostBlobRows(t, db, uri))

	// The author sees their own post while its admission is pending.
	views, err := postgres.NewPostRepository(db).GetViewsByURIs(context.Background(), []string{uri}, pv2Author)
	require.NoError(t, err)
	view := views[uri]
	require.NotNil(t, view)
	posts.TransformBlobRefsToURLs(view)
	embed, ok := view.Embed.(map[string]interface{})
	require.Truef(t, ok, "view embed is not an object: %#v", view.Embed)
	require.Equal(t, "social.coves.embed.imagesWithAttachment#view", embed["$type"])
	gallery, ok := embed["images"].(map[string]interface{})
	require.Truef(t, ok, "view embed has no gallery object: %#v", embed)
	require.Equal(t, "social.coves.embed.images#view", gallery["$type"])
	images, _ := gallery["images"].([]interface{})
	require.Len(t, images, 2)
}

// B6: the image cap and the gallery's schema type apply to an untyped or
// mistyped gallery exactly as to a typed one.
func TestPostConsumer_UntypedGallery_RejectedPermanently(t *testing.T) {
	for _, tc := range []struct {
		name  string
		embed map[string]interface{}
	}{
		{"untyped gallery with nine images", untypedGalleryEmbed("", untypedGalleryNineCIDs...)},
		{"gallery typed as another schema", untypedGalleryEmbed("social.coves.embed.external", untypedGalleryFirstCID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			fixture := newPostBlobFixture(t, db)
			const rkey = "untypedreject"
			err := fixture.consumer.HandleEvent(context.Background(), pv2Event(
				pv2Author, "create", rkey, testkit.TID(), "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
				time.Now().UnixMicro(), postBlobRecord(pv2Community, tc.embed)))
			require.Truef(t, errors.Is(err, ErrPermanentEvent), "HandleEvent() error = %v, want ErrPermanentEvent", err)
			uri := pv2URI(pv2Author, rkey)
			require.Equal(t, 0, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
			require.Empty(t, readPostBlobRows(t, db, uri))
		})
	}
}

// B7: an image-only comment whose gallery omits $type is indexed canonical
// with its image references.
func TestCommentConsumer_UntypedGallery_ImageOnlyStoredCanonicalWithReferences(t *testing.T) {
	db := testkit.DB(t)
	postURI, postCID := setupCommentThread(t, db)
	consumer := newCommentBlobConsumer(db)
	rkey := testkit.TID()
	record := revCommentRecord("", postURI, postCID, postURI, postCID)
	record["embed"] = untypedGalleryEmbed("", untypedGalleryFirstCID, untypedGallerySecondCID)

	require.NoError(t, consumer.HandleEvent(t.Context(), revCommitEvent(bridgedTestCommenter, CommentCollection, "create",
		rkey, testkit.TID(), "bafcommentuntypedgallery", time.Now().UnixMicro(), record)))

	uri := "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + rkey
	require.Equal(t, "social.coves.embed.images", storedGalleryType(t, db, "comments", uri), "stored gallery must carry its $type")
	require.ElementsMatch(t, []postBlobRow{
		{"did:plc:brtestcommenter", "bafkreigqcbqof2mpgbvdm7lueiz5aryqk2gkghzscwcdgil2izgkrm2sma", "comment_image", true},
		{"did:plc:brtestcommenter", "bafkreiclrl4ktnbj6zgb523ih4yy4e7jpzv34m45so2z5kzufto45xoid4", "comment_image", true},
	}, readPostBlobRows(t, db, uri))
}

// B7: a comment gallery typed as another schema is a permanent rejection,
// even with text content that would otherwise make the comment valid.
func TestCommentConsumer_MistypedGallery_RejectedPermanently(t *testing.T) {
	db := testkit.DB(t)
	postURI, postCID := setupCommentThread(t, db)
	consumer := newCommentBlobConsumer(db)
	rkey := testkit.TID()
	record := revCommentRecord("comment text", postURI, postCID, postURI, postCID)
	record["embed"] = untypedGalleryEmbed("social.coves.embed.external", untypedGalleryFirstCID)

	err := consumer.HandleEvent(t.Context(), revCommitEvent(bridgedTestCommenter, CommentCollection, "create",
		rkey, testkit.TID(), "bafcommentmistypedgallery", time.Now().UnixMicro(), record))

	require.Truef(t, errors.Is(err, ErrPermanentEvent), "HandleEvent() error = %v, want ErrPermanentEvent", err)
	uri := "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + rkey
	require.Equal(t, 0, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, uri))
	require.Empty(t, readPostBlobRows(t, db, uri))
}
