//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 034's shape, read out of the catalog rather than out of the .sql
// file.
//
// The distinction matters: a test that greps the migration proves the text was
// written, while these prove the database ended up in the state the text was
// supposed to produce — which is what every query in the AppView actually runs
// against. Three things here cannot be observed any other way and are silent,
// expensive failures if they are wrong:
//
//   - COLLATE "C" on last_community_rev. Under a locale-aware default collation
//     the rev comparison at the heart of §5.2 stops being bytewise, and the
//     ordering gate degrades into something that mostly works. Migration 033
//     pins the same thing for the same reason.
//   - The partial index predicate. Feed queries read accepted rows only; an
//     index built without the WHERE clause is still a valid index, so nothing
//     fails — it just stops being the one the planner wanted.
//   - posts.fk_author being GONE. It is the constraint that makes a federated
//     author's post unindexable (§5.3), and its removal is the point of the
//     migration.

const admissionsTable = "community_post_admissions"

// admissionStatuses is the exact status vocabulary of PRD §6.1. The CHECK
// constraint is asserted against this set rather than against a substring so
// that ADDING a status is as visible as removing one — a new state that no read
// path knows about is exactly the kind of drift a substring match sails past.
var admissionStatuses = []string{
	"accepted",
	"pending",
	"pending_reacceptance",
	"rejected",
	"removed",
}

func TestAdmissionsTable_Columns(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	requireTableExists(t, db, admissionsTable)

	type columnShape struct {
		dataType string
		nullable bool
	}
	want := map[string]columnShape{
		"community_did":          {"text", false},
		"post_uri":               {"text", false},
		"status":                 {"text", false},
		"acceptance_uri":         {"text", true},
		"acceptance_rkey":        {"text", true},
		"accepted_cid":           {"text", true},
		"decision_code":          {"text", true},
		"decision_at":            {"timestamp with time zone", true},
		"evaluated_cid":          {"text", true},
		"redrivable":             {"boolean", false},
		"last_community_rev":     {"text", true},
		"last_community_op_rank": {"smallint", true},
		"created_at":             {"timestamp with time zone", false},
		"updated_at":             {"timestamp with time zone", false},
	}

	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable, coalesce(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
	`, admissionsTable)
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
		if !assert.Truef(t, ok, "%s.%s is missing", admissionsTable, name) {
			continue
		}
		assert.Equalf(t, wantShape.dataType, gotShape.dataType, "%s.%s type", admissionsTable, name)
		assert.Equalf(t, wantShape.nullable, gotShape.nullable, "%s.%s nullability", admissionsTable, name)
	}

	// A transient evaluation failure must stay redrivable; only a policy
	// rejection opts out. Defaulting the other way would make every dead letter
	// terminal by omission.
	assert.Contains(t, defaults["redrivable"], "true", "redrivable must default to true")

	assert.Equal(t, []string{"community_did", "post_uri"}, primaryKeyColumns(t, db, admissionsTable),
		"the primary key is the subject: one decision per (community, post), so a post can carry independent decisions from several communities")
}

func TestAdmissionsTable_CheckConstraints(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	requireTableExists(t, db, admissionsTable)

	definitions := checkConstraintDefinitions(t, db, admissionsTable)
	require.NotEmpty(t, definitions, "%s must constrain its statuses and decisions", admissionsTable)

	t.Run("status is restricted to exactly the PRD vocabulary", func(t *testing.T) {
		var matched string
		for name, definition := range definitions {
			if !strings.Contains(definition, "status") {
				continue
			}
			if assert.ObjectsAreEqual(admissionStatuses, sortedLiterals(definition)) {
				matched = name
			}
		}
		assert.NotEmptyf(t, matched,
			"no CHECK constraint restricts status to exactly %v; constraints found: %v", admissionStatuses, definitions)
	})

	t.Run("a decided row must carry its code", func(t *testing.T) {
		// rejected and removed are the two states a reader RENDERS differently
		// (#removedPost carries the code; a rejection has to explain itself to
		// its author). A NULL code there is an unexplained decision, and the
		// only place that can be made impossible is the schema.
		var matched string
		for name, definition := range definitions {
			if strings.Contains(definition, "decision_code") &&
				strings.Contains(definition, "'rejected'") &&
				strings.Contains(definition, "'removed'") {
				matched = name
			}
		}
		assert.NotEmptyf(t, matched,
			"no CHECK constraint requires decision_code for rejected/removed; constraints found: %v", definitions)
	})
}

func TestAdmissionsTable_ConstraintsAreEnforced(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, admissionsTable)

	insert := func(t *testing.T, status string, decisionCode any) error {
		t.Helper()
		subject := newAdmissionSubject(t, db)
		_, err := db.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %s (community_did, post_uri, status, decision_code, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())
		`, admissionsTable), subject.CommunityDID, subject.PostURI, status, decisionCode)
		return err
	}

	for _, status := range admissionStatuses {
		decisionCode := any(nil)
		if status == "rejected" || status == "removed" {
			decisionCode = "rule_violation"
		}
		assert.NoErrorf(t, insert(t, status, decisionCode), "status %q must be accepted by the CHECK", status)
	}

	assert.Error(t, insert(t, "quarantined", nil),
		"a status outside the PRD vocabulary must be rejected: read paths switch on this column exhaustively")
	assert.Error(t, insert(t, "removed", nil),
		"a removal with no code renders as #removedPost with nothing to show the author")
	assert.Error(t, insert(t, "rejected", nil),
		"a rejection with no code is a decision the author can never be told the reason for")
}

func TestAdmissionsTable_RevCollationIsBytewise(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	requireTableExists(t, db, admissionsTable)

	var collation string
	err := db.QueryRowContext(context.Background(), `
		SELECT coalesce(collation_name, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name = 'last_community_rev'
	`, admissionsTable).Scan(&collation)
	require.NoError(t, err)

	assert.Equal(t, "C", collation,
		"last_community_rev must be COLLATE \"C\": the watermark compares revs as strings, and only bytewise order IS commit order for TIDs")
}

func TestAdmissionsTable_Indexes(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	requireTableExists(t, db, admissionsTable)

	definitions := indexDefinitions(t, db, admissionsTable)

	t.Run("accepted rows are indexed by subject for feed queries", func(t *testing.T) {
		var matched string
		for name, definition := range definitions {
			if indexColumns(definition) == "community_did, post_uri" &&
				normalizePredicate(indexPredicate(definition)) == "status='accepted'" {
				matched = name
			}
		}
		assert.NotEmptyf(t, matched,
			"no partial index on (community_did, post_uri) WHERE status = 'accepted'; the primary key covers the same columns for ALL statuses, so a feed query would scan pending and removed rows too. Indexes found: %v",
			definitions)
	})

	t.Run("a post's admissions are reachable without its community", func(t *testing.T) {
		var matched string
		for name, definition := range definitions {
			if indexColumns(definition) == "post_uri" {
				matched = name
			}
		}
		assert.NotEmptyf(t, matched,
			"no index on (post_uri); the primary key leads with community_did, so 'every community's decision about this post' — the author's own view — would be a sequential scan. Indexes found: %v",
			definitions)
	})

	t.Run("a community's queue is indexed by status and age", func(t *testing.T) {
		var matched string
		for name, definition := range definitions {
			if indexColumns(definition) == "community_did, status, created_at" {
				matched = name
			}
		}
		assert.NotEmptyf(t, matched,
			"no index on (community_did, status, created_at); that is the moderation queue's query. Indexes found: %v",
			definitions)
	})
}

func TestPostsTable_AuthorForeignKeyIsDropped(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'f' AND conname = 'fk_author'
	`, "posts").Scan(&count))
	assert.Zero(t, count, "posts.fk_author must be dropped: it is what makes a federated author's post unindexable")

	// Name-independent, because re-adding the same constraint under another name
	// would restore the defect while leaving the assertion above green.
	rows, err := db.QueryContext(ctx, `
		SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'f'
	`, "posts")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name, definition string
		require.NoError(t, rows.Scan(&name, &definition))
		if strings.Contains(definition, "author_did") {
			assert.Failf(t, "posts still has an author foreign key",
				"constraint %s: %s — an open federated author has no users row, and ON DELETE CASCADE would erase indexed posts when a profile row goes away",
				name, definition)
		}
	}
	require.NoError(t, rows.Err())
}

func TestMigration034_DownRestoresTheAuthorForeignKeyUnvalidated(t *testing.T) {
	t.Parallel()

	// A rollback that cannot run is not a rollback. By the time 034 is deployed
	// the posts table holds rows for authors with no users row — that is the
	// whole point of dropping the constraint — so a Down that re-adds a
	// validating FK aborts, and the only way back is to delete real content.
	// NOT VALID re-adds the constraint for future writes while leaving the rows
	// already there alone, which is what makes the migration reversible at all.
	db := testkit.DB(t)
	ctx := context.Background()

	communityName := testkit.UniqueIDWithPrefix(t, "down")
	communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
	require.NoError(t, err)

	orphanAuthorDID := fixtures.DID(testkit.UniqueID(t))
	rkey := testkit.TID()
	postURI := "at://" + orphanAuthorDID + "/social.coves.community.postv2/" + rkey

	_, err = db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, postURI, "bafyreiorphan", rkey, orphanAuthorDID, communityDID, "a post by an author this AppView has never indexed", time.Now())
	require.NoError(t, err,
		"with fk_author dropped, a federated author's post must index even though no users row exists for them")

	// The expected-version parameter is the tripwire, and it has now fired
	// four times: migration 035 (post_submissions), 036 (deleted_accounts),
	// 037 (the re-materialization ledger) and 038 (communities.origin) all sit
	// on top of 034, so all four have to come off first. Rolling back
	// explicitly, one asserted step at a time, is what keeps the assertions
	// below pointed at 034's Down rather than at whatever happens to be newest.
	require.EqualValues(t, 38, testkit.MigrateDownOne(t, db, 38),
		"038 (communities.origin) sits on top of 034 and must be rolled back first; asserting which migration came off is what stops this test drifting onto a newer one")
	require.EqualValues(t, 37, testkit.MigrateDownOne(t, db, 37),
		"037 (the re-materialization ledger) sits on top of 034 and must be rolled back next; asserting which migration came off is what stops this test drifting onto a newer one")
	require.EqualValues(t, 36, testkit.MigrateDownOne(t, db, 36),
		"036 sits on top of 034 and must be rolled back next; asserting which migration came off is what stops this test drifting onto a newer one")
	require.EqualValues(t, 35, testkit.MigrateDownOne(t, db, 35),
		"035 sits on top of 034 and must be rolled back next; asserting which migration came off is what stops this test drifting onto a newer one")
	assert.EqualValues(t, 34, testkit.MigrateDownOne(t, db, 34),
		"this test asserts on 034's Down section; rolling back a different migration would prove nothing about it")

	var surviving int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE uri = $1`, postURI).Scan(&surviving))
	assert.Equal(t, 1, surviving,
		"the rollback destroyed a federated author's post; the restored FK must be NOT VALID so existing rows are left alone")

	var validated bool
	err = db.QueryRowContext(ctx, `
		SELECT convalidated FROM pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'f' AND conname = 'fk_author'
	`, "posts").Scan(&validated)
	require.NoError(t, err, "the Down section must restore posts.fk_author")
	assert.False(t, validated,
		"fk_author must come back NOT VALID; a validating re-add would abort the rollback against any real dataset")

	// Put the schema back, so the clone is dropped in the state the rest of the
	// suite assumes and a failure here cannot be mistaken for a migration bug.
	testkit.MigrateUp(t, db)

	var restored int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint WHERE conrelid = $1::regclass AND conname = 'fk_author'
	`, "posts").Scan(&restored))
	assert.Zero(t, restored, "re-applying 034 must drop fk_author again")
}

// ---------------------------------------------------------------------------
// Catalog helpers
// ---------------------------------------------------------------------------

func requireTableExists(t *testing.T, db *sql.DB, table string) {
	t.Helper()

	var count int
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = $1
	`, table).Scan(&count))
	require.Equalf(t, 1, count, "table %s does not exist; the migration that creates it has not been written", table)
}

// primaryKeyColumns returns the table's primary key columns in key order.
func primaryKeyColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT a.attname
		FROM pg_constraint c
		JOIN unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conrelid = $1::regclass AND c.contype = 'p'
		ORDER BY k.ord
	`, table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var columns []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())
	return columns
}

// checkConstraintDefinitions maps constraint name to its rendered definition.
func checkConstraintDefinitions(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT conname, pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'c'
	`, table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	definitions := map[string]string{}
	for rows.Next() {
		var name, definition string
		require.NoError(t, rows.Scan(&name, &definition))
		definitions[name] = definition
	}
	require.NoError(t, rows.Err())
	return definitions
}

// indexDefinitions maps index name to its CREATE INDEX statement.
func indexDefinitions(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = $1
	`, table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	definitions := map[string]string{}
	for rows.Next() {
		var name, definition string
		require.NoError(t, rows.Scan(&name, &definition))
		definitions[name] = definition
	}
	require.NoError(t, rows.Err())
	return definitions
}

// literalPattern captures the single-quoted literals Postgres renders inside a
// constraint definition, whichever spelling the author used: an IN list and an
// = ANY (ARRAY[...]) both come back as quoted literals.
var literalPattern = regexp.MustCompile(`'([^']*)'`)

// sortedLiterals returns the distinct quoted literals of a definition, sorted,
// so a constraint can be compared as a SET rather than as text.
func sortedLiterals(definition string) []string {
	seen := map[string]bool{}
	var literals []string
	for _, match := range literalPattern.FindAllStringSubmatch(definition, -1) {
		if !seen[match[1]] {
			seen[match[1]] = true
			literals = append(literals, match[1])
		}
	}
	sort.Strings(literals)
	return literals
}

var btreeColumnPattern = regexp.MustCompile(`USING btree \(([^)]*)\)`)

// indexColumns returns an index's key columns as a normalised comma-separated
// list, with sort direction and NULLS placement removed: those change the
// planner's options, not whether the index covers the query the test is about.
func indexColumns(definition string) string {
	match := btreeColumnPattern.FindStringSubmatch(definition)
	if match == nil {
		return ""
	}
	var columns []string
	for _, column := range strings.Split(match[1], ",") {
		column = strings.TrimSpace(column)
		for _, modifier := range []string{" DESC", " ASC", " NULLS FIRST", " NULLS LAST"} {
			column = strings.ReplaceAll(column, modifier, "")
		}
		columns = append(columns, strings.TrimSpace(column))
	}
	return strings.Join(columns, ", ")
}

// indexPredicate returns a partial index's WHERE clause, or "" for a full index.
func indexPredicate(definition string) string {
	_, predicate, found := strings.Cut(definition, " WHERE ")
	if !found {
		return ""
	}
	return predicate
}

// normalizePredicate strips the decoration Postgres adds when it renders a
// predicate — outer parentheses, explicit casts, whitespace — so a predicate
// written as WHERE status = 'accepted' can be compared to what comes back.
func normalizePredicate(predicate string) string {
	predicate = strings.ReplaceAll(predicate, "::text", "")
	predicate = strings.ReplaceAll(predicate, "(", "")
	predicate = strings.ReplaceAll(predicate, ")", "")
	return strings.ReplaceAll(predicate, " ", "")
}

func TestAdmissionsTable_OpRankCheck(t *testing.T) {
	t.Parallel()

	// The op-rank vocabulary is exactly {0 = delete, 1 = put} (§5.2). The rank
	// is half of the ordering tuple, and SMALLINT admits 32766 values that
	// would silently outrank every genuine put forever — a CHECK is the only
	// place that can close the vocabulary where every writer meets it.
	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, admissionsTable)

	t.Run("the constraint exists in the catalog", func(t *testing.T) {
		var matched string
		for name, definition := range checkConstraintDefinitions(t, db, admissionsTable) {
			if strings.Contains(definition, "last_community_op_rank") &&
				strings.Contains(definition, "0") && strings.Contains(definition, "1") {
				matched = name
			}
		}
		assert.NotEmpty(t, matched,
			"no CHECK constraint restricts last_community_op_rank to (0, 1); a rank outside the vocabulary would outrank every genuine event")
	})

	t.Run("the constraint is enforced", func(t *testing.T) {
		insertWithRank := func(t *testing.T, rank int16) error {
			t.Helper()
			subject := newAdmissionSubject(t, db)
			_, err := db.ExecContext(ctx, fmt.Sprintf(`
				INSERT INTO %s (community_did, post_uri, status, last_community_rev, last_community_op_rank, created_at, updated_at)
				VALUES ($1, $2, 'pending', $3, $4, NOW(), NOW())
			`, admissionsTable), subject.CommunityDID, subject.PostURI, testkit.TID(), rank)
			return err
		}

		assert.NoError(t, insertWithRank(t, 0), "rank 0 (delete) is in the vocabulary")
		assert.NoError(t, insertWithRank(t, 1), "rank 1 (put) is in the vocabulary")
		assert.Error(t, insertWithRank(t, 2),
			"a rank outside {0, 1} would compare greater than every genuine put and freeze the subject forever")
		assert.Error(t, insertWithRank(t, -1),
			"a negative rank is equally outside the §5.2 vocabulary")
	})
}
