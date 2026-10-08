package postgres

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// qualifyingUpvoteSQLPanic returns what qualifyingUpvoteSQL panicked with, or
// nil when it returned normally.
func qualifyingUpvoteSQLPanic(voteAlias, subjectExpr, recipientExpr string) (recovered any) {
	defer func() { recovered = recover() }()
	qualifyingUpvoteSQL(voteAlias, subjectExpr, recipientExpr)
	return nil
}

// An unqualified name inside the fragment binds to the innermost scope first,
// so each of these compiles to SQL that silently asks the wrong question.
func TestQualifyingUpvoteSQL_PanicsOnArgumentsThatBindInsideTheFragment(t *testing.T) {
	for _, test := range []struct {
		name          string
		voteAlias     string
		subjectExpr   string
		recipientExpr string
		badArgument   string
	}{
		// Compiles to v.subject_uri = v.subject_uri: every subject qualifies.
		{"unqualified_subject_column", "v", "subject_uri", "$1", "subject_uri"},
		// Resolves against user_blocks and votes before any outer row.
		{"unqualified_recipient_column", "v", "$1", "recipient_did", "recipient_did"},
		// The chunk-12 sweep shape: both columns left unqualified.
		{"unqualified_sweep_columns", "v", "subject_uri", "recipient_did", "subject_uri"},
		{"subject_qualified_with_vote_alias", "v", "v.subject_uri", "$1", "v.subject_uri"},
		{"recipient_qualified_with_vote_alias", "v", "$1", "v.voter_did", "v.voter_did"},
		{"subject_qualified_with_reserved_erasure_alias", "v", "qualifying_upvote_erasure.did", "$1", "qualifying_upvote_erasure.did"},
		{"recipient_qualified_with_reserved_aggregator_alias", "v", "$1", "qualifying_upvote_aggregator.did", "qualifying_upvote_aggregator.did"},
		{"recipient_qualified_with_reserved_block_alias", "v", "$1", "qualifying_upvote_block.blocker_did", "qualifying_upvote_block.blocker_did"},
		{"reserved_vote_alias", "qualifying_upvote_x", "$1", "$2", "qualifying_upvote_x"},
		{"reserved_block_vote_alias", "qualifying_upvote_block", "$1", "$2", "qualifying_upvote_block"},
		{"empty_vote_alias", "", "$1", "$2", ""},
		{"vote_alias_with_space", "v w", "$1", "$2", "v w"},
		{"vote_alias_with_punctuation", "v;", "$1", "$2", "v;"},
		{"uppercase_vote_alias", "V", "$1", "$2", "V"},
		{"string_literal_subject", "v", "'at://did:plc:subject/post/1'", "$2", "'at://did:plc:subject/post/1'"},
		{"zero_placeholder", "v", "$0", "$2", "$0"},
		{"empty_recipient", "v", "$1", "", ""},
		{"expression_subject", "v", "n.subject_uri OR TRUE", "$2", "n.subject_uri OR TRUE"},
		{"quoted_alias_subject", "v", `"n".subject_uri`, "$2", `"n".subject_uri`},
		{"schema_qualified_subject", "v", "public.notifications.subject_uri", "$2", "public.notifications.subject_uri"},
		{"uppercase_outer_alias", "v", "$1", "N.recipient_did", "N.recipient_did"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recovered := qualifyingUpvoteSQLPanic(test.voteAlias, test.subjectExpr, test.recipientExpr)
			require.NotNil(t, recovered, "qualifyingUpvoteSQL(%q, %q, %q) must panic",
				test.voteAlias, test.subjectExpr, test.recipientExpr)
			message, ok := recovered.(string)
			require.True(t, ok, "panic value must be a string message, got %T", recovered)
			require.Contains(t, message, fmt.Sprintf("%q", test.badArgument),
				"the panic message must name the bad argument")
		})
	}
}

func TestQualifyingUpvoteSQL_AcceptsPlaceholdersAndOuterQualifiedColumns(t *testing.T) {
	for _, test := range []struct {
		name          string
		voteAlias     string
		subjectExpr   string
		recipientExpr string
	}{
		{"repository_delete_if_empty", "qualifying_vote", "$2", "$1"},
		{"placeholders", "v", "$1", "$2"},
		{"multi_digit_placeholder", "v", "$12", "$3"},
		{"outer_qualified_columns", "qv", "g.subject_uri", "g.recipient_did"},
		{"sweep_qualified_columns", "v", "n.subject_uri", "n.recipient_did"},
		{"mixed_forms", "vote_1", "$1", "notification_row.recipient_did"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Nil(t, qualifyingUpvoteSQLPanic(test.voteAlias, test.subjectExpr, test.recipientExpr))
			fragment := qualifyingUpvoteSQL(test.voteAlias, test.subjectExpr, test.recipientExpr)
			require.Contains(t, fragment, test.voteAlias+".subject_uri = "+test.subjectExpr)
			require.Contains(t, fragment, test.voteAlias+".voter_did <> "+test.recipientExpr)
		})
	}
}
