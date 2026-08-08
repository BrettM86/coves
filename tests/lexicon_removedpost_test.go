package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lexicon half of task 7's read-path rebuild (PRD §3.4, §6.2). post.get
// grows one union member and postView grows two optional fields, all additive
// so no published consumer breaks:
//
//   - social.coves.community.post.get's output union gains #removedPost, the
//     mirror of #notFoundPost/#blockedPost — a removed post is served as a
//     tombstone carrying its removal code, not silently omitted like a
//     never-indexed one, so a client can render "removed by moderators: spam"
//     rather than a blank permalink.
//   - social.coves.community.post.defs gains the #removedPost object.
//   - postView gains optional `status` and `acceptanceUri`, the per-community
//     admission context an author's own view renders (PRD §6.2). Optional, so a
//     public postView that omits them still validates.
//
// These are asserted against the raw schema JSON rather than the indigo catalog
// because the properties in question are UNOBSERVABLE from record fixtures:
// atproto lexicons are open, so no data sample can prove a union gained a member
// or an object gained an optional field. Reading the schema source is the only
// thing that fails when the additive change is missing — which is the whole
// point of pinning it at T0.

// postDefsPath and postGetPath are the two schema files this task edits.
const (
	postDefsPath = "../internal/atproto/lexicon/social/coves/community/post/defs.json"
	postGetPath  = "../internal/atproto/lexicon/social/coves/community/post/get.json"

	removedPostRef = "social.coves.community.post.defs#removedPost"
)

// admissionStatusValues is the closed vocabulary the admissions table and
// getStatus already speak (migration 034, status.go). postView.status must offer
// the same set so a client switching on it switches on the real state machine.
var admissionStatusValues = []string{"pending", "accepted", "pending_reacceptance", "rejected", "removed"}

// readLexiconJSON parses a schema file into a generic tree for shape assertions.
func readLexiconJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path))
	require.NoErrorf(t, err, "reading lexicon %s", path)
	var doc map[string]interface{}
	require.NoErrorf(t, json.Unmarshal(raw, &doc), "parsing lexicon %s", path)
	return doc
}

// defProperties reaches defs.<name>.properties, failing the test if the def or
// its properties object is missing.
func defProperties(t *testing.T, doc map[string]interface{}, defName string) map[string]interface{} {
	t.Helper()
	defs, ok := doc["defs"].(map[string]interface{})
	require.True(t, ok, "lexicon has no defs object")
	def, ok := defs[defName].(map[string]interface{})
	require.Truef(t, ok, "lexicon has no def %q", defName)
	props, ok := def["properties"].(map[string]interface{})
	require.Truef(t, ok, "def %q has no properties object", defName)
	return props
}

func asStrings(t *testing.T, v interface{}, what string) []string {
	t.Helper()
	arr, ok := v.([]interface{})
	require.Truef(t, ok, "%s is not an array (got %T)", what, v)
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		require.Truef(t, ok, "%s contains a non-string element %v", what, item)
		out = append(out, s)
	}
	return out
}

// TestPostGetUnionCarriesRemovedPost pins that post.get's output union offers
// #removedPost. A removed post that came back as #notFoundPost would tell the
// author their post vanished; the distinct member is what lets the client
// explain the removal.
func TestPostGetUnionCarriesRemovedPost(t *testing.T) {
	doc := readLexiconJSON(t, postGetPath)

	main, ok := doc["defs"].(map[string]interface{})["main"].(map[string]interface{})
	require.True(t, ok, "get.json has no main def")
	output := main["output"].(map[string]interface{})
	schema := output["schema"].(map[string]interface{})
	props := schema["properties"].(map[string]interface{})
	postsProp, ok := props["posts"].(map[string]interface{})
	require.True(t, ok, "get.json main output has no posts property")
	items, ok := postsProp["items"].(map[string]interface{})
	require.True(t, ok, "posts property has no items")

	refs := asStrings(t, items["refs"], "post.get union refs")
	assert.Containsf(t, refs, removedPostRef,
		"post.get's output union does not offer %s. A removed post must be served as its own tombstone member carrying "+
			"the removal code, not collapsed into notFoundPost (which is indistinguishable from 'never existed') or "+
			"silently omitted", removedPostRef)
}

// TestRemovedPostDefShape pins the #removedPost object: it mirrors notFoundPost
// (uri + a const discriminator) and additionally carries the removal `code` a
// client renders to the author.
func TestRemovedPostDefShape(t *testing.T) {
	doc := readLexiconJSON(t, postDefsPath)

	defs := doc["defs"].(map[string]interface{})
	def, ok := defs["removedPost"].(map[string]interface{})
	require.True(t, ok, "post/defs.json has no removedPost def — the union member has nothing to resolve to")

	required := asStrings(t, def["required"], "removedPost required")
	assert.Contains(t, required, "uri", "removedPost must echo the URI it is about")
	assert.Contains(t, required, "removed", "removedPost must carry a boolean discriminator, like notFoundPost/blockedPost")

	props := def["properties"].(map[string]interface{})
	require.Contains(t, props, "code",
		"removedPost must carry the removal code — a removal without one is an unexplained disappearance (PRD §3.3)")

	removedDiscriminator, ok := props["removed"].(map[string]interface{})
	require.True(t, ok, "removedPost has no removed property")
	assert.Equal(t, true, removedDiscriminator["const"],
		"the removed discriminator must be const true, so a client can tell this union member apart structurally")
}

// TestPostViewCarriesAdmissionContext pins the two optional fields postView
// gains: status and acceptanceUri (PRD §6.2). Both must be OPTIONAL — a public
// postView that omits them still validates — and status must offer the full
// admission vocabulary so an author's client renders the real state.
func TestPostViewCarriesAdmissionContext(t *testing.T) {
	doc := readLexiconJSON(t, postDefsPath)

	props := defProperties(t, doc, "postView")

	status, ok := props["status"].(map[string]interface{})
	require.True(t, ok, "postView has no status property — an author's own view cannot render 'pending'/'removed' without it")
	assert.Equal(t, "string", status["type"], "postView.status must be a string")
	known := asStrings(t, status["knownValues"], "postView.status knownValues")
	assert.ElementsMatchf(t, admissionStatusValues, known,
		"postView.status must offer the same admission vocabulary the table and getStatus speak (%v), so a client "+
			"switches on the real state machine and not a display translation", admissionStatusValues)

	acceptanceURI, ok := props["acceptanceUri"].(map[string]interface{})
	require.True(t, ok, "postView has no acceptanceUri property — a client cannot follow the acceptance to verify it")
	assert.Equal(t, "string", acceptanceURI["type"])
	assert.Equal(t, "at-uri", acceptanceURI["format"], "acceptanceUri must be an at-uri so it resolves to the acceptance record")

	// Both fields are ADDITIVE-OPTIONAL: a postView without them must still be
	// valid, or every existing consumer breaks. postView's required set is the
	// published seven and must not have grown.
	defs := doc["defs"].(map[string]interface{})
	postView := defs["postView"].(map[string]interface{})
	required := asStrings(t, postView["required"], "postView required")
	assert.NotContains(t, required, "status", "status must be optional — a public postView omits it")
	assert.NotContains(t, required, "acceptanceUri", "acceptanceUri must be optional — only an accepted post has one")
	assert.ElementsMatch(t, []string{"uri", "cid", "author", "record", "community", "createdAt", "indexedAt"}, required,
		"postView's required set must stay exactly the published seven; the admission context is additive-optional")
}
