package tests

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const moderationDefsPath = "../internal/atproto/lexicon/social/coves/moderation/defs.json"

var publicReasonRefs = []string{
	"social.coves.moderation.defs#reasonSpam",
	"social.coves.moderation.defs#reasonHarassment",
	"social.coves.moderation.defs#reasonDoxing",
	"social.coves.moderation.defs#reasonIllegalContent",
	"social.coves.moderation.defs#reasonRuleViolation",
	"social.coves.moderation.defs#reasonModeratorDiscretion",
}

type moderationPropertyExpectation struct {
	schemaType    string
	reference     string
	itemReference string
	format        string
	knownValues   []string
	minLength     int
	maxLength     int
	maxGraphemes  int
	minimum       int
	maximum       int
	defaultValue  interface{}
	hasDefault    bool
}

type moderationDefinitionExpectation struct {
	required   []string
	properties map[string]moderationPropertyExpectation
}

func moderationObjectExpectations() map[string]moderationDefinitionExpectation {
	stringProperty := func(maxLength int, knownValues ...string) moderationPropertyExpectation {
		return moderationPropertyExpectation{schemaType: "string", maxLength: maxLength, knownValues: knownValues}
	}
	formattedString := func(format string) moderationPropertyExpectation {
		return moderationPropertyExpectation{schemaType: "string", format: format}
	}
	opaqueString := moderationPropertyExpectation{schemaType: "string", minLength: 1, maxLength: 128}
	reference := func(ref string) moderationPropertyExpectation {
		return moderationPropertyExpectation{schemaType: "ref", reference: ref}
	}
	referenceArray := func(ref string) moderationPropertyExpectation {
		return moderationPropertyExpectation{schemaType: "array", itemReference: ref, maxLength: 100}
	}

	return map[string]moderationDefinitionExpectation{
		"actionRef": {
			required: []string{"serviceDid", "actionId"},
			properties: map[string]moderationPropertyExpectation{
				"serviceDid": formattedString("did"),
				"actionId":   opaqueString,
			},
		},
		"scopeView": {
			required: []string{"kind"},
			properties: map[string]moderationPropertyExpectation{
				"kind":         stringProperty(64, "instance", "community"),
				"communityDid": formattedString("did"),
			},
		},
		"subjectRef": {
			required: []string{"uri"},
			properties: map[string]moderationPropertyExpectation{
				"uri": formattedString("at-uri"),
				"cid": formattedString("cid"),
			},
		},
		"actorRef": {
			required: []string{"did"},
			properties: map[string]moderationPropertyExpectation{
				"did": formattedString("did"),
			},
		},
		"actionView": {
			required: []string{"ref", "action", "authorityDid", "scope", "createdAt", "origin"},
			properties: map[string]moderationPropertyExpectation{
				"ref":          reference("#actionRef"),
				"action":       stringProperty(64, "remove", "restore", "apply-removal", "retract-removal", "label", "retract-label"),
				"authorityDid": formattedString("did"),
				"scope":        reference("#scopeView"),
				"createdAt":    formattedString("datetime"),
				"origin":       stringProperty(64, "local", "inherited"),
				"subject":      reference("#subjectRef"),
				"reason":       reference("#reasonType"),
				"labelValue":   {schemaType: "string", minLength: 1, maxLength: 128, knownValues: []string{"nsfw"}},
				"actor":        reference("#actorRef"),
				"reverses":     reference("#actionRef"),
				"issuedAt":     formattedString("datetime"),
			},
		},
		"adminActionView": {
			required: []string{"action"},
			properties: map[string]moderationPropertyExpectation{
				"action":         reference("#actionView"),
				"actorDid":       formattedString("did"),
				"privateSubject": reference("#subjectRef"),
				"privateNote":    {schemaType: "string", maxLength: 10000, maxGraphemes: 1000},
			},
		},
		"sourceView": {
			required: []string{"authorityDid", "scope"},
			properties: map[string]moderationPropertyExpectation{
				"authorityDid": formattedString("did"),
				"scope":        reference("#scopeView"),
				"action":       reference("#actionRef"),
				"reason":       reference("#reasonType"),
				"label":        reference("com.atproto.label.defs#label"),
			},
		},
		"moderationView": {
			required: []string{"state"},
			properties: map[string]moderationPropertyExpectation{
				"state":            stringProperty(64, "clear", "removed"),
				"sources":          referenceArray("#sourceView"),
				"sourcesTruncated": {schemaType: "boolean", defaultValue: false, hasDefault: true},
				"contentLabels":    referenceArray("#contentLabelView"),
			},
		},
		"contentLabelView": {
			required: []string{"value"},
			properties: map[string]moderationPropertyExpectation{
				"value":            {schemaType: "string", minLength: 1, maxLength: 128, knownValues: []string{"nsfw"}},
				"sources":          referenceArray("#sourceView"),
				"sourcesTruncated": {schemaType: "boolean", defaultValue: false, hasDefault: true},
			},
		},
		"localLabelView": {
			required: []string{"value", "action"},
			properties: map[string]moderationPropertyExpectation{
				"value":  {schemaType: "string", minLength: 1, maxLength: 128, knownValues: []string{"nsfw"}},
				"action": reference("#actionRef"),
			},
		},
		"subjectState": {
			required: []string{"subject", "version", "moderation", "recordState"},
			properties: map[string]moderationPropertyExpectation{
				"subject":        formattedString("at-uri"),
				"version":        opaqueString,
				"moderation":     reference("#moderationView"),
				"recordState":    stringProperty(64, "present", "deleted", "unavailable"),
				"currentSubject": reference("com.atproto.repo.strongRef"),
				"localRemoval":   reference("#actionRef"),
				"localLabels":    referenceArray("#localLabelView"),
			},
		},
		"mutationResult": {
			required: []string{"outcome", "state"},
			properties: map[string]moderationPropertyExpectation{
				"outcome": stringProperty(64, "applied", "unchanged"),
				"state":   reference("#subjectState"),
				"action":  reference("#adminActionView"),
			},
		},
	}
}

func definitionMap(t *testing.T, doc map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	defs, ok := doc["defs"].(map[string]interface{})
	require.True(t, ok, "moderation lexicon has no defs object")
	definition, ok := defs[name].(map[string]interface{})
	require.Truef(t, ok, "moderation lexicon has no def %q; the moderation contract cannot be published", name)
	return definition
}

func sortedPropertyNames(properties map[string]interface{}) []string {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func assertOptionalStringField(t *testing.T, schema map[string]interface{}, field, expected string) {
	t.Helper()
	if expected == "" {
		assert.NotContainsf(t, schema, field, "%s must be omitted when the moderation contract does not specify it", field)
		return
	}
	assert.Equal(t, expected, schema[field], "%s must match the moderation contract", field)
}

func assertOptionalNumberField(t *testing.T, schema map[string]interface{}, field string, expected int) {
	t.Helper()
	if expected == 0 {
		assert.NotContainsf(t, schema, field, "%s must be omitted when the moderation contract does not specify it", field)
		return
	}
	assert.Equal(t, float64(expected), schema[field], "%s must match the moderation contract", field)
}

func assertModerationProperty(t *testing.T, name string, property map[string]interface{}, expected moderationPropertyExpectation) {
	t.Helper()
	assert.Equal(t, expected.schemaType, property["type"], "%s must have the specified schema type", name)
	description, ok := property["description"].(string)
	assert.Truef(t, ok && strings.TrimSpace(description) != "", "%s must describe its public contract", name)
	assertOptionalStringField(t, property, "ref", expected.reference)
	assertOptionalStringField(t, property, "format", expected.format)
	assertOptionalNumberField(t, property, "minLength", expected.minLength)
	assertOptionalNumberField(t, property, "maxLength", expected.maxLength)
	assertOptionalNumberField(t, property, "maxGraphemes", expected.maxGraphemes)
	assertOptionalNumberField(t, property, "minimum", expected.minimum)
	assertOptionalNumberField(t, property, "maximum", expected.maximum)

	if expected.knownValues == nil {
		assert.NotContains(t, property, "knownValues", "%s must not gain a closed-looking documented value set", name)
	} else {
		assert.ElementsMatch(t, expected.knownValues, asStrings(t, property["knownValues"], name+" knownValues"),
			"%s knownValues must match the moderation protocol", name)
	}

	if expected.hasDefault {
		assert.Equal(t, expected.defaultValue, property["default"], "%s default must match the moderation protocol", name)
	} else {
		assert.NotContains(t, property, "default", "%s must not acquire an unspecified default", name)
	}

	if expected.itemReference == "" {
		assert.NotContains(t, property, "items", "%s must not declare array items when it is not the specified array", name)
		return
	}
	items, ok := property["items"].(map[string]interface{})
	require.Truef(t, ok, "%s must declare array items", name)
	assert.Equal(t, "ref", items["type"], "%s array items must be refs", name)
	assert.Equal(t, expected.itemReference, items["ref"], "%s array item ref must match the moderation protocol", name)
}

// TestLexiconModerationDefinitionShapes pins the complete additive moderation
// object contract. Raw schema assertions are necessary because Lexicon objects
// are open and fixtures cannot detect omitted optional declarations.
func TestLexiconModerationDefinitionShapes(t *testing.T) {
	doc := readLexiconJSON(t, moderationDefsPath)
	for name, expected := range moderationObjectExpectations() {
		t.Run(name, func(t *testing.T) {
			definition := definitionMap(t, doc, name)
			assert.Equal(t, "object", definition["type"], "%s must remain an object definition", name)
			assert.ElementsMatch(t, expected.required, asStrings(t, definition["required"], name+" required"),
				"%s required fields must match the wire contract exactly", name)

			properties := defProperties(t, doc, name)
			expectedNames := make([]string, 0, len(expected.properties))
			for propertyName := range expected.properties {
				expectedNames = append(expectedNames, propertyName)
			}
			assert.ElementsMatch(t, expectedNames, sortedPropertyNames(properties),
				"%s must declare exactly its required and optional protocol properties", name)
			for propertyName, propertyExpectation := range expected.properties {
				property, ok := properties[propertyName].(map[string]interface{})
				require.Truef(t, ok, "%s.%s is missing or is not a property schema", name, propertyName)
				assertModerationProperty(t, name+"."+propertyName, property, propertyExpectation)
			}
		})
	}
}

// TestLexiconModerationReasonTypes keeps public reasons extensible and pins the
// vocabulary to exactly the six public tokens: there is no separate CSAM
// classification, so CSAM removals are recorded as reasonIllegalContent.
func TestLexiconModerationReasonTypes(t *testing.T) {
	doc := readLexiconJSON(t, moderationDefsPath)
	reasonType := definitionMap(t, doc, "reasonType")
	assert.Equal(t, "string", reasonType["type"], "reasonType must be represented as a token-ref string")
	assert.Equal(t, float64(640), reasonType["maxLength"], "reasonType must allow fully-qualified token refs")
	assert.NotContains(t, reasonType, "format", "fragment-bearing token refs are not NSIDs")
	knownValues := asStrings(t, reasonType["knownValues"], "reasonType knownValues")
	assert.ElementsMatch(t, publicReasonRefs, knownValues, "reasonType must list exactly the six public reason tokens")
}

// TestLexiconModerationReasonTokens requires every reason reference to resolve
// to a documented token and rejects any token outside the public six.
func TestLexiconModerationReasonTokens(t *testing.T) {
	doc := readLexiconJSON(t, moderationDefsPath)
	tokens := []string{
		"reasonSpam",
		"reasonHarassment",
		"reasonDoxing",
		"reasonIllegalContent",
		"reasonRuleViolation",
		"reasonModeratorDiscretion",
	}
	for _, name := range tokens {
		t.Run(name, func(t *testing.T) {
			definition := definitionMap(t, doc, name)
			assert.Equal(t, "token", definition["type"], "%s must be a Lexicon token", name)
			description, ok := definition["description"].(string)
			assert.Truef(t, ok && strings.TrimSpace(description) != "", "%s must precisely describe its moderation meaning", name)
		})
	}
	defs, ok := doc["defs"].(map[string]interface{})
	require.True(t, ok, "moderation.defs must declare defs")
	_, hasCsam := defs["reasonCsam"]
	assert.False(t, hasCsam, "there is no separate CSAM classification; CSAM removals are recorded as reasonIllegalContent")
}

// TestLexiconModerationActionVocabulary prevents action producers and consumers
// from drifting onto different names for the six public moderation transitions.
func TestLexiconModerationActionVocabulary(t *testing.T) {
	doc := readLexiconJSON(t, moderationDefsPath)
	action := defProperties(t, doc, "actionView")["action"].(map[string]interface{})
	assert.ElementsMatch(t,
		[]string{"remove", "restore", "apply-removal", "retract-removal", "label", "retract-label"},
		asStrings(t, action["knownValues"], "actionView.action knownValues"),
		"actionView.action must expose exactly the transitions understood by moderation clients")
}

// TestLexiconModerationCompatibility guards the pre-existing ban view and the
// decision not to publish a publicationView in this package.
func TestLexiconModerationCompatibility(t *testing.T) {
	doc := readLexiconJSON(t, moderationDefsPath)
	banView := definitionMap(t, doc, "banView")
	assert.ElementsMatch(t, []string{"uri", "cid", "record", "indexedAt"},
		asStrings(t, banView["required"], "banView required"),
		"additive moderation definitions must not change banView's required wire shape")
	defs := doc["defs"].(map[string]interface{})
	assert.NotContains(t, defs, "publicationView", "publicationView is intentionally outside this lexicon package")
}

type lexiconDeclarationWalker struct {
	t         *testing.T
	documents map[string]map[string]interface{}
	visited   map[string]struct{}
	names     map[string]struct{}
}

func collectReachablePropertyNames(t *testing.T, startRef string) map[string]struct{} {
	t.Helper()
	walker := &lexiconDeclarationWalker{
		t:         t,
		documents: make(map[string]map[string]interface{}),
		visited:   make(map[string]struct{}),
		names:     make(map[string]struct{}),
	}
	walker.walkRef(startRef, "")
	return walker.names
}

func (walker *lexiconDeclarationWalker) loadDocument(schemaID string) map[string]interface{} {
	if document, ok := walker.documents[schemaID]; ok {
		return document
	}
	path := filepath.Join(lexiconDir, filepath.FromSlash(strings.ReplaceAll(schemaID, ".", "/")+".json"))
	document := readLexiconJSON(walker.t, path)
	assert.Equal(walker.t, schemaID, document["id"], "schema path and declared id must agree while walking refs")
	walker.documents[schemaID] = document
	return document
}

func (walker *lexiconDeclarationWalker) walkRef(ref, currentSchemaID string) {
	if strings.HasPrefix(ref, "#") {
		require.NotEmpty(walker.t, currentSchemaID, "a local ref cannot be resolved without its containing schema")
		ref = currentSchemaID + ref
	}
	if _, ok := walker.visited[ref]; ok {
		return
	}
	walker.visited[ref] = struct{}{}

	schemaID, definitionName, hasFragment := strings.Cut(ref, "#")
	if !hasFragment {
		definitionName = "main"
	}
	document := walker.loadDocument(schemaID)
	definition := definitionMap(walker.t, document, definitionName)
	walker.walkSchema(definition, schemaID)
}

func (walker *lexiconDeclarationWalker) walkSchema(schema map[string]interface{}, currentSchemaID string) {
	if properties, ok := schema["properties"].(map[string]interface{}); ok {
		for name, rawProperty := range properties {
			walker.names[name] = struct{}{}
			property, ok := rawProperty.(map[string]interface{})
			require.Truef(walker.t, ok, "%s.%s is not a property schema", currentSchemaID, name)
			walker.walkSchema(property, currentSchemaID)
		}
	}
	if ref, ok := schema["ref"].(string); ok {
		walker.walkRef(ref, currentSchemaID)
	}
	if refs, ok := schema["refs"].([]interface{}); ok {
		for _, rawRef := range refs {
			ref, ok := rawRef.(string)
			require.True(walker.t, ok, "union ref in %s must be a string", currentSchemaID)
			walker.walkRef(ref, currentSchemaID)
		}
	}
	if items, ok := schema["items"].(map[string]interface{}); ok {
		walker.walkSchema(items, currentSchemaID)
	}
	if record, ok := schema["record"].(map[string]interface{}); ok {
		walker.walkSchema(record, currentSchemaID)
	}
	for _, envelopeName := range []string{"input", "output"} {
		envelope, ok := schema[envelopeName].(map[string]interface{})
		if !ok {
			continue
		}
		if nestedSchema, ok := envelope["schema"].(map[string]interface{}); ok {
			walker.walkSchema(nestedSchema, currentSchemaID)
		}
	}
}

// TestLexiconModerationPublicDeclarationPrivacy walks every schema declaration
// reachable from public moderation views. This proves declaration hygiene only:
// Lexicon objects are open, so runtime projection code must still redact private
// fields from actual response data.
func TestLexiconModerationPublicDeclarationPrivacy(t *testing.T) {
	publicDefinitions := []string{"actionView", "sourceView", "moderationView", "contentLabelView"}
	privateNames := []string{"privateNote", "actorDid", "privateSubject", "private_classification"}
	for _, definitionName := range publicDefinitions {
		t.Run(definitionName, func(t *testing.T) {
			names := collectReachablePropertyNames(t, "social.coves.moderation.defs#"+definitionName)
			for _, privateName := range privateNames {
				assert.NotContainsf(t, names, privateName,
					"public %s reaches private declaration %s; this would publish admin-only moderation data",
					definitionName, privateName)
			}
		})
	}
}

type moderationEndpointExpectation struct {
	name       string
	schemaType string
	errors     []string
	// authenticatedServiceContext is true for endpoints whose authority and
	// actor identity come from the authenticated service context.
	authenticatedServiceContext bool
}

func moderationEndpointExpectations() []moderationEndpointExpectation {
	return []moderationEndpointExpectation{
		{
			name:                        "removeContent",
			authenticatedServiceContext: true,
			schemaType:                  "procedure",
			errors: []string{"AuthRequired", "Forbidden", "InvalidRequest", "InvalidSubject", "SubjectNotFound", "ContentChanged",
				"StateConflict", "IdempotencyConflict", "UnsupportedReason", "ModerationUnavailable"},
		},
		{
			name:                        "restoreContent",
			authenticatedServiceContext: true,
			schemaType:                  "procedure",
			errors: []string{"AuthRequired", "Forbidden", "InvalidRequest", "DecisionNotFound", "InvalidDecision", "ContentChanged",
				"StateConflict", "IdempotencyConflict", "UnsupportedReason", "ModerationUnavailable"},
		},
		{
			name:                        "labelContent",
			authenticatedServiceContext: true,
			schemaType:                  "procedure",
			errors: []string{"AuthRequired", "Forbidden", "InvalidRequest", "InvalidSubject", "SubjectNotFound", "ContentChanged",
				"StateConflict", "IdempotencyConflict", "UnsupportedReason", "UnsupportedLabel", "ModerationUnavailable"},
		},
		{
			name:                        "retractContentLabel",
			authenticatedServiceContext: true,
			schemaType:                  "procedure",
			errors: []string{"AuthRequired", "Forbidden", "InvalidRequest", "DecisionNotFound", "InvalidDecision", "ContentChanged",
				"StateConflict", "IdempotencyConflict", "UnsupportedReason", "ModerationUnavailable"},
		},
		{
			name:       "listActions",
			schemaType: "query",
			errors:     []string{"InvalidRequest", "InvalidCursor", "ModerationUnavailable"},
		},
		{
			name:                        "listAdminActions",
			authenticatedServiceContext: true,
			schemaType:                  "query",
			errors:                      []string{"AuthRequired", "Forbidden", "InvalidRequest", "InvalidCursor", "ModerationUnavailable"},
		},
		{
			name:                        "getSubjectState",
			authenticatedServiceContext: true,
			schemaType:                  "query",
			errors:                      []string{"AuthRequired", "Forbidden", "InvalidSubject", "ModerationUnavailable"},
		},
	}
}

func moderationEndpointDocument(t *testing.T, endpointName string) map[string]interface{} {
	t.Helper()
	return readLexiconJSON(t, filepath.Join(lexiconDir, "social", "coves", "moderation", endpointName+".json"))
}

func requireNonemptyDescription(t *testing.T, schema map[string]interface{}, what string) string {
	t.Helper()
	description, ok := schema["description"].(string)
	require.Truef(t, ok && strings.TrimSpace(description) != "", "%s must explain its moderation contract", what)
	return description
}

func requireSchemaEnvelope(t *testing.T, envelope map[string]interface{}, expectedRef, what string) {
	t.Helper()
	assert.Equal(t, "application/json", envelope["encoding"], "%s must use the shared JSON transport", what)
	schema, ok := envelope["schema"].(map[string]interface{})
	require.Truef(t, ok, "%s must declare a schema", what)
	assert.Equal(t, "ref", schema["type"], "%s must refer directly to the contract object", what)
	assert.Equal(t, expectedRef, schema["ref"], "%s must not introduce an alias or a different response shape", what)
}

func endpointErrorNames(t *testing.T, main map[string]interface{}, endpointName string) []string {
	t.Helper()
	rawErrors, ok := main["errors"].([]interface{})
	require.Truef(t, ok, "%s must declare its endpoint errors", endpointName)
	names := make([]string, 0, len(rawErrors))
	for _, rawError := range rawErrors {
		errorDefinition, ok := rawError.(map[string]interface{})
		require.Truef(t, ok, "%s has a malformed error declaration", endpointName)
		name, ok := errorDefinition["name"].(string)
		require.Truef(t, ok, "%s has an error without a name", endpointName)
		description := strings.ToLower(requireNonemptyDescription(t, errorDefinition, endpointName+" error "+name))
		guidance := map[string][]string{
			"ContentChanged":      {"refetch", "getsubjectstate", "re-inspect"},
			"StateConflict":       {"expectedversion", "stale", "refetch"},
			"IdempotencyConflict": {"same key", "different request", "do not retry"},
		}
		for _, phrase := range guidance[name] {
			assert.Containsf(t, description, phrase, "%s error %s must give callers the specified recovery guidance", endpointName, name)
		}
		names = append(names, name)
	}
	return names
}

// TestLexiconModerationEndpointEnvelopes pins the seven endpoint files, their
// direct input/output references, and their exact error surfaces so transport
// wrappers cannot accidentally publish a different moderation protocol.
func TestLexiconModerationEndpointEnvelopes(t *testing.T) {
	allowedErrors := map[string]struct{}{
		"AuthRequired": {}, "Forbidden": {}, "InvalidRequest": {}, "InvalidSubject": {}, "SubjectNotFound": {},
		"DecisionNotFound": {}, "InvalidDecision": {}, "ContentChanged": {}, "StateConflict": {},
		"IdempotencyConflict": {}, "UnsupportedReason": {}, "UnsupportedLabel": {}, "InvalidCursor": {},
		"ModerationUnavailable": {},
	}

	for _, expected := range moderationEndpointExpectations() {
		t.Run(expected.name, func(t *testing.T) {
			document := moderationEndpointDocument(t, expected.name)
			expectedID := "social.coves.moderation." + expected.name
			assert.Equal(t, float64(1), document["lexicon"], "%s must use Lexicon version 1", expected.name)
			assert.Equal(t, expectedID, document["id"], "%s filename and NSID must agree", expected.name)

			main := definitionMap(t, document, "main")
			assert.Equal(t, expected.schemaType, main["type"], "%s must use the specified endpoint kind", expected.name)
			description := strings.ToLower(requireNonemptyDescription(t, main, expected.name+" main definition"))
			if expected.authenticatedServiceContext {
				assert.Containsf(t, description, "authenticated service context",
					"%s must state that authority and actor identity come from the authenticated service context", expected.name)
			} else {
				assert.Containsf(t, description, "authentication is optional",
					"%s is the public query and must state that authentication is optional", expected.name)
				assert.NotContainsf(t, description, "authenticated service context",
					"%s takes no authority or actor input and must not copy the admin service-context sentence", expected.name)
			}

			output, ok := main["output"].(map[string]interface{})
			require.Truef(t, ok, "%s must declare an output envelope", expected.name)
			if expected.schemaType == "procedure" {
				input, ok := main["input"].(map[string]interface{})
				require.Truef(t, ok, "%s must declare an input envelope", expected.name)
				requireSchemaEnvelope(t, input, "#input", expected.name+" input")
				requireSchemaEnvelope(t, output, "social.coves.moderation.defs#mutationResult", expected.name+" output")
			} else {
				requireSchemaEnvelope(t, output, "#output", expected.name+" output")
				parameters, ok := main["parameters"].(map[string]interface{})
				require.Truef(t, ok, "%s must declare query parameters", expected.name)
				assert.Equal(t, "params", parameters["type"], "%s parameters must use the params schema kind", expected.name)
				properties, ok := parameters["properties"].(map[string]interface{})
				require.Truef(t, ok, "%s parameters must declare properties", expected.name)
				for propertyName, rawProperty := range properties {
					property, ok := rawProperty.(map[string]interface{})
					require.Truef(t, ok, "%s parameter %s is not a schema", expected.name, propertyName)
					propertyType, ok := property["type"].(string)
					require.Truef(t, ok, "%s parameter %s has no type", expected.name, propertyName)
					assert.Contains(t, []string{"string", "integer", "boolean", "array"}, propertyType,
						"query parameters must be primitives or arrays of primitives")
					if propertyType == "array" {
						items, ok := property["items"].(map[string]interface{})
						require.Truef(t, ok, "%s array parameter %s has no items schema", expected.name, propertyName)
						assert.Contains(t, []string{"string", "integer", "boolean"}, items["type"],
							"query parameter arrays must contain primitives")
					}
				}
			}

			errorNames := endpointErrorNames(t, main, expected.name)
			assert.ElementsMatch(t, expected.errors, errorNames, "%s must declare exactly its specified errors", expected.name)
			for _, name := range errorNames {
				assert.Containsf(t, allowedErrors, name, "%s declares %s outside the PRD error vocabulary", expected.name, name)
			}
		})
	}
}

func moderationProcedureInputExpectations() map[string]moderationDefinitionExpectation {
	strongRef := moderationPropertyExpectation{schemaType: "ref", reference: "com.atproto.repo.strongRef"}
	opaque := moderationPropertyExpectation{schemaType: "string", minLength: 1, maxLength: 128}
	reason := moderationPropertyExpectation{schemaType: "ref", reference: "social.coves.moderation.defs#reasonType"}
	privateNote := moderationPropertyExpectation{schemaType: "string", maxLength: 10000, maxGraphemes: 1000}
	return map[string]moderationDefinitionExpectation{
		"removeContent": {
			required: []string{"subject", "expectedVersion", "idempotencyKey", "reason"},
			properties: map[string]moderationPropertyExpectation{
				"subject": strongRef, "expectedVersion": opaque, "idempotencyKey": opaque, "reason": reason, "privateNote": privateNote,
			},
		},
		"restoreContent": {
			required: []string{"actionId", "expectedVersion", "idempotencyKey", "reason"},
			properties: map[string]moderationPropertyExpectation{
				"actionId": opaque, "expectedVersion": opaque, "idempotencyKey": opaque, "reason": reason,
				"reviewedSubject": strongRef, "privateNote": privateNote,
			},
		},
		"labelContent": {
			required: []string{"subject", "labelValue", "expectedVersion", "idempotencyKey"},
			properties: map[string]moderationPropertyExpectation{
				"subject":         strongRef,
				"labelValue":      {schemaType: "string", minLength: 1, maxLength: 128, knownValues: []string{"nsfw"}},
				"expectedVersion": opaque, "idempotencyKey": opaque, "reason": reason, "privateNote": privateNote,
			},
		},
		"retractContentLabel": {
			required: []string{"actionId", "expectedVersion", "idempotencyKey"},
			properties: map[string]moderationPropertyExpectation{
				"actionId": opaque, "expectedVersion": opaque, "idempotencyKey": opaque, "reason": reason,
				"reviewedSubject": strongRef, "privateNote": privateNote,
			},
		},
	}
}

// TestLexiconModerationProcedureInputs pins every required and optional write
// field, including optimistic-concurrency and idempotency tokens, so authority
// or actor identity cannot migrate into caller-controlled request data.
func TestLexiconModerationProcedureInputs(t *testing.T) {
	for endpointName, expected := range moderationProcedureInputExpectations() {
		t.Run(endpointName, func(t *testing.T) {
			document := moderationEndpointDocument(t, endpointName)
			input := definitionMap(t, document, "input")
			assert.Equal(t, "object", input["type"], "%s#input must be an object", endpointName)
			requireNonemptyDescription(t, input, endpointName+"#input")
			assert.ElementsMatch(t, expected.required, asStrings(t, input["required"], endpointName+"#input required"),
				"%s#input required fields must match the mutation contract", endpointName)
			properties := defProperties(t, document, "input")
			expectedNames := make([]string, 0, len(expected.properties))
			for propertyName := range expected.properties {
				expectedNames = append(expectedNames, propertyName)
			}
			assert.ElementsMatch(t, expectedNames, sortedPropertyNames(properties),
				"%s#input must expose exactly its specified request fields", endpointName)
			for propertyName, propertyExpectation := range expected.properties {
				property, ok := properties[propertyName].(map[string]interface{})
				require.Truef(t, ok, "%s#input.%s is missing or malformed", endpointName, propertyName)
				assertModerationProperty(t, endpointName+"#input."+propertyName, property, propertyExpectation)
			}
		})
	}
}

func listActionParameterExpectations(includeActionID bool) map[string]moderationPropertyExpectation {
	properties := map[string]moderationPropertyExpectation{
		"limit":      {schemaType: "integer", minimum: 1, maximum: 100, defaultValue: float64(50), hasDefault: true},
		"cursor":     {schemaType: "string", maxLength: 2048},
		"subject":    {schemaType: "string", format: "at-uri"},
		"collection": {schemaType: "string", format: "nsid"},
		"action": {schemaType: "string", maxLength: 64,
			knownValues: []string{"remove", "restore", "apply-removal", "retract-removal", "label", "retract-label"}},
		"origin":    {schemaType: "string", maxLength: 64, knownValues: []string{"local", "inherited"}},
		"authority": {schemaType: "string", format: "at-identifier"},
		"actor":     {schemaType: "string", format: "at-identifier"},
		"community": {schemaType: "string", minLength: 1, maxLength: 320},
		"since":     {schemaType: "string", format: "datetime"},
		"until":     {schemaType: "string", format: "datetime"},
	}
	if includeActionID {
		properties["actionId"] = moderationPropertyExpectation{schemaType: "string", minLength: 1, maxLength: 128}
	}
	return properties
}

// TestLexiconModerationQueryParameters pins public/admin filter parity and the
// required getSubjectState subject, preventing hidden filters or inconsistent
// pagination bounds from changing what callers can enumerate.
func TestLexiconModerationQueryParameters(t *testing.T) {
	queryProperties := map[string]map[string]moderationPropertyExpectation{
		"listActions":      listActionParameterExpectations(false),
		"listAdminActions": listActionParameterExpectations(true),
		"getSubjectState": {
			"subject": {schemaType: "string", format: "at-uri"},
		},
	}
	for endpointName, expectedProperties := range queryProperties {
		t.Run(endpointName, func(t *testing.T) {
			document := moderationEndpointDocument(t, endpointName)
			main := definitionMap(t, document, "main")
			parameters, ok := main["parameters"].(map[string]interface{})
			require.Truef(t, ok, "%s must declare parameters", endpointName)
			properties, ok := parameters["properties"].(map[string]interface{})
			require.Truef(t, ok, "%s parameters must declare properties", endpointName)
			expectedNames := make([]string, 0, len(expectedProperties))
			for propertyName := range expectedProperties {
				expectedNames = append(expectedNames, propertyName)
			}
			assert.ElementsMatch(t, expectedNames, sortedPropertyNames(properties),
				"%s parameters must expose exactly the specified filters", endpointName)
			for propertyName, propertyExpectation := range expectedProperties {
				property, ok := properties[propertyName].(map[string]interface{})
				require.Truef(t, ok, "%s parameter %s is missing or malformed", endpointName, propertyName)
				assertModerationProperty(t, endpointName+" parameter "+propertyName, property, propertyExpectation)
			}

			if endpointName == "getSubjectState" {
				assert.ElementsMatch(t, []string{"subject"}, asStrings(t, parameters["required"], "getSubjectState required parameters"),
					"getSubjectState must require the subject URI")
			} else {
				assert.NotContains(t, parameters, "required", "%s filters must all remain optional", endpointName)
				since := properties["since"].(map[string]interface{})
				until := properties["until"].(map[string]interface{})
				assert.Contains(t, strings.ToLower(requireNonemptyDescription(t, since, endpointName+" since")), "inclusive",
					"since must document its inclusive createdAt boundary")
				assert.Contains(t, strings.ToLower(requireNonemptyDescription(t, until, endpointName+" until")), "exclusive",
					"until must document its exclusive createdAt boundary")
			}
		})
	}
}

// TestLexiconModerationQueryOutputs pins the public/admin action projection
// split and subject-state response shape so private action fields cannot enter
// the public endpoint through an output alias change.
func TestLexiconModerationQueryOutputs(t *testing.T) {
	actionOutputs := map[string]string{
		"listActions":      "social.coves.moderation.defs#actionView",
		"listAdminActions": "social.coves.moderation.defs#adminActionView",
	}
	for endpointName, itemRef := range actionOutputs {
		t.Run(endpointName, func(t *testing.T) {
			document := moderationEndpointDocument(t, endpointName)
			output := definitionMap(t, document, "output")
			assert.Equal(t, "object", output["type"], "%s#output must be an object", endpointName)
			requireNonemptyDescription(t, output, endpointName+"#output")
			assert.ElementsMatch(t, []string{"actions"}, asStrings(t, output["required"], endpointName+"#output required"),
				"%s#output must require only actions", endpointName)
			properties := defProperties(t, document, "output")
			assert.ElementsMatch(t, []string{"actions", "cursor"}, sortedPropertyNames(properties),
				"%s#output must contain only actions and the optional cursor", endpointName)
			actions := properties["actions"].(map[string]interface{})
			cursor := properties["cursor"].(map[string]interface{})
			assertModerationProperty(t, endpointName+"#output.actions", actions,
				moderationPropertyExpectation{schemaType: "array", itemReference: itemRef, maxLength: 100})
			assertModerationProperty(t, endpointName+"#output.cursor", cursor,
				moderationPropertyExpectation{schemaType: "string", maxLength: 2048})
		})
	}

	t.Run("getSubjectState", func(t *testing.T) {
		document := moderationEndpointDocument(t, "getSubjectState")
		output := definitionMap(t, document, "output")
		assert.Equal(t, "object", output["type"], "getSubjectState#output must be an object")
		requireNonemptyDescription(t, output, "getSubjectState#output")
		assert.ElementsMatch(t, []string{"state"}, asStrings(t, output["required"], "getSubjectState#output required"),
			"getSubjectState#output must require exactly state")
		properties := defProperties(t, document, "output")
		assert.ElementsMatch(t, []string{"state"}, sortedPropertyNames(properties),
			"getSubjectState#output must not expose retained raw content")
		state := properties["state"].(map[string]interface{})
		assertModerationProperty(t, "getSubjectState#output.state", state,
			moderationPropertyExpectation{schemaType: "ref", reference: "social.coves.moderation.defs#subjectState"})
	})
}

// TestLexiconModerationEndpointFragments ensures every non-main endpoint object
// resolves through Indigo; files with main definitions are skipped by the
// definition-only resolver and would otherwise leave broken local refs hidden.
func TestLexiconModerationEndpointFragments(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory(lexiconDir), "the endpoint fragment contract requires the repository catalog")
	fragments := []string{
		"social.coves.moderation.removeContent#input",
		"social.coves.moderation.restoreContent#input",
		"social.coves.moderation.labelContent#input",
		"social.coves.moderation.retractContentLabel#input",
		"social.coves.moderation.listActions#output",
		"social.coves.moderation.listAdminActions#output",
		"social.coves.moderation.getSubjectState#output",
	}
	for _, ref := range fragments {
		t.Run(ref, func(t *testing.T) {
			_, err := catalog.Resolve(ref)
			require.NoErrorf(t, err, "endpoint fragment %s must resolve because generic main-schema coverage skips it", ref)
		})
	}
}

// TestLexiconModerationEndpointPrivacy proves the public action output cannot
// reach admin-only declarations and uses the admin output as a positive control
// that the recursive walker actually crosses endpoint-local and external refs.
func TestLexiconModerationEndpointPrivacy(t *testing.T) {
	privateNames := []string{"privateNote", "actorDid", "privateSubject", "private_classification"}
	publicNames := collectReachablePropertyNames(t, "social.coves.moderation.listActions#output")
	for _, privateName := range privateNames {
		assert.NotContainsf(t, publicNames, privateName,
			"public listActions reaches private declaration %s; authenticated callers must receive the same public shape", privateName)
	}

	adminNames := collectReachablePropertyNames(t, "social.coves.moderation.listAdminActions#output")
	assert.Contains(t, adminNames, "privateNote",
		"admin output must reach privateNote or the privacy walker is not following endpoint and defs refs")
}
