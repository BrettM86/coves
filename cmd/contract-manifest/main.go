// Command contract-manifest enforces that every record collection the AppView
// ingests has a pipeline contract proving it.
//
// # THE PROBLEM IT EXISTS TO SOLVE
//
// docs/TEST_ARCHITECTURE.md §3.4a makes one ingestion contract per consumed
// collection mandatory: a record written straight to the PDS, indexed by the
// AppView's own consumers, observed on a serving endpoint. Nothing else proves
// the firehose is alive, because several domains index synchronously on the
// client path and pass an endpoint-in/endpoint-out test with the consumers dead.
//
// A list of which collections need such a contract was tried by hand once and
// was wrong on day one — it missed both block collections and collapsed the two
// aggregator record types. So the inventory is not written down: it is read from
// jetstream.ConsumedCollections(), the same table cmd/server derives its
// wantedCollections subscribe filters from. Adding a collection to a consumer
// therefore breaks this check until a contract exists for it.
//
// # WHERE IT LIVES, AND WHY IT IS NOT IN testkit
//
// tests/testkit may import no domain package (see its package doc): T1 tests
// are in-package, so a testkit that imported internal/core/... would be an
// import cycle for every domain it touched. Reading the consumer topology means
// importing internal/atproto/jetstream, whose consumers pull in seven domain
// packages — squarely outside that boundary. It is a command rather than a test
// for the same reason it is not a lint: it must import the real table, and it
// must run once for the whole repository rather than once per package.
//
// # THE THREE STATES OF A COLLECTION
//
//	contracted  a comment in tests/e2e whose first word is coves:ingestion-contract
//	            names it. This is the goal state.
//	pending     tests/ci/pending_contracts.txt lists it with the task that owns
//	            writing the contract. Phase 4 lands contracts one domain at a
//	            time, and a check that fails until the last one arrives would be
//	            switched off long before then. This is the burn-down, and it
//	            ratchets: a pending collection that gains a contract makes its
//	            pending entry STALE, which fails — exactly like ci-report's
//	            stale-allowlist rule. Entries can only leave the file.
//	missing     neither. This fails the gate.
//
// Markers naming a collection nothing consumes fail too, in both directions: a
// stale marker is a contract testing a pipeline that no longer exists, and a
// stale pending entry is a promise to write one.
//
// Phase 6 (task 20) empties pending_contracts.txt and flips -allow-pending to
// false, at which point "contracted" is the only passing state.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"Coves/internal/atproto/jetstream"
)

// markerToken is the first word of a contract marker comment. A comment line
// that begins with it is a marker and must be well formed; a line that merely
// mentions it — this doc comment, for instance — is prose.
const markerToken = "coves:ingestion-contract"

// markerPattern matches a whole marker line, after the comment characters have
// been stripped. Exactly one argument: two would be ambiguous about which
// collection is contracted, and zero contracts nothing.
var markerPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(markerToken) + `\s+(\S+)$`)

// taskPattern pulls the owning task number out of a pending entry's reason.
// Requiring it is what makes the file a burn-down rather than a list of
// excuses: every pending collection names the iteration that will remove it.
var taskPattern = regexp.MustCompile(`(?i)\btask\s+(\d+)\b`)

// marker is one contract declaration found in the e2e tier.
type marker struct {
	Collection string
	File       string
	Line       int
}

// Position renders the marker's source location for a message.
func (m marker) Position() string { return fmt.Sprintf("%s:%d", m.File, m.Line) }

// pendingEntry is one line of the burn-down file: a collection that is
// knowingly uncontracted, and the task that owes it a contract.
type pendingEntry struct {
	Collection string
	Task       int
	Reason     string
	Line       int
}

// result is the evaluated state of the manifest.
type result struct {
	Contracted []contractedCollection
	Pending    []pendingCollection
	Missing    []string
	Violations []string
}

type contractedCollection struct {
	Collection string
	Consumers  []string
	Markers    []marker
}

type pendingCollection struct {
	Collection string
	Consumers  []string
	Entry      pendingEntry
}

func main() {
	e2eDir := flag.String("e2e-dir", "tests/e2e",
		"root of the pipeline tier, scanned for contract markers")
	pendingPath := flag.String("pending", "tests/ci/pending_contracts.txt",
		"path to the committed burn-down of collections that do not have a contract yet")
	allowPending := flag.Bool("allow-pending", true,
		"when false, any remaining pending entry fails the check (the phase 6 ratchet)")
	flag.Parse()

	code, err := run(os.Stdout, jetstream.ConsumedCollections(), *e2eDir, *pendingPath, *allowPending)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contract-manifest: %v\n", err)
		os.Exit(2)
	}
	os.Exit(code)
}

// run is the whole tool, parameterised for testability. It returns the process
// exit code: 0 when every consumed collection is accounted for, 1 when it is
// not. A non-nil error means the tool could not do its job — a malformed
// burn-down file, an unparseable test file — which main turns into exit code 2,
// deliberately distinct from a failed check.
func run(w io.Writer, consumed map[string][]string, e2eDir, pendingPath string, allowPending bool) (int, error) {
	markers, err := scanMarkers(e2eDir)
	if err != nil {
		return 0, err
	}
	pending, err := loadPending(pendingPath)
	if err != nil {
		return 0, err
	}
	res := evaluate(consumed, markers, pending, allowPending)
	report(w, res, e2eDir, pendingPath)
	if len(res.Violations) > 0 {
		return 1, nil
	}
	return 0, nil
}

// evaluate applies the manifest rules. It never reads the filesystem, so every
// rule below is covered by a table test rather than by a fixture tree.
func evaluate(
	consumed map[string][]string,
	markers map[string][]marker,
	pending []pendingEntry,
	allowPending bool,
) result {
	var res result

	pendingByCollection := make(map[string]pendingEntry, len(pending))
	for _, entry := range pending {
		pendingByCollection[entry.Collection] = entry
	}

	for _, collection := range sortedKeys(consumed) {
		consumers := consumed[collection]
		found, contracted := markers[collection]
		entry, isPending := pendingByCollection[collection]

		switch {
		case contracted && isPending:
			// The ratchet. A contract exists, so the promise to write one is
			// spent — and leaving it in the file would let the contract be
			// deleted again without the check noticing.
			res.Contracted = append(res.Contracted, contractedCollection{
				Collection: collection, Consumers: consumers, Markers: found,
			})
			res.Violations = append(res.Violations, fmt.Sprintf(
				"STALE pending entry: %s is listed at %s:%d (task %d) but now has a contract at %s — delete the line",
				collection, pendingRelPath, entry.Line, entry.Task, found[0].Position()))
		case contracted:
			res.Contracted = append(res.Contracted, contractedCollection{
				Collection: collection, Consumers: consumers, Markers: found,
			})
		case isPending:
			res.Pending = append(res.Pending, pendingCollection{
				Collection: collection, Consumers: consumers, Entry: entry,
			})
		default:
			res.Missing = append(res.Missing, collection)
			res.Violations = append(res.Violations, fmt.Sprintf(
				"MISSING contract: %s is indexed by consumer(s) %s and no test in the pipeline tier declares it — "+
					"write the ingestion contract (docs/TEST_ARCHITECTURE.md §3.4a) or list it in the burn-down with its owning task",
				collection, strings.Join(consumers, ", ")))
		}
	}

	// One contract per collection. Two markers for the same collection means
	// two tests each believe they are THE proof that it is ingested, and the
	// failure mode is quiet: delete one and the check stays green, so the
	// coverage loss never surfaces. §3.4a says one per collection; this is that
	// sentence, enforced.
	for _, collection := range sortedKeys(markers) {
		found := markers[collection]
		if len(found) < 2 {
			continue
		}
		positions := make([]string, 0, len(found))
		for _, m := range found {
			positions = append(positions, m.Position())
		}
		res.Violations = append(res.Violations, fmt.Sprintf(
			"DUPLICATE contract: %s is declared %d times (%s) — exactly one test owns each collection's "+
				"ingestion proof, so fold them together or drop the marker from the one that is not it",
			collection, len(found), strings.Join(positions, ", ")))
	}

	// A marker for a collection nothing consumes: the contract is exercising a
	// pipeline that was removed, so it is either dead weight or evidence that a
	// consumer lost a filter it should still have.
	for _, collection := range sortedKeys(markers) {
		if _, ok := consumed[collection]; ok {
			continue
		}
		for _, m := range markers[collection] {
			res.Violations = append(res.Violations, fmt.Sprintf(
				"UNCONSUMED marker: %s declares a contract for %s at %s, which no Jetstream consumer indexes — "+
					"either the collection was dropped from internal/atproto/jetstream/feeds.go by mistake, or the contract is obsolete",
				m.File, collection, m.Position()))
		}
	}

	// Same, for the burn-down: owing a contract for something nothing ingests
	// would keep a task alive forever.
	for _, entry := range pending {
		if _, ok := consumed[entry.Collection]; ok {
			continue
		}
		res.Violations = append(res.Violations, fmt.Sprintf(
			"UNCONSUMED pending entry: %s:%d lists %s, which no Jetstream consumer indexes — delete the line",
			pendingRelPath, entry.Line, entry.Collection))
	}

	if !allowPending && len(res.Pending) > 0 {
		res.Violations = append(res.Violations, fmt.Sprintf(
			"%d collection(s) still pending with -allow-pending=false — the burn-down must be empty",
			len(res.Pending)))
	}

	return res
}

// pendingRelPath is the burn-down's canonical location, used in messages so a
// reader is told which file to edit rather than which flag was passed.
const pendingRelPath = "tests/ci/pending_contracts.txt"

// ---------------------------------------------------------------------------
// Scanning the pipeline tier
// ---------------------------------------------------------------------------

// scanMarkers walks dir for pipeline-tier test files and collects their
// contract markers.
//
// Three filters decide what is even looked at, and each closes a way a marker
// could claim a contract that does not run:
//
//   - Only *_test.go. A marker in a non-test file is a comment in a helper; the
//     go tool would never turn it into a test.
//   - Only files the go tool would COMPILE under -tags e2e, decided by
//     go/build's own MatchFile rather than by reading the //go:build line by
//     hand. That evaluates the constraint expression and the filename's
//     GOOS/GOARCH suffixes with the same code the toolchain uses, so
//     "e2e && linux" is accepted where a hand-rolled reader rejected it, and
//     "contract_windows_test.go" is correctly excluded on a Linux runner.
//   - No testdata/, no _foo/, no .foo/ — the directories the go tool itself
//     ignores. A fixture tree is not the test suite.
//
// Parsing rather than grepping is the fourth: a marker has to be a real
// comment, because a string literal mentioning one proves nothing.
//
// NOTE ON PLATFORM RELATIVITY: MatchFile answers for THIS build (host GOOS and
// GOARCH). That is the honest answer — a contract that cannot compile here does
// not run here — but it means a platform-constrained contract would be visible
// to the gate's Linux runner and invisible on a developer's Mac. The tier has
// no such file today and should not grow one; if it ever does, this is where
// the divergence comes from.
func scanMarkers(dir string) (map[string][]marker, error) {
	markers := make(map[string][]marker)
	ctxt := build.Default
	ctxt.BuildTags = append([]string{"e2e"}, ctxt.BuildTags...)

	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == dir {
				return nil
			}
			if isIgnoredDir(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		included, matchErr := ctxt.MatchFile(filepath.Dir(path), entry.Name())
		if matchErr != nil {
			return fmt.Errorf("reading build constraints of %s: %w", path, matchErr)
		}
		if !included {
			// Not in the e2e build. A marker here would be a contract that
			// never runs, so it must not pass silently either.
			return rejectMarkersOutsideTheTier(path)
		}
		found, err := markersInFile(path)
		if err != nil {
			return err
		}
		for _, m := range found {
			markers[m.Collection] = append(markers[m.Collection], m)
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("pipeline tier %s does not exist: this check reads contract markers from it", dir)
		}
		return nil, walkErr
	}

	for _, found := range markers {
		sort.Slice(found, func(i, j int) bool {
			if found[i].File != found[j].File {
				return found[i].File < found[j].File
			}
			return found[i].Line < found[j].Line
		})
	}
	return markers, nil
}

// isIgnoredDir reports whether the go tool would ignore a directory of this
// name: testdata and anything beginning with _ or . are invisible to it.
func isIgnoredDir(name string) bool {
	return name == "testdata" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
}

// rejectMarkersOutsideTheTier fails when a file the e2e build EXCLUDES still
// declares a contract.
//
// Silently ignoring it would be worse than either alternative: the collection
// would report as uncontracted while a file in the tree looks like it covers
// it, and the next person deletes the burn-down entry to make the check happy.
func rejectMarkersOutsideTheTier(path string) error {
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	for i, line := range strings.Split(string(source), "\n") {
		if markerPattern.MatchString(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "/*"))) {
			return fmt.Errorf(
				"%s:%d: declares a contract marker, but the go tool does NOT include this file in a "+
					"-tags e2e build (check its //go:build line and its filename's GOOS/GOARCH suffix) — "+
					"the contract would never run",
				path, i+1)
		}
	}
	return nil
}

func markersInFile(path string) ([]marker, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var found []marker
	for _, group := range file.Comments {
		for _, comment := range group.List {
			for offset, text := range commentLines(comment.Text) {
				line := strings.TrimSpace(text)
				if !strings.HasPrefix(line, markerToken) {
					continue
				}
				at := fset.Position(comment.Slash)
				match := markerPattern.FindStringSubmatch(line)
				if match == nil {
					return nil, fmt.Errorf(
						"%s:%d: malformed contract marker %q — the line must be exactly \"%s <collection>\"",
						path, at.Line+offset, line, markerToken)
				}
				found = append(found, marker{Collection: match[1], File: path, Line: at.Line + offset})
			}
		}
	}

	if len(found) > 0 {
		if err := enforceTierDiscipline(path, file, fset); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// forbiddenContractImports are the imports that contradict what a contract
// claims to prove. Each maps to why.
var forbiddenContractImports = map[string]string{
	"github.com/gorilla/websocket": "a contract that dials Jetstream itself proves that Jetstream delivers, " +
		"not that the AppView's consumers are wired up — the serving endpoint is the only witness that answers that",
	"Coves/internal/atproto/jetstream": "a contract that can construct a consumer is one edit away from feeding " +
		"itself events, which passes with the shipped consumer wiring dead",
}

// enforceTierDiscipline rejects a marker-carrying file that breaks the rules
// the marker asserts it follows (docs/TEST_ARCHITECTURE.md §3.4).
//
// Scoped to files that actually declare a contract, deliberately: the two
// legacy files in tests/e2e do instantiate consumers and do clone the test
// database, and they carry no marker, so they stay buildable until task 16
// rebuilds them. The moment anyone adds a marker to such a file, this fires.
func enforceTierDiscipline(path string, file *ast.File, fset *token.FileSet) error {
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if reason, forbidden := forbiddenContractImports[imported]; forbidden {
			return fmt.Errorf("%s:%d: a file declaring an ingestion contract must not import %q: %s",
				path, fset.Position(spec.Pos()).Line, imported, reason)
		}
	}

	var violation error
	ast.Inspect(file, func(node ast.Node) bool {
		if violation != nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "DB" {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "testkit" {
			return true
		}
		violation = fmt.Errorf(
			"%s:%d: a file declaring an ingestion contract must not call testkit.DB: the AppView writes "+
				"coves_dev and testkit.DB hands out clones of a template, so an assertion against a clone "+
				"reads a database nothing under test ever wrote to — observe through serving endpoints",
			path, fset.Position(call.Pos()).Line)
		return false
	})
	return violation
}

// commentLines splits a comment's source text into content lines with the
// comment characters removed, so a marker reads the same whether it was written
// as // or inside a /* */ block.
func commentLines(text string) []string {
	if trimmed, ok := strings.CutPrefix(text, "//"); ok {
		return []string{strings.TrimSpace(trimmed)}
	}
	body := strings.TrimSuffix(strings.TrimPrefix(text, "/*"), "*/")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
	}
	return lines
}

// ---------------------------------------------------------------------------
// The burn-down file
// ---------------------------------------------------------------------------

// loadPending reads the committed list of collections that do not have a
// contract yet.
//
// Format, one entry per line:
//
//	<collection>  # task <N>: why it is not contracted yet
//
// Both halves are mandatory, for the same reason ci-report's allowlist demands
// a reason: a bare list of collections is unreviewable, and a reason without a
// task number is a deferral with nobody holding it. A missing file is an error
// rather than an empty list — "the burn-down is empty" and "the burn-down was
// deleted" must not look the same.
func loadPending(path string) ([]pendingEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("burn-down file %s does not exist: this check requires an explicit "+
				"(possibly empty) list of collections that are knowingly uncontracted — create it, with a "+
				"header comment explaining what it is for", path)
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var entries []pendingEntry
	seen := make(map[string]int)
	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}

		spec, reason, hasReason := strings.Cut(raw, "#")
		reason = strings.TrimSpace(reason)
		if !hasReason || reason == "" {
			return nil, fmt.Errorf("%s:%d: entry %q has no reason — append \"# task <N>: why it is not contracted yet\"",
				path, lineNo, strings.TrimSpace(spec))
		}

		fields := strings.Fields(spec)
		if len(fields) != 1 {
			return nil, fmt.Errorf("%s:%d: expected \"<collection>  # task <N>: reason\", got %q",
				path, lineNo, strings.TrimSpace(spec))
		}
		collection := fields[0]

		task := taskPattern.FindStringSubmatch(reason)
		if task == nil {
			return nil, fmt.Errorf("%s:%d: %s has no owning task — the reason must name one, as in "+
				"\"# task 12: post ingestion contract lands with the post decomposition\"",
				path, lineNo, collection)
		}
		number, err := strconv.Atoi(task[1])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: unreadable task number %q: %w", path, lineNo, task[1], err)
		}

		if first, duplicate := seen[collection]; duplicate {
			return nil, fmt.Errorf("%s:%d: %s is already listed on line %d — two owners means no owner",
				path, lineNo, collection, first)
		}
		seen[collection] = lineNo

		entries = append(entries, pendingEntry{
			Collection: collection, Task: number, Reason: reason, Line: lineNo,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// report writes the human-facing summary. It always prints the full inventory,
// passing or not: the burn-down's whole job is to be visible in every CI log,
// so that "eight collections still uncontracted" is a number somebody watches
// shrink rather than a fact buried in a file.
func report(w io.Writer, res result, e2eDir, pendingPath string) {
	total := len(res.Contracted) + len(res.Pending) + len(res.Missing)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "───────────────────────────────────────────────────────────────")
	fmt.Fprintf(w, " Pipeline contract manifest — %d consumed collection(s)\n", total)
	fmt.Fprintf(w, " markers: %s   burn-down: %s\n", e2eDir, pendingPath)
	fmt.Fprintln(w, "───────────────────────────────────────────────────────────────")

	for _, c := range res.Contracted {
		fmt.Fprintf(w, "  ✓ %-42s %s\n", c.Collection, c.Markers[0].Position())
	}
	for _, p := range res.Pending {
		fmt.Fprintf(w, "  … %-42s pending, task %d\n", p.Collection, p.Entry.Task)
	}
	for _, collection := range res.Missing {
		fmt.Fprintf(w, "  ✗ %-42s NO CONTRACT\n", collection)
	}

	fmt.Fprintln(w)
	if len(res.Violations) == 0 {
		fmt.Fprintf(w, "✓ contract manifest OK — %d contracted, %d pending\n",
			len(res.Contracted), len(res.Pending))
		if len(res.Pending) == 0 {
			fmt.Fprintln(w, "  the burn-down is empty: every consumed collection is proven end to end")
		}
		fmt.Fprintln(w)
		return
	}

	fmt.Fprintln(w, "✗ CONTRACT MANIFEST FAILED")
	for _, v := range res.Violations {
		fmt.Fprintf(w, "    · %s\n", v)
	}
	fmt.Fprintln(w)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
