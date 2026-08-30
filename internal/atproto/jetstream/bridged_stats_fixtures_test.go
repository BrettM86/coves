package jetstream

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Record fixtures and the parser assertions for bridged vote stats. Building a
// record and parsing it needs nothing out of process, so this half of the
// bridged-stats coverage stays in the unit tier; the consumer behaviour those
// records drive lives in bridged_stats_test.go behind the integration tag.
//
// The shared identifiers live here rather than beside the database tests so a
// tagless build can still compile the fixtures.

const (
	bridgedTestPrefix    = "did:plc:brtest"
	bridgedTestCommunity = "did:plc:brtestcommunity"
	bridgedTestAuthor    = "did:plc:brtestauthor"
	bridgedTestVoter     = "did:plc:brtestvoter"
	bridgedTestCommenter = bridgedTestPrefix + "commenter"

	// bridgedTestPDS is the trusted bridge PDS host used across these tests. Test
	// users/communities are created with this pds_url and the consumers are constructed
	// trusting it, so the provenance gate lets their bridgedStats through. Tests that
	// exercise the default-deny path override the repo's pds_url instead.
	bridgedTestPDS       = "https://bridge.test"
	bridgedTestNativePDS = "https://native.pds.test"

	asOfEarly = "2026-01-01T00:00:00Z"
	asOfLate  = "2026-06-01T00:00:00Z"
)

func bridgedStatsRecord(up, down int, asOf string) map[string]interface{} {
	return map[string]interface{}{
		"upvotes":   up,
		"downvotes": down,
		"asOf":      asOf,
	}
}

func bridgedPostV2Record(title, content string, bridged map[string]interface{}) map[string]interface{} {
	rec := map[string]interface{}{
		"$type":     PostV2Collection,
		"community": bridgedTestCommunity,
		"title":     title,
		"content":   content,
		"createdAt": "2026-01-01T00:00:00Z",
	}
	if bridged != nil {
		rec["bridgedStats"] = bridged
	}
	return rec
}

func commentRecord(content, rootURI, rootCID, parentURI, parentCID string, bridged map[string]interface{}) map[string]interface{} {
	rec := map[string]interface{}{
		"$type":   "social.coves.community.comment",
		"content": content,
		"reply": map[string]interface{}{
			"root":   map[string]interface{}{"uri": rootURI, "cid": rootCID},
			"parent": map[string]interface{}{"uri": parentURI, "cid": parentCID},
		},
		"createdAt": "2026-01-02T00:00:00Z",
	}
	if bridged != nil {
		rec["bridgedStats"] = bridged
	}
	return rec
}

// TestParseRecord_BridgedStats verifies the record parsers tolerate presence/absence
// of bridgedStats without a database.
func TestParseRecord_BridgedStats(t *testing.T) {
	withStats, err := parseAuthorPostRecord(bridgedPostV2Record("t", "c", bridgedStatsRecord(7, 2, asOfEarly)))
	require.NoError(t, err)
	require.NotNil(t, withStats.BridgedStats)
	assert.Equal(t, 7, withStats.BridgedStats.Upvotes)
	assert.Equal(t, 2, withStats.BridgedStats.Downvotes)
	assert.Equal(t, asOfEarly, withStats.BridgedStats.AsOf)

	without, err := parseAuthorPostRecord(bridgedPostV2Record("t", "c", nil))
	require.NoError(t, err)
	assert.Nil(t, without.BridgedStats, "absent bridgedStats parses as nil")

	cWith, err := parseCommentRecord(commentRecord("hi", "at://x/c/1", "cid", "at://x/c/1", "cid", bridgedStatsRecord(3, 1, asOfLate)))
	require.NoError(t, err)
	require.NotNil(t, cWith.BridgedStats)
	assert.Equal(t, 3, cWith.BridgedStats.Upvotes)

	cWithout, err := parseCommentRecord(commentRecord("hi", "at://x/c/1", "cid", "at://x/c/1", "cid", nil))
	require.NoError(t, err)
	assert.Nil(t, cWithout.BridgedStats)
}
