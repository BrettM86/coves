package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpvoteGroupSQL_RejectsArgumentsThatBindInsideFragments(t *testing.T) {
	for _, bridgedTotals := range []bool{false, true} {
		gate := "off"
		if bridgedTotals {
			gate = "on"
		}
		for _, tc := range []struct{ name, expression string }{
			{"bare column", "subject_uri"},
			{"string literal", "'x'"},
			{"placeholder expression", "$1 OR true"},
			{"statement expression", "n.subject_uri; DROP"},
			{"qualifying upvote alias", "qualifying_upvote_block.blocker_did"},
			{"bridged post probe alias", "bridged_post.uri"},
			{"bridged comment probe alias", "bridged_comment.uri"},
		} {
			t.Run(gate+"/subject/"+tc.name, func(t *testing.T) {
				require.Panics(t, func() { upvoteGroupAliveSQL(tc.expression, "$1", bridgedTotals) })
			})
			t.Run(gate+"/recipient/"+tc.name, func(t *testing.T) {
				require.Panics(t, func() { upvoteGroupAliveSQL("$2", tc.expression, bridgedTotals) })
			})
			t.Run(gate+"/bridged-total/"+tc.name, func(t *testing.T) {
				require.Panics(t, func() { bridgedUpvoteTotalSQL(tc.expression, bridgedTotals) })
			})
		}
	}
}

func TestUpvoteGroupSQL_AcceptsPlaceholdersAndOuterQualifiedColumns(t *testing.T) {
	for _, bridgedTotals := range []bool{false, true} {
		gate := "off"
		if bridgedTotals {
			gate = "on"
		}
		for _, tc := range []struct{ name, subject, recipient string }{
			{"placeholders", "$2", "$1"},
			{"notification row", "n.subject_uri", "n.recipient_did"},
			{"sweep row", "s.subject_uri", "n.recipient_did"},
		} {
			t.Run(gate+"/"+tc.name, func(t *testing.T) {
				require.NotPanics(t, func() { upvoteGroupAliveSQL(tc.subject, tc.recipient, bridgedTotals) })
				require.NotPanics(t, func() { bridgedUpvoteTotalSQL(tc.subject, bridgedTotals) })
			})
		}
	}
}
