package tests

import (
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

// TestActorQueries_ActorParameterAcceptsScopedIdentifier checks that the three
// actor queries accept a bridged user's name@host address as `actor`, and that
// the DIDs and handles they accepted before still validate. The actor parameter
// is bounded at 2048 bytes, the limit getPosts and getComments enforce in code.
func TestActorQueries_ActorParameterAcceptsScopedIdentifier(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory(lexiconDir))

	queries := []string{
		"social.coves.actor.getProfile",
		"social.coves.actor.getPosts",
		"social.coves.actor.getComments",
	}
	const actorHost = "@lemmy.world"
	actors := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "name@host", value: "Alice_B@lemmy.world", valid: true},
		{name: "DID", value: "did:plc:abc123", valid: true},
		{name: "handle", value: "alice.example.com", valid: true},
		{name: "2048 bytes", value: strings.Repeat("a", 2048-len(actorHost)) + actorHost, valid: true},
		{name: "2049 bytes", value: strings.Repeat("a", 2049-len(actorHost)) + actorHost, valid: false},
	}

	for _, nsid := range queries {
		for _, actor := range actors {
			t.Run(nsid+"/"+actor.name, func(t *testing.T) {
				resolved, err := catalog.Resolve(nsid)
				require.NoError(t, err)
				query, ok := resolved.Def.(lexicon.SchemaQuery)
				require.True(t, ok, "%s must be a query, got %T", nsid, resolved.Def)
				require.NotNil(t, query.Parameters, "%s must declare parameters", nsid)
				property, ok := query.Parameters.Properties["actor"]
				require.True(t, ok, "%s must declare an actor parameter", nsid)
				actorSchema, ok := property.Inner.(lexicon.SchemaString)
				require.True(t, ok, "%s actor must be a string, got %T", nsid, property.Inner)

				err = actorSchema.Validate(actor.value, 0)
				if actor.valid {
					require.NoError(t, err, "%s actor must accept %q", nsid, actor.value)
				} else {
					require.Error(t, err, "%s actor must reject a %d-byte value", nsid, len(actor.value))
				}
			})
		}
	}
}
