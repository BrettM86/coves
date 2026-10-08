package posts

import (
	neturl "net/url"
	"strings"

	"Coves/internal/core/blobs"
	"Coves/internal/core/embeds"
	"Coves/internal/core/imageproxy"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
	"github.com/rivo/uniseg"
)

// PreviewThumbnail projects a stored post embed once and returns its proxied
// first-image or link-card thumbnail and, for images, bounded alt text.
// It may mutate view.Embed through the shared feed projection.
func PreviewThumbnail(view *PostView) (url string, alt string) {
	config := blobs.GetImageURLConfig()
	if view == nil || !config.ProxyEnabled {
		return "", ""
	}
	base := config.ProxyBaseURL
	if config.CDNURL != "" {
		base = config.CDNURL
	}
	// HydrateImageProxyURL trims the trailing slash, turning a host-less "http://" base into "http://img/..." that passes post-projection checks.
	proxy, err := neturl.Parse(base)
	if err != nil || (proxy.Scheme != "http" && proxy.Scheme != "https") || proxy.Host == "" {
		return "", ""
	}
	embed, ok := view.Embed.(map[string]interface{})
	if !ok {
		return "", ""
	}

	var media map[string]interface{}
	var blob interface{}
	var viewType, urlField string
	switch embed["$type"] {
	case embeds.TypeExternal:
		media, ok = embed["external"].(map[string]interface{})
		if !ok {
			return "", ""
		}
		blob, viewType, urlField = media["thumb"], embeds.TypeExternal+"#view", "thumb"
	case embeds.TypeImages:
		images, isList := embed["images"].([]interface{})
		if !isList || len(images) == 0 {
			return "", ""
		}
		media, ok = images[0].(map[string]interface{})
		if !ok {
			return "", ""
		}
		blob, viewType, urlField = media["image"], embeds.TypeImages+"#view", "thumb"
		alt, _ = media["alt"].(string)
	default:
		return "", ""
	}
	if _, isBlob := blob.(map[string]interface{}); !isBlob {
		return "", ""
	}
	blobCID := embeds.BlobCID(blob)
	if len(blobCID) == 0 || len(blobCID) > 256 {
		return "", ""
	}
	if _, err := cid.Decode(blobCID); err != nil {
		return "", ""
	}
	// cid.Decode accepts every multibase; the image proxy serves only CIDs its own check accepts.
	if imageproxy.ValidateCID(blobCID) != nil {
		return "", ""
	}

	TransformBlobRefsToURLs(view)
	projectedEmbed, ok := view.Embed.(map[string]interface{})
	if !ok || projectedEmbed["$type"] != viewType {
		return "", ""
	}
	if viewType == embeds.TypeExternal+"#view" {
		media, ok = projectedEmbed["external"].(map[string]interface{})
	} else {
		images, isList := projectedEmbed["images"].([]interface{})
		if !isList || len(images) == 0 {
			return "", ""
		}
		media, ok = images[0].(map[string]interface{})
	}
	if !ok {
		return "", ""
	}
	projected, ok := media[urlField].(string)
	if !ok {
		return "", ""
	}
	if _, err := syntax.ParseURI(projected); err != nil {
		return "", ""
	}
	parsed, err := neturl.Parse(projected)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", ""
	}
	if viewType == embeds.TypeExternal+"#view" {
		return projected, ""
	}
	return projected, boundedThumbnailAlt(alt)
}

func boundedThumbnailAlt(alt string) string {
	clusters := uniseg.NewGraphemes(alt)
	end := 0
	for count := 0; count < 1000 && clusters.Next(); count++ {
		_, next := clusters.Positions()
		if next > 10000 {
			break
		}
		end = next
	}
	bounded := alt[:end]
	if strings.TrimSpace(bounded) == "" {
		return ""
	}
	return bounded
}
