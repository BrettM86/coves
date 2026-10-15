package tests

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

func TestNotificationListLexicon_BridgedOnlyUpvoteOutputContract(t *testing.T) {
	catalog, recordID, _ := placeholderListLexicon(t)
	const root = `"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot"}`
	for _, tc := range []struct {
		name, subject, aggregate string
	}{
		{"live without recent voters", `"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot","preview":"Root"}`, `"upvoteCount":5`},
		{"live with empty recent voters", `"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot","preview":"Root"}`, `"upvoteCount":5,"recentUpvoters":[]`},
		{"deleted placeholder without recent voters", `"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot","status":"deleted"}`, `"upvoteCount":4`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := `{"notifications":[{"reason":"upvote","sortAt":"2026-09-20T09:00:00Z","isRead":false,` + root + `,` + tc.subject + `,` + tc.aggregate + `}]}`
			data, err := atdata.UnmarshalJSON([]byte(fixture))
			require.NoError(t, err)
			data["$type"] = recordID
			require.NoError(t, lexicon.ValidateRecord(catalog, data, recordID, 0))
		})
	}
}

// The basic profileView (reply author, recent upvoter) carries a bridged
// actor's origin with the same limits as profileViewDetailed.
func TestNotificationListLexicon_ProfileViewOriginFields(t *testing.T) {
	catalog, recordID, _ := placeholderListLexicon(t)
	const root = `"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot1","title":"Root"}`
	fixtures := map[string]func(author string) string{
		"reply author": func(author string) string {
			return `{"notifications":[{"reason":"postReply","sortAt":"2026-09-20T09:04:00Z","isRead":false,` + root + `,"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot1","preview":"Root"},"record":{"uri":"at://did:plc:actor/social.coves.community.comment/livepost","cid":"bafyreply1","createdAt":"2026-09-20T09:04:00Z","excerpt":"Reply"},"author":` + author + `}]}`
		},
		"recent upvoter": func(author string) string {
			return `{"notifications":[{"reason":"upvote","sortAt":"2026-09-20T09:00:00Z","isRead":false,` + root + `,"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot1","preview":"Root"},"upvoteCount":1,"recentUpvoters":[` + author + `]}]}`
		},
	}
	for _, tc := range []struct {
		name, field, value string
		valid              bool
	}{
		{"bridged values", "", "", true},
		{"origin at 253 bytes", "origin", strings.Repeat("a.", 126) + "a", true},
		{"originUsername at 256 bytes", "originUsername", strings.Repeat("u", 256), true},
		{"originSoftware at 64 bytes", "originSoftware", strings.Repeat("s", 64), true},
		{"origin at 254 bytes", "origin", strings.Repeat("a.", 126) + "ab", false},
		{"originUsername at 257 bytes", "originUsername", strings.Repeat("u", 257), false},
		{"originSoftware at 65 bytes", "originSoftware", strings.Repeat("s", 65), false},
		{"originUrl not a uri", "originUrl", "not a uri", false},
	} {
		for fixtureName, build := range fixtures {
			t.Run(tc.name+"/"+fixtureName, func(t *testing.T) {
				author := map[string]string{
					"did":            "did:plc:bridgedactor",
					"handle":         "alice-b.lemmy-world.bridge.test",
					"origin":         "lemmy.world",
					"originUsername": "Alice_B",
					"originUrl":      "https://lemmy.world/u/Alice_B",
					"originSoftware": "lemmy",
				}
				if tc.field != "" {
					author[tc.field] = tc.value
				}
				encoded, err := json.Marshal(author)
				require.NoError(t, err)
				data, err := atdata.UnmarshalJSON([]byte(build(string(encoded))))
				require.NoError(t, err)
				data["$type"] = recordID
				err = lexicon.ValidateRecord(catalog, data, recordID, 0)
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.Error(t, err, "%s must be rejected", tc.name)
				}
			})
		}
	}
}
