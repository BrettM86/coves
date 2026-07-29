package posts

import (
	"encoding/json"
	"testing"
)

// The blockedPost union member's shape on the wire.
//
// service_get_posts_test.go already proves the SELECTION — which results become
// blockedPost, that a checker error fails closed, that the lookup is batched and
// deduplicated. What it cannot show, because Go structs hide it, is what a
// client receives: a union member is discriminated by its own fields, and this
// one's job is to say "blocked" while carrying nothing about the post it stands
// in for.
//
// The absence half is the load-bearing half. A blockedPost that also serialised
// a title, a body or a community would leak exactly the content the viewer
// blocked the author to stop seeing, and every assertion phrased as
// "results[0].Blocked is set" would still pass.
func TestBlockedPost_SerialisesWithoutAnyPostContent(t *testing.T) {
	const uri = "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/blocked1"
	const authorDID = "did:plc:blockedauthor"

	member, ok := blockedByAuthorResult(uri, authorDID).Member()
	if !ok {
		t.Fatal("a blocked result must expose exactly one union member")
	}

	encoded, err := json.Marshal(member)
	if err != nil {
		t.Fatalf("marshalling the blocked union member: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("the union member did not encode as an object: %v (%s)", err, encoded)
	}

	// blocked is a lexicon const: it is what tells a client which member of
	// (postView | blockedPost | notFoundPost) this is.
	want := map[string]string{
		"uri":       `"` + uri + `"`,
		"blocked":   "true",
		"blockedBy": `"author"`,
		"author":    `{"did":"` + authorDID + `"}`,
	}
	for key, expected := range want {
		got, present := fields[key]
		if !present {
			t.Errorf("the blockedPost is missing %q (encoded: %s)", key, encoded)
			continue
		}
		if string(got) != expected {
			t.Errorf("blockedPost.%s = %s, want %s", key, got, expected)
		}
	}

	// Nothing else may appear — not the post's record, community or title, and
	// not the notFound discriminator that would make this two union members at
	// once.
	for key := range fields {
		if _, expected := want[key]; !expected {
			t.Errorf("the blockedPost carries an unexpected field %q (encoded: %s)", key, encoded)
		}
	}
}
