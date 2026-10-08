// Package embeds projects the atproto embed union from the shape it has on the
// wire between repositories — blob references — into the shape the AppView
// serves to clients: fetchable URLs.
//
// Doing this on the server rather than in each client is a moderation
// requirement, not a convenience. Media is meant to converge on a single
// CDN-fronted hostname, because a scanner can only match content that crosses
// its edge, and a view that hands a client a com.atproto.sync.getBlob URL
// routes around it. Centralizing URL construction here also means changing the
// CDN, adding a preset, or purging a blob needs no client release.
//
// Two limits on that claim, both deliberate and both recorded in the PRD's
// residual-gaps table:
//
//   - Video is exempt. The proxy transcodes stills and cannot stream, so
//     video#view points at the hosting PDS (see projectVideo).
//   - This projects the *view*, not the record. Post and comment responses also
//     carry the verbatim atproto record, whose embed keeps its blob references
//     — the same bytes any client can already read from the PDS. The invariant
//     is that the AppView does not construct unproxied URLs, not that a blob
//     reference never reaches a client.
//
// The CDN routing itself is workstream 3 and is configured outside this repo;
// until it is in place this package is emitting the right URLs at a hostname
// that is not yet scanned. See docs/PRD_CSAM_SCANNING.md.
package embeds

import (
	"log/slog"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"

	"Coves/internal/core/blobs"
)

// Embed union member $type discriminators, and the "#view" projections of the
// ones that carry blobs. Keep in sync with the union refs declared in
// social.coves.community.post.defs#postView and
// social.coves.community.comment.defs#commentView.
const (
	TypeImages   = "social.coves.embed.images"
	TypeVideo    = "social.coves.embed.video"
	TypeExternal = "social.coves.embed.external"
	TypePost     = "social.coves.embed.post"

	// viewSuffix marks the served projection of a record type. A view carries
	// URL strings where the record carries blobs, so it must not claim to be
	// the record type: readers key off $type, and a client that trusts the
	// record schema would look for a ref.$link that is no longer there.
	viewSuffix = "#view"
)

// Image proxy presets used for embedded media. The registry that defines their
// dimensions lives in internal/core/imageproxy; these names are the contract
// between it and the URLs we emit.
const (
	presetEmbedThumbnail = "embed_thumbnail" // 720x360 cover — external link cards
	presetContentPreview = "content_preview" // 800w contain — in-feed image
	presetContentFull    = "content_full"    // 1600w contain — lightbox / detail
)

// mutation is a single deferred write into an embed map.
//
// Projection is computed before anything is written, and the writes are applied
// only once the whole embed is known to project. Mutating as we go produced a
// torn embed: a thumbnail already rewritten to a URL under an embed that then
// failed on its gallery and kept its record $type, leaving a URI string where
// the record schema declares a blob. That state was also unrecoverable — the
// rewritten field no longer carries a CID, so a later pass could not finish the
// job. Staging the writes makes the all-or-nothing rule structural rather than
// something each helper has to remember.
type mutation func()

// HydrateView rewrites embed in place from its record shape into its #view
// shape, replacing every blob reference it understands with a URL.
//
// ownerDID and ownerPDSURL identify the repository holding the embed's blobs.
// That is the community for post embeds — the AppView signs community post
// records into the community's repo and uploads their blobs there — and the
// comment author for comment embeds, whose records live in the user's own repo.
//
// Hydration is all-or-nothing: an embed projects completely or is left exactly
// as it arrived, still carrying its blob references and its record $type. It is
// idempotent because a projected embed's $type is a #view type, which this
// function does not act on.
//
// social.coves.embed.post carries no blobs; Bluesky quotes are resolved by
// posts.TransformPostEmbeds, while Coves quotes retain only their strongRef.
func HydrateView(embed map[string]interface{}, ownerDID, ownerPDSURL string) {
	if embed == nil {
		return
	}

	embedType, _ := embed["$type"].(string)

	// Types this function does not act on are not failures and get no log. That
	// includes the #view types of the ones it does act on, which is what makes
	// it idempotent, and social.coves.embed.post, which carries no blobs.
	switch embedType {
	case TypeExternal, TypeImages, TypeVideo:
	default:
		return
	}

	// The #view suffix is a promise about shape — #viewImage requires thumb and
	// fullsize, video#view requires a video URI — so it is only stamped once the
	// projection has actually delivered that shape. Claiming the view type over
	// an embed still carrying blobs would be worse than not projecting at all: a
	// client that switched on #view would find none of the fields the schema
	// guarantees and render nothing, with no error anywhere.
	var commits []mutation
	var projected bool

	// Every path that leaves projected false falls through to the warning
	// below. These used to return early and silently — an embed whose owning
	// repository is unknown, or whose union member arrived in a shape the view
	// cannot declare, produced exactly the same missing image as the logged
	// cases with nothing anywhere to explain it.
	if ownerDID != "" {
		switch embedType {
		case TypeExternal:
			// A record that reached the index without the external object has
			// no media to hydrate and no view to declare.
			if external, isObject := embed["external"].(map[string]interface{}); isObject {
				commits, projected = projectExternal(external, ownerDID, ownerPDSURL)
			}

		case TypeImages:
			// social.coves.embed.images#view requires a non-empty list (the
			// lexicon sets minLength: 1), so an empty one is a malformed record
			// rather than an absent gallery. The sibling gallery on
			// #viewExternal is optional and treats empty as nothing-to-do; that
			// distinction lives here rather than inside projectImages, which
			// serves both.
			if images, isList := embed["images"].([]interface{}); isList && len(images) > 0 {
				commits, projected = projectImages(images, ownerDID, ownerPDSURL)
			}

		case TypeVideo:
			commits, projected = projectVideo(embed, ownerDID, ownerPDSURL)
		}
	}

	if !projected {
		// Reached whenever any part of the embed could not be turned into a
		// URL: an unknown owning repository, a blob whose encoding we do not
		// recognize, a union member in a shape the view cannot declare, an
		// empty images list, or — the configuration case — the proxy disabled
		// with no PDS URL on the owning repo to fall back to. The visible
		// symptom is a missing image, which otherwise has no explanation, so it
		// is worth a line.
		slog.Warn("[EMBED-VIEW] embed could not be projected to its view shape; serving the record shape",
			"embed_type", embedType,
			"owner_did", ownerDID,
			"owner_pds_url", ownerPDSURL,
			"proxy_enabled", blobs.GetImageURLConfig().ProxyEnabled,
		)
		return
	}

	for _, commit := range commits {
		commit()
	}
	embed["$type"] = embedType + viewSuffix
}

// HydrateCommentView projects a comment's embed, restricted to the union that
// comments actually declare.
//
// Comments carry a narrower union than posts: social.coves.embed.images and
// social.coves.embed.post, on the served view
// (social.coves.community.comment.defs#commentView) and on the create and
// update inputs alike. Posts additionally allow video and external.
//
// The restriction has to be enforced here because nothing upstream enforces it.
// Comment records live in the author's own repository and reach the index
// through the firehose, which applies no embed validation — only the create
// endpoint does, and a federated peer never goes through it. So a comment
// carrying a video or external embed is a shape we can receive, and running the
// full projection over it would stamp a #view type the comment union does not
// declare. A client switching on the union would find a member it has no case
// for; left in record shape it at least names a type the client can recognize
// and skip.
func HydrateCommentView(embed map[string]interface{}, ownerDID, ownerPDSURL string) {
	if embed == nil {
		return
	}

	// TypePost carries no blobs; anything other than images is outside
	// the comment union entirely.
	if embedType, _ := embed["$type"].(string); embedType != TypeImages {
		return
	}

	HydrateView(embed, ownerDID, ownerPDSURL)
}

// ServableEmbed reduces a stored embed to what a post or comment view may
// serve, and runs before HydrateView or HydrateCommentView projects it. It is
// the serving half of the rule moderation blocks depend on: the only media URLs
// a view carries are the ones HydrateView derives from blob references, which
// are exactly the CIDs PostBlobCIDs and CommentImageCIDs report for blocking.
//
// The stored embed is the author's record, and nothing on the firehose path
// validates it, so a URL string in it is author-written. Served verbatim it
// would point readers at an image no removal blocks, including one under the
// author's own DID that an illegal-content removal must refuse. The AppView
// itself never stores a media URL: post create rejects a string thumb, the
// unfurl path uploads thumbnails as blobs, and posts.TransformPostEmbeds
// resolves Bluesky quote previews at serve time, after this runs.
//
// So:
//   - images, video and external keep their record shape, minus any field the
//     record declares as a blob whose value is not an object (image, thumb,
//     video, thumbnail) and minus the view-only thumb and fullsize on image
//     entries, including an external gallery's.
//   - A quote, record or #view, is rebuilt to its strongRef; see projectQuote.
//   - Anything else is not served and yields nil: a stored #view of a media
//     type, a type outside the record union, an untyped or non-object embed.
//
// It edits the embed it is given. Callers pass a freshly decoded copy; the
// verbatim record decodes its own and keeps every byte.
func ServableEmbed(stored interface{}) interface{} {
	embed, isObject := stored.(map[string]interface{})
	if !isObject {
		return nil
	}

	switch embedType, _ := embed["$type"].(string); embedType {
	case TypeImages:
		dropViewImageURLs(embed["images"])
	case TypeVideo:
		dropNonBlob(embed, "video")
		dropNonBlob(embed, "thumbnail")
	case TypeExternal:
		dropNonBlob(embed, "external")
		if external, isObject := embed["external"].(map[string]interface{}); isObject {
			dropNonBlob(external, "thumb")
			dropViewImageURLs(external["images"])
		}
	case TypePost, TypePost + viewSuffix:
		return projectQuote(embed)
	default:
		return nil
	}
	return embed
}

// dropNonBlob deletes object[field] when it is present and not an object. A
// blob reference is always an object, so anything else in a blob position is
// an author-written value the view must not carry.
func dropNonBlob(object map[string]interface{}, field string) {
	value, present := object[field]
	if !present {
		return
	}
	if _, isObject := value.(map[string]interface{}); !isObject {
		delete(object, field)
	}
}

// dropViewImageURLs removes the view-only URL fields from every image entry in
// a record's image list. HydrateView writes thumb and fullsize from the entry's
// blob when it projects; left in place they would be served whenever it
// cannot.
func dropViewImageURLs(value interface{}) {
	images, _ := value.([]interface{})
	for _, entry := range images {
		image, isObject := entry.(map[string]interface{})
		if !isObject {
			continue
		}
		dropNonBlob(image, "image")
		delete(image, "thumb")
		delete(image, "fullsize")
	}
}

// projectQuote rebuilds a quote embed, record or #view, to its strongRef.
//
// D-QUOTES: Coves-quoted posts are not hydrated server-side. The served quote
// is the strongRef only, so a removed quoted post leaves no copied content;
// post.get on its URI returns #moderatedPost. A Bluesky quote is rebuilt the
// same way: posts.TransformPostEmbeds resolves its preview at serve time from
// the record type, so a stored resolved object, which only an author can have
// written, is never served in its place.
//
// Decision: record.embed is not projected. The lexicon serves record as the
// quoter's own authored record verbatim, so its embed keeps every byte the
// quoter wrote, including preview fields it forged. None of that is
// AppView-held content of the quoted post; the AppView adds resolved only at
// serve time, only for Bluesky URIs, and only to the top-level embed.
func projectQuote(embed map[string]interface{}) map[string]interface{} {
	projected := map[string]interface{}{"$type": TypePost}
	post, _ := embed["post"].(map[string]interface{})
	strongRef := make(map[string]interface{}, 2)
	if uri, ok := post["uri"].(string); ok && uri != "" {
		strongRef["uri"] = uri
	}
	if cid, ok := post["cid"].(string); ok && cid != "" {
		strongRef["cid"] = cid
	}
	if len(strongRef) > 0 {
		projected["post"] = strongRef
	}
	return projected
}

// blueskyPostCollection is the NSID of a Bluesky post record.
const blueskyPostCollection = "app.bsky.feed.post"

// IsBlueskyPostURI reports whether uri is a syntactically valid AT-URI whose
// collection segment is app.bsky.feed.post and which names a record. The
// substring elsewhere in a URI (a Coves collection's record key path, a
// fragment, a handle authority) does not make it a Bluesky post.
func IsBlueskyPostURI(uri string) bool {
	parsed, err := syntax.ParseATURI(uri)
	if err != nil {
		return false
	}
	return parsed.Collection().String() == blueskyPostCollection && parsed.RecordKey() != ""
}

// CommentImageCIDs returns the canonical CIDs of every image blob that
// HydrateCommentView can serve through the image proxy for a comment embed.
// Moderation blocks exactly these, so blocking and serving cannot drift apart.
//
// The embed is author-controlled and unvalidated, so this never fails: only a
// social.coves.embed.images embed contributes, and an entry that is not an
// object, carries no blob CID in either encoding blobCID accepts, or whose CID
// does not decode is skipped. Each CID is decoded and re-encoded in its
// canonical string form, the form the proxy route normalizes requests to, and
// the result is de-duplicated in first-seen order.
func CommentImageCIDs(embed map[string]interface{}) []string {
	if embedType, _ := embed["$type"].(string); embedType != TypeImages {
		return nil
	}
	images, _ := embed["images"].([]interface{})
	var cids []string
	seen := make(map[string]bool, len(images))
	for _, entry := range images {
		image, isObject := entry.(map[string]interface{})
		if !isObject {
			continue
		}
		parsed, err := cid.Decode(blobCID(image["image"]))
		if err != nil {
			continue
		}
		canonical := parsed.String()
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		cids = append(cids, canonical)
	}
	return cids
}

// projectExternal computes the URL-bearing fields of
// social.coves.embed.external#viewExternal: the link card thumbnail, plus the
// gallery preview images an image-hosting provider can contribute.
//
// Both are optional there, so an external embed carrying no media — including
// one whose gallery is present but empty — projects successfully. Only a blob
// we failed to turn into a URL, or a field whose shape the view cannot declare,
// fails it.
func projectExternal(external map[string]interface{}, ownerDID, ownerPDSURL string) ([]mutation, bool) {
	var commits []mutation

	if cid := blobCID(external["thumb"]); cid != "" {
		url := imageURL(ownerPDSURL, ownerDID, cid, presetEmbedThumbnail)
		if url == "" {
			return nil, false
		}
		commits = append(commits, func() { external["thumb"] = url })
	} else if _, isBlob := external["thumb"].(map[string]interface{}); isBlob {
		// A thumb object we could not read a CID from would survive into the
		// view as an object where the schema declares a URI string.
		return nil, false
	}

	raw, present := external["images"]
	if !present {
		return commits, true
	}
	images, isList := raw.([]interface{})
	if !isList {
		// Present but not a list: it would survive as a shape #viewExternal
		// does not declare.
		return nil, false
	}

	imageCommits, ok := projectImages(images, ownerDID, ownerPDSURL)
	if !ok {
		return nil, false
	}
	return append(commits, imageCommits...), true
}

// projectImages computes the #viewImage projection for every entry in the list:
// the two rendered sizes clients display — thumb for the feed, fullsize for the
// lightbox — leaving alt and aspectRatio untouched.
//
// Every entry projects or none does. A half-hydrated array — some entries with
// URLs, some still carrying blobs — satisfies neither schema and would force a
// client to handle both shapes inside one list.
//
// An empty list yields no work and succeeds; callers that require a non-empty
// list assert that themselves.
func projectImages(images []interface{}, ownerDID, ownerPDSURL string) ([]mutation, bool) {
	commits := make([]mutation, 0, len(images))

	for _, entry := range images {
		image, isObject := entry.(map[string]interface{})
		if !isObject {
			return nil, false
		}

		cid := blobCID(image["image"])
		if cid == "" {
			return nil, false
		}

		thumb := imageURL(ownerPDSURL, ownerDID, cid, presetContentPreview)
		fullsize := imageURL(ownerPDSURL, ownerDID, cid, presetContentFull)
		if thumb == "" || fullsize == "" {
			return nil, false
		}

		commits = append(commits, func() {
			delete(image, "image")
			image["thumb"] = thumb
			image["fullsize"] = fullsize
		})
	}

	return commits, true
}

// projectVideo computes social.coves.embed.video#view.
//
// The still is served through the image proxy like any other image. The video
// blob is not: the proxy decodes and re-encodes images and cannot stream video,
// so its URL points at the hosting PDS directly. That URL is the one piece of
// Coves-served media the scanning CDN never sees — the known, accepted gap
// recorded in docs/PRD_CSAM_SCANNING.md workstream 5, closed later by
// ingest-time hash matching rather than by scan-on-serve.
//
// video is required on the view; thumbnail is optional, so a video with no
// still still projects.
func projectVideo(embed map[string]interface{}, ownerDID, ownerPDSURL string) ([]mutation, bool) {
	videoCID := blobCID(embed["video"])
	if videoCID == "" {
		return nil, false
	}
	videoURL := blobs.HydrateBlobURL(ownerPDSURL, ownerDID, videoCID)
	if videoURL == "" {
		return nil, false
	}
	commits := []mutation{func() { embed["video"] = videoURL }}

	if cid := blobCID(embed["thumbnail"]); cid != "" {
		url := imageURL(ownerPDSURL, ownerDID, cid, presetContentPreview)
		if url == "" {
			return nil, false
		}
		commits = append(commits, func() { embed["thumbnail"] = url })
	} else if _, isBlob := embed["thumbnail"].(map[string]interface{}); isBlob {
		return nil, false
	}

	return commits, true
}

// imageURL renders a proxy URL for a blob under the given preset, reading the
// process-wide configuration published at startup.
func imageURL(pdsURL, did, cid, preset string) string {
	return blobs.HydrateImageURL(blobs.GetImageURLConfig(), pdsURL, did, cid, preset)
}

// blobCID extracts the CID from an atproto blob reference.
//
// It returns "" for anything that is not a blob carrying a CID, including a ref
// in a shape we do not recognize, which is left as-is rather than guessed at.
//
// Two encodings are accepted. ref.$link is the current form, and is the only
// one consulted when ref is an object — a ref object without a usable $link
// yields "" rather than falling through. A top-level cid is the legacy blob
// encoding that predates the CID-link format, used whenever ref is absent or is
// not an object; records carrying it are still in circulation on the network
// and reach our index through federation.
func blobCID(value interface{}) string {
	blob, ok := value.(map[string]interface{})
	if !ok {
		return ""
	}

	if ref, ok := blob["ref"].(map[string]interface{}); ok {
		if cid, ok := ref["$link"].(string); ok {
			return cid
		}
		return ""
	}

	if cid, ok := blob["cid"].(string); ok {
		return cid
	}

	return ""
}

// BlobCID exposes the same extraction rule to callers validating stored blobs
// before asking the view projector to turn them into URLs.
func BlobCID(value interface{}) string { return blobCID(value) }

// PostBlobCIDs returns the canonical, first-seen CIDs of post blobs served
// through the image proxy. Malformed entries are skipped; video blobs are
// served directly by the PDS, so only their thumbnails are included.
func PostBlobCIDs(embed map[string]interface{}) []string {
	var cids []string
	seen := make(map[string]bool)
	addBlob := func(value interface{}) {
		parsed, err := cid.Decode(blobCID(value))
		if err != nil {
			return
		}
		canonical := parsed.String()
		if !seen[canonical] {
			seen[canonical] = true
			cids = append(cids, canonical)
		}
	}
	addImages := func(value interface{}) {
		images, _ := value.([]interface{})
		for _, entry := range images {
			image, ok := entry.(map[string]interface{})
			if ok {
				addBlob(image["image"])
			}
		}
	}

	switch embedType, _ := embed["$type"].(string); embedType {
	case TypeImages:
		addImages(embed["images"])
	case TypeVideo:
		addBlob(embed["thumbnail"])
	case TypeExternal:
		if external, ok := embed["external"].(map[string]interface{}); ok {
			addBlob(external["thumb"])
			addImages(external["images"])
		}
	}
	return cids
}
