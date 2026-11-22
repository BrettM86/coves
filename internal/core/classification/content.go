package classification

import (
	"encoding/json"
	"strings"

	"Coves/internal/atproto/embedblobs"
)

// PostFields are the classified fields of a post record.
type PostFields struct {
	Title         string
	Body          string
	ExternalURI   string
	ExternalTitle string
}

// externalLink is the link part of an external embed.
type externalLink struct {
	Type     string `json:"$type"`
	External struct {
		URI   string `json:"uri"`
		Title string `json:"title"`
	} `json:"external"`
}

// PostFieldsFromRecord extracts the classified fields from a post record:
// title, body and the link URI and title of an external embed, or of the
// external attachment of an imagesWithAttachment embed. Other embeds, and an
// embed that does not decode, give no link.
func PostFieldsFromRecord(title, body *string, embedJSON string) PostFields {
	var fields PostFields
	if title != nil {
		fields.Title = *title
	}
	if body != nil {
		fields.Body = *body
	}
	if embedJSON == "" {
		return fields
	}
	var embed struct {
		externalLink
		Attachment externalLink `json:"attachment"`
	}
	if err := json.Unmarshal([]byte(embedJSON), &embed); err != nil {
		return fields
	}
	link := embed.externalLink
	if embed.Type == embedblobs.TypeImagesWithAttachment {
		link = embed.Attachment
	}
	if link.Type != embedblobs.TypeExternal {
		return fields
	}
	fields.ExternalURI = link.External.URI
	fields.ExternalTitle = link.External.Title
	return fields
}

// Content is what a classifier is asked about.
type Content struct {
	Text          string
	TextTruncated bool
	Images        []Image
}

// NewPostContent builds the classifier content for a post: the non-empty
// parts title, body, external title and external URI joined by blank lines,
// cut to charLimit runes. TextTruncated reports that runes were dropped.
func NewPostContent(fields PostFields, charLimit int) Content {
	return joinedContent([]string{fields.Title, fields.Body, fields.ExternalTitle, fields.ExternalURI}, charLimit)
}

// NewCommentContent builds the classifier content for a comment: its body,
// cut to charLimit runes.
func NewCommentContent(body string, charLimit int) Content {
	return joinedContent([]string{body}, charLimit)
}

// CommunityFields are the classified fields of a community profile.
type CommunityFields struct{ Name, DisplayName, Description string }

// NewCommunityContent builds the classifier content for a community: the
// non-empty name, display name and description joined by blank lines, cut to
// charLimit runes.
func NewCommunityContent(fields CommunityFields, charLimit int) Content {
	return joinedContent([]string{fields.Name, fields.DisplayName, fields.Description}, charLimit)
}

// joinedContent joins the non-empty parts with blank lines and cuts the text
// to charLimit runes, setting TextTruncated when runes were dropped.
func joinedContent(parts []string, charLimit int) Content {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	text := strings.Join(kept, "\n\n")
	runes := []rune(text)
	if len(runes) <= charLimit {
		return Content{Text: text}
	}
	return Content{Text: string(runes[:charLimit]), TextTruncated: true}
}
