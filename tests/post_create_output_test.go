package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The create endpoint's response, as the LEXICON declares it and as Go actually
// emits it.
//
// # WHY A SCHEMA THE SERVER IGNORES STILL MATTERS
//
// Nothing in this process validates an outgoing response against its lexicon, so
// a field the Go struct emits and the schema omits works perfectly — here. The
// lexicon is not for us. It is the contract third parties generate clients
// from: `status` is how a client tells a post that is live in its community from
// one still waiting for a decision (PRD §4.2 steps 4 and 5), and a generated
// client built from a schema that does not mention it has no field to put the
// value in. The AppView would be answering a question nobody downstream can hear
// itself asking.
//
// The undeclared direction is the worse one for a federated protocol. An
// undeclared field is not a small omission — it is the AppView emitting a shape
// no peer can validate, which is precisely what publishing lexicons is for.
//
// # THE ASSERTION IS BIDIRECTIONAL ON PURPOSE
//
// Checking only that the schema declares `status` would leave the drift that
// actually bites: a field added to the Go struct next year, invisible again.
// So this compares the two sets both ways — every JSON key the response type
// emits must be declared, and every declared property must exist on the type.
// That is a property of the pair, and it keeps holding for fields nobody has
// thought of yet.
func TestPostCreateOutput_LexiconAndGoAgree(t *testing.T) {
	t.Parallel()

	schema := loadCreateOutputSchema(t)

	properties, ok := schema["properties"].(map[string]any)
	require.Truef(t, ok, "the create lexicon's output declares no properties object: %#v", schema)

	// 1. status is declared at all.
	status, declared := properties["status"].(map[string]any)
	require.Truef(t, declared,
		"social.coves.community.post.create's output schema does not declare `status`, but the server "+
			"emits it (posts.CreatePostResponse.Status). A client generated from this schema has "+
			"nowhere to put the value, so it cannot tell an accepted post from a pending one — which "+
			"is the entire difference §4.2 asks a client to render")

	assert.Equal(t, "string", status["type"])

	// 2. And declared as the OPEN set it is. knownValues rather than an enum,
	// matching how DecisionCode is spelled elsewhere in these lexicons: the
	// vocabulary may grow, and a closed enum would make adding a status a
	// breaking schema change for every generated client in the network.
	known := stringsOf(t, status["knownValues"])
	assert.ElementsMatchf(t, []string{posts.PostStatusAccepted, posts.PostStatusPending}, known,
		"status must declare exactly the values the server can emit; got %v", known)

	// 3. status is OPTIONAL. A pre-flip client decoding a response into a struct
	// with a required-field check must keep working, and the Go type omits the
	// field when empty for the same reason.
	assert.NotContainsf(t, requiredOf(t, schema), "status",
		"status must not be required: the Go type omits it when empty, so a schema demanding it would "+
			"describe a response the server does not always send")

	// 4. The two directions of drift.
	assertResponseShapeMatches(t, properties, posts.CreatePostResponse{})
}

// assertResponseShapeMatches compares a lexicon's declared property names with
// the JSON keys a Go response type emits, in both directions.
func assertResponseShapeMatches(t *testing.T, properties map[string]any, response any) {
	t.Helper()

	declared := make([]string, 0, len(properties))
	for name := range properties {
		declared = append(declared, name)
	}

	emitted := jsonFieldNames(reflect.TypeOf(response))

	assert.ElementsMatchf(t, declared, emitted,
		"the create lexicon and posts.CreatePostResponse describe different responses.\n"+
			"  declared in the lexicon: %v\n"+
			"  emitted by the Go type:  %v\n"+
			"A key the server emits and the schema omits is a value no generated client can read; a "+
			"property the schema declares and the server never sends is a field every generated "+
			"client will show as empty.", declared, emitted)
}

// jsonFieldNames returns the wire names of a struct's exported fields, honouring
// the json tag and skipping fields explicitly excluded with "-".
func jsonFieldNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = field.Name
		}
		names = append(names, name)
	}
	return names
}

// loadCreateOutputSchema reads the create lexicon's main output schema.
func loadCreateOutputSchema(t *testing.T) map[string]any {
	t.Helper()

	path := filepath.Join(lexiconDir, "social", "coves", "community", "post", "create.json")
	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "reading %s", path)

	var doc struct {
		Defs struct {
			Main struct {
				Output struct {
					Schema map[string]any `json:"schema"`
				} `json:"output"`
			} `json:"main"`
		} `json:"defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotNilf(t, doc.Defs.Main.Output.Schema, "%s declares no main output schema", path)
	return doc.Defs.Main.Output.Schema
}

// stringsOf coerces a decoded JSON array into a string slice, failing loudly on
// anything else — a knownValues list of numbers is a schema bug, not an empty
// result.
func stringsOf(t *testing.T, value any) []string {
	t.Helper()

	if value == nil {
		return nil
	}
	items, ok := value.([]any)
	require.Truef(t, ok, "expected a JSON array, got %T", value)

	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		require.Truef(t, ok, "expected a string in the array, got %T", item)
		out = append(out, s)
	}
	return out
}

// requiredOf returns a schema's required-field list.
func requiredOf(t *testing.T, schema map[string]any) []string {
	t.Helper()
	return stringsOf(t, schema["required"])
}
