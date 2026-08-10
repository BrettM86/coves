package tests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The update lexicon's error vocabulary must name the errors the service can
// actually return, and must keep naming the ones it already published.
//
// # THE DRIFT
//
// social.coves.community.post.update declared EditWindowExpired and
// InvalidUpdate, neither of which UpdatePost has ever emitted, while the two
// codes it CAN return — ConcurrentModification (a lost swap guard: the post
// changed between the read and the write) and NoAuthorCredentials (nothing to
// sign the author's repo with) — were mapped in
// internal/api/handlers/post/errors.go and declared nowhere. A generated client
// therefore had no case for the two errors it will really see, and two cases for
// errors that do not exist.
//
// # WHY THE UNUSED ONES STAY
//
// Adding an error name is additive and safe. REMOVING one is not: a client
// generated against the published schema has a branch for it, and a stricter
// consumer built against the trimmed schema rejects a response naming it. This
// file's neighbour (lexicon_editnote_test.go) exists because exactly that
// deletion was made once and had to be reverted. So EditWindowExpired and
// InvalidUpdate are marked deprecated in their descriptions and left declared;
// retiring them, if ever, is a new-NSID change.
//
// Asserted against the raw schema JSON because an error list is unobservable
// from any response sample — only reading the schema fails when a name is
// missing.
func TestPostUpdate_DeclaresTheErrorsTheServiceCanReturn(t *testing.T) {
	declared := declaredErrorNames(t, postUpdatePath)

	for _, name := range []string{"PostNotFound", "NotAuthorized"} {
		assert.Containsf(t, declared, name,
			"social.coves.community.post.update stopped declaring %s, which the update handler still returns", name)
	}

	// The two the service returns and the lexicon did not name.
	assert.Containsf(t, declared, "ConcurrentModification",
		"UpdatePost answers a lost swap guard with ErrConcurrentModification, mapped to a 409 "+
			"ConcurrentModification in internal/api/handlers/post/errors.go. A client generated from this "+
			"lexicon has no case for the one error a concurrent edit actually produces.")
	assert.Containsf(t, declared, "NoAuthorCredentials",
		"UpdatePost answers ErrNoAuthorCredentials when nothing can sign the author's repo, mapped to a 503 "+
			"NoAuthorCredentials in internal/api/handlers/post/errors.go, and the lexicon must name it.")

	// And the two that are published, unused, and must not be deleted.
	for _, name := range []string{"EditWindowExpired", "InvalidUpdate"} {
		assert.Containsf(t, declared, name,
			"%s ships on main. Removing a declared error is a NON-ADDITIVE break — a client generated "+
				"against the published schema has a branch for it — so it stays declared (deprecated in its "+
				"description is the right answer) until a new-NSID change retires it. See "+
				"lexicon_editnote_test.go for the time this lesson was learned the expensive way.", name)
	}
}

// declaredErrorNames reads defs.main.errors[].name from a procedure lexicon.
func declaredErrorNames(t *testing.T, path string) []string {
	t.Helper()

	main := mustChild(t, readLexiconJSON(t, path), "defs", "main")
	rawErrors, ok := main["errors"].([]interface{})
	require.Truef(t, ok, "lexicon %s declares no errors array", path)

	names := make([]string, 0, len(rawErrors))
	for i, raw := range rawErrors {
		entry, ok := raw.(map[string]interface{})
		require.Truef(t, ok, "errors[%d] is not an object", i)
		name, ok := entry["name"].(string)
		require.Truef(t, ok, "errors[%d] has no name", i)
		names = append(names, name)
	}
	return names
}
