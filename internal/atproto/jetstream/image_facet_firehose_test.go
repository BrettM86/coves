//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Firehose ingest keeps #image facets exactly as the author's repo holds them,
// even when they break the write-path rules (richtext.CheckImageFacets). Those
// rules only gate API writes; a record already signed elsewhere is indexed as
// received, and only structurally broken byte ranges are dropped, as for any
// facet.

// imageFacetContent is the text every record below annotates: 26 ASCII bytes.
const imageFacetContent = "abcdefghijklmnopqrstuvwxyz"

const imageFacetRecordCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"

// imageFacetCase is one record: the facets it arrives with and the facets the
// row must hold afterwards, both as literal JSON.
type imageFacetCase struct {
	name     string
	noEmbed  bool // the record carries no embed instead of a two-image embed
	received string
	stored   string
}

// imageFacetIndexer drives one firehose create carrying content, facets and
// embed through the real consumer, requires the row to be indexed and live, and
// returns its stored facets column.
type imageFacetIndexer func(t *testing.T, facets []interface{}, embed map[string]interface{}) sql.NullString

type imageFacetTarget struct {
	name  string
	setup func(t *testing.T, db *sql.DB) imageFacetIndexer
}

var imageFacetTargets = []imageFacetTarget{
	{name: "post", setup: imageFacetPostIndexer},
	{name: "comment", setup: imageFacetCommentIndexer},
}

func imageFacetPostIndexer(t *testing.T, db *sql.DB) imageFacetIndexer {
	f := newPV2Fixture(t, db)
	return func(t *testing.T, facets []interface{}, embed map[string]interface{}) sql.NullString {
		t.Helper()
		rkey := testkit.TID()
		record := pv2Record(pv2Community, "image facets", imageFacetContent)
		record["facets"] = facets
		if embed != nil {
			record["embed"] = embed
		}
		require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
			pv2Author, "create", rkey, testkit.TID(), imageFacetRecordCID, time.Now().UnixMicro(), record,
		)))
		uri := pv2URI(pv2Author, rkey)
		_, _, _, _, deletedAt := readPV2Post(t, db, uri)
		require.Nil(t, deletedAt, "post must be indexed live")
		var stored sql.NullString
		require.NoError(t, db.QueryRow(`SELECT content_facets::text FROM posts WHERE uri = $1`, uri).Scan(&stored))
		return stored
	}
}

func imageFacetCommentIndexer(t *testing.T, db *sql.DB) imageFacetIndexer {
	postURI, postCID := setupCommentThread(t, db)
	consumer := newCommentConsumer(db)
	return func(t *testing.T, facets []interface{}, embed map[string]interface{}) sql.NullString {
		t.Helper()
		rkey := testkit.TID()
		record := revCommentRecord(imageFacetContent, postURI, postCID, postURI, postCID)
		record["facets"] = facets
		if embed != nil {
			record["embed"] = embed
		}
		require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
			bridgedTestCommenter, CommentCollection, "create", rkey, testkit.TID(), imageFacetRecordCID,
			time.Now().UnixMicro(), record,
		)))
		uri := "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + rkey
		var stored sql.NullString
		var deletedAt *time.Time
		err := db.QueryRow(`SELECT content_facets::text, deleted_at FROM comments WHERE uri = $1`, uri).Scan(&stored, &deletedAt)
		require.NoErrorf(t, err, "no comment row for %s", uri)
		require.Nil(t, deletedAt, "comment must be indexed live")
		return stored
	}
}

// runImageFacetCases indexes every case as a separate record on both a post and
// a comment, comparing the stored facets with the case's literal semantically
// (jsonb reorders keys) while keeping array order.
func runImageFacetCases(t *testing.T, cases []imageFacetCase) {
	t.Helper()
	for _, target := range imageFacetTargets {
		t.Run(target.name, func(t *testing.T) {
			t.Parallel()
			index := target.setup(t, testkit.DB(t))
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					var facets []interface{}
					require.NoError(t, json.Unmarshal([]byte(tc.received), &facets))
					var embed map[string]interface{}
					if !tc.noEmbed {
						embed = postBlobImages(
							"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
							"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si",
						)
					}

					stored := index(t, facets, embed)

					require.True(t, stored.Valid, "facets column must not be null")
					var want, got interface{}
					require.NoError(t, json.Unmarshal([]byte(tc.stored), &want))
					require.NoError(t, json.Unmarshal([]byte(stored.String), &got))
					if !reflect.DeepEqual(want, got) {
						t.Fatalf("stored facets differ\n got: %s\nwant: %s", stored.String, tc.stored)
					}
				})
			}
		})
	}
}

func TestFirehoseImageFacets_RuleBreakingFacetsStoredAsReceived(t *testing.T) {
	t.Parallel()
	runImageFacetCases(t, []imageFacetCase{
		{
			name:     "index beyond the embed images",
			received: `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":5}]}]`,
			stored:   `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":5}]}]`,
		},
		{
			name: "index placed by two facets",
			received: `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
				{"index":{"byteStart":2,"byteEnd":3},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`,
			stored: `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
				{"index":{"byteStart":2,"byteEnd":3},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`,
		},
		{
			name:     "image shares its facet with bold",
			received: `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":1},{"$type":"social.coves.richtext.facet#bold"}]}]`,
			stored:   `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":1},{"$type":"social.coves.richtext.facet#bold"}]}]`,
		},
		{
			name:     "negative index",
			received: `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":-1}]}]`,
			stored:   `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":-1}]}]`,
		},
		{
			name: "image inside a code range",
			received: `[{"index":{"byteStart":0,"byteEnd":10},"features":[{"$type":"social.coves.richtext.facet#code"}]},
				{"index":{"byteStart":2,"byteEnd":3},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`,
			stored: `[{"index":{"byteStart":0,"byteEnd":10},"features":[{"$type":"social.coves.richtext.facet#code"}]},
				{"index":{"byteStart":2,"byteEnd":3},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`,
		},
		{
			name:     "image without an images embed",
			noEmbed:  true,
			received: `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`,
			stored:   `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`,
		},
	})
}

func TestFirehoseImageFacets_RangePastContentDropped(t *testing.T) {
	t.Parallel()
	runImageFacetCases(t, []imageFacetCase{{
		name: "image ending past the content",
		received: `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
			{"index":{"byteStart":2,"byteEnd":27},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]},
			{"index":{"byteStart":4,"byteEnd":8},"features":[{"$type":"social.coves.richtext.facet#bold"}]}]`,
		stored: `[{"index":{"byteStart":0,"byteEnd":1},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
			{"index":{"byteStart":4,"byteEnd":8},"features":[{"$type":"social.coves.richtext.facet#bold"}]}]`,
	}})
}
