package tests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lexicon half of the community `origin` field. A community's DNS handle
// cannot losslessly express which instance it lives on once it has been bridged
// (comicstrips.lemmy-world.tdpl.io is lemmy.world's comicstrips), so the profile
// record grows an optional, self-asserted `origin`, and the three views a client
// reads a community from serve it back — alongside `displayHandle`, which the
// AppView has served for as long as the views existed without ever declaring.
//
// All additive and all optional, so an existing record or consumer is
// unaffected; asserted against the schema source because an optional property
// is unobservable from record fixtures (see lexicon_removedpost_test.go).

const (
	communityProfilePath = "../internal/atproto/lexicon/social/coves/community/profile.json"
	communityDefsPath    = "../internal/atproto/lexicon/social/coves/community/defs.json"
)

// originProperty is the shape every declaration of `origin` must share.
func assertOriginProperty(t *testing.T, props map[string]interface{}, where string) {
	t.Helper()
	origin, ok := props["origin"].(map[string]interface{})
	require.Truef(t, ok, "%s has no origin property", where)
	assert.Equalf(t, "string", origin["type"], "%s: origin must be a string", where)
	assert.EqualValuesf(t, 253, origin["maxLength"], "%s: origin is a hostname, bounded at DNS's 253", where)
	assert.NotEmptyf(t, origin["description"], "%s: origin must say it is self-asserted and validated by the AppView", where)
}

func TestCommunityProfile_DeclaresOptionalOrigin(t *testing.T) {
	t.Parallel()
	doc := readLexiconJSON(t, communityProfilePath)

	defs := doc["defs"].(map[string]interface{})
	record := defs["main"].(map[string]interface{})["record"].(map[string]interface{})
	props, ok := record["properties"].(map[string]interface{})
	require.True(t, ok, "profile record has no properties")
	assertOriginProperty(t, props, "community.profile")

	required := asStrings(t, record["required"], "profile.required")
	assert.NotContains(t, required, "origin",
		"origin must stay optional: every community record already published omits it")
	assert.Contains(t, required, "name", "name is what origin is paired with; it was already required")
}

func TestCommunityViews_DeclareOriginAndDisplayHandle(t *testing.T) {
	t.Parallel()
	doc := readLexiconJSON(t, communityDefsPath)

	for _, def := range []string{"communityView", "communityViewDetailed"} {
		props := defProperties(t, doc, def)
		assertOriginProperty(t, props, def)

		displayHandle, ok := props["displayHandle"].(map[string]interface{})
		require.Truef(t, ok, "%s has no displayHandle property, though the AppView has always served one", def)
		assert.Equalf(t, "string", displayHandle["type"], "%s: displayHandle must be a string", def)

		defs := doc["defs"].(map[string]interface{})
		required := asStrings(t, defs[def].(map[string]interface{})["required"], def+".required")
		assert.NotContains(t, required, "origin", "%s: origin is optional", def)
		assert.NotContains(t, required, "displayHandle", "%s: displayHandle is optional", def)
	}
}

func TestCommunityRef_DeclaresOptionalOrigin(t *testing.T) {
	t.Parallel()
	doc := readLexiconJSON(t, postDefsPath)

	props := defProperties(t, doc, "communityRef")
	assertOriginProperty(t, props, "post.defs#communityRef")

	defs := doc["defs"].(map[string]interface{})
	required := asStrings(t, defs["communityRef"].(map[string]interface{})["required"], "communityRef.required")
	assert.NotContains(t, required, "origin", "communityRef.origin is optional")
}
