//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 035's shape, read out of the catalog rather than out of the .sql
// file — the same distinction admission_repo_schema_test.go draws for 034: a
// grep proves the text was written, these prove the database ended up in the
// state the text was supposed to produce.
//
// Three things here are load-bearing and silent when wrong:
//
//   - THE UNIQUE CONSTRAINT IS THE DEDUPE GATE. admitPost does not SELECT and
//     then INSERT; it INSERTs and reads the unique violation as the answer,
//     because the database is the only participant two racing double-taps both
//     talk to. Without the constraint, dedupe still "works" under every
//     sequential test and silently stops working under concurrency.
//   - NO FOREIGN KEYS. Migration 034 dropped posts.fk_author precisely because
//     a federated author has no users row (§5.3); a ledger that referenced one
//     would make the very submissions 034 exists to admit unrecordable, and the
//     refusal would arrive as a dead letter rather than as a decision.
//   - THE RATE-LIMIT INDEX. The quota is a COUNT over (author, community) in a
//     rolling window, run on the write path of every post. Migration 012 built
//     idx_aggregator_posts_rate_limit for the identical query shape; without
//     the equivalent here the count degrades to a scan of every submission the
//     instance has ever accepted.

const submissionsTable = "post_submissions"

func TestSubmissionsTable_Columns(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	requireTableExists(t, db, submissionsTable)

	type columnShape struct {
		dataType string
		nullable bool
	}
	want := map[string]columnShape{
		// The surrogate key exists so a reservation can be RELEASED by identity.
		// Releasing by the natural key would work too, right up until the row
		// being released is not the one this request inserted.
		"id":            {"bigint", false},
		"author_did":    {"text", false},
		"community_did": {"text", false},

		// The hash of the canonical record minus createdAt. Text rather than
		// bytea so it is greppable in an incident and comparable in psql; the
		// column is never interpreted, only equated.
		"fingerprint": {"text", false},

		// The window index the dedupe key is scoped to, derived from the
		// application's injected clock. It is an integer rather than a
		// timestamp deliberately: a timestamp here invites comparison against
		// created_at, and the two come from different clocks — one the app's,
		// one the database's.
		"dedupe_bucket": {"bigint", false},

		// Server-stamped, because it is what the rolling window is measured
		// against and a client-supplied time would be a client-supplied quota.
		"created_at": {"timestamp with time zone", false},
	}

	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable, coalesce(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
	`, submissionsTable)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	got := map[string]columnShape{}
	defaults := map[string]string{}
	for rows.Next() {
		var name, dataType, isNullable, columnDefault string
		require.NoError(t, rows.Scan(&name, &dataType, &isNullable, &columnDefault))
		got[name] = columnShape{dataType: dataType, nullable: isNullable == "YES"}
		defaults[name] = columnDefault
	}
	require.NoError(t, rows.Err())

	for name, wantShape := range want {
		gotShape, ok := got[name]
		if !assert.Truef(t, ok, "%s.%s is missing", submissionsTable, name) {
			continue
		}
		assert.Equalf(t, wantShape.dataType, gotShape.dataType, "%s.%s type", submissionsTable, name)
		assert.Equalf(t, wantShape.nullable, gotShape.nullable, "%s.%s nullability", submissionsTable, name)
	}

	assert.Containsf(t, strings.ToLower(defaults["created_at"]), "now()",
		"created_at must be stamped by the server: the rolling window is measured against it, so a caller that could set it could set its own quota")

	assert.Equal(t, []string{"id"}, primaryKeyColumns(t, db, submissionsTable),
		"the primary key is the surrogate id, so a reservation can be released by identity")
}

// The dedupe gate itself. Both halves are asserted — that the constraint exists
// over exactly the right columns, and that Postgres actually refuses the second
// insert — because a constraint over the wrong column set is present in the
// catalog and useless in practice.
func TestSubmissionsTable_DedupeKeyIsUniqueAndEnforced(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	requireTableExists(t, db, submissionsTable)

	wantKey := []string{"author_did", "community_did", "fingerprint", "dedupe_bucket"}

	t.Run("the constraint exists over the dedupe key", func(t *testing.T) {
		found := uniqueKeyColumnSets(t, db, submissionsTable)
		var matched bool
		for _, columns := range found {
			if assert.ObjectsAreEqual(wantKey, columns) {
				matched = true
			}
		}
		assert.Truef(t, matched,
			"no UNIQUE key over %v; the INSERT is the dedupe gate, so without it two concurrent identical submissions both succeed. Unique keys found: %v",
			wantKey, found)
	})

	t.Run("a repeat inside the same bucket is refused", func(t *testing.T) {
		author, community := newSubmissionSubject(t)

		require.NoError(t, insertSubmission(t, db, author, community, "fp-repeat", 100))
		assert.Error(t, insertSubmission(t, db, author, community, "fp-repeat", 100),
			"an identical submission in the same dedupe window must be refused by the database, not merely by a prior SELECT")
	})

	t.Run("the same content in a later bucket is a repost, not a duplicate", func(t *testing.T) {
		author, community := newSubmissionSubject(t)

		require.NoError(t, insertSubmission(t, db, author, community, "fp-later", 100))
		assert.NoError(t, insertSubmission(t, db, author, community, "fp-later", 101),
			"the bucket is what makes dedupe expire; without it an author could never repost the same content again")
	})

	t.Run("the key is scoped per author and per community", func(t *testing.T) {
		author, community := newSubmissionSubject(t)
		otherAuthor, otherCommunity := newSubmissionSubject(t)

		require.NoError(t, insertSubmission(t, db, author, community, "fp-scope", 100))
		assert.NoError(t, insertSubmission(t, db, otherAuthor, community, "fp-scope", 100),
			"two authors posting the same link are not duplicates of each other")
		assert.NoError(t, insertSubmission(t, db, author, otherCommunity, "fp-scope", 100),
			"cross-posting the same content to a second community is not a duplicate")
	})
}

func TestSubmissionsTable_HasNoForeignKeys(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, submissionsTable)

	rows, err := db.QueryContext(ctx, `
		SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'f'
	`, submissionsTable)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name, definition string
		require.NoError(t, rows.Scan(&name, &definition))
		assert.Failf(t, "the submission ledger has a foreign key",
			"constraint %s: %s — migration 034 dropped posts.fk_author because a federated author has no users row, "+
				"and a community may be one this AppView has not indexed; an FK here turns an ordinary submission into an insert failure",
			name, definition)
	}
	require.NoError(t, rows.Err())

	// And the behaviour that follows from it: a DID this instance has never
	// heard of can still be metered.
	unknownAuthor := fixtures.DID(testkit.UniqueID(t))
	unknownCommunity := fixtures.DID(testkit.UniqueID(t))
	assert.NoError(t, insertSubmission(t, db, unknownAuthor, unknownCommunity, "fp-federated", 100),
		"a submission from an author with no users row must record; that author is exactly who §5.3 exists for")
}

func TestSubmissionsTable_RateLimitIndex(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	requireTableExists(t, db, submissionsTable)

	definitions := indexDefinitions(t, db, submissionsTable)

	var matched string
	for name, definition := range definitions {
		if indexColumns(definition) == "author_did, community_did, created_at" {
			matched = name
		}
	}
	assert.NotEmptyf(t, matched,
		"no index on (author_did, community_did, created_at); that is the rolling-window quota query, run on the write path of every post — migration 012 built idx_aggregator_posts_rate_limit for the identical shape. Indexes found: %v",
		definitions)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newSubmissionSubject returns an author and a community that no other subtest
// shares, so subtests of one parallel test cannot collide on the dedupe key.
// Neither needs to exist anywhere else — that is the point of the no-FK rule.
func newSubmissionSubject(t *testing.T) (authorDID, communityDID string) {
	t.Helper()
	return fixtures.DID(testkit.UniqueID(t)), fixtures.DID(testkit.UniqueID(t))
}

func insertSubmission(t *testing.T, db *sql.DB, authorDID, communityDID, fingerprint string, bucket int64) error {
	t.Helper()

	_, err := db.ExecContext(context.Background(), fmt.Sprintf(`
		INSERT INTO %s (author_did, community_did, fingerprint, dedupe_bucket)
		VALUES ($1, $2, $3, $4)
	`, submissionsTable), authorDID, communityDID, fingerprint, bucket)
	return err
}

// uniqueKeyColumnSets returns the column sets covered by a UNIQUE constraint or
// a unique index, in key order.
//
// Both spellings are accepted because both enforce the same thing and Postgres
// reports them differently: a table constraint appears in pg_constraint, while
// a bare CREATE UNIQUE INDEX appears only in pg_index. Insisting on one would
// fail a migration that closed the race perfectly well the other way.
func uniqueKeyColumnSets(t *testing.T, db *sql.DB, table string) [][]string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT array_agg(a.attname ORDER BY k.ord)
		FROM pg_index i
		JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = $1::regclass AND i.indisunique
		GROUP BY i.indexrelid
	`, table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var sets [][]string
	for rows.Next() {
		var columns []string
		require.NoError(t, rows.Scan(pq.Array(&columns)))
		sets = append(sets, columns)
	}
	require.NoError(t, rows.Err())
	return sets
}
