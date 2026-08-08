package posts

import "testing"

// IsPostCollection is the one question every aggregation site asks of a subject
// URI, so its table is the specification of what "a post" means during the
// dual-collection window.
func TestIsPostCollection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		collection string
		want       bool
	}{
		{"the author-repo record every new post is written to", PostV2Collection, true},
		{"the deprecated community-repo record production still holds", LegacyPostCollection, true},
		{"comments count into their own table", "social.coves.community.comment", false},
		{"a community's acceptance is a decision about a post, not a post", AcceptanceCollection, false},
		{"a removal likewise", RemovalCollection, false},
		{"an unparseable URI yields no collection at all", "", false},
		{"a near-miss NSID must not be admitted by a prefix match", "social.coves.community.postv3", false},
		{"nor must a longer name that merely starts with the legacy one", "social.coves.community.post.get", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsPostCollection(tc.collection); got != tc.want {
				t.Errorf("IsPostCollection(%q) = %v, want %v", tc.collection, got, tc.want)
			}
		})
	}
}
