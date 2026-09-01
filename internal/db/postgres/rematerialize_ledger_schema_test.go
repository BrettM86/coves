//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"

	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 037's ledger table, and the state vocabulary the whole cutover turns
// on (docs/PRD_AUTHOR_OWNED_POSTS.md §11 the rev-2.8 deploy runbook).
//
// The re-materialization tool moves every legacy social.coves.community.post to
// an author-owned postv2 plus a community acceptance, and then — and ONLY then —
// deletes the old record. Getting the order wrong makes live posts vanish or
// relaunders a removed post, so the tool is resumable and idempotent, and this
// LEDGER is what makes resume possible: one row per legacy record recording how
// far it got.
//
// The distinction this table has to make representable is migrated ≠ done.
// `migrated` means "verified safe to delete, old record STILL PRESENT"; `done`
// means "old record deleted". A crash between them resumes by retrying ONLY the
// delete, and a delete of an already-gone record is success — which is only
// coherent if the checkpoint BEFORE the delete is its own persisted state.
//
// The fallback state is the credential census (§11 step 3): a record whose
// author credentials cannot be restored is left as legacy, never re-authored
// under a forged signature, and the run refuses to report "complete" while any
// such row survives.
//
// There is exactly ONE fallback state, and the schema is where that is enforced.
// An earlier revision also admitted 'fallback_no_creds' and NO code path ever
// wrote it. A state the vocabulary permits but nothing produces is worse than
// missing: it is what an operator writes recovery SQL against at 2am, silently
// matching nothing, and it is a second name one WHERE clause eventually forgets.

const rematerializeLedgerTable = "post_rematerialization_ledger"

func TestRematerializeLedgerTable_Columns(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	requireTableExists(t, db, rematerializeLedgerTable)

	type columnShape struct {
		dataType string
		nullable bool
	}
	want := map[string]columnShape{
		// The OLD community.post URI is the whole key: one ledger row per legacy
		// record, so a re-run finds the existing row rather than accumulating a
		// second one for the same post.
		"old_uri": {"text", false},

		// The state machine's cursor. NOT NULL — a row with no state cannot be
		// resumed from.
		"state": {"text", false},

		// Who the postv2 is re-authored under (the legacy record's `author`
		// field). Nullable is acceptable — a fallback row may never resolve one —
		// but it is the audit trail for which repo the tool wrote into.
		"author_did": {"text", true},

		// The postv2 coordinates, populated at the postv2_written transition and
		// read back (never recomputed) on resume. NULL until then.
		"new_uri":  {"text", true},
		"new_cid":  {"text", true},
		"new_rkey": {"text", true},

		// The community repo the legacy record lives in. STORED rather than
		// parsed back out of old_uri, because the destructive half of the tool is
		// scoped by it: a staged run resumes, counts and DELETES only rows
		// carrying this value.
		"community_did": {"text", true},

		// The legacy record's CID as of the read the postv2 was built from. It is
		// a safety interlock, not audit trim: the delete is refused unless a fresh
		// read still reports it, and it is the swapRecord the delete is sent
		// under, so the PDS refuses a stale delete independently.
		"source_cid": {"text", true},

		// The human-readable note on a fallback row.
		"reason": {"text", true},

		"created_at": {"timestamp with time zone", false},
		"updated_at": {"timestamp with time zone", false},
	}

	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
	`, rematerializeLedgerTable)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	got := map[string]columnShape{}
	for rows.Next() {
		var name, dataType, isNullable string
		require.NoError(t, rows.Scan(&name, &dataType, &isNullable))
		got[name] = columnShape{dataType: dataType, nullable: isNullable == "YES"}
	}
	require.NoError(t, rows.Err())

	for name, wantShape := range want {
		gotShape, ok := got[name]
		if !assert.Truef(t, ok, "%s.%s is missing", rematerializeLedgerTable, name) {
			continue
		}
		assert.Equalf(t, wantShape.dataType, gotShape.dataType, "%s.%s type", rematerializeLedgerTable, name)
		assert.Equalf(t, wantShape.nullable, gotShape.nullable, "%s.%s nullability", rematerializeLedgerTable, name)
	}

	assert.Equal(t, []string{"old_uri"}, primaryKeyColumns(t, db, rematerializeLedgerTable),
		"the OLD community.post URI is the whole key: one row per legacy record, so a re-run updates in place rather than duplicating the row it resumes from")
}

func TestRematerializeLedgerTable_StateVocabularyIsClosed(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	requireTableExists(t, db, rematerializeLedgerTable)

	// The tool switches on state exhaustively, so the vocabulary is closed in the
	// schema rather than by convention: a typo'd state string would otherwise sit
	// in the table as an unresumable row nothing would ever advance.
	definitions := checkConstraintDefinitions(t, db, rematerializeLedgerTable)
	all := strings.Join(valuesOf(definitions), " ")

	for _, state := range []string{
		"discovered",
		"postv2_written",
		"verified",
		"migrated",
		"done",
		"fallback_left_legacy",
	} {
		assert.Containsf(t, all, state,
			"the state CHECK constraint must admit %q; a missing state is one the tool can never persist", state)
	}

	// The inverse, asserted rather than merely omitted: a state nothing produces
	// must not be in the vocabulary at all. Leaving it admitted is how recovery
	// SQL comes to be written against a state that cannot exist.
	assert.NotContainsf(t, all, "fallback_no_creds",
		"the CHECK constraint still admits 'fallback_no_creds', which no code path writes. One cause — the author-repo factory reporting no restorable "+
			"credentials — has exactly one state, fallback_left_legacy; a second admitted spelling is a trap for whoever writes the recovery UPDATE")
}

func TestRematerializeLedgerTable_ValidStatesInsertAndBogusIsRejected(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, rematerializeLedgerTable)

	// migrated and done are BOTH valid and DISTINCT — the checkpoint-before-delete
	// property depends on it. A bogus state must be refused at the schema, where
	// every writer meets the constraint, not left to repository discipline.
	valid := []string{"discovered", "postv2_written", "verified", "migrated", "done", "fallback_left_legacy"}
	for i, state := range valid {
		oldURI := "at://did:plc:community2222222222222222/social.coves.community.post/valid" + string(rune('a'+i))
		_, err := db.ExecContext(ctx, `
			INSERT INTO post_rematerialization_ledger (old_uri, state, created_at, updated_at)
			VALUES ($1, $2, NOW(), NOW())
		`, oldURI, state)
		require.NoErrorf(t, err, "state %q must be a permitted ledger state", state)
	}

	for _, bogus := range []string{"half_migrated", "fallback_no_creds"} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO post_rematerialization_ledger (old_uri, state, created_at, updated_at)
			VALUES ($1, $2, NOW(), NOW())
		`, "at://did:plc:community2222222222222222/social.coves.community.post/bogus-"+bogus, bogus)
		require.Errorf(t, err,
			"the state %q was accepted; the vocabulary must be closed by a CHECK, or a typo — or a state no code path writes — lands as a row nothing resumes", bogus)
	}
}

func TestRematerializeLedgerMigration_RollsBack(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	requireTableExists(t, db, rematerializeLedgerTable)

	// The expected-version tripwire: 042 (the dead-letter retention index), 041
	// (the future comment created_at repair), 040 (the vote-drift repair), 039
	// (the communities (name, origin) index), and 038 (communities.origin) sit on
	// top of 037 and come off first, one asserted step at a time. Asserting which
	// migration rolled back is what keeps this pointed at 037's Down rather than
	// drifting onto a newer one later.
	require.EqualValues(t, 43, testkit.MigrateDownOne(t, db, 43),
		"043 (the bridged-vote poll watermark) sits on top and must be rolled back first")
	require.EqualValues(t, 42, testkit.MigrateDownOne(t, db, 42),
		"042 (the dead-letter retention index) sits on top of 041 and must be rolled back first")
	require.EqualValues(t, 41, testkit.MigrateDownOne(t, db, 41),
		"041 (the future comment created_at repair) sits on top of 040 and must be rolled back next")
	require.EqualValues(t, 40, testkit.MigrateDownOne(t, db, 40),
		"040 (the vote-drift repair) sits on top of 039 and must be rolled back next")
	require.EqualValues(t, 39, testkit.MigrateDownOne(t, db, 39),
		"039 (the communities (name, origin) index) sits on top of 038 and must be rolled back next")
	require.EqualValues(t, 38, testkit.MigrateDownOne(t, db, 38),
		"038 (communities.origin) sits on top of 037 and must be rolled back next")
	assert.EqualValues(t, 37, testkit.MigrateDownOne(t, db, 37),
		"this test asserts on migration 037's Down section; rolling back a different migration would prove nothing about it")

	var count int
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = $1
	`, rematerializeLedgerTable).Scan(&count))
	assert.Equalf(t, 0, count, "migration 037's Down must drop %s", rematerializeLedgerTable)
}

// valuesOf returns a map's values, so the state-vocabulary check can scan every
// CHECK definition on the table at once.
func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
