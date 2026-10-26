package posts

import (
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postWriteComposite(t *testing.T) map[string]any {
	t.Helper()
	return jsonObject(t, `{"$type":"social.coves.embed.imagesWithAttachment","images":`+imageFacetTwoImages+`,"attachment":{"$type":"social.coves.embed.external","external":{"uri":"https://news.example.com/story","thumb":`+lexiconBlobJPEG+`}}}`)
}

func postWriteCompositeStored(t *testing.T, embed map[string]any) map[string]any {
	t.Helper()
	record := jsonObject(t, `{"$type":"social.coves.community.postv2","community":"`+lexiconCommunityDID+`","createdAt":"2026-10-01T09:00:00Z","title":"Harbour repairs","content":"The pier is open."}`)
	record["embed"] = embed
	return record
}

func postWriteCompositeCID(t *testing.T, code uint64, length int) string {
	t.Helper()
	digest, err := multihash.Sum([]byte("composite nested main"), code, length)
	require.NoError(t, err)
	return cid.NewCidV1(cid.Raw, digest).String()
}

// B4 is the sole detailed rule owner. JSON-shaped values exercise the same
// task26 blob classes under the composite's nested main gallery. Schema-only
// rules keep their validator; unconditional shape/blob/thumb checks also run
// without it. No #image placements belong to this task.
func TestCreatePost_ImagesWithAttachmentInvalidEmbed(t *testing.T) {
	tests := []struct {
		name, target, key  string
		value              any
		remove, schemaOnly bool
	}{
		{name: "missing main", target: "composite", key: "images", remove: true},
		{name: "main is not an object", target: "composite", key: "images", value: []any{}},
		{name: "main missing type", target: "main", key: "$type", remove: true},
		{name: "non-image main", target: "main", key: "$type", value: "social.coves.embed.video"},
		{name: "main gallery missing", target: "main", key: "images", remove: true},
		{name: "main gallery is not array", target: "main", key: "images", value: map[string]any{}},
		{name: "main gallery empty", target: "main", key: "images", value: []any{}},
		{name: "main entry not object", target: "main", key: "images", value: []any{"image"}},
		{name: "main image missing", target: "entry", key: "image", remove: true},
		{name: "main image not blob object", target: "entry", key: "image", value: "https://images.example.com/pier.jpg"},
		{name: "blob type missing", target: "blob", key: "$type", remove: true},
		{name: "blob type wrong", target: "blob", key: "$type", value: "bytes"},
		{name: "blob ref missing", target: "blob", key: "ref", remove: true},
		{name: "blob ref not object", target: "blob", key: "ref", value: "cid"},
		{name: "blob link missing", target: "ref", key: "$link", remove: true},
		{name: "blob link not string", target: "ref", key: "$link", value: 42},
		{name: "blob link malformed", target: "ref", key: "$link", value: "not-a-cid"},
		{name: "blob identity CID", target: "ref", key: "$link", value: postWriteCompositeCID(t, multihash.IDENTITY, -1)},
		{name: "blob sha512 CID", target: "ref", key: "$link", value: postWriteCompositeCID(t, multihash.SHA2_512, -1)},
		{name: "blob truncated sha256 CID", target: "ref", key: "$link", value: postWriteCompositeCID(t, multihash.SHA2_256, 20)},
		{name: "blob MIME missing", target: "blob", key: "mimeType", remove: true},
		{name: "blob MIME unsupported", target: "blob", key: "mimeType", value: "image/avif"},
		{name: "blob size missing", target: "blob", key: "size", remove: true},
		{name: "blob size string", target: "blob", key: "size", value: "1000"},
		{name: "blob size fractional", target: "blob", key: "size", value: 1.5},
		{name: "blob size negative", target: "blob", key: "size", value: float64(-1)},
		{name: "blob size integer negative", target: "blob", key: "size", value: -1},
		{name: "blob size overflow", target: "blob", key: "size", value: float64(1 << 63)},
		{name: "blob size one over cap", target: "blob", key: "size", value: float64(10000001)},
		{name: "blob size integer one over cap", target: "blob", key: "size", value: 10000001},
		{name: "missing attachment", target: "composite", key: "attachment", remove: true},
		{name: "attachment not object", target: "composite", key: "attachment", value: "quote"},
		{name: "attachment type missing", target: "attachment", key: "$type", remove: true},
		{name: "unsupported attachment", target: "attachment", key: "$type", value: "social.coves.embed.images"},
		{name: "external object missing", target: "attachment", key: "external", remove: true},
		{name: "external object malformed", target: "attachment", key: "external", value: []any{}},
		{name: "external URI missing", target: "external", key: "uri", remove: true},
		{name: "external URI empty", target: "external", key: "uri", value: ""},
		{name: "external URI javascript scheme", target: "external", key: "uri", value: "javascript:alert(1)"},
		{name: "quote strongref missing", target: "composite", key: "attachment", value: jsonObject(t, `{"$type":"social.coves.embed.post"}`)},
		{name: "quote URI missing", target: "composite", key: "attachment", value: jsonObject(t, `{"$type":"social.coves.embed.post","post":{"cid":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}}`)},
		{name: "quote CID empty", target: "composite", key: "attachment", value: jsonObject(t, `{"$type":"social.coves.embed.post","post":{"uri":"at://did:plc:quotedauthor/social.coves.community.postv2/3m2quotedpostab","cid":""}}`)},
		{name: "thumb URL", target: "external", key: "thumb", value: "https://images.example.com/thumb.jpg"},
		{name: "thumb scalar", target: "external", key: "thumb", value: 42},
		{name: "thumb wrong type", target: "thumb", key: "$type", value: "bytes"},
		{name: "thumb ref missing", target: "thumb", key: "ref", remove: true},
		{name: "thumb MIME missing", target: "thumb", key: "mimeType", remove: true},
		{name: "main alt schema violation", target: "entry", key: "alt", value: 42, schemaOnly: true},
		{name: "main aspect ratio schema violation", target: "entry", key: "aspectRatio", value: map[string]any{"width": 0, "height": 1}, schemaOnly: true},
		{name: "thumb MIME schema violation", target: "thumb", key: "mimeType", value: "video/mp4", schemaOnly: true},
		{name: "thumb size schema violation", target: "thumb", key: "size", value: 6000001, schemaOnly: true},
	}
	for _, validator := range []bool{true, false} {
		mode := "validator"
		if !validator {
			mode = "nil-validator"
		}
		for _, test := range tests {
			if !validator && test.schemaOnly {
				continue
			}
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				embed := postWriteComposite(t)
				main := embed["images"].(map[string]any)
				entry := main["images"].([]any)[1].(map[string]any)
				blob := entry["image"].(map[string]any)
				attachment := embed["attachment"].(map[string]any)
				external := attachment["external"].(map[string]any)
				targets := map[string]map[string]any{"composite": embed, "main": main, "entry": entry, "blob": blob, "ref": blob["ref"].(map[string]any), "attachment": attachment, "external": external, "thumb": external["thumb"].(map[string]any)}
				if test.remove {
					delete(targets[test.target], test.key)
				} else {
					targets[test.target][test.key] = test.value
				}
				h := newUnvalidatedLexiconHarness(t)
				if validator {
					h = newLexiconHarness(t, nil)
				}
				_, err := h.create(CreatePostRequest{Title: lexiconStringPtr("Harbour repairs"), Embed: embed})
				assert.ErrorIs(t, err, ErrInvalidEmbed)
				assert.NotErrorIs(t, err, ErrTooManyImages)
				assert.Empty(t, h.repo.records, "no record may be written")
				assert.Empty(t, h.ledger.rows, "no admission state may be created")
				assert.Zero(t, h.ledger.nextID, "no reservation may ever be taken")
			})
		}
	}
}

func TestUpdatePost_ImagesWithAttachmentInvalidMergedEmbed(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "replacement"
		if retained {
			name = "retained-standing-composite"
		}
		t.Run(name, func(t *testing.T) {
			h := newUnvalidatedLexiconHarness(t)
			bad := postWriteComposite(t)
			bad["images"].(map[string]any)["images"].([]any)[1].(map[string]any)["image"].(map[string]any)["size"] = float64(10000001)
			standing := postWriteComposite(t)
			req := UpdatePostRequest{Embed: bad}
			if retained {
				standing, req = bad, UpdatePostRequest{Content: lexiconStringPtr("The pier is open again.")}
			}
			h.repo.seed(t, postWriteCompositeStored(t, standing))
			before := h.repo.snapshot(t, lexiconEditRkey)
			_, err := h.update(req)
			assert.ErrorIs(t, err, ErrInvalidEmbed)
			assert.Zero(t, h.repo.puts, "invalid merged records must not reach PUT")
			assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record and CID remain unchanged")
		})
	}
}
