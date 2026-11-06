package tests

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLexiconCommunityBanSurfaces pins the public surfaces a community ban
// reaches: the write endpoints a banned account is refused by, and the
// viewer state that tells the account it is banned.
func TestLexiconCommunityBanSurfaces(t *testing.T) {
	t.Run("write endpoints declare Banned", func(t *testing.T) {
		for _, endpoint := range []struct {
			name string
			path []string
		}{
			{name: "social.coves.community.comment.create", path: []string{"community", "comment", "create.json"}},
			{name: "social.coves.feed.vote.create", path: []string{"feed", "vote", "create.json"}},
		} {
			t.Run(endpoint.name, func(t *testing.T) {
				doc := readLexiconJSON(t, filepath.Join(append([]string{lexiconDir, "social", "coves"}, endpoint.path...)...))
				main := definitionMap(t, doc, "main")
				assert.Contains(t, endpointErrorNames(t, main, endpoint.name), "Banned")
			})
		}
	})

	t.Run("social.coves.community.defs#viewerState", func(t *testing.T) {
		doc := readLexiconJSON(t, filepath.Join(lexiconDir, "social", "coves", "community", "defs.json"))
		viewerState := definitionMap(t, doc, "viewerState")
		properties, ok := viewerState["properties"].(map[string]interface{})
		require.True(t, ok, "viewerState has no properties object")

		banReason, ok := properties["banReason"].(map[string]interface{})
		require.True(t, ok, "viewerState must declare banReason")
		assert.Equal(t, "string", banReason["type"], "banReason type")

		banExpiresAt, ok := properties["banExpiresAt"].(map[string]interface{})
		require.True(t, ok, "viewerState must declare banExpiresAt")
		assert.Equal(t, "string", banExpiresAt["type"], "banExpiresAt type")
		assert.Equal(t, "datetime", banExpiresAt["format"], "banExpiresAt format")

		required, _ := viewerState["required"].([]interface{})
		assert.NotContains(t, required, "banReason", "banReason is optional")
		assert.NotContains(t, required, "banExpiresAt", "banExpiresAt is optional")
	})
}
