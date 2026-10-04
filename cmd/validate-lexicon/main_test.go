package main

import (
	"testing"

	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

// TestIsRecordSchema pins the coverage-summary rule: only record definitions
// count toward "record types with test data", so object fixtures routed
// through ValidateData cannot inflate the ratio past 100%.
func TestIsRecordSchema(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../internal/atproto/lexicon"),
		"the coverage rule cannot be tested without the repository lexicons")

	require.True(t, isRecordSchema(catalog, "social.coves.community.removal"),
		"a record definition must count toward record coverage")
	require.False(t, isRecordSchema(catalog, "social.coves.moderation.defs#banView"),
		"an object definition must not count toward record coverage")
	require.False(t, isRecordSchema(catalog, "social.coves.moderation.defs#missing"),
		"an unresolvable ref must not count toward record coverage")
}
