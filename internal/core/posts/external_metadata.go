package posts

import (
	"fmt"
	"strings"

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
	externalDomainMaxBytes          = 253
	externalEmbedTypeMaxBytes       = 64
)

// mergeUnfurlMetadata copies unfurled metadata into an external embed.
// Client-supplied title and description (even empty strings) are kept; the
// type, provider and domain always come from the unfurl, replacing any client
// value. Unfurled text comes from an arbitrary third-party page, so it is fitted
// to the lexicon limits: the title, description and provider are truncated, and
// a type or domain over its cap is removed rather than cut, because a cut one is
// not meaningful.
func mergeUnfurlMetadata(external map[string]interface{}, result *unfurl.UnfurlResult) {
	if external["title"] == nil {
		external["title"] = truncateGraphemes(result.Title, externalTitleMaxGraphemes, externalTitleMaxBytes)
	}
	if external["description"] == nil {
		external["description"] = truncateGraphemes(result.Description, externalDescriptionMaxGraphemes, externalDescriptionMaxBytes)
	}
	setUnfurlMetadata(external, "embedType", result.Type, externalEmbedTypeMaxBytes)
	external["provider"] = truncateGraphemes(result.Provider, 0, externalProviderMaxBytes)
	setUnfurlMetadata(external, "domain", result.Domain, externalDomainMaxBytes)
}

// setUnfurlMetadata sets an unfurl-supplied metadata field that fits within
// maxBytes, and removes the field when the value does not fit. Invalid UTF-8 is
// replaced first, as in truncateGraphemes, so the measured length is the
// encoded one.
func setUnfurlMetadata(external map[string]interface{}, key, value string, maxBytes int) {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) > maxBytes {
		delete(external, key)
		return
	}
	external[key] = value
}

// truncateGraphemes returns the longest prefix of text made of whole grapheme
// clusters that has at most maxGraphemes clusters and maxBytes bytes. A
// maxGraphemes of 0 means no grapheme limit. Invalid UTF-8 is first replaced
// with U+FFFD, as JSON encoding would replace it, so the byte count measured
// here is the one written.
func truncateGraphemes(text string, maxGraphemes, maxBytes int) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
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
		// mergeUnfurlMetadata keeps any non-nil client value, so rejecting it
		// here gives a field-specific 400 before the record-level lexicon check.
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
