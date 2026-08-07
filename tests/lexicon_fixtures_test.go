package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
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

// invalidFixtureExpectedErrors pins WHY each invalid fixture is rejected, keyed
// by basename. Without this, an invalid fixture that starts failing for an
// unrelated reason — a typo in a field name, a schema that stopped resolving —
// still "passes", and the case it was written for goes untested. Every
// "-invalid-" fixture MUST have an entry here; the walker fails on any that
// does not, so adding a fixture forces adding its expected error.
var invalidFixtureExpectedErrors = map[string]string{
	"acceptance-invalid-missing-subject.json": "subject",
	"comment-invalid-content.json":            "content",
	"post-invalid-missing-community.json":     "community",
	"postv2-invalid-community-not-a-did.json": "DID",
	"postv2-invalid-missing-community.json":   "community",
	"postv2-invalid-missing-createdat.json":   "createdAt",
	"profile-invalid-moderation-type.json":    "expected a string",
	"removal-invalid-missing-code.json":       "code",
	"removal-invalid-reason-too-long.json":    "graphemes",
	"rule-proposal-invalid-status.json":       "enum",
	"rule-proposal-invalid-threshold.json":    "outside specified range",
	"rule-proposal-invalid-type.json":         "enum",
	"rules-invalid-moderation.json":           "$type",
	"subscription-invalid-visibility.json":    "outside specified range",
	"tribunal-vote-invalid-decision.json":     "AT-URI",
	"vote-invalid-option.json":                "enum",
	"wiki-invalid-slug.json":                  "length outside specified range",
}

// expectedFixtureFamilies is the closed list of top-level fixture directories
// as of today. A family losing its directory (or all of its valid fixtures)
// fails the coverage guard instead of silently shrinking the suite; a new
// family must be added here to count.
var expectedFixtureFamilies = []string{
	"acceptance",
	"actor",
	"community",
	"feed",
	"interaction",
	"moderation",
	"post",
	"postv2",
	"removal",
}

// familiesRequiringInvalidFixtures lists the families that must also carry at
// least one rejection case: the records that gate community membership and
// moderation must prove the validator rejects their malformed forms.
var familiesRequiringInvalidFixtures = []string{"postv2", "acceptance", "removal"}

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
// fixture passes in one and fails in the other. On top of that, blob-shaped
// objects are converted to atdata.Blob (see convertBlobs), which
// cmd/validate-lexicon does not do yet: blob-bearing fixtures currently
// validate only here.
func decodeFixture(t *testing.T, path string) map[string]interface{} {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading fixture %s", path)

	var record map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&record), "parsing fixture %s", path)
	require.False(t, decoder.More(),
		"fixture %s has trailing data after the JSON record", path)

	record = narrowNumbers(record).(map[string]interface{})
	return convertBlobs(t, path, record).(map[string]interface{})
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

// convertBlobs walks a decoded record and replaces every blob-shaped object
// ({"$type": "blob", ...}) with an atdata.Blob value. indigo's SchemaBlob
// validator type-asserts on atdata.Blob, so a blob left as a plain map fails
// validation with "expected a blob" no matter how well-formed it is.
func convertBlobs(t *testing.T, path string, value interface{}) interface{} {
	t.Helper()

	switch typed := value.(type) {
	case map[string]interface{}:
		if typed["$type"] == "blob" {
			raw, err := json.Marshal(typed)
			require.NoError(t, err, "re-encoding blob in fixture %s", path)
			var blob atdata.Blob
			require.NoError(t, json.Unmarshal(raw, &blob), "parsing blob in fixture %s", path)
			return blob
		}
		converted := make(map[string]interface{}, len(typed))
		for key, member := range typed {
			converted[key] = convertBlobs(t, path, member)
		}
		return converted
	case []interface{}:
		converted := make([]interface{}, len(typed))
		for i, member := range typed {
			converted[i] = convertBlobs(t, path, member)
		}
		return converted
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
		if info.IsDir() || !strings.EqualFold(filepath.Ext(path), ".json") {
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
// must be rejected — for the reason pinned in invalidFixtureExpectedErrors —
// anything else means it must be accepted.
func TestLexiconFixtures(t *testing.T) {
	catalog := loadFixtureCatalog(t)
	fixturePaths := collectFixturePaths(t)

	for _, relPath := range fixturePaths {
		t.Run(filepath.ToSlash(relPath), func(t *testing.T) {
			record := decodeFixture(t, filepath.Join(fixtureDir, relPath))

			recordType, ok := record["$type"].(string)
			require.True(t, ok, "fixture %s has no top-level string $type", relPath)

			// An unresolvable $type must fail loudly here, not surface as a
			// generic validation error: an invalid fixture whose schema went
			// missing would otherwise "fail validation" for the wrong reason
			// and keep passing vacuously.
			_, resolveErr := catalog.Resolve(recordType)
			require.NoError(t, resolveErr,
				"fixture %s names a schema that does not resolve", relPath)

			// AllowLenientDatetime matches cmd/validate-lexicon's default mode.
			err := lexicon.ValidateRecord(catalog, record, recordType, lexicon.AllowLenientDatetime)

			basename := filepath.Base(relPath)
			if strings.Contains(basename, invalidFixtureMarker) {
				expectedError, ok := invalidFixtureExpectedErrors[basename]
				if !ok {
					t.Fatalf("invalid fixture %s has no entry in invalidFixtureExpectedErrors; add its expected error substring", relPath)
				}
				require.ErrorContains(t, err, expectedError,
					"expected %s to be rejected as %s for its declared reason", relPath, recordType)
				return
			}
			require.NoError(t, err, "expected %s to validate as %s", relPath, recordType)
		})
	}

	t.Run("family coverage", func(t *testing.T) {
		validCountByFamily := make(map[string]int)
		invalidCountByFamily := make(map[string]int)
		for _, relPath := range fixturePaths {
			family := strings.SplitN(filepath.ToSlash(relPath), "/", 2)[0]
			if strings.Contains(filepath.Base(relPath), invalidFixtureMarker) {
				invalidCountByFamily[family]++
			} else {
				validCountByFamily[family]++
			}
		}

		for _, family := range expectedFixtureFamilies {
			assert.NotZero(t, validCountByFamily[family],
				"fixture family %s has no valid fixture — deleted directory or all cases renamed?", family)
		}
		for _, family := range familiesRequiringInvalidFixtures {
			assert.NotZero(t, invalidCountByFamily[family],
				"fixture family %s has no invalid fixture — its rejection paths are untested", family)
		}
	})
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

// requireStringProperty asserts a record property exists and is a string
// schema, returning it for further shape assertions.
func requireStringProperty(t *testing.T, record lexicon.SchemaRecord, name string) lexicon.SchemaString {
	t.Helper()

	property, ok := record.Record.Properties[name]
	require.True(t, ok, "record has no %s property", name)
	propertyString, ok := property.Inner.(lexicon.SchemaString)
	require.True(t, ok, "%s is not a string (got %T)", name, property.Inner)
	return propertyString
}

// requireDatetimeCreatedAt asserts a record's createdAt is a datetime-format string.
func requireDatetimeCreatedAt(t *testing.T, record lexicon.SchemaRecord) {
	t.Helper()

	createdAt := requireStringProperty(t, record, "createdAt")
	require.NotNil(t, createdAt.Format, "createdAt must declare a format")
	assert.Equal(t, "datetime", *createdAt.Format, "createdAt format")
}

// requireStrongRefSubject asserts a record's subject is a ref to
// com.atproto.repo.strongRef, pinning the exact version it points at.
func requireStrongRefSubject(t *testing.T, record lexicon.SchemaRecord) {
	t.Helper()

	subject, ok := record.Record.Properties["subject"]
	require.True(t, ok, "record has no subject property")
	subjectRef, ok := subject.Inner.(lexicon.SchemaRef)
	require.True(t, ok, "subject is not a ref (got %T)", subject.Inner)
	assert.Equal(t, "com.atproto.repo.strongRef", subjectRef.Ref,
		"subject must pin the exact version of the record it references")
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

		community := requireStringProperty(t, record, "community")
		require.NotNil(t, community.Format, "postv2 community must declare a format")
		assert.Equal(t, "did", *community.Format,
			"postv2 community must be a DID, not a free-form name")

		requireDatetimeCreatedAt(t, record)
	})

	t.Run("social.coves.community.acceptance", func(t *testing.T) {
		record := resolveRecordSchema(t, catalog, "social.coves.community.acceptance")

		assert.Equal(t, "any", record.Key, "an acceptance is keyed by the subject it accepts")
		assert.ElementsMatch(t, []string{"subject", "createdAt"}, record.Record.Required,
			"acceptance required fields")

		requireStrongRefSubject(t, record)
		requireDatetimeCreatedAt(t, record)
	})

	t.Run("social.coves.community.removal", func(t *testing.T) {
		record := resolveRecordSchema(t, catalog, "social.coves.community.removal")

		assert.Equal(t, "any", record.Key, "a removal is keyed by the subject it removes")
		assert.ElementsMatch(t, []string{"subject", "code", "createdAt"}, record.Record.Required,
			"removal required fields")

		requireStrongRefSubject(t, record)
		requireDatetimeCreatedAt(t, record)

		code := requireStringProperty(t, record, "code")

		require.NotNil(t, code.MaxLength, "removal code must bound its length")
		assert.Equal(t, 64, *code.MaxLength, "removal code maxLength")

		// knownValues, never enum: federated peers must be able to send removal
		// codes this AppView has not heard of yet without their records being
		// rejected outright.
		assert.Empty(t, code.Enum,
			"removal code must stay an open knownValues set, not a closed enum")
		assert.ElementsMatch(t, []string{
			"rule-violation",
			"spam",
			"off-topic",
			"illegal-content",
			"author-banned",
			"moderator-discretion",
		}, code.KnownValues, "removal code knownValues must document the common codes")

		reason := requireStringProperty(t, record, "reason")
		require.NotNil(t, reason.MaxGraphemes, "removal reason must bound its grapheme length")
		assert.Equal(t, 1000, *reason.MaxGraphemes, "removal reason maxGraphemes")
	})
}
