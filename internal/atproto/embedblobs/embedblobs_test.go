package embedblobs

import (
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

const (
	firstCID  = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	secondCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	thirdCID  = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
)

func testBlob(cid string) map[string]interface{} {
	return map[string]interface{}{"ref": map[string]interface{}{"$link": cid}}
}

func testImage(blob interface{}) map[string]interface{} {
	return map[string]interface{}{"image": blob}
}

func testImages(entries ...interface{}) map[string]interface{} {
	return map[string]interface{}{"$type": TypeImages, "images": entries}
}

func testBase58Alias(t *testing.T) string {
	t.Helper()
	parsed, err := cid.Decode(firstCID)
	require.NoError(t, err)
	alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	return alias
}

func TestPostReferences(t *testing.T) {
	alias := testBase58Alias(t)
	legacy := map[string]interface{}{"cid": secondCID, "mimeType": "image/png"}
	tests := []struct {
		name  string
		embed map[string]interface{}
		want  []Reference
	}{
		{
			name:  "three images in order",
			embed: testImages(testImage(testBlob(firstCID)), testImage(testBlob(secondCID)), testImage(testBlob(thirdCID))),
			want:  []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image"}, {"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image"}, {"QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG", "post_image"}},
		},
		{
			name: "external thumbnail before gallery images",
			embed: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"thumb": testBlob(firstCID), "images": []interface{}{testImage(testBlob(secondCID)), testImage(testBlob(thirdCID))},
			}},
			want: []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "external_thumb"}, {"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image"}, {"QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG", "post_image"}},
		},
		{
			name: "same CID in thumbnail and gallery has both kinds",
			embed: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"thumb": testBlob(firstCID), "images": []interface{}{testImage(testBlob(firstCID))},
			}},
			want: []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "external_thumb"}, {"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image"}},
		},
		{
			name:  "video thumbnail only",
			embed: map[string]interface{}{"$type": TypeVideo, "video": testBlob(firstCID), "thumbnail": testBlob(secondCID)},
			want:  []Reference{{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "video_thumb"}},
		},
		{name: "video without thumbnail", embed: map[string]interface{}{"$type": TypeVideo, "video": testBlob(firstCID)}},
		{
			name:  "legacy image CID",
			embed: testImages(testImage(legacy)),
			want:  []Reference{{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image"}},
		},
		{
			name:  "base58 alias canonicalized",
			embed: testImages(testImage(testBlob(alias))),
			want:  []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image"}},
		},
		{
			name: "malformed images skipped between valid images",
			embed: testImages(
				testImage(testBlob(firstCID)), "not an object", map[string]interface{}{"alt": "missing image"},
				testImage(map[string]interface{}{"ref": map[string]interface{}{}}),
				testImage(map[string]interface{}{"ref": map[string]interface{}{"$link": 5}}),
				testImage(testBlob("bafynotacid")), testImage(testBlob("")),
				testImage(testBlob(secondCID)),
			),
			want: []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image"}, {"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image"}},
		},
		{
			name: "string thumbnail skipped before valid gallery",
			embed: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"thumb": firstCID, "images": []interface{}{testImage(testBlob(secondCID))},
			}},
			want: []Reference{{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image"}},
		},
		{
			name:  "duplicate canonical and legacy alias has one reference",
			embed: testImages(testImage(testBlob(firstCID)), testImage(map[string]interface{}{"cid": alias, "mimeType": "image/png"})),
			want:  []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image"}},
		},
		{name: "nil embed"},
		{name: "empty embed", embed: map[string]interface{}{}},
		{name: "quote embed", embed: map[string]interface{}{"$type": "social.coves.embed.post", "post": map[string]interface{}{"embed": testImages(testImage(testBlob(firstCID)))}}},
		{name: "view embed", embed: map[string]interface{}{"$type": "social.coves.embed.images#view", "images": []interface{}{testImage(testBlob(firstCID))}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := PostReferences(test.embed)
			if len(test.want) == 0 {
				require.Empty(t, got)
			} else {
				require.Equal(t, test.want, got)
			}
		})
	}
}

func TestCommentReferences(t *testing.T) {
	alias := testBase58Alias(t)
	for _, test := range []struct {
		name  string
		embed map[string]interface{}
		want  []Reference
	}{
		{name: "two images", embed: testImages(testImage(testBlob(firstCID)), testImage(testBlob(secondCID))), want: []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image"}, {"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image"}}},
		{name: "legacy CID", embed: testImages(testImage(map[string]interface{}{"cid": firstCID, "mimeType": "image/png"})), want: []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image"}}},
		{name: "base58 alias canonicalized", embed: testImages(testImage(testBlob(alias))), want: []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image"}}},
		{name: "quote embed", embed: map[string]interface{}{"$type": "social.coves.embed.post", "post": map[string]interface{}{"embed": testImages(testImage(testBlob(firstCID)))}}},
		{name: "external embed outside comment union", embed: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{"thumb": testBlob(firstCID)}}},
		{name: "video embed outside comment union", embed: map[string]interface{}{"$type": TypeVideo, "video": testBlob(firstCID), "thumbnail": testBlob(secondCID)}},
		{name: "nil embed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := CommentReferences(test.embed)
			if len(test.want) == 0 {
				require.Empty(t, got)
			} else {
				require.Equal(t, test.want, got)
			}
		})
	}
}

func TestBlobCID(t *testing.T) {
	alias := testBase58Alias(t)
	for _, test := range []struct {
		name  string
		value interface{}
		want  string
	}{
		{name: "link wins over legacy CID", value: map[string]interface{}{"ref": map[string]interface{}{"$link": firstCID}, "cid": secondCID}, want: "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},
		{name: "object ref without link does not fall through", value: map[string]interface{}{"ref": map[string]interface{}{}, "cid": secondCID}},
		{name: "non-object ref uses legacy CID", value: map[string]interface{}{"ref": "x", "cid": secondCID}, want: "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"},
		{name: "legacy CID", value: map[string]interface{}{"cid": secondCID}, want: "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"},
		{name: "base58 alias is unchanged", value: map[string]interface{}{"ref": map[string]interface{}{"$link": alias}}, want: alias},
		{name: "invalid CID is unchanged", value: map[string]interface{}{"ref": map[string]interface{}{"$link": "not-a-cid"}}, want: "not-a-cid"},
		{name: "string", value: "string"},
		{name: "nil"},
		{name: "number", value: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, BlobCID(test.value))
		})
	}
}

func testHashedCID(t *testing.T, codec, hashCode uint64, data []byte, length int) string {
	t.Helper()
	hash, err := multihash.Sum(data, hashCode, length)
	require.NoError(t, err)
	return cid.NewCidV1(codec, hash).String()
}

func TestProfileReferences(t *testing.T) {
	for _, test := range []struct {
		name   string
		avatar string
		banner string
		want   []Reference
	}{
		{
			name: "avatar and banner", avatar: firstCID, banner: secondCID,
			want: []Reference{
				{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar"},
				{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner"},
			},
		},
		{name: "base58 alias canonicalized", avatar: testBase58Alias(t), want: []Reference{{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar"}}},
		{name: "CIDv0 kept verbatim", banner: thirdCID, want: []Reference{{"QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG", "banner"}}},
		{name: "malformed CID", avatar: "bafynotacid"},
		{name: "empty CIDs"},
		{name: "same CID with both kinds", avatar: firstCID, banner: firstCID, want: []Reference{
			{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar"},
			{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "banner"},
		}},
		{name: "non-sha2-256 CID", avatar: testHashedCID(t, cid.Raw, multihash.SHA2_512, []byte("sha2-512"), -1)},
		{name: "truncated sha2-256 digest", banner: testHashedCID(t, cid.Raw, multihash.SHA2_256, []byte("truncated"), 20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := ProfileReferences(test.avatar, test.banner)
			if len(test.want) == 0 {
				require.Empty(t, got)
			} else {
				require.Equal(t, test.want, got)
			}
		})
	}
}

func TestReferencesDropNonSHA256BlobCIDs(t *testing.T) {
	oversizedIdentity := testHashedCID(t, cid.Raw, multihash.IDENTITY, make([]byte, 3000), -1)
	require.Greater(t, len(oversizedIdentity), 4000)
	rejected := []string{
		oversizedIdentity,
		testHashedCID(t, cid.Raw, multihash.IDENTITY, []byte("short"), -1),
		testHashedCID(t, cid.Raw, multihash.SHA2_256, []byte("truncated"), 20),
		testHashedCID(t, cid.Raw, multihash.SHA2_512, []byte("sha2-512"), -1),
		testHashedCID(t, cid.DagCBOR, multihash.BLAKE2B_MIN+31, []byte("blake2b-256"), -1),
	}
	rawCID := testHashedCID(t, cid.Raw, multihash.SHA2_256, []byte("raw"), -1)
	require.Contains(t, rawCID, "bafkrei")

	for _, rejectedCID := range rejected {
		t.Run(rejectedCID[:min(len(rejectedCID), 24)], func(t *testing.T) {
			_, err := cid.Decode(rejectedCID)
			require.NoError(t, err, "fixture must be a decodable CID")

			images := testImages(testImage(testBlob(rawCID)), testImage(testBlob(rejectedCID)), testImage(testBlob(firstCID)), testImage(testBlob(thirdCID)))
			require.Equal(t, []Reference{{rawCID, KindPostImage}, {firstCID, KindPostImage}, {thirdCID, KindPostImage}}, PostReferences(images))
			require.Equal(t, []Reference{{rawCID, KindCommentImage}, {firstCID, KindCommentImage}, {thirdCID, KindCommentImage}}, CommentReferences(images))

			external := map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"thumb": testBlob(rejectedCID), "images": []interface{}{testImage(testBlob(secondCID))},
			}}
			require.Equal(t, []Reference{{secondCID, KindPostImage}}, PostReferences(external))
		})
	}
}

func TestExceedsImageLimit(t *testing.T) {
	entries := func(count int) []interface{} {
		images := make([]interface{}, 0, count)
		for index := 0; index < count; index++ {
			images = append(images, map[string]interface{}{"alt": "no blob"})
		}
		return images
	}
	external := func(images interface{}) map[string]interface{} {
		return map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{"images": images}}
	}
	tests := []struct {
		name      string
		embed     map[string]interface{}
		wantCount int
		exceeds   bool
	}{
		{"nil embed", nil, 0, false},
		{"images with exactly eight", map[string]interface{}{"$type": TypeImages, "images": entries(8)}, 8, false},
		{"images with nine", map[string]interface{}{"$type": TypeImages, "images": entries(9)}, 9, true},
		{"images counts entries without a valid blob", map[string]interface{}{"$type": TypeImages, "images": append(entries(8), "not an object")}, 9, true},
		{"images not an array", map[string]interface{}{"$type": TypeImages, "images": map[string]interface{}{"0": "a"}}, 0, false},
		{"external images with exactly eight", external(entries(8)), 8, false},
		{"external images with nine", external(entries(9)), 9, true},
		{"external images not an array", external("nine"), 0, false},
		{"external not an object", map[string]interface{}{"$type": TypeExternal, "external": entries(9)}, 0, false},
		{"video ignored", map[string]interface{}{"$type": TypeVideo, "images": entries(9)}, 0, false},
		{"unknown type ignored", map[string]interface{}{"$type": "social.coves.embed.post", "images": entries(9)}, 0, false},
		{"missing type ignored", map[string]interface{}{"images": entries(9)}, 0, false},
		{"top-level images on external ignored", map[string]interface{}{"$type": TypeExternal, "images": entries(9)}, 0, false},
	}
	require.Equal(t, 8, MaxEmbedImages)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantCount, ImageCount(tc.embed))
			require.Equal(t, tc.exceeds, ExceedsImageLimit(tc.embed))
		})
	}
}

func TestReferencesAndSkipped(t *testing.T) {
	alias := testBase58Alias(t)
	malformed := testImage(testBlob("bafynotacid"))
	tests := []struct {
		name        string
		embed       map[string]interface{}
		comment     bool
		wantRefs    int
		wantSkipped int
	}{
		{"nil embed", nil, false, 0, 0},
		{"all valid images", testImages(testImage(testBlob(firstCID)), testImage(testBlob(secondCID))), false, 2, 0},
		{"malformed and duplicate images", testImages(testImage(testBlob(firstCID)), testImage(testBlob(alias)), malformed, "not an object"), false, 1, 3},
		{"external without thumb", map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{"uri": "https://example.com"}}, false, 0, 0},
		{"external with malformed thumb", map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{"thumb": testBlob("bafynotacid")}}, false, 0, 1},
		{"video without thumbnail", map[string]interface{}{"$type": TypeVideo, "video": testBlob(firstCID)}, false, 0, 0},
		{"video with valid thumbnail", map[string]interface{}{"$type": TypeVideo, "thumbnail": testBlob(firstCID)}, false, 1, 0},
		{"comment malformed and duplicate images", testImages(testImage(testBlob(firstCID)), testImage(testBlob(firstCID)), malformed), true, 1, 2},
		{"comment external ignored", map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{"thumb": testBlob("bafynotacid")}}, true, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var references []Reference
			var skipped int
			if tc.comment {
				references, skipped = CommentReferencesAndSkipped(tc.embed)
				require.Equal(t, CommentReferences(tc.embed), references)
			} else {
				references, skipped = PostReferencesAndSkipped(tc.embed)
				require.Equal(t, PostReferences(tc.embed), references)
			}
			require.Len(t, references, tc.wantRefs)
			require.Equal(t, tc.wantSkipped, skipped)
		})
	}
}
