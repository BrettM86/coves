package posts

import (
	"fmt"

	"Coves/internal/core/unfurl"

	"github.com/rivo/uniseg"
)

// Length limits from social.coves.embed.external#external.
const (
	externalTitleMaxGraphemes       = 500
	externalTitleMaxBytes           = 5000
	externalDescriptionMaxGraphemes = 1000
	externalDescriptionMaxBytes     = 10000
	externalProviderMaxBytes        = 100
)

// mergeUnfurlMetadata copies unfurled metadata into an external embed.
// Client-supplied title and description (even empty strings) are kept; the
// type, provider and domain are always set. Unfurled text comes from an
// arbitrary third-party page, so it is truncated to the lexicon limits.
func mergeUnfurlMetadata(external map[string]interface{}, result *unfurl.UnfurlResult) {
	if external["title"] == nil {
		external["title"] = truncateGraphemes(result.Title, externalTitleMaxGraphemes, externalTitleMaxBytes)
	}
	if external["description"] == nil {
		external["description"] = truncateGraphemes(result.Description, externalDescriptionMaxGraphemes, externalDescriptionMaxBytes)
	}
	external["embedType"] = result.Type
	external["provider"] = truncateGraphemes(result.Provider, 0, externalProviderMaxBytes)
	external["domain"] = result.Domain
}

// truncateGraphemes returns the longest prefix of text made of whole grapheme
// clusters that has at most maxGraphemes clusters and maxBytes bytes. A
// maxGraphemes of 0 means no grapheme limit.
func truncateGraphemes(text string, maxGraphemes, maxBytes int) string {
	if len(text) <= maxBytes && (maxGraphemes == 0 || len(text) <= maxGraphemes) {
		return text
	}
	end, count := 0, 0
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		_, to := graphemes.Positions()
		if to > maxBytes || (maxGraphemes > 0 && count == maxGraphemes) {
			break
		}
		end = to
		count++
	}
	return text[:end]
}

// validateExternalTextLimits rejects a client-supplied external title or
// description that is not a string or exceeds the lexicon limits.
func validateExternalTextLimits(external map[string]interface{}) error {
	fields := []struct {
		key          string
		maxGraphemes int
		maxBytes     int
	}{
		{"title", externalTitleMaxGraphemes, externalTitleMaxBytes},
		{"description", externalDescriptionMaxGraphemes, externalDescriptionMaxBytes},
	}
	for _, field := range fields {
		raw := external[field.key]
		if raw == nil {
			// Absent or JSON null: mergeUnfurlMetadata fills it from the unfurl.
			continue
		}
		// A present-but-wrong-typed value must be rejected, not skipped:
		// mergeUnfurlMetadata keeps any non-nil client value and the record is
		// signed without lexicon validation.
		value, ok := raw.(string)
		if !ok {
			return NewValidationError("embed.external."+field.key,
				fmt.Sprintf("embed.external.%s must be a string, got %T", field.key, raw))
		}
		if len(value) > field.maxBytes || uniseg.GraphemeClusterCount(value) > field.maxGraphemes {
			return NewValidationError("embed.external."+field.key,
				fmt.Sprintf("embed.external.%s exceeds the limit of %d graphemes or %d bytes",
					field.key, field.maxGraphemes, field.maxBytes))
		}
	}
	return nil
}
