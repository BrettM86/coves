package tests

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	commentDefsPath = "../internal/atproto/lexicon/social/coves/community/comment/defs.json"
	embedPostPath   = "../internal/atproto/lexicon/social/coves/embed/post.json"

	moderatedPostRef  = "social.coves.community.post.defs#moderatedPost"
	moderationViewRef = "social.coves.moderation.defs#moderationView"
)

// TestCommentViewModerationPlaceholderShape pins PRD §14.4's output-loosening
// exception so deleted comments can retain thread position without leaking the
// removed record, while active comments continue to carry their verbatim record.
func TestCommentViewModerationPlaceholderShape(t *testing.T) {
	document := readLexiconJSON(t, commentDefsPath)
	commentView := definitionMap(t, document, "commentView")
	required := asStrings(t, commentView["required"], "commentView required")
	assert.ElementsMatch(t, []string{"uri", "cid", "author", "post", "createdAt", "indexedAt", "stats"}, required,
		"commentView must require exactly the stable placeholder fields; requiring record would prevent deleted placeholders")
	assert.ElementsMatch(t, []string{"record"}, asStrings(t, commentView["nullable"], "commentView nullable"),
		"record must be the sole nullable field so a deleted placeholder can explicitly serve record: null")

	properties := defProperties(t, document, "commentView")
	expectations := map[string]moderationPropertyExpectation{
		"isDeleted":      {schemaType: "boolean"},
		"deletionReason": {schemaType: "string", maxLength: 64, knownValues: []string{"author", "moderator"}},
		"deletedAt":      {schemaType: "string", format: "datetime"},
		"moderation":     {schemaType: "ref", reference: moderationViewRef},
	}
	for propertyName, expectation := range expectations {
		property, ok := properties[propertyName].(map[string]interface{})
		require.Truef(t, ok, "commentView.%s must exist so placeholders expose the PRD §14.4 deletion contract", propertyName)
		assertModerationProperty(t, "commentView."+propertyName, property, expectation)
		assert.NotContainsf(t, required, propertyName, "commentView.%s must remain optional for active comments", propertyName)
	}

	record, ok := properties["record"].(map[string]interface{})
	require.True(t, ok, "commentView.record must remain declared for active comments")
	assert.Contains(t, requireNonemptyDescription(t, record, "commentView.record"), "isDeleted",
		"commentView.record must document that its absence or null value is governed by isDeleted")

	blockedBy, ok := defProperties(t, document, "blockedComment")["blockedBy"].(map[string]interface{})
	require.True(t, ok, "blockedComment.blockedBy must remain declared for compatibility")
	assert.Contains(t, strings.ToLower(requireNonemptyDescription(t, blockedBy, "blockedComment.blockedBy")), "never emitted",
		"blockedComment must say moderator is never emitted because moderator removal uses a commentView placeholder")
	assert.Contains(t, asStrings(t, blockedBy["knownValues"], "blockedComment.blockedBy knownValues"), "moderator",
		"the published moderator known value must remain for compatibility even though it is never emitted")
}

// TestModeratedPostShape pins PRD §14.4's content-free instance/inherited
// tombstone so AppView moderation cannot accidentally return the removed post's
// record, title, embed, or CID.
func TestModeratedPostShape(t *testing.T) {
	document := readLexiconJSON(t, postDefsPath)
	moderatedPost := definitionMap(t, document, "moderatedPost")
	assert.Equal(t, "object", moderatedPost["type"], "moderatedPost must be an object union member")
	required := asStrings(t, moderatedPost["required"], "moderatedPost required")
	assert.ElementsMatch(t, []string{"uri", "moderation"}, required,
		"moderatedPost must require only its subject URI and public moderation state")

	properties := defProperties(t, document, "moderatedPost")
	expectations := map[string]moderationPropertyExpectation{
		"uri":        {schemaType: "string", format: "at-uri"},
		"moderation": {schemaType: "ref", reference: moderationViewRef},
		"authorDid":  {schemaType: "string", format: "did"},
		"community":  {schemaType: "ref", reference: "#communityRef"},
	}
	for propertyName, expectation := range expectations {
		property, ok := properties[propertyName].(map[string]interface{})
		require.Truef(t, ok, "moderatedPost.%s must exist to represent the public tombstone contract", propertyName)
		assertModerationProperty(t, "moderatedPost."+propertyName, property, expectation)
	}
	assert.NotContains(t, required, "authorDid", "moderatedPost.authorDid must remain optional when identity cannot be projected")
	assert.NotContains(t, required, "community", "moderatedPost.community must remain optional when no community context is available")
	for _, contentProperty := range []string{"record", "title", "embed", "cid"} {
		assert.NotContainsf(t, properties, contentProperty,
			"moderatedPost must not declare %s; a moderation tombstone carries no content", contentProperty)
	}
}

// TestPostViewCarriesActiveModeration pins PRD §14.4's additive moderation
// projection while preserving the published seven required fields, allowing an
// NSFW-only post to retain its verbatim record.
func TestPostViewCarriesActiveModeration(t *testing.T) {
	document := readLexiconJSON(t, postDefsPath)
	postView := definitionMap(t, document, "postView")
	required := asStrings(t, postView["required"], "postView required")
	assert.ElementsMatch(t, []string{"uri", "cid", "author", "record", "community", "createdAt", "indexedAt"}, required,
		"postView must retain exactly its published seven required fields when moderation is added")

	moderation, ok := defProperties(t, document, "postView")["moderation"].(map[string]interface{})
	require.True(t, ok, "postView.moderation must exist so active NSFW state can be projected without a tombstone")
	assertModerationProperty(t, "postView.moderation", moderation,
		moderationPropertyExpectation{schemaType: "ref", reference: moderationViewRef})
	assert.NotContains(t, required, "moderation", "postView.moderation must remain optional for compatibility")
}

// TestPostGetUnionCarriesModerationTombstones pins PRD §14.4's distinct
// instance-moderation and community-removal members without closing the union
// against future federated post outcomes.
func TestPostGetUnionCarriesModerationTombstones(t *testing.T) {
	document := readLexiconJSON(t, postGetPath)
	main := definitionMap(t, document, "main")
	output, ok := main["output"].(map[string]interface{})
	require.True(t, ok, "post.get must declare an output envelope")
	schema, ok := output["schema"].(map[string]interface{})
	require.True(t, ok, "post.get output must declare a schema")
	properties, ok := schema["properties"].(map[string]interface{})
	require.True(t, ok, "post.get output schema must declare properties")
	posts, ok := properties["posts"].(map[string]interface{})
	require.True(t, ok, "post.get output must declare posts")
	items, ok := posts["items"].(map[string]interface{})
	require.True(t, ok, "post.get posts must declare union items")
	refs := asStrings(t, items["refs"], "post.get posts union refs")
	assert.Contains(t, refs, moderatedPostRef,
		"post.get must return moderatedPost for instance-scoped or inherited removal")
	assert.Contains(t, refs, removedPostRef,
		"post.get must retain removedPost for the community's own removal decision")
	assert.NotEqual(t, true, items["closed"], "post.get's result union must remain open to future federated outcomes")
}

// TestContentViewModerationDescriptions pins PRD §14.4's semantic boundary:
// community removal remains removedPost, instance moderation becomes
// moderatedPost, and removed quoted content cannot be copied into embeds.
func TestContentViewModerationDescriptions(t *testing.T) {
	postDocument := readLexiconJSON(t, postDefsPath)
	removedPostDescription := strings.ToLower(requireNonemptyDescription(t,
		definitionMap(t, postDocument, "removedPost"), "removedPost"))
	assert.Contains(t, removedPostDescription, "community",
		"removedPost must identify the tombstone as the community's own decision")
	blockedBy := defProperties(t, postDocument, "blockedPost")["blockedBy"].(map[string]interface{})
	assert.Contains(t, strings.ToLower(requireNonemptyDescription(t, blockedBy, "blockedPost.blockedBy")), "community",
		"blockedPost.blockedBy must distinguish community decisions from instance moderation")

	embedDocument := readLexiconJSON(t, embedPostPath)
	resolved := defProperties(t, embedDocument, "view")["resolved"].(map[string]interface{})
	assert.Equal(t, "unknown", resolved["type"], "embed post resolution must remain an open projection")
	description := requireNonemptyDescription(t, resolved, "embed.post#view.resolved")
	assert.Contains(t, description, "post.get",
		"removed embedded Coves posts must direct clients to the tombstone returned by post.get")
	assert.Contains(t, description, "strongRef",
		"removed embedded Coves posts must retain nothing beyond their strongRef")
}

// TestContentViewModerationDeclarationPrivacy walks every declaration reachable
// from PRD §14.4's public content views so adding moderation cannot expose
// operator notes, private subjects, actors, or private classifications.
func TestContentViewModerationDeclarationPrivacy(t *testing.T) {
	publicRefs := []string{
		moderatedPostRef,
		"social.coves.community.post.defs#postView",
		"social.coves.community.comment.defs#commentView",
	}
	privateNames := []string{"privateNote", "actorDid", "privateSubject", "private_classification"}
	for _, publicRef := range publicRefs {
		t.Run(publicRef, func(t *testing.T) {
			names := collectReachablePropertyNames(t, publicRef)
			for _, privateName := range privateNames {
				assert.NotContainsf(t, names, privateName,
					"public content view %s reaches private declaration %s; this would expose admin-only moderation data",
					publicRef, privateName)
			}
		})
	}
}
