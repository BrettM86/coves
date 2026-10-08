package notifications_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

const previewCID = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
const secondPreviewCID = "bafkreie5737gdxlw5i64vzichcalba3z2v5n6icifvx5xytvske7mr3hpm"
const thirdPreviewCID = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
const previewPDSURL = "https://pds.example.test"
const previewGetBlobURL = "https://pds.example.test/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Aowner&cid=bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"

// previewPostView builds a fresh stored view. Legacy blobs belong to the
// community repository; postv2 blobs belong to the author repository.
func previewPostView(rkey string, legacy bool, embed map[string]interface{}, title, body string) *posts.PostView {
	view := &posts.PostView{CID: thirdPreviewCID, Embed: embed, Record: map[string]interface{}{"title": title, "content": body},
		Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.test", Name: "Community", PDSURL: previewPDSURL}}
	if legacy {
		view.URI = "at://did:plc:community/social.coves.community.post/" + rkey
	} else {
		view.URI = "at://did:plc:owner/social.coves.community.postv2/" + rkey
		view.Author = &posts.AuthorView{DID: "did:plc:owner", Handle: "owner.test", PDSURL: previewPDSURL}
	}
	return view
}

func previewBlob(cid string) map[string]interface{} {
	return map[string]interface{}{"$type": "blob", "ref": map[string]interface{}{"$link": cid}, "mimeType": "image/jpeg", "size": 1234}
}

func previewLink(thumb interface{}) map[string]interface{} {
	return map[string]interface{}{"$type": "social.coves.embed.external", "external": map[string]interface{}{"uri": "https://article.example.test/story", "title": "Story", "thumb": thumb}}
}

func previewImages(image interface{}, alt interface{}) map[string]interface{} {
	return map[string]interface{}{"$type": "social.coves.embed.images", "images": []interface{}{map[string]interface{}{"image": image, "alt": alt}}}
}

// Each view occupies a reply, an upvote, and a post mention. The same URI is
// both root and subject on the first two, and root and record on the mention.
func previewPage(t *testing.T, views ...*posts.PostView) ([]map[string]json.RawMessage, string) {
	t.Helper()
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	rows := make([]notifications.ListedNotification, 0, len(views)*3)
	lookup := anonymousPosts{}
	commentLookup := cannedComments{}
	for index, view := range views {
		lookup[view.URI] = view
		replyURI := "at://did:plc:actor/social.coves.community.comment/reply" + string(rune('a'+index))
		commentLookup[replyURI] = &comments.Comment{URI: replyURI, CID: "bafyreply", Content: "Reply"}
		reference := notifications.ListedReference{CID: view.CID}
		rows = append(rows,
			notifications.ListedNotification{Reason: notifications.ReasonPostReply, RootPostURI: view.URI, RootPost: reference, SubjectURI: view.URI, Subject: reference, RecordURI: replyURI, Record: notifications.ListedReference{CID: "bafyreply"}, ActorDID: "did:plc:actor", SortAt: at, RecordCreatedAt: at},
			notifications.ListedNotification{Reason: notifications.ReasonUpvote, RootPostURI: view.URI, RootPost: reference, SubjectURI: view.URI, Subject: reference, UpvoteCount: 1, SortAt: at},
			notifications.ListedNotification{Reason: notifications.ReasonMention, RootPostURI: view.URI, RootPost: reference, RecordURI: view.URI, Record: reference, ActorDID: "did:plc:actor", SortAt: at, RecordCreatedAt: at},
		)
	}
	service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: rows}}, cannedProfiles{}, lookup, commentLookup)
	output, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
	require.NoError(t, err)
	require.Len(t, output.Notifications, len(rows), "every row remains listed")
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	validatePreviewOutput(t, encoded)
	var page struct {
		Notifications []map[string]json.RawMessage `json:"notifications"`
	}
	require.NoError(t, json.Unmarshal(encoded, &page))
	return page.Notifications, string(encoded)
}

// Indigo validates records rather than query outputs: wrap the actual output
// schema as a temporary record exactly as placeholderListLexicon does.
func validatePreviewOutput(t *testing.T, encoded []byte) {
	t.Helper()
	directory := filepath.Join("..", "..", "atproto", "lexicon")
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory(directory))
	raw, err := os.ReadFile(filepath.Join(directory, "social", "coves", "notification", "listNotifications.json"))
	require.NoError(t, err)
	var document struct {
		Defs struct {
			Main struct {
				Output struct {
					Schema json.RawMessage `json:"schema"`
				} `json:"output"`
			} `json:"main"`
		} `json:"defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &document))
	var output lexicon.SchemaObject
	require.NoError(t, json.Unmarshal(document.Defs.Main.Output.Schema, &output))
	const recordID = "test.coves.notification.previewListOutput"
	require.NoError(t, catalog.AddSchemaFile(lexicon.SchemaFile{Lexicon: 1, ID: recordID, Defs: map[string]lexicon.SchemaDef{
		"main": {Inner: lexicon.SchemaRecord{Type: "record", Key: "any", Record: output}},
	}}))
	data, err := atdata.UnmarshalJSON(encoded)
	require.NoError(t, err)
	data["$type"] = recordID
	require.NoError(t, lexicon.ValidateRecord(catalog, data, recordID, 0), "listNotifications#output must validate")
}

func previewReference(t *testing.T, row map[string]json.RawMessage, field string) map[string]json.RawMessage {
	t.Helper()
	var reference map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(row[field], &reference), field)
	return reference
}

func assertPreviewField(t *testing.T, reference map[string]json.RawMessage, field, expected string) {
	t.Helper()
	raw, present := reference[field]
	if expected == "" {
		require.False(t, present, "%s must be absent", field)
		return
	}
	require.True(t, present, "%s must contain %q", field, expected)
	var value string
	require.NoError(t, json.Unmarshal(raw, &value))
	require.Equal(t, expected, value, field)
}

func assertPreviewMedia(t *testing.T, rows []map[string]json.RawMessage, offset int, thumbnail, alt string) {
	t.Helper()
	for _, position := range []struct {
		index int
		field string
	}{
		{offset, "rootPost"}, {offset, "subject"}, {offset + 1, "rootPost"}, {offset + 1, "subject"},
		{offset + 2, "rootPost"}, {offset + 2, "record"},
	} {
		reference := previewReference(t, rows[position.index], position.field)
		assertPreviewField(t, reference, "thumbnail", thumbnail)
		assertPreviewField(t, reference, "thumbnailAlt", alt)
	}
}

func setPreviewImageConfig(t *testing.T, config blobs.ImageURLConfig) {
	t.Helper()
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(config)
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
}

func TestListNotifications_NoThumbnailWithoutValidatedProxiedBlob(t *testing.T) {
	const validURL = "https://img.example.test/img/embed_thumbnail/plain/did:plc:owner/bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	const imageURL = "https://img.example.test/img/content_preview/plain/did:plc:owner/bafkreie5737gdxlw5i64vzichcalba3z2v5n6icifvx5xytvske7mr3hpm"
	const attackerURL = "https://evil.example/x.jpg"
	oversized := strings.Repeat("a", 9000)
	valid := func() *posts.PostView {
		return previewPostView("valid", false, previewLink(previewBlob(previewCID)), "Valid", "Body")
	}
	cases := []struct {
		name         string
		build        func() *posts.PostView
		mediaStrings []string
	}{
		{"video still", func() *posts.PostView {
			return previewPostView("bad", false, map[string]interface{}{"$type": "social.coves.embed.video", "video": previewBlob(previewCID), "thumbnail": previewBlob(secondPreviewCID)}, "Video", "Body")
		}, nil},
		{"quoted post", func() *posts.PostView {
			return previewPostView("bad", false, map[string]interface{}{"$type": "social.coves.embed.post", "post": map[string]interface{}{"uri": "at://did:plc:owner/social.coves.community.postv2/quoted"}}, "Quote", "Body")
		}, nil},
		{"external gallery without thumb", func() *posts.PostView {
			e := previewLink(nil)
			delete(e["external"].(map[string]interface{}), "thumb")
			e["external"].(map[string]interface{})["images"] = []interface{}{map[string]interface{}{"image": previewBlob(secondPreviewCID), "alt": "leaked alt"}}
			return previewPostView("bad", false, e, "Gallery", "Body")
		}, nil},
		{"postv2 with no author", func() *posts.PostView {
			e := previewImages(previewBlob(secondPreviewCID), "leaked alt")
			first := e["images"].([]interface{})[0].(map[string]interface{})
			first["thumb"], first["fullsize"] = attackerURL, attackerURL
			p := previewPostView("bad", false, e, "Image", "Body")
			p.Author = nil
			return p
		}, []string{attackerURL}},
		{"string thumb not a URI", func() *posts.PostView { return previewPostView("bad", false, previewLink("not a URI"), "Link", "Body") }, []string{"not a URI"}},
		{"string thumb direct getBlob", func() *posts.PostView {
			return previewPostView("bad", false, previewLink(previewGetBlobURL), "Link", "Body")
		}, []string{previewGetBlobURL}},
		{"string thumb attacker URL", func() *posts.PostView { return previewPostView("bad", false, previewLink(attackerURL), "Link", "Body") }, []string{attackerURL}},
		{"string first image", func() *posts.PostView {
			return previewPostView("bad", false, previewImages(attackerURL, "leaked alt"), "Image", "Body")
		}, []string{attackerURL}},
		{"forged external view", func() *posts.PostView {
			e := previewLink(attackerURL)
			e["$type"] = "social.coves.embed.external#view"
			return previewPostView("bad", false, e, "Link", "Body")
		}, []string{attackerURL}},
		{"forged images view attacker URL", func() *posts.PostView {
			return previewPostView("bad", false, map[string]interface{}{"$type": "social.coves.embed.images#view", "images": []interface{}{map[string]interface{}{"thumb": attackerURL, "fullsize": attackerURL, "alt": "leaked alt"}}}, "Image", "Body")
		}, []string{attackerURL}},
		{"forged images view getBlob URL", func() *posts.PostView {
			return previewPostView("bad", false, map[string]interface{}{"$type": "social.coves.embed.images#view", "images": []interface{}{map[string]interface{}{"thumb": previewGetBlobURL, "fullsize": previewGetBlobURL, "alt": "leaked alt"}}}, "Image", "Body")
		}, []string{previewGetBlobURL}},
		{"valid first image malformed second", func() *posts.PostView {
			e := previewImages(previewBlob(secondPreviewCID), "leaked alt")
			first := e["images"].([]interface{})[0].(map[string]interface{})
			first["thumb"], first["fullsize"] = attackerURL, attackerURL
			e["images"] = append(e["images"].([]interface{}), "malformed second")
			return previewPostView("bad", false, e, "Image", "Body")
		}, []string{"malformed second", attackerURL}},
	}
	for _, shape := range []string{"external", "images"} {
		for _, cidCase := range []struct {
			name, cid string
			legacy    bool
		}{
			{"malformed link", "not a cid!", false}, {"short non CID link", "abcdefgh12", false},
			{"oversized link", oversized, false}, {"oversized legacy cid", oversized, true},
			// Valid CIDs in multibases the image proxy's CID check rejects.
			{"base64url multibase legacy cid", "uAVUSIOOwxEKY_BwUmvv0yJlvuSQnrkHkZJuTTKSVmRt4UrhV", true},
			{"base64 multibase legacy cid", "mAVUSIJ3+/mHdduo9yuUCOICwg3nVet8gSC1v2+J1kon2R2d7", true},
		} {
			shape, cidCase := shape, cidCase
			cases = append(cases, struct {
				name         string
				build        func() *posts.PostView
				mediaStrings []string
			}{
				name: shape + " " + cidCase.name,
				build: func() *posts.PostView {
					var blob interface{} = previewBlob(cidCase.cid)
					if cidCase.legacy {
						blob = map[string]interface{}{"cid": cidCase.cid, "mimeType": "image/jpeg"}
					}
					if shape == "external" {
						return previewPostView("bad", false, previewLink(blob), "Link", "Body")
					}
					return previewPostView("bad", false, previewImages(blob, "leaked alt"), "Image", "Body")
				},
				mediaStrings: []string{cidCase.cid},
			})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setPreviewImageConfig(t, blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test"})
			rows, encoded := previewPage(t, tc.build(), valid())
			assertPreviewMedia(t, rows, 0, "", "")
			require.NotContains(t, encoded, "leaked alt")
			for _, media := range tc.mediaStrings {
				require.NotContains(t, encoded, media)
			}
			require.NotContains(t, encoded, `"$link"`)
			require.NotContains(t, encoded, `"mimeType"`)
			assertPreviewMedia(t, rows, 3, validURL, "")
		})
	}
	t.Run("valid legacy cid is a positive image control", func(t *testing.T) {
		setPreviewImageConfig(t, blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test"})
		rows, _ := previewPage(t, previewPostView("legacyblob", false, previewImages(map[string]interface{}{"cid": secondPreviewCID, "mimeType": "image/jpeg"}, "Legacy alt"), "", "Body"))
		assertPreviewMedia(t, rows, 0, imageURL, "Legacy alt")
	})
	t.Run("one image is both root and subject", func(t *testing.T) {
		setPreviewImageConfig(t, blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test"})
		rows, _ := previewPage(t, previewPostView("shared", false, previewImages(previewBlob(secondPreviewCID), "Shared alt"), "", "Body"))
		assertPreviewMedia(t, rows, 0, imageURL, "Shared alt")
	})
	t.Run("proxy configurations", func(t *testing.T) {
		const linkURL = "https://img.example.test/img/embed_thumbnail/plain/did:plc:owner/bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
		const imageURL = "https://img.example.test/img/content_preview/plain/did:plc:owner/bafkreie5737gdxlw5i64vzichcalba3z2v5n6icifvx5xytvske7mr3hpm"
		build := func() []*posts.PostView {
			return []*posts.PostView{
				previewPostView("link", false, previewLink(previewBlob(previewCID)), "Link", "Body"),
				previewPostView("image", false, previewImages(previewBlob(secondPreviewCID), "A red kite"), "", "Body"),
			}
		}
		const cdnLinkURL = "https://cdn.example.test/img/embed_thumbnail/plain/did:plc:owner/bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
		const cdnImageURL = "https://cdn.example.test/img/content_preview/plain/did:plc:owner/bafkreie5737gdxlw5i64vzichcalba3z2v5n6icifvx5xytvske7mr3hpm"
		for _, tc := range []struct {
			name              string
			config            blobs.ImageURLConfig
			linkURL, imageURL string
		}{
			{"CDN only", blobs.ImageURLConfig{ProxyEnabled: true, CDNURL: "https://cdn.example.test"}, cdnLinkURL, cdnImageURL},
			{"host-less CDN overrides valid proxy base", blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test", CDNURL: "http://"}, "", ""},
			{"disabled", blobs.ImageURLConfig{}, "", ""},
			{"disabled with proxy base configured", blobs.ImageURLConfig{ProxyEnabled: false, ProxyBaseURL: "https://img.example.test"}, "", ""},
			{"empty proxy and CDN bases", blobs.ImageURLConfig{ProxyEnabled: true}, "", ""},
			{"non HTTP absolute URL", blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "ftp://img.example.test"}, "", ""},
			{"HTTP URL without host", blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "http://"}, "", ""},
			{"projected URL above URI length limit", blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test/" + strings.Repeat("a", 8200)}, "", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.name == "disabled" {
					setPreviewImageConfig(t, blobs.ImageURLConfig{})
					feedView := previewPostView("link", false, previewLink(previewBlob(previewCID)), "Link", "Body")
					posts.TransformBlobRefsToURLs(feedView)
					thumb := feedView.Embed.(map[string]interface{})["external"].(map[string]interface{})["thumb"]
					require.Equal(t, previewGetBlobURL, thumb, "feed still serves the direct blob URL")
				}
				setPreviewImageConfig(t, blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test"})
				control, _ := previewPage(t, build()...)
				assertPreviewMedia(t, control, 0, linkURL, "")
				assertPreviewMedia(t, control, 3, imageURL, "A red kite")
				setPreviewImageConfig(t, tc.config)
				rows, encoded := previewPage(t, build()...)
				if tc.linkURL != "" {
					assertPreviewMedia(t, rows, 0, tc.linkURL, "")
					assertPreviewMedia(t, rows, 3, tc.imageURL, "A red kite")
					return
				}
				assertPreviewMedia(t, rows, 0, "", "")
				assertPreviewMedia(t, rows, 3, "", "")
				require.NotContains(t, encoded, "A red kite")
				require.NotContains(t, encoded, previewGetBlobURL)
				require.NotContains(t, encoded, `"$link"`)
				require.NotContains(t, encoded, `"mimeType"`)
			})
		}
	})
}
