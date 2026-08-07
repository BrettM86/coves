package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureDir holds hand-written record samples, one JSON file per case, grouped
// into a subdirectory per record family. cmd/validate-lexicon walks the same
// tree; this test exists so the fixtures are also a merge gate at T0.
const fixtureDir = "lexicon-test-data"

// invalidFixtureMarker in a fixture's basename declares that the record MUST be
// rejected. Both hyphens are load-bearing: cmd/validate-lexicon matches the
// same substring, so renaming a fixture to "foo-invalid.json" silently flips it
// into a should-pass case in both harnesses.
const invalidFixtureMarker = "-invalid-"

// loadFixtureCatalog builds the catalog every fixture is validated against.
func loadFixtureCatalog(t *testing.T) *lexicon.BaseCatalog {
	t.Helper()

	catalog := lexicon.NewBaseCatalog()
	if err := catalog.LoadDirectory(lexiconDir); err != nil {
		t.Fatalf("Failed to load lexicon schemas from %s: %v", lexiconDir, err)
	}
	return &catalog
}

// decodeFixture parses a fixture the way cmd/validate-lexicon does: numbers are
// decoded with UseNumber and then narrowed to int64 where possible. Plain
// json.Unmarshal yields float64 for every number, which fails validation of
// integer-typed fields — a divergence between the two harnesses would mean a
// fixture passes in one and fails in the other.
func decodeFixture(t *testing.T, path string) map[string]interface{} {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading fixture %s", path)

	var record map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&record), "parsing fixture %s", path)

	return narrowNumbers(record).(map[string]interface{})
}

// narrowNumbers walks a decoded record and turns every json.Number into the
// concrete Go type indigo's validator expects.
func narrowNumbers(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		converted := make(map[string]interface{}, len(typed))
		for key, member := range typed {
			converted[key] = narrowNumbers(member)
		}
		return converted
	case []interface{}:
		converted := make([]interface{}, len(typed))
		for i, member := range typed {
			converted[i] = narrowNumbers(member)
		}
		return converted
	case json.Number:
		if asInt, err := typed.Int64(); err == nil {
			return asInt
		}
		if asFloat, err := typed.Float64(); err == nil {
			return asFloat
		}
		return typed.String()
	default:
		return value
	}
}

// collectFixturePaths returns every fixture JSON under fixtureDir, keyed by its
// path relative to fixtureDir so subtest names read as "postv2/postv2-valid-text.json".
func collectFixturePaths(t *testing.T) []string {
	t.Helper()

	var paths []string
	err := filepath.Walk(fixtureDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		relPath, err := filepath.Rel(fixtureDir, path)
		if err != nil {
			return err
		}
		paths = append(paths, relPath)
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to walk %s: %v", fixtureDir, err)
	}
	// A moved or renamed fixture directory must not pass vacuously.
	if len(paths) == 0 {
		t.Fatalf("No fixture files found under %s", fixtureDir)
	}
	return paths
}

// TestLexiconFixtures validates every record fixture against the published
// schemas. The basename decides the expectation: "-invalid-" means the record
// must be rejected, anything else means it must be accepted.
func TestLexiconFixtures(t *testing.T) {
	catalog := loadFixtureCatalog(t)

	for _, relPath := range collectFixturePaths(t) {
		t.Run(filepath.ToSlash(relPath), func(t *testing.T) {
			record := decodeFixture(t, filepath.Join(fixtureDir, relPath))

			recordType, ok := record["$type"].(string)
			require.True(t, ok, "fixture %s has no top-level string $type", relPath)

			// AllowLenientDatetime matches cmd/validate-lexicon's default mode.
			err := lexicon.ValidateRecord(catalog, record, recordType, lexicon.AllowLenientDatetime)

			if strings.Contains(filepath.Base(relPath), invalidFixtureMarker) {
				assert.Error(t, err, "expected validation failure but record validated as %s", recordType)
				return
			}
			assert.NoError(t, err, "expected %s to validate as %s", relPath, recordType)
		})
	}
}

// resolveRecordSchema resolves an NSID and asserts it is a record schema.
func resolveRecordSchema(t *testing.T, catalog *lexicon.BaseCatalog, nsid string) lexicon.SchemaRecord {
	t.Helper()

	schema, err := catalog.Resolve(nsid)
	require.NoError(t, err, "resolving %s", nsid)

	record, ok := schema.Def.(lexicon.SchemaRecord)
	require.True(t, ok, "%s is not a record schema (got %T)", nsid, schema.Def)
	return record
}

// TestLexiconRecordShapes pins schema shape rather than record content.
//
// atproto lexicons are OPEN: an unknown field on a record validates fine. That
// makes field ABSENCE unobservable from data fixtures — no fixture can prove
// that postv2 dropped "author", because a fixture carrying "author" would
// validate either way. The same goes for a knownValues set being tightened into
// a closed enum: every existing fixture keeps passing, and only records using a
// new value start failing, in production. These assertions read the schema
// directly so those regressions fail here instead.
func TestLexiconRecordShapes(t *testing.T) {
	catalog := loadFixtureCatalog(t)

	t.Run("social.coves.community.postv2", func(t *testing.T) {
		record := resolveRecordSchema(t, catalog, "social.coves.community.postv2")

		assert.Equal(t, "tid", record.Key, "postv2 records are keyed by TID")

		// postv2 exists to drop the author field: authorship comes from the
		// repo the record lives in, not from a self-asserted DID.
		assert.NotContains(t, record.Record.Properties, "author",
			"postv2 must not carry a self-asserted author DID")

		assert.ElementsMatch(t, []string{"community", "createdAt"}, record.Record.Required,
			"postv2 required fields")
	})

	t.Run("social.coves.community.acceptance", func(t *testing.T) {
		record := resolveRecordSchema(t, catalog, "social.coves.community.acceptance")

		assert.Equal(t, "any", record.Key, "an acceptance is keyed by the subject it accepts")
		assert.ElementsMatch(t, []string{"subject", "createdAt"}, record.Record.Required,
			"acceptance required fields")

		subject, ok := record.Record.Properties["subject"]
		require.True(t, ok, "acceptance has no subject property")
		subjectRef, ok := subject.Inner.(lexicon.SchemaRef)
		require.True(t, ok, "acceptance subject is not a ref (got %T)", subject.Inner)
		assert.Equal(t, "com.atproto.repo.strongRef", subjectRef.Ref,
			"acceptance must pin the exact version of what it accepts")
	})

	t.Run("social.coves.community.removal", func(t *testing.T) {
		record := resolveRecordSchema(t, catalog, "social.coves.community.removal")

		assert.Equal(t, "any", record.Key, "a removal is keyed by the subject it removes")
		assert.ElementsMatch(t, []string{"subject", "code", "createdAt"}, record.Record.Required,
			"removal required fields")

		code, ok := record.Record.Properties["code"]
		require.True(t, ok, "removal has no code property")
		codeString, ok := code.Inner.(lexicon.SchemaString)
		require.True(t, ok, "removal code is not a string (got %T)", code.Inner)

		require.NotNil(t, codeString.MaxLength, "removal code must bound its length")
		assert.Equal(t, 64, *codeString.MaxLength, "removal code maxLength")

		// knownValues, never enum: federated peers must be able to send removal
		// codes this AppView has not heard of yet without their records being
		// rejected outright.
		assert.Empty(t, codeString.Enum,
			"removal code must stay an open knownValues set, not a closed enum")
		assert.Contains(t, codeString.KnownValues, "spam",
			"removal code knownValues must document the common codes")
	})
}
