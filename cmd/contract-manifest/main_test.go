package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"Coves/internal/atproto/jetstream"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rules live in evaluate, which touches no filesystem, so the state matrix
// below is a table rather than a fixture tree. Only the two IO paths — parsing
// markers out of Go source and reading the burn-down — need files.

func consumedBy(pairs map[string]string) map[string][]string {
	consumed := make(map[string][]string, len(pairs))
	for collection, consumer := range pairs {
		consumed[collection] = []string{consumer}
	}
	return consumed
}

func markersFor(collection string) map[string][]marker {
	return map[string][]marker{
		collection: {{Collection: collection, File: "tests/e2e/x_contract_test.go", Line: 12}},
	}
}

func TestEvaluate_ContractedCollectionPasses(t *testing.T) {
	res := evaluate(
		consumedBy(map[string]string{"social.coves.community.post": "posts"}),
		markersFor("social.coves.community.post"),
		nil,
		true,
	)
	assert.Empty(t, res.Violations)
	require.Len(t, res.Contracted, 1)
	assert.Equal(t, []string{"posts"}, res.Contracted[0].Consumers)
}

func TestEvaluate_PendingCollectionPasses(t *testing.T) {
	res := evaluate(
		consumedBy(map[string]string{"social.coves.feed.vote": "votes"}),
		nil,
		[]pendingEntry{{Collection: "social.coves.feed.vote", Task: 14, Line: 22}},
		true,
	)
	assert.Empty(t, res.Violations)
	require.Len(t, res.Pending, 1)
	assert.Equal(t, 14, res.Pending[0].Entry.Task)
}

func TestEvaluate_PendingThatGainedAContractIsStale(t *testing.T) {
	// The ratchet. Without it the burn-down would keep a collection's entry
	// forever, and deleting the contract again would go unnoticed.
	res := evaluate(
		consumedBy(map[string]string{"social.coves.feed.vote": "votes"}),
		markersFor("social.coves.feed.vote"),
		[]pendingEntry{{Collection: "social.coves.feed.vote", Task: 14, Line: 22}},
		true,
	)
	require.Len(t, res.Violations, 1)
	assert.Contains(t, res.Violations[0], "STALE pending entry")
	assert.Contains(t, res.Violations[0], "social.coves.feed.vote")
	assert.Contains(t, res.Violations[0], "tests/e2e/x_contract_test.go:12",
		"the message must name the contract that made the entry stale")
	assert.Len(t, res.Contracted, 1, "the collection is still contracted; only the entry is wrong")
}

func TestEvaluate_UnconsumedMarkerFails(t *testing.T) {
	res := evaluate(
		consumedBy(map[string]string{"social.coves.community.post": "posts"}),
		markersFor("social.coves.retired.thing"),
		[]pendingEntry{{Collection: "social.coves.community.post", Task: 12, Line: 3}},
		true,
	)
	require.Len(t, res.Violations, 1)
	assert.Contains(t, res.Violations[0], "UNCONSUMED marker")
	assert.Contains(t, res.Violations[0], "social.coves.retired.thing")
}

func TestEvaluate_UnconsumedPendingEntryFails(t *testing.T) {
	res := evaluate(
		consumedBy(map[string]string{"social.coves.community.post": "posts"}),
		markersFor("social.coves.community.post"),
		[]pendingEntry{{Collection: "social.coves.retired.thing", Task: 15, Line: 7}},
		true,
	)
	require.Len(t, res.Violations, 1)
	assert.Contains(t, res.Violations[0], "UNCONSUMED pending entry")
	assert.Contains(t, res.Violations[0], "social.coves.retired.thing")
}

func TestEvaluate_MissingContractFails(t *testing.T) {
	res := evaluate(
		consumedBy(map[string]string{"social.coves.actor.block": "users"}),
		nil,
		nil,
		true,
	)
	require.Len(t, res.Violations, 1)
	assert.Contains(t, res.Violations[0], "MISSING contract")
	assert.Contains(t, res.Violations[0], "social.coves.actor.block")
	assert.Contains(t, res.Violations[0], "users", "the message must name the consumer that ingests it")
	assert.Equal(t, []string{"social.coves.actor.block"}, res.Missing)
}

func TestEvaluate_PendingFailsWhenDisallowed(t *testing.T) {
	// Task 20's flip: with the burn-down closed, pending stops being a passing
	// state and only a contract will do.
	res := evaluate(
		consumedBy(map[string]string{"social.coves.feed.vote": "votes"}),
		nil,
		[]pendingEntry{{Collection: "social.coves.feed.vote", Task: 14, Line: 22}},
		false,
	)
	require.Len(t, res.Violations, 1)
	assert.Contains(t, res.Violations[0], "still pending")
}

func TestEvaluate_TwoMarkersForOneCollectionFail(t *testing.T) {
	// Two tests each believing they are THE proof is a quiet coverage loss:
	// delete one and the check stays green.
	res := evaluate(
		consumedBy(map[string]string{"social.coves.community.post": "posts"}),
		map[string][]marker{"social.coves.community.post": {
			{Collection: "social.coves.community.post", File: "tests/e2e/a_test.go", Line: 10},
			{Collection: "social.coves.community.post", File: "tests/e2e/b_test.go", Line: 20},
		}},
		nil,
		true,
	)
	require.Len(t, res.Violations, 1)
	assert.Contains(t, res.Violations[0], "DUPLICATE contract")
	assert.Contains(t, res.Violations[0], "tests/e2e/a_test.go:10")
	assert.Contains(t, res.Violations[0], "tests/e2e/b_test.go:20",
		"the message must name both files, or the reader has to go looking")
}

func TestEvaluate_MultipleConsumersAreAllReported(t *testing.T) {
	res := evaluate(
		map[string][]string{"social.coves.actor.profile": {"aggregators", "users"}},
		nil,
		nil,
		true,
	)
	require.Len(t, res.Violations, 1)
	assert.Contains(t, res.Violations[0], "aggregators, users")
}

// ---------------------------------------------------------------------------
// Marker scanning
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// markerLine builds a marker without writing one literally in this file: a
// literal would be picked up by any future check that scans the repository, and
// the token is assembled the same way the tool matches it.
func markerLine(collection string) string { return "//" + markerToken + " " + collection }

func TestScanMarkers_FindsMarkersInE2ETaggedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "post_contract_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		markerLine("social.coves.community.post")+"\nfunc TestPostIngestion() {}\n")

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	require.Len(t, markers["social.coves.community.post"], 1)
	assert.Equal(t, 5, markers["social.coves.community.post"][0].Line)
}

func TestScanMarkers_IgnoresProseAndStringLiterals(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "harness_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		"// Contracts declare themselves with a "+markerToken+" comment.\n"+
		"var doc = \""+markerToken+" social.coves.not.real\"\n")

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	assert.Empty(t, markers,
		"only a comment line that BEGINS with the token is a marker; prose and string literals are not")
}

func TestScanMarkers_MalformedMarkerIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		"//"+markerToken+"\n")

	_, err := scanMarkers(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed contract marker")
}

func TestScanMarkers_TwoCollectionsOnOneMarkerIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		markerLine("social.coves.a social.coves.b")+"\n")

	_, err := scanMarkers(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed contract marker")
}

func TestScanMarkers_MarkerOutsideTheE2ETierIsAnError(t *testing.T) {
	// A contract that does not build under -tags e2e never runs, so a marker in
	// such a file would satisfy the manifest while proving nothing.
	dir := t.TempDir()
	writeFile(t, dir, "integration_test.go", "//go:build integration\n\npackage e2e\n\n"+
		markerLine("social.coves.community.post")+"\n")

	_, err := scanMarkers(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does NOT include this file in a -tags e2e build")
}

func TestScanMarkers_UntaggedTestFileIsPartOfTheE2EBuild(t *testing.T) {
	// Build tags are ADDITIVE: `go test -tags e2e ./tests/e2e/...` compiles the
	// untagged files in that directory too, so a marker in one does run in the
	// tier. This is decided by go/build's own MatchFile rather than by reading
	// the //go:build line, which is why the answer matches the toolchain's.
	dir := t.TempDir()
	writeFile(t, dir, "helpers_test.go", "package e2e\n\n"+
		markerLine("social.coves.community.post")+"\n")

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	assert.Len(t, markers["social.coves.community.post"], 1)
}

func TestScanMarkers_AcceptsACompoundConstraintThatIncludesE2E(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "contract_test.go", "//go:build e2e && !windows\n\npackage e2e\n\n"+
		markerLine("social.coves.feed.vote")+"\n")

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	assert.Len(t, markers["social.coves.feed.vote"], 1)
}

func TestScanMarkers_AcceptsAConstraintCombiningE2EWithThisPlatform(t *testing.T) {
	// The case a hand-rolled "//go:build must mention e2e" reader got wrong:
	// `e2e && linux` evaluates false when only e2e is defined, so the marker was
	// rejected as being outside the tier on the very runner that would run it.
	// MatchFile evaluates GOOS as the toolchain does.
	dir := t.TempDir()
	writeFile(t, dir, "contract_test.go", "//go:build e2e && "+runtime.GOOS+"\n\npackage e2e\n\n"+
		markerLine("social.coves.feed.vote")+"\n")

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	assert.Len(t, markers["social.coves.feed.vote"], 1)
}

// --- The three verified bypasses -------------------------------------------

func TestScanMarkers_IgnoresNonTestFiles(t *testing.T) {
	// BYPASS: a marker in a plain .go file satisfied the manifest, but the go
	// tool never turns such a file into a test — nothing ran.
	dir := t.TempDir()
	writeFile(t, dir, "helpers.go", "//go:build e2e\n\npackage e2e\n\n"+
		markerLine("social.coves.community.post")+"\n")

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	assert.Empty(t, markers, "only *_test.go files can hold a contract")
}

func TestScanMarkers_IgnoresToolInvisibleDirectories(t *testing.T) {
	// BYPASS: fixtures under testdata/ (and _foo/, .foo/) are invisible to the
	// go tool, so a marker there declared a contract that could not compile,
	// let alone run.
	dir := t.TempDir()
	for _, invisible := range []string{"testdata", "_scratch", ".cache"} {
		nested := filepath.Join(dir, invisible)
		require.NoError(t, os.Mkdir(nested, 0o755))
		writeFile(t, nested, "contract_test.go", "//go:build e2e\n\npackage e2e\n\n"+
			markerLine("social.coves.community.post")+"\n")
	}

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	assert.Empty(t, markers, "directories the go tool ignores cannot hold a contract")
}

func TestScanMarkers_RejectsAMarkerExcludedByItsFilenameSuffix(t *testing.T) {
	// BYPASS: constraints also come from the FILENAME. contract_plan9_test.go
	// builds on exactly one platform, and this is not it — the old reader saw
	// "//go:build e2e", accepted the marker, and the contract never ran.
	dir := t.TempDir()
	writeFile(t, dir, "contract_plan9_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		markerLine("social.coves.community.post")+"\n")

	_, err := scanMarkers(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GOOS/GOARCH suffix")
}

// --- Discipline inside a marker-carrying file -------------------------------

func TestScanMarkers_ContractMayNotDialWebsockets(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "contract_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		"import \"github.com/gorilla/websocket\"\n\n"+
		markerLine("social.coves.community.post")+"\n")

	_, err := scanMarkers(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gorilla/websocket")
}

func TestScanMarkers_ContractMayNotImportTheConsumerPackage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "contract_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		"import \"Coves/internal/atproto/jetstream\"\n\n"+
		markerLine("social.coves.community.post")+"\n")

	_, err := scanMarkers(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal/atproto/jetstream")
}

func TestScanMarkers_ContractMayNotCloneTheTestDatabase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "contract_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		markerLine("social.coves.community.post")+"\n"+
		"func TestX(t *testing.T) { db := testkit.DB(t); _ = db }\n")

	_, err := scanMarkers(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not call testkit.DB")
}

func TestScanMarkers_DisciplineAppliesOnlyToMarkerCarryingFiles(t *testing.T) {
	// The two legacy files in tests/e2e do instantiate consumers and do clone
	// the test database. They carry no marker, so they stay buildable until task
	// 16 rebuilds them — the rules bind what claims to be a contract.
	dir := t.TempDir()
	writeFile(t, dir, "legacy_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		"import \"Coves/internal/atproto/jetstream\"\n\n"+
		"func TestLegacy(t *testing.T) { db := testkit.DB(t); _ = db; _ = jetstream.ConsumerUsers }\n")

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	assert.Empty(t, markers)
}

func TestScanMarkers_FindsMarkersInBlockComments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "contract_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		"/*\nIngestion contract.\n"+markerToken+" social.coves.community.comment\n*/\nfunc TestX() {}\n")

	markers, err := scanMarkers(dir)
	require.NoError(t, err)
	require.Len(t, markers["social.coves.community.comment"], 1)
	assert.Equal(t, 7, markers["social.coves.community.comment"][0].Line,
		"a marker inside a block comment must report its own line, not the block's")
}

func TestScanMarkers_MissingDirectoryIsAnError(t *testing.T) {
	_, err := scanMarkers(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

// ---------------------------------------------------------------------------
// The burn-down file
// ---------------------------------------------------------------------------

func TestLoadPending_ParsesEntriesAndSkipsComments(t *testing.T) {
	path := writeFile(t, t.TempDir(), "pending.txt",
		"# header\n\nsocial.coves.feed.vote  # task 14: vote contract\n")

	entries, err := loadPending(path)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "social.coves.feed.vote", entries[0].Collection)
	assert.Equal(t, 14, entries[0].Task)
	assert.Equal(t, 3, entries[0].Line)
}

func TestLoadPending_RequiresAReason(t *testing.T) {
	path := writeFile(t, t.TempDir(), "pending.txt", "social.coves.feed.vote\n")

	_, err := loadPending(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no reason")
}

func TestLoadPending_RequiresAnOwningTask(t *testing.T) {
	path := writeFile(t, t.TempDir(), "pending.txt",
		"social.coves.feed.vote  # we will get to it\n")

	_, err := loadPending(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no owning task")
}

func TestLoadPending_RejectsDuplicateCollections(t *testing.T) {
	path := writeFile(t, t.TempDir(), "pending.txt",
		"social.coves.feed.vote  # task 14: a\nsocial.coves.feed.vote  # task 15: b\n")

	_, err := loadPending(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already listed on line 1")
}

func TestLoadPending_RejectsExtraFields(t *testing.T) {
	path := writeFile(t, t.TempDir(), "pending.txt",
		"social.coves.feed.vote votes  # task 14: a\n")

	_, err := loadPending(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected")
}

func TestLoadPending_MissingFileIsAnError(t *testing.T) {
	// Deleting the burn-down must not read as "nothing is pending".
	_, err := loadPending(filepath.Join(t.TempDir(), "nope.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestLoadPending_EmptyFileIsAnEmptyBurnDown(t *testing.T) {
	path := writeFile(t, t.TempDir(), "pending.txt", "# nothing pending\n")

	entries, err := loadPending(path)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// ---------------------------------------------------------------------------
// End to end
// ---------------------------------------------------------------------------

func TestRun_GreenRunReportsTheInventory(t *testing.T) {
	dir := t.TempDir()
	e2eDir := filepath.Join(dir, "e2e")
	require.NoError(t, os.Mkdir(e2eDir, 0o755))
	writeFile(t, e2eDir, "post_contract_test.go", "//go:build e2e\n\npackage e2e\n\n"+
		markerLine("social.coves.community.post")+"\n")
	pending := writeFile(t, dir, "pending.txt", "social.coves.feed.vote  # task 14: vote contract\n")

	var out bytes.Buffer
	code, err := run(&out, consumedBy(map[string]string{
		"social.coves.community.post": "posts",
		"social.coves.feed.vote":      "votes",
	}), e2eDir, pending, true)

	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "2 consumed collection(s)")
	assert.Contains(t, out.String(), "1 contracted, 1 pending")
}

func TestRun_MissingContractExitsOne(t *testing.T) {
	dir := t.TempDir()
	e2eDir := filepath.Join(dir, "e2e")
	require.NoError(t, os.Mkdir(e2eDir, 0o755))
	pending := writeFile(t, dir, "pending.txt", "# empty\n")

	var out bytes.Buffer
	code, err := run(&out, consumedBy(map[string]string{"social.coves.feed.vote": "votes"}),
		e2eDir, pending, true)

	require.NoError(t, err)
	assert.Equal(t, 1, code, "a failed check is exit 1, distinct from the tool erroring")
	assert.Contains(t, out.String(), "CONTRACT MANIFEST FAILED")
}

func TestRun_ToolErrorIsNotACheckFailure(t *testing.T) {
	// An unreadable burn-down must surface as "the check did not run" (exit 2
	// from main), never as a passing or failing verdict.
	dir := t.TempDir()
	e2eDir := filepath.Join(dir, "e2e")
	require.NoError(t, os.Mkdir(e2eDir, 0o755))

	var out bytes.Buffer
	_, err := run(&out, nil, e2eDir, filepath.Join(dir, "nope.txt"), true)
	require.Error(t, err)
	assert.Empty(t, out.String(), "nothing may be reported when the inputs could not be read")
}

// TestRealManifest_IsSatisfied runs the check exactly as CI does, against the
// repository's own consumer table, pipeline tier and burn-down.
//
// It is the unit-tier half of the CI stage: `make ci` runs the command inside
// the stack, but a developer running `go test ./cmd/...` should learn about a
// missing contract in seconds rather than in the gate.
func TestRealManifest_IsSatisfied(t *testing.T) {
	root := repoRoot(t)
	var out bytes.Buffer
	code, err := run(&out,
		jetstream.ConsumedCollections(),
		filepath.Join(root, "tests", "e2e"),
		filepath.Join(root, "tests", "ci", "pending_contracts.txt"),
		true)
	require.NoError(t, err)
	assert.Equal(t, 0, code, "contract manifest is not satisfied:\n%s", out.String())
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate the repository root above " + dir)
		}
		dir = parent
	}
}

func TestMarkerToken_IsTheSpelledContract(t *testing.T) {
	// docs/TEST_ARCHITECTURE.md §3.4a names this marker verbatim, and every
	// contract written in phase 4 will carry it. Changing it silently would
	// orphan every contract at once.
	assert.Equal(t, "coves:ingestion-contract", markerToken)
	assert.True(t, strings.HasPrefix(markerLine("x"), "//coves:"))
}
