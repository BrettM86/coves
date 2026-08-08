package tests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// editNote is a PUBLISHED field, and removing it is a non-additive break (whole-
// branch review, P3).
//
// social.coves.community.post.update ships on main declaring an optional
// `editNote` string input. The atProto evolution rules are explicit that a
// published lexicon may only change additively — non-optional fields cannot be
// removed and, for an INPUT schema, dropping even an optional field breaks any
// client that still sends it: a stricter consumer built against the new schema
// rejects the request as carrying an unknown property. The branch deleted
// editNote outright. It must stay declared (a deprecated-optional input is fine);
// retiring it, if ever, is a new-NSID change, not an in-place deletion.
//
// Asserted against the raw schema JSON for the same reason the removedPost shape
// is: an open lexicon means no request sample can prove an optional input
// property is ABSENT — reading the schema source is the only thing that fails
// when the field is missing.

const postUpdatePath = "../internal/atproto/lexicon/social/coves/community/post/update.json"

func TestPostUpdate_StillDeclaresEditNote(t *testing.T) {
	doc := readLexiconJSON(t, postUpdatePath)

	// defs.main.input.schema.properties.editNote must still exist.
	main := mustChild(t, doc, "defs", "main")
	input := mustChild(t, main, "input")
	schema := mustChild(t, input, "schema")
	properties := mustChild(t, schema, "properties")

	editNote, ok := properties["editNote"].(map[string]interface{})
	require.Truef(t, ok,
		"social.coves.community.post.update no longer declares the `editNote` input. It ships on main (published), so removing it is a NON-ADDITIVE break: "+
			"a client that still sends editNote is rejected by a stricter consumer. Restore it as a deprecated-optional string input.")

	assert.Equalf(t, "string", editNote["type"],
		"editNote must stay a string input to remain compatible with the published shape")

	// It must NOT be required — the whole point is that it is an optional,
	// backward-compatible input.
	if required, ok := schema["required"].([]interface{}); ok {
		for _, r := range required {
			assert.NotEqualf(t, "editNote", r, "editNote must remain OPTIONAL; making it required is itself a non-additive break")
		}
	}
}

// mustChild descends one level into a lexicon tree, failing if the key is absent
// or not an object.
func mustChild(t *testing.T, node map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	current := node
	for _, key := range path {
		next, ok := current[key].(map[string]interface{})
		require.Truef(t, ok, "lexicon path element %q is missing or not an object", key)
		current = next
	}
	return current
}
