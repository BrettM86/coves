package embeds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/blobs"
)

const (
	testDID = "did:plc:testcommunity"
	testPDS = "http://localhost:3001" // coves:allow-host-literal: expected-output fixture for a pure URL projection; never dialled
	testCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
)

// withProxy enables the image proxy at the given base URL for the rest of the
// test. The URL configuration is process-wide, so it is restored on cleanup —
// which also means no test in this package may call t.Parallel.
func withProxy(t *testing.T, baseURL string) {
	t.Helper()
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: baseURL})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
}

// withProxyDisabled turns the proxy off for the rest of the test — the
// configuration a self-hosted deployment gets when it opts out of proxied
// media. Process-wide, restored on cleanup; see withProxy.
func withProxyDisabled(t *testing.T) {
	t.Helper()
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: false})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
}

func blobRef(cid string) map[string]interface{} {
	return map[string]interface{}{
		"$type":    "blob",
		"ref":      map[string]interface{}{"$link": cid},
		"mimeType": "image/jpeg",
		"size":     52813,
	}
}

func TestHydrateView_External(t *testing.T) {
	t.Run("thumb becomes an embed_thumbnail proxy URL and the type becomes a view", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type": TypeExternal,
			"external": map[string]interface{}{
				"uri":   "https://example.com/article",
				"title": "An article",
				"thumb": blobRef(testCID),
			},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeExternal+"#view", embed["$type"])
		external := embed["external"].(map[string]interface{})
		assert.Equal(t,
			"https://img.coves.social/img/embed_thumbnail/plain/"+testDID+"/"+testCID,
			external["thumb"])
		assert.Equal(t, "An article", external["title"], "unrelated fields are preserved")
	})

	t.Run("gallery preview images are hydrated too", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type": TypeExternal,
			"external": map[string]interface{}{
				"uri": "https://imgur.com/a/abc",
				"images": []interface{}{
					map[string]interface{}{
						"image": blobRef(testCID),
						"alt":   "first",
					},
				},
			},
		}

		HydrateView(embed, testDID, testPDS)

		external := embed["external"].(map[string]interface{})
		images := external["images"].([]interface{})
		image := images[0].(map[string]interface{})

		assert.Equal(t,
			"https://img.coves.social/img/content_preview/plain/"+testDID+"/"+testCID,
			image["thumb"])
		assert.Equal(t,
			"https://img.coves.social/img/content_full/plain/"+testDID+"/"+testCID,
			image["fullsize"])
		assert.NotContains(t, image, "image", "the blob is replaced, not left alongside the URLs")
		assert.Equal(t, "first", image["alt"])
	})

	t.Run("an external embed without a thumb still declares the view type", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type":    TypeExternal,
			"external": map[string]interface{}{"uri": "https://example.com"},
		}

		HydrateView(embed, testDID, testPDS)

		// The served shape is the view shape whether or not media is present;
		// a client must not have to guess which schema it received.
		assert.Equal(t, TypeExternal+"#view", embed["$type"])
	})

	// The gallery is optional on #viewExternal and an empty array carries no
	// media, so it is nothing to do rather than a failure. The sibling images
	// embed treats empty as malformed (lexicon minLength: 1) — the two must not
	// share one rule just because they share a helper.
	t.Run("an empty gallery is nothing to do, not a failure", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type": TypeExternal,
			"external": map[string]interface{}{
				"uri":    "https://example.com",
				"thumb":  blobRef(testCID),
				"images": []interface{}{},
			},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeExternal+viewSuffix, embed["$type"])
		external := embed["external"].(map[string]interface{})
		assert.Equal(t,
			"https://img.coves.social/img/embed_thumbnail/plain/"+testDID+"/"+testCID,
			external["thumb"])
	})

	// The regression that made staging necessary: the thumb used to be written
	// before the gallery was attempted, so a gallery failure left a URL string
	// under a record $type — a shape matching neither schema, and unrecoverable,
	// since the rewritten thumb no longer carried a CID for a later pass.
	t.Run("a failing gallery leaves the thumb unmutated", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		thumb := blobRef(testCID)
		embed := map[string]interface{}{
			"$type": TypeExternal,
			"external": map[string]interface{}{
				"uri":   "https://example.com",
				"thumb": thumb,
				"images": []interface{}{
					map[string]interface{}{"image": map[string]interface{}{"$type": "blob"}},
				},
			},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeExternal, embed["$type"],
			"a partial projection must not claim the view type")
		external := embed["external"].(map[string]interface{})
		assert.Equal(t, thumb, external["thumb"],
			"the thumb must not be committed when the gallery cannot project")
	})

	t.Run("a gallery that is not a list fails rather than being stamped", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type": TypeExternal,
			"external": map[string]interface{}{
				"uri":    "https://example.com",
				"thumb":  blobRef(testCID),
				"images": map[string]interface{}{"not": "a list"},
			},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeExternal, embed["$type"],
			"a shape #viewExternal cannot declare must not be stamped as a view")
		external := embed["external"].(map[string]interface{})
		assert.IsType(t, map[string]interface{}{}, external["thumb"],
			"the thumb must not be committed")
	})

	t.Run("an unreadable thumb object fails without touching the gallery", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		gallery := map[string]interface{}{"image": blobRef(testCID)}
		embed := map[string]interface{}{
			"$type": TypeExternal,
			"external": map[string]interface{}{
				"uri":    "https://example.com",
				"thumb":  map[string]interface{}{"$type": "blob"},
				"images": []interface{}{gallery},
			},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeExternal, embed["$type"])
		assert.Contains(t, gallery, "image", "no commit runs when any part fails")
		assert.NotContains(t, gallery, "thumb")
	})

	t.Run("a malformed external embed is left untouched", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type":    TypeExternal,
			"external": "not-an-object",
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeExternal, embed["$type"], "no view is claimed for a shape we could not project")
	})
}

func TestHydrateView_Images(t *testing.T) {
	t.Run("each image gets both rendered sizes and keeps its metadata", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		secondCID := "bafyreicaqaqvvlyzhgqmvhkzqvmtcwrgrqzxbxnwqxqvvvxqxqvvxqxqvv"
		embed := map[string]interface{}{
			"$type": TypeImages,
			"images": []interface{}{
				map[string]interface{}{
					"image":       blobRef(testCID),
					"alt":         "a cat",
					"aspectRatio": map[string]interface{}{"width": 4, "height": 3},
				},
				map[string]interface{}{
					"image": blobRef(secondCID),
				},
			},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeImages+"#view", embed["$type"])
		images := embed["images"].([]interface{})
		require.Len(t, images, 2)

		first := images[0].(map[string]interface{})
		assert.Equal(t,
			"https://img.coves.social/img/content_preview/plain/"+testDID+"/"+testCID,
			first["thumb"])
		assert.Equal(t,
			"https://img.coves.social/img/content_full/plain/"+testDID+"/"+testCID,
			first["fullsize"])
		assert.Equal(t, "a cat", first["alt"])
		assert.Equal(t,
			map[string]interface{}{"width": 4, "height": 3},
			first["aspectRatio"],
			"aspectRatio is a client rendering hint and must survive projection")

		second := images[1].(map[string]interface{})
		assert.Contains(t, second["thumb"], secondCID)
		assert.NotContains(t, second, "alt", "absent optional fields are not invented")
	})

	t.Run("the legacy blob encoding is recognized", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		// Federated records predating the CID-link format carry the CID at the
		// top level with no ref. They are still on the network, so they still
		// reach our index.
		embed := map[string]interface{}{
			"$type": TypeImages,
			"images": []interface{}{map[string]interface{}{
				"image": map[string]interface{}{"$type": "blob", "cid": testCID, "mimeType": "image/jpeg"},
			}},
		}

		HydrateView(embed, testDID, testPDS)

		image := embed["images"].([]interface{})[0].(map[string]interface{})
		assert.Contains(t, image["thumb"], testCID)
	})

	t.Run("an unrecognized ref shape is not guessed at", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		malformed := map[string]interface{}{"$type": "blob", "ref": "not-a-ref-object"}
		embed := map[string]interface{}{
			"$type":  TypeImages,
			"images": []interface{}{map[string]interface{}{"image": malformed}},
		}

		HydrateView(embed, testDID, testPDS)

		image := embed["images"].([]interface{})[0].(map[string]interface{})
		assert.Equal(t, malformed, image["image"],
			"treating the ref as a CID would emit a proxy URL that can only 400")
	})

	t.Run("hydration is idempotent", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type":  TypeImages,
			"images": []interface{}{map[string]interface{}{"image": blobRef(testCID), "alt": "a cat"}},
		}

		HydrateView(embed, testDID, testPDS)
		first := embed["images"].([]interface{})[0].(map[string]interface{})
		thumb, fullsize := first["thumb"], first["fullsize"]

		// A second pass runs on a view whose $type no longer matches a record
		// type, so it is a no-op — but even reaching the image list it would
		// find strings, not blobs.
		HydrateView(embed, testDID, testPDS)

		second := embed["images"].([]interface{})[0].(map[string]interface{})
		assert.Equal(t, thumb, second["thumb"])
		assert.Equal(t, fullsize, second["fullsize"])
	})

	t.Run("an image with no resolvable CID keeps its blob rather than losing it", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		unresolvable := map[string]interface{}{"$type": "blob", "mimeType": "image/jpeg"}
		embed := map[string]interface{}{
			"$type":  TypeImages,
			"images": []interface{}{map[string]interface{}{"image": unresolvable}},
		}

		HydrateView(embed, testDID, testPDS)

		image := embed["images"].([]interface{})[0].(map[string]interface{})
		assert.Equal(t, unresolvable, image["image"],
			"dropping the blob would make the image unrecoverable for any client")
		assert.NotContains(t, image, "thumb")
		assert.Equal(t, TypeImages, embed["$type"],
			"an embed still carrying a blob must not claim the view type")
	})

	// social.coves.embed.images#view sets minLength: 1, so an empty list is a
	// malformed record — the opposite of the optional gallery on #viewExternal.
	t.Run("an empty image list is malformed, not empty", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type":  TypeImages,
			"images": []interface{}{},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeImages, embed["$type"],
			"the view requires at least one image, so there is no view to declare")
	})

	t.Run("one unprojectable image leaves the whole set unhydrated", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		// A partially hydrated array — some entries with URLs, some with blobs —
		// satisfies neither schema, so a client would have to handle both shapes
		// inside one list. All or nothing instead.
		embed := map[string]interface{}{
			"$type": TypeImages,
			"images": []interface{}{
				map[string]interface{}{"image": blobRef(testCID)},
				map[string]interface{}{"image": map[string]interface{}{"$type": "blob"}},
			},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeImages, embed["$type"])
		first := embed["images"].([]interface{})[0].(map[string]interface{})
		assert.NotContains(t, first, "thumb", "the projectable entry is not committed either")
		assert.Contains(t, first, "image")
	})
}

// The #view suffix promises a shape: #viewImage requires thumb and fullsize,
// video#view requires a video URI. Stamping it over an embed that still carries
// blobs is worse than leaving the record type alone — a client switching on
// #view finds none of the guaranteed fields and renders nothing, with no error
// raised anywhere. This is reachable in the supported self-hosted configuration
// (proxy disabled) whenever the owning repo has no indexed PDS URL: a community
// row with a null pds_url, or a comment author who is not indexed yet.
func TestHydrateView_NeverClaimsAViewItCouldNotProduce(t *testing.T) {
	tests := []struct {
		name  string
		embed map[string]interface{}
	}{
		{
			name: "images",
			embed: map[string]interface{}{
				"$type":  TypeImages,
				"images": []interface{}{map[string]interface{}{"image": blobRef(testCID), "alt": "a cat"}},
			},
		},
		{
			name: "video",
			embed: map[string]interface{}{
				"$type": TypeVideo,
				"video": blobRef(testCID),
			},
		},
		{
			name: "external with a thumb",
			embed: map[string]interface{}{
				"$type": TypeExternal,
				"external": map[string]interface{}{
					"uri":   "https://example.com",
					"thumb": blobRef(testCID),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withProxyDisabled(t)
			recordType := tt.embed["$type"]

			// No proxy and no PDS URL: there is no URL to build from anything.
			HydrateView(tt.embed, testDID, "")

			assert.Equal(t, recordType, tt.embed["$type"],
				"the record type must survive when the view could not be produced")
		})
	}
}

// An external embed carrying no media at all is fully projectable — thumb and
// images are both optional on #viewExternal — so it still declares the view.
func TestHydrateView_ExternalWithNoMediaProjects(t *testing.T) {
	withProxyDisabled(t)

	embed := map[string]interface{}{
		"$type":    TypeExternal,
		"external": map[string]interface{}{"uri": "https://example.com", "title": "An article"},
	}

	HydrateView(embed, testDID, "")

	assert.Equal(t, TypeExternal+viewSuffix, embed["$type"])
	assert.NotContains(t, embed["external"], "thumb",
		"projection must not invent a thumb field on an embed that had none")
}

// Comments declare a narrower embed union than posts: images and post only, on
// the served view and on the create/update inputs alike. Comment records reach
// the index from the author's own repository over the firehose, which applies no
// embed validation, so a post-only embed type is a shape we can actually
// receive — and stamping it with a #view type the comment union does not declare
// would hand clients a union member they have no case for.
func TestHydrateCommentView_RestrictsToTheCommentUnion(t *testing.T) {
	t.Run("images hydrate exactly as they do on posts", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type":  TypeImages,
			"images": []interface{}{map[string]interface{}{"image": blobRef(testCID), "alt": "a cat"}},
		}

		HydrateCommentView(embed, testDID, testPDS)

		assert.Equal(t, TypeImages+viewSuffix, embed["$type"])
		image := embed["images"].([]interface{})[0].(map[string]interface{})
		assert.Equal(t, "https://img.coves.social/img/content_preview/plain/"+testDID+"/"+testCID, image["thumb"])
		assert.Equal(t, "https://img.coves.social/img/content_full/plain/"+testDID+"/"+testCID, image["fullsize"])
		assert.Equal(t, "a cat", image["alt"])
	})

	for _, tc := range []struct {
		name  string
		embed map[string]interface{}
	}{
		{
			name: "video is post-only and must be left in record shape",
			embed: map[string]interface{}{
				"$type": TypeVideo,
				"video": blobRef(testCID),
			},
		},
		{
			name: "external is post-only and must be left in record shape",
			embed: map[string]interface{}{
				"$type": TypeExternal,
				"external": map[string]interface{}{
					"uri":   "https://example.com/article",
					"thumb": blobRef(testCID),
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withProxy(t, "https://img.coves.social")
			recordType := tc.embed["$type"]

			HydrateCommentView(tc.embed, testDID, testPDS)

			assert.Equal(t, recordType, tc.embed["$type"],
				"a type outside the comment union must keep its record $type")
			assert.NotContains(t, tc.embed["$type"], viewSuffix)
		})
	}

	t.Run("the blob references survive untouched so the record stays readable", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type": TypeVideo,
			"video": blobRef(testCID),
		}

		HydrateCommentView(embed, testDID, testPDS)

		video, isBlob := embed["video"].(map[string]interface{})
		assert.True(t, isBlob, "video must still be a blob reference, not a URL string")
		assert.Equal(t, testCID, video["ref"].(map[string]interface{})["$link"])
	})

	t.Run("a nil embed is a no-op", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")
		HydrateCommentView(nil, testDID, testPDS)
	})
}

func TestHydrateView_Video(t *testing.T) {
	t.Run("the still goes through the proxy and the video goes to the PDS", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		videoCID := "bafyreivideoqvvlyzhgqmvhkzqvmtcwrgrqzxbxnwqxqvvvxqxqvvxqxqv"
		embed := map[string]interface{}{
			"$type":     TypeVideo,
			"video":     blobRef(videoCID),
			"thumbnail": blobRef(testCID),
			"duration":  42,
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypeVideo+"#view", embed["$type"])
		assert.Equal(t,
			"https://img.coves.social/img/content_preview/plain/"+testDID+"/"+testCID,
			embed["thumbnail"])

		// The image proxy cannot stream video, so this URL is the accepted
		// scanning gap recorded in the CSAM PRD (workstream 5). It must point
		// at the hosting PDS, not at the proxy, which would 400 on the blob.
		assert.Equal(t,
			blobs.HydrateBlobURL(testPDS, testDID, videoCID),
			embed["video"])
		assert.Equal(t, 42, embed["duration"])
	})

	t.Run("a video with no PDS URL keeps its blob", func(t *testing.T) {
		withProxy(t, "https://img.coves.social")

		embed := map[string]interface{}{
			"$type": TypeVideo,
			"video": blobRef(testCID),
		}

		HydrateView(embed, testDID, "")

		assert.IsType(t, map[string]interface{}{}, embed["video"],
			"without a PDS URL there is no video URL to emit, so the ref must survive")
	})
}

func TestHydrateView_ProxyDisabled(t *testing.T) {
	// The self-hosted opt-out: no proxy, so URLs address the PDS directly.
	// Exercised because it is a supported deployment, not a fallback we hope
	// never runs.
	t.Run("images fall back to direct PDS blob URLs", func(t *testing.T) {
		withProxyDisabled(t)

		embed := map[string]interface{}{
			"$type":  TypeImages,
			"images": []interface{}{map[string]interface{}{"image": blobRef(testCID)}},
		}

		HydrateView(embed, testDID, testPDS)

		image := embed["images"].([]interface{})[0].(map[string]interface{})
		direct := blobs.HydrateBlobURL(testPDS, testDID, testCID)
		assert.Equal(t, direct, image["thumb"])
		assert.Equal(t, direct, image["fullsize"])
	})

	t.Run("without a PDS URL there is nothing to fall back to, so blobs survive", func(t *testing.T) {
		withProxyDisabled(t)

		embed := map[string]interface{}{
			"$type":  TypeImages,
			"images": []interface{}{map[string]interface{}{"image": blobRef(testCID)}},
		}

		HydrateView(embed, testDID, "")

		image := embed["images"].([]interface{})[0].(map[string]interface{})
		assert.NotContains(t, image, "thumb")
		assert.Contains(t, image, "image")
	})
}

func TestHydrateView_LeavesOtherEmbedsAlone(t *testing.T) {
	withProxy(t, "https://img.coves.social")

	t.Run("post embeds carry no blobs and are projected elsewhere", func(t *testing.T) {
		embed := map[string]interface{}{
			"$type": TypePost,
			"post":  map[string]interface{}{"uri": "at://did:plc:x/app.bsky.feed.post/abc", "cid": "bafy"},
		}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, TypePost, embed["$type"])
	})

	t.Run("an unknown embed type is passed through untouched", func(t *testing.T) {
		embed := map[string]interface{}{"$type": "social.coves.embed.future", "data": "x"}

		HydrateView(embed, testDID, testPDS)

		assert.Equal(t, "social.coves.embed.future", embed["$type"])
		assert.Equal(t, "x", embed["data"])
	})

	t.Run("no owner DID means no URL can be built", func(t *testing.T) {
		embed := map[string]interface{}{
			"$type":  TypeImages,
			"images": []interface{}{map[string]interface{}{"image": blobRef(testCID)}},
		}

		HydrateView(embed, "", testPDS)

		assert.Equal(t, TypeImages, embed["$type"])
	})

	t.Run("a nil embed does not panic", func(t *testing.T) {
		assert.NotPanics(t, func() { HydrateView(nil, testDID, testPDS) })
	})
}

func TestBlobCID(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		want  string
	}{
		{"spec blob ref", blobRef(testCID), testCID},
		{"legacy top-level cid", map[string]interface{}{"$type": "blob", "cid": testCID}, testCID},
		{"unrecognized string ref", map[string]interface{}{"ref": testCID}, ""},
		{"already hydrated URL", "https://img.coves.social/img/x/plain/y/z", ""},
		{"empty link", map[string]interface{}{"ref": map[string]interface{}{"$link": ""}}, ""},
		{"non-string link", map[string]interface{}{"ref": map[string]interface{}{"$link": 42}}, ""},
		{"nil", nil, ""},
		{"number", 7, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, blobCID(tt.value))
		})
	}
}
