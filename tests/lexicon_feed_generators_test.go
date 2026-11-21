package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// feedLexiconDocument reads one schema file under social/coves/feed.
func feedLexiconDocument(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	return readLexiconJSON(t, filepath.Join(lexiconDir, "social", "coves", "feed", name+".json"))
}

// lookupDeclaration follows a key path through a decoded lexicon document.
func lookupDeclaration(t *testing.T, document map[string]interface{}, path []string) map[string]interface{} {
	t.Helper()
	current := document
	for i, key := range path {
		next, ok := current[key].(map[string]interface{})
		require.Truef(t, ok, "%s has no object at %s", document["id"], strings.Join(path[:i+1], "."))
		current = next
	}
	return current
}

// TestLexiconFeedGeneratorTimelineOutputGuard keeps the existing getTimeline
// response valid while feed.defs#feedViewPost gains optional fields: a
// timeline page without feedContext must still validate against the inline
// output schema getTimeline declares today.
func TestLexiconFeedGeneratorTimelineOutputGuard(t *testing.T) {
	catalog := loadFixtureCatalog(t)

	resolved, err := catalog.Resolve("social.coves.feed.getTimeline")
	require.NoError(t, err)
	query, ok := resolved.Def.(lexicon.SchemaQuery)
	require.Truef(t, ok, "getTimeline must be a query, got %T", resolved.Def)
	require.NotNil(t, query.Output, "getTimeline must declare an output")
	require.NotNil(t, query.Output.Schema, "getTimeline output must declare a schema")
	output, ok := query.Output.Schema.Inner.(lexicon.SchemaObject)
	require.Truef(t, ok, "getTimeline output is an inline object, got %T", query.Output.Schema.Inner)

	const wrapperID = "test.coves.feed.getTimelineOutput"
	addPreferencesRecord(t, catalog, wrapperID, output)

	page, err := atdata.UnmarshalJSON([]byte(`{
		"$type": "test.coves.feed.getTimelineOutput",
		"feed": [{
			"post": {
				"uri": "at://did:plc:abc123/social.coves.community.postv2/3k7a3dmb5bk2c",
				"cid": "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
				"author": {"did": "did:plc:abc123", "handle": "alice.coves.social"},
				"record": {
					"$type": "social.coves.community.postv2",
					"community": "did:plc:def456",
					"content": "Hello from the home timeline",
					"createdAt": "2026-01-01T00:00:00Z"
				},
				"community": {"did": "did:plc:def456", "name": "example"},
				"createdAt": "2026-01-01T00:00:00Z",
				"indexedAt": "2026-01-01T00:00:01Z"
			}
		}],
		"cursor": "c2NvcmU9MTIzOzNrN2EzZG1iNWJrMmM"
	}`))
	require.NoError(t, err)
	require.NoError(t, lexicon.ValidateRecord(catalog, page, wrapperID, lexicon.AllowLenientDatetime),
		"a getTimeline page without feedContext must keep validating")

	delete(page, "feed")
	require.ErrorContains(t, lexicon.ValidateRecord(catalog, page, wrapperID, lexicon.AllowLenientDatetime),
		"required field missing: feed", "getTimeline must still require feed")
}

// TestLexiconFeedGeneratorDeclarations pins what the fixtures cannot: every
// new endpoint's envelope refers to its named #output / #input def, and the
// parameter, error and knownValues declarations match the feed-generator
// design. knownValues must stay open sets, never enum.
func TestLexiconFeedGeneratorDeclarations(t *testing.T) {
	t.Run("envelopes", func(t *testing.T) {
		for _, endpoint := range []struct {
			name       string
			schemaType string
		}{
			{"getFeed", "query"},
			{"getFeedSkeleton", "query"},
			{"getCommentSkeleton", "query"},
			{"describeFeedGenerator", "query"},
			{"getFeedGenerators", "query"},
			{"sendInteractions", "procedure"},
		} {
			t.Run(endpoint.name, func(t *testing.T) {
				main := definitionMap(t, feedLexiconDocument(t, endpoint.name), "main")
				assert.Equal(t, endpoint.schemaType, main["type"])
				output, ok := main["output"].(map[string]interface{})
				require.Truef(t, ok, "%s must declare an output envelope", endpoint.name)
				requireSchemaEnvelope(t, output, "#output", endpoint.name+" output")
				if endpoint.schemaType == "procedure" {
					input, ok := main["input"].(map[string]interface{})
					require.Truef(t, ok, "%s must declare an input envelope", endpoint.name)
					requireSchemaEnvelope(t, input, "#input", endpoint.name+" input")
				}
			})
		}
	})

	timeframes := []interface{}{"hour", "day", "week", "month", "year", "all"}
	requestScopes := []interface{}{"all", "home", "community"}
	kinds := []interface{}{"posts", "comments"}
	feedContext := map[string]interface{}{"type": "string", "maxLength": 2000.0}
	reqID := map[string]interface{}{"type": "string", "maxLength": 100.0}
	tokens := []string{
		"requestLess", "requestMore", "clickthroughItem", "clickthroughAuthor", "clickthroughReposter", "clickthroughEmbed",
		"interactionSeen", "interactionLike", "interactionRepost", "interactionReply", "interactionQuote", "interactionShare",
	}
	tokenRefs := make([]interface{}, 0, len(tokens))
	for _, token := range tokens {
		tokenRefs = append(tokenRefs, "social.coves.feed.defs#"+token)
	}

	getFeedParams := []string{"defs", "main", "parameters"}
	getFeedSkeletonParams := []string{"defs", "main", "parameters"}
	param := func(params []string, name string) []string {
		return append(append([]string{}, params...), "properties", name)
	}
	outputProperty := func(name string) []string {
		return []string{"defs", "output", "properties", name}
	}

	declarations := []struct {
		file string
		path []string
		want map[string]interface{}
	}{
		{"getFeed", getFeedParams, map[string]interface{}{"required": []interface{}{"scope"}}},
		{"getFeed", param(getFeedParams, "feed"), map[string]interface{}{"type": "string", "format": "at-uri"}},
		{"getFeed", param(getFeedParams, "scope"), map[string]interface{}{"type": "string", "knownValues": requestScopes}},
		{"getFeed", param(getFeedParams, "community"), map[string]interface{}{"type": "string", "maxLength": 320.0}},
		{"getFeed", param(getFeedParams, "timeframe"), map[string]interface{}{"type": "string", "knownValues": timeframes, "default": "day"}},
		{"getFeed", param(getFeedParams, "limit"), map[string]interface{}{"type": "integer", "minimum": 1.0, "maximum": 50.0, "default": 15.0}},
		{"getFeed", param(getFeedParams, "cursor"), map[string]interface{}{"type": "string", "maxLength": 500.0}},
		{"getFeed", outputProperty("reqId"), reqID},
		{"getFeedSkeleton", getFeedSkeletonParams, map[string]interface{}{"required": []interface{}{"feed", "scope"}}},
		{"getFeedSkeleton", param(getFeedSkeletonParams, "feed"), map[string]interface{}{"type": "string", "format": "at-uri"}},
		{"getFeedSkeleton", param(getFeedSkeletonParams, "scope"), map[string]interface{}{"type": "string", "knownValues": requestScopes}},
		{"getFeedSkeleton", param(getFeedSkeletonParams, "community"), map[string]interface{}{"type": "string", "format": "did"}},
		{"getFeedSkeleton", outputProperty("reqId"), reqID},
		{"getCommentSkeleton", []string{"defs", "main", "parameters"}, map[string]interface{}{"required": []interface{}{"feed", "parents"}}},
		{"getCommentSkeleton", param([]string{"defs", "main", "parameters"}, "parents"), map[string]interface{}{
			"type": "array", "minLength": 1.0, "maxLength": 25.0,
			"items": map[string]interface{}{"type": "string", "format": "at-uri"},
		}},
		{"getCommentSkeleton", outputProperty("reqId"), reqID},
		// No default: an unset comment timeframe means all time, as getComments treats it today.
		{"getCommentSkeleton", param([]string{"defs", "main", "parameters"}, "timeframe"), map[string]interface{}{"type": "string", "knownValues": timeframes, "default": nil}},
		{"getFeed", []string{"defs", "output"}, map[string]interface{}{"required": []interface{}{"feed"}}},
		{"getFeedSkeleton", []string{"defs", "output"}, map[string]interface{}{"required": []interface{}{"feed"}}},
		{"getCommentSkeleton", []string{"defs", "output"}, map[string]interface{}{"required": []interface{}{"replies"}}},
		{"defs", []string{"defs", "feedViewPost", "properties", "feedContext"}, feedContext},
		{"defs", []string{"defs", "skeletonFeedPost", "properties", "feedContext"}, feedContext},
		{"defs", []string{"defs", "skeletonComment", "properties", "feedContext"}, feedContext},
		{"defs", []string{"defs", "interaction", "properties", "feedContext"}, feedContext},
		{"defs", []string{"defs", "interaction", "properties", "event"}, map[string]interface{}{"type": "string", "knownValues": tokenRefs}},
		{"defs", []string{"defs", "scope"}, map[string]interface{}{"type": "string", "knownValues": []interface{}{"all", "home", "community", "thread"}, "maxLength": 64.0}},
		{"defs", []string{"defs", "generatorView", "properties", "kind"}, map[string]interface{}{"type": "string", "knownValues": kinds}},
		{"defs", []string{"defs", "generatorView", "properties", "acceptsInteractions"}, map[string]interface{}{"type": "boolean"}},
		{"generator", []string{"defs", "main", "record"}, map[string]interface{}{"required": []interface{}{"did", "displayName", "kind", "scopes", "createdAt"}}},
		{"generator", []string{"defs", "main", "record", "properties", "scopes"}, map[string]interface{}{
			"type": "array", "minLength": 1.0,
			"items": map[string]interface{}{"type": "ref", "ref": "social.coves.feed.defs#scope"},
		}},
		{"generator", []string{"defs", "main", "record", "properties", "kind"}, map[string]interface{}{"type": "string", "knownValues": kinds}},
		{"generator", []string{"defs", "main", "record", "properties", "acceptsInteractions"}, map[string]interface{}{"type": "boolean"}},
	}
	for _, token := range tokens {
		declarations = append(declarations, struct {
			file string
			path []string
			want map[string]interface{}
		}{"defs", []string{"defs", token}, map[string]interface{}{"type": "token"}})
	}

	for _, declaration := range declarations {
		t.Run(declaration.file+"."+strings.Join(declaration.path, "."), func(t *testing.T) {
			got := lookupDeclaration(t, feedLexiconDocument(t, declaration.file), declaration.path)
			for key, want := range declaration.want {
				if wantList, ok := want.([]interface{}); ok {
					gotList, ok := got[key].([]interface{})
					require.Truef(t, ok, "%s must declare %s as a list", declaration.file, key)
					assert.ElementsMatch(t, wantList, gotList, "%s %s", declaration.file, key)
					continue
				}
				assert.Equal(t, want, got[key], "%s %s", declaration.file, key)
			}
			assert.NotContains(t, got, "enum", "feed-generator vocabularies stay open knownValues")
		})
	}

	t.Run("getFeed errors", func(t *testing.T) {
		main := definitionMap(t, feedLexiconDocument(t, "getFeed"), "main")
		rawErrors, ok := main["errors"].([]interface{})
		require.True(t, ok, "getFeed must declare its errors")
		names := make([]string, 0, len(rawErrors))
		for _, rawError := range rawErrors {
			declared, ok := rawError.(map[string]interface{})
			require.True(t, ok, "getFeed has a malformed error declaration")
			name, _ := declared["name"].(string)
			names = append(names, name)
		}
		assert.ElementsMatch(t,
			[]string{"UnknownFeed", "UnsupportedFeedScope", "InvalidCursor", "CommunityNotFound", "AmbiguousCommunity", "DiscoverUnavailable"},
			names)
	})

	// community.comment.getComments takes a comment feed; sort stays as the
	// deprecated alias.
	t.Run("getComments feed", func(t *testing.T) {
		document := readLexiconJSON(t, filepath.Join(lexiconDir, "social", "coves", "community", "comment", "getComments.json"))
		feed := lookupDeclaration(t, document, []string{"defs", "main", "parameters", "properties", "feed"})
		assert.Equal(t, "string", feed["type"])
		assert.Equal(t, "at-uri", feed["format"])

		main := definitionMap(t, document, "main")
		rawErrors, ok := main["errors"].([]interface{})
		require.True(t, ok, "getComments must declare its errors")
		names := make([]string, 0, len(rawErrors))
		for _, rawError := range rawErrors {
			declared, ok := rawError.(map[string]interface{})
			require.True(t, ok, "getComments has a malformed error declaration")
			name, _ := declared["name"].(string)
			names = append(names, name)
		}
		assert.Subset(t, names, []string{"UnknownFeed", "UnsupportedFeedScope"})
	})
}

// TestLexiconFeedGeneratorReferencesResolve walks every ref reachable from the
// new feed-generator schemas and the changed feed defs, query parameters
// included, and proves each one resolves in the loaded catalog.
func TestLexiconFeedGeneratorReferencesResolve(t *testing.T) {
	catalog := loadFixtureCatalog(t)
	walker := &lexiconDeclarationWalker{
		t:         t,
		documents: make(map[string]map[string]interface{}),
		visited:   make(map[string]struct{}),
		names:     make(map[string]struct{}),
	}
	for _, schemaID := range []string{
		"social.coves.feed.generator",
		"social.coves.feed.getFeed",
		"social.coves.feed.getFeedSkeleton",
		"social.coves.feed.getCommentSkeleton",
		"social.coves.feed.describeFeedGenerator",
		"social.coves.feed.getFeedGenerators",
		"social.coves.feed.sendInteractions",
	} {
		walker.walkRef(schemaID, "")
		main := definitionMap(t, walker.loadDocument(schemaID), "main")
		if parameters, ok := main["parameters"].(map[string]interface{}); ok {
			walker.walkSchema(parameters, schemaID)
		}
	}
	for _, definitionName := range []string{
		"feedViewPost", "scope", "skeletonFeedPost", "skeletonComment", "skeletonReplies", "generatorView", "interaction",
	} {
		walker.walkRef("social.coves.feed.defs#"+definitionName, "")
	}

	// Positive controls: the walk must cross local, cross-file and nested refs.
	for _, expected := range []string{
		"social.coves.feed.getFeed#output",
		"social.coves.feed.sendInteractions#input",
		"social.coves.feed.describeFeedGenerator#feed",
		"social.coves.feed.defs#skeletonComment",
		"social.coves.community.post.defs#postView",
		"social.coves.feed.defs#reasonPin",
	} {
		assert.Contains(t, walker.visited, expected, "the reference walk must reach %s", expected)
	}

	for ref := range walker.visited {
		_, err := catalog.Resolve(ref)
		assert.NoErrorf(t, err, "%s is reachable from the feed-generator schemas and must resolve", ref)
	}
}

// TestLexiconFeedGeneratorRetiredReferences keeps the two retired feed
// endpoints from being named in the code directories or feed docs except the runbook's
// retired-schemas section, which must still list both for unpublishing.
func TestLexiconFeedGeneratorRetiredReferences(t *testing.T) {
	const namespace = "social.coves" + ".feed."
	retiredIDs := []string{namespace + "get" + "All", namespace + "get" + "Community"}

	const runbookPath = "docs/LEXICON_PUBLISHING.md"
	raw, err := os.ReadFile(filepath.Join("..", runbookPath))
	require.NoError(t, err)
	retiredSection := markdownSection(string(raw), "Retired schemas")
	require.NotEmpty(t, retiredSection, "%s must keep a ## Retired schemas section", runbookPath)
	assert.Contains(t, strings.ToLower(retiredSection), "to unpublish after merge",
		"the retired-schemas section must say the feed endpoints are to unpublish after merge")

	countMentions := func(text, id string) int {
		count := 0
		for rest := text; ; {
			index := strings.Index(rest, id)
			if index == -1 {
				return count
			}
			rest = rest[index+len(id):]
			if rest == "" || !isNSIDCharacter(rest[0]) {
				count++
			}
		}
	}
	for _, id := range retiredIDs {
		assert.NotZerof(t, countMentions(retiredSection, id), "the retired-schemas section must name %s", id)
	}

	scannedSelf := false
	for _, relPath := range retiredReferenceScanFiles(t) {
		if relPath == "tests/lexicon_feed_generators_test.go" {
			scannedSelf = true
		}
		content, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(relPath)))
		require.NoError(t, err)
		text := string(content)
		for _, id := range retiredIDs {
			mentions := countMentions(text, id)
			if relPath == runbookPath {
				mentions -= countMentions(retiredSection, id)
			}
			assert.Zerof(t, mentions, "%s names retired %s outside the runbook's retired-schemas section", relPath, id)
		}
	}
	assert.True(t, scannedSelf, "the retired-reference scan must include this test file")
}

// retiredReferenceScanFiles lists the .go, .json and .md files the retired-id
// scan covers, slash-separated and relative to the repository root: every such
// file under the code directories, skipping nested checkouts, plus the
// committed docs this chunk owns or edits. docs/ is not walked because
// deliberately uncommitted PRD drafts live there and may name the retired ids.
func retiredReferenceScanFiles(t *testing.T) []string {
	t.Helper()
	files := []string{"docs/FEEDS.md", "docs/LEXICON_PUBLISHING.md"}
	for _, root := range []string{"cmd", "internal", "scripts", "tests"} {
		err := filepath.WalkDir(filepath.Join("..", root), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".go", ".json", ".md":
			default:
				return nil
			}
			relPath, err := filepath.Rel("..", path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(relPath))
			return nil
		})
		require.NoErrorf(t, err, "walking %s", root)
	}
	return files
}

// isNSIDCharacter reports whether a byte can continue an NSID name segment, so
// a longer NSID that merely starts with a retired one is not counted.
func isNSIDCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}
