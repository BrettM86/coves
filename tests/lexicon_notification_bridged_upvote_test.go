package tests

import (
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
