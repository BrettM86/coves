package main

import (
	"strings"
	"testing"

	"Coves/internal/core/posts"
)

// The reconciliation tool exists to REPAIR vote counts, so a subject collection
// it cannot route is a subject it silently refuses to repair — the failure mode
// is a rerun that reports success and changes nothing.
func TestVoteCountUpdateQuery_RoutesBothPostCollectionsToPosts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		collection string
		direction  string
		wantTable  string
		wantColumn string
	}{
		{"author-owned post, upvote", posts.PostV2Collection, "up", "UPDATE posts", "upvote_count"},
		{"author-owned post, downvote", posts.PostV2Collection, "down", "UPDATE posts", "downvote_count"},
		{"deprecated community-repo post, upvote", posts.LegacyPostCollection, "up", "UPDATE posts", "upvote_count"},
		{"deprecated community-repo post, downvote", posts.LegacyPostCollection, "down", "UPDATE posts", "downvote_count"},
		{"comment, upvote", commentCollection, "up", "UPDATE comments", "upvote_count"},
		{"comment, downvote", commentCollection, "down", "UPDATE comments", "downvote_count"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			query := voteCountUpdateQuery(tc.collection, tc.direction)
			if !strings.Contains(query, tc.wantTable) || !strings.Contains(query, tc.wantColumn) {
				t.Errorf("voteCountUpdateQuery(%q, %q) = %q; want a %q touching %s",
					tc.collection, tc.direction, query, tc.wantTable, tc.wantColumn)
			}
		})
	}
}

// A subject the tool does not understand must produce no statement at all: an
// UPDATE aimed at a guessed table would corrupt counts rather than leave them
// unrepaired.
func TestVoteCountUpdateQuery_UnknownCollectionUpdatesNothing(t *testing.T) {
	t.Parallel()

	for _, collection := range []string{"", "app.bsky.feed.post", "social.coves.community.acceptance"} {
		if query := voteCountUpdateQuery(collection, "up"); query != "" {
			t.Errorf("voteCountUpdateQuery(%q, \"up\") = %q; want no statement", collection, query)
		}
	}
}
