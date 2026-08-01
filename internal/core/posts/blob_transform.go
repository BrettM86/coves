package posts

import (
	"context"
	"errors"
	"log"
	"strings"

	"Coves/internal/core/blueskypost"
	"Coves/internal/core/embeds"
)

// TransformBlobRefsToURLs projects a post's embed from its record shape into
// the #view shape served to clients, replacing blob references with fetchable
// image-proxy URLs. It modifies the Embed field in place.
//
// Post embeds resolve against the community's repository: the AppView signs
// community post records into the community's PDS and uploads their blobs
// there, so the community DID owns every blob in the embed regardless of who
// authored the post.
//
// Must run before the response is written. A post whose embed still carries
// blob references forces the client to build its own blob URLs, which routes
// media around the image proxy and therefore around CSAM scanning — see
// internal/core/embeds.
func TransformBlobRefsToURLs(postView *PostView) {
	if postView == nil || postView.Embed == nil || postView.Community == nil {
		return
	}

	embedMap, ok := postView.Embed.(map[string]interface{})
	if !ok {
		return
	}

	embeds.HydrateView(embedMap, postView.Community.DID, postView.Community.PDSURL)
}

// TransformPostEmbeds enriches post embeds with resolved Bluesky post data
// This modifies the Embed field in-place, adding a "resolved" field with BlueskyPostResult
// Only processes social.coves.embed.post embeds with app.bsky.feed.post URIs
func TransformPostEmbeds(ctx context.Context, postView *PostView, blueskyService blueskypost.Service) {
	if postView == nil || postView.Embed == nil || blueskyService == nil {
		log.Printf("[DEBUG] [TRANSFORM-EMBED] Skipping: postView nil=%v, embed nil=%v, blueskyService nil=%v",
			postView == nil, postView == nil || postView.Embed == nil, blueskyService == nil)
		return
	}

	// Check if embed is a map (should be for post embeds)
	embedMap, ok := postView.Embed.(map[string]interface{})
	if !ok {
		log.Printf("[DEBUG] [TRANSFORM-EMBED] Skipping: embed is not a map (type: %T)", postView.Embed)
		return
	}

	// Check embed type
	embedType, ok := embedMap["$type"].(string)
	if !ok || embedType != "social.coves.embed.post" {
		log.Printf("[DEBUG] [TRANSFORM-EMBED] Skipping: embed type is not social.coves.embed.post (type: %v)", embedType)
		return
	}

	// Extract the post reference
	postRef, ok := embedMap["post"].(map[string]interface{})
	if !ok {
		log.Printf("[DEBUG] [TRANSFORM-EMBED] Skipping: post reference is not a map")
		return
	}

	// Get the AT-URI from the post reference
	atURI, ok := postRef["uri"].(string)
	if !ok || atURI == "" {
		log.Printf("[DEBUG] [TRANSFORM-EMBED] Skipping: AT-URI is missing or not a string")
		return
	}

	// Only process app.bsky.feed.post URIs (Bluesky posts)
	// Format: at://did:plc:xxx/app.bsky.feed.post/abc123
	if len(atURI) < 20 || atURI[:5] != "at://" {
		log.Printf("[DEBUG] [TRANSFORM-EMBED] Skipping: invalid AT-URI format: %s", atURI)
		return
	}

	// Simple check for app.bsky.feed.post collection
	// We don't want to process other types of embeds (e.g., Coves posts)
	if !strings.Contains(atURI, "/app.bsky.feed.post/") {
		log.Printf("[DEBUG] [TRANSFORM-EMBED] Skipping: not a Bluesky post (URI: %s)", atURI)
		return
	}

	// Resolve the Bluesky post
	result, err := blueskyService.ResolvePost(ctx, atURI)
	if err != nil {
		// Log the error but don't fail - set unavailable instead
		log.Printf("[TRANSFORM-EMBED] Failed to resolve Bluesky post %s: %v", atURI, err)

		// Differentiate between temporary and permanent failures using typed errors
		errorMessage := "This Bluesky post is unavailable"
		retryable := false

		// Check if it's a circuit breaker error (temporary/retryable)
		if errors.Is(err, blueskypost.ErrCircuitOpen) {
			errorMessage = "Bluesky is temporarily unavailable, please try again later"
			retryable = true
		} else if errors.Is(err, context.DeadlineExceeded) {
			errorMessage = "Failed to load Bluesky post, please try again"
			retryable = true
		} else if strings.Contains(err.Error(), "timeout") ||
			strings.Contains(err.Error(), "temporary failure") {
			errorMessage = "Failed to load Bluesky post, please try again"
			retryable = true
		}

		embedMap["resolved"] = map[string]interface{}{
			"unavailable": true,
			"message":     errorMessage,
			"retryable":   retryable,
		}
		embedMap["$type"] = "social.coves.embed.post#view"
		return
	}

	// Add resolved data to embed
	embedMap["resolved"] = result
	embedMap["$type"] = "social.coves.embed.post#view"
}
