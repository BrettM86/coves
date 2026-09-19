package blueskypost

import "time"

// ProjectMediaURLs validates media from a cached or freshly fetched result and
// builds the JSON-ready serving view without mutating the cached input.
func ProjectMediaURLs(result *BlueskyPostResult) map[string]interface{} {
	if result == nil {
		return nil
	}
	return projectPostView(result, true)
}

func projectPostView(result *BlueskyPostResult, includeQuote bool) map[string]interface{} {
	view := map[string]interface{}{
		"uri":         result.URI,
		"cid":         result.CID,
		"text":        result.Text,
		"createdAt":   result.CreatedAt.Format(time.RFC3339Nano),
		"replyCount":  result.ReplyCount,
		"repostCount": result.RepostCount,
		"likeCount":   result.LikeCount,
		"mediaCount":  result.MediaCount,
		"hasMedia":    result.HasMedia,
		"unavailable": result.Unavailable,
	}
	if result.Message != "" {
		view["message"] = result.Message
	}
	if result.Author != nil {
		view["author"] = projectAuthorView(result.Author)
	}

	images := projectImagesView(result.Images)
	if result.Images != nil {
		view["mediaCount"] = len(images)
		view["hasMedia"] = len(images) > 0
	}
	if len(images) > 0 {
		view["images"] = images
	}
	if result.Embed != nil {
		view["embed"] = projectEmbedView(result.Embed)
	}
	if includeQuote && result.QuotedPost != nil {
		view["quotedPost"] = projectPostView(result.QuotedPost, false)
	}
	return view
}

func projectAuthorView(author *Author) map[string]interface{} {
	view := map[string]interface{}{
		"did":    author.DID,
		"handle": author.Handle,
	}
	if author.DisplayName != "" {
		view["displayName"] = author.DisplayName
	}
	if isValidBlueskyCDNURL(author.Avatar) {
		view["avatar"] = author.Avatar
	}
	return view
}

func projectImagesView(images []BlueskyImage) []map[string]interface{} {
	var view []map[string]interface{}
	for _, image := range images {
		if !isValidBlueskyCDNURL(image.Thumb) || !isValidBlueskyCDNURL(image.Fullsize) {
			continue
		}
		entry := map[string]interface{}{
			"thumb":    image.Thumb,
			"fullsize": image.Fullsize,
			"alt":      image.Alt,
		}
		if image.AspectRatio != nil && image.AspectRatio.Width > 0 && image.AspectRatio.Height > 0 {
			entry["aspectRatio"] = map[string]interface{}{
				"width":  image.AspectRatio.Width,
				"height": image.AspectRatio.Height,
			}
		}
		view = append(view, entry)
	}
	return view
}

func projectEmbedView(embed *ExternalEmbed) map[string]interface{} {
	view := map[string]interface{}{"uri": embed.URI}
	if embed.Title != "" {
		view["title"] = embed.Title
	}
	if embed.Description != "" {
		view["description"] = embed.Description
	}
	if isValidBlueskyCDNURL(embed.Thumb) {
		view["thumb"] = embed.Thumb
	}
	return view
}
