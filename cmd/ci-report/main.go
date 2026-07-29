// Command ci-report turns `go test -json` output into a merge gate.
//
// # THE PROBLEM IT EXISTS TO SOLVE
//
// `go test` reports a skipped test as "not a failure", so a suite that quietly
// stops running is indistinguishable from a suite that passes. This repository
// is unusually exposed to that: tests/ carries 191 t.Skip and 164
// testing.Short() call sites, and many of them are infrastructure probes of the
// form
//
//	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
//	if err != nil {
//	    t.Skipf("PDS not running at %s: %v", pdsURL, err)
//	}
//
// Those guards are good developer ergonomics — running one package against a
// partial stack should not drown you in failures. But they make a *gate*
// meaningless: stop the PDS and `make test-all` still prints "ALL TESTS PASSED",
// having silently skipped every real PDS write and every firehose round-trip.
//
// So this tool inverts the default. A skip is a failure unless it appears in an
// allowlist committed to the repository with a stated reason. The allowlist
// becomes the explicit, reviewable contract for what the gate does not cover,
// and any new skip has to be justified in a diff rather than vanishing into a
// green checkmark.
//
// It also fails on a stale allowlist — an entry that did not skip this run —
// because an allowlist that drifts out of sync with reality is how this kind of
// mechanism rots into decoration.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// retainedOutputLines bounds how much output is held per in-flight test. Output
// is discarded as soon as a test passes, so the real ceiling is the number of
// failing or skipping tests, not the size of the suite.
const retainedOutputLines = 40

// event is one line of `go test -json`. Only the fields the gate needs are
// declared; the rest are ignored by encoding/json.
type event struct {
	Action string `json:"Action"`
	// Package is the import path for test events. Empty on build events, which
	// carry ImportPath instead.
	Package    string `json:"Package"`
	Test       string `json:"Test"`
	Output     string `json:"Output"`
	ImportPath string `json:"ImportPath"`
}

// testKey identifies a test uniquely. Subtests arrive as "Parent/Child", and a
// skip in a subtest is reported against that full name, so the key must include
// it rather than collapsing to the parent.
type testKey struct {
	Pkg  string
	Test string
}

// testResult is the accumulated state of one test.
type testResult struct {
	key    testKey
	action string // "pass", "fail", or "skip"
	output []string
}

// allowEntry is one allowlisted skip.
type allowEntry struct {
	Pkg string
	// Test is the test name, optionally ending in "/*" to cover a test and all
	// of its subtests — the common shape when a parent skips before subtests
	// are reached.
	Test   string
	Reason string
	// Conditional marks an entry written "~pkg Test": one that may or may not
	// skip from run to run, so its absence is not staleness.
	//
	// This exists for tests whose skip depends on something outside the stack —
	// the unfurl tests skip when a third-party URL is unreachable and run when
	// it is not. Enforcing staleness on those would make the gate flake on
	// someone else's outage, which is the exact failure mode a merge gate must
	// not have. Deterministic skips deliberately do NOT get this marker, so
	// they still fail the gate the day they stop skipping.
	Conditional bool
	Line        int
	matched     bool
}

// matches reports whether this entry covers the given test.
func (e allowEntry) matches(k testKey) bool {
	if e.Pkg != k.Pkg {
		return false
	}
	if prefix, ok := strings.CutSuffix(e.Test, "/*"); ok {
		return k.Test == prefix || strings.HasPrefix(k.Test, prefix+"/")
	}
	return e.Test == k.Test
}

// summary is the machine-readable artifact. An agent reads this file rather
// than scraping the human-facing text, which is free to change.
type summary struct {
	OK       bool `json:"ok"`
	Packages struct {
		Total  int `json:"total"`
		Failed int `json:"failed"`
	} `json:"packages"`
	Tests struct {
		Total   int `json:"total"`
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
	} `json:"tests"`
	Failures         []failureReport `json:"failures"`
	UnexpectedSkips  []skipReport    `json:"unexpected_skips"`
	StaleAllowlist   []staleReport   `json:"stale_allowlist"`
	BuildFailures    []string        `json:"build_failures"`
	AllowedSkipsUsed int             `json:"allowed_skips_used"`
	Violations       []string        `json:"violations"`
}

type failureReport struct {
	Package string   `json:"package"`
	Test    string   `json:"test"`
	Output  []string `json:"output"`
}

type skipReport struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	Reason  string `json:"reason"`
}

type staleReport struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	Reason  string `json:"reason"`
	Line    int    `json:"line"`
}

func main() {
	allowlistPath := flag.String("allowlist", "tests/ci/allowed_skips.txt",
		"path to the committed list of skips the gate tolerates")
	summaryPath := flag.String("summary", "",
		"if set, write the machine-readable JSON summary here")
	allowStale := flag.String("allow-stale", "false",
		"when true, an allowlist entry that did not skip this run is reported but does not fail the gate")
	flag.Parse()

	code, err := run(os.Stdin, os.Stdout, *allowlistPath, *summaryPath, *allowStale == "true")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ci-report: %v\n", err)
		os.Exit(2)
	}
	os.Exit(code)
}

// run is the whole tool, parameterised for testability. It returns the process
// exit code: 0 when the gate passes, 1 when it does not. A non-nil error means
// ci-report itself could not do its job, which is exit code 2 — deliberately
// distinct, because "the gate failed" and "the gate did not run" must never
// look the same to a caller.
func run(stdin io.Reader, stdout io.Writer, allowlistPath, summaryPath string, allowStale bool) (int, error) {
	allow, err := loadAllowlist(allowlistPath)
	if err != nil {
		return 0, err
	}

	results, pkgResults, buildFailures, err := parse(stdin)
	if err != nil {
		return 0, err
	}

	sum := evaluate(results, pkgResults, buildFailures, allow, allowStale)
	report(stdout, sum)

	if summaryPath != "" {
		if err := writeSummary(summaryPath, sum); err != nil {
			return 0, err
		}
	}

	if sum.OK {
		return 0, nil
	}
	return 1, nil
}

// parse consumes the -json event stream.
//
// Non-JSON lines are tolerated: the go toolchain writes some diagnostics (and a
// panicking test process writes its trace) straight to stdout, unwrapped. They
// are surfaced as build failures only when the toolchain marks them as such.
func parse(r io.Reader) (map[testKey]*testResult, map[string]string, []string, error) {
	results := make(map[testKey]*testResult)
	pkgResults := make(map[string]string)
	var buildFailures []string

	scanner := bufio.NewScanner(r)
	// Test output lines can be long (assertion diffs especially); the default
	// 64KiB limit is not enough for a failed require.Equal on large structs.
	scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			// A malformed event is not worth failing the run over, but it must
			// not be silent either.
			fmt.Fprintf(os.Stderr, "ci-report: skipping unparseable event: %v\n", err)
			continue
		}

		// Build events (Go 1.24+) carry ImportPath, not Package. A build failure
		// means a package's tests never ran at all — the most important thing
		// not to mistake for success.
		if ev.Action == "build-fail" {
			target := ev.ImportPath
			if target == "" {
				target = ev.Package
			}
			buildFailures = append(buildFailures, target)
			continue
		}

		// Package-level events have no Test. A package-level "skip" means "no
		// test files", which is benign and must not be confused with a test
		// that skipped itself.
		if ev.Test == "" {
			switch ev.Action {
			case "pass", "fail", "skip":
				pkgResults[ev.Package] = ev.Action
			}
			continue
		}

		key := testKey{Pkg: ev.Package, Test: ev.Test}
		res, ok := results[key]
		if !ok {
			res = &testResult{key: key}
			results[key] = res
		}

		switch ev.Action {
		case "output":
			if trimmed := strings.TrimRight(ev.Output, "\n"); trimmed != "" {
				res.output = append(res.output, trimmed)
				if len(res.output) > retainedOutputLines {
					res.output = res.output[len(res.output)-retainedOutputLines:]
				}
			}
		case "pass":
			res.action = "pass"
			// Output for a passing test is dead weight; releasing it here is
			// what keeps memory proportional to problems, not to suite size.
			res.output = nil
		case "fail", "skip":
			res.action = ev.Action
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("reading go test output: %w", err)
	}

	return results, pkgResults, buildFailures, nil
}

// evaluate applies the gate's rules to the parsed run.
func evaluate(
	results map[testKey]*testResult,
	pkgResults map[string]string,
	buildFailures []string,
	allow []*allowEntry,
	allowStale bool,
) summary {
	var sum summary
	sum.BuildFailures = buildFailures
	sum.Failures = []failureReport{}
	sum.UnexpectedSkips = []skipReport{}
	sum.StaleAllowlist = []staleReport{}
	sum.Violations = []string{}

	keys := make([]testKey, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Pkg != keys[j].Pkg {
			return keys[i].Pkg < keys[j].Pkg
		}
		return keys[i].Test < keys[j].Test
	})

	for _, k := range keys {
		res := results[k]
		switch res.action {
		case "pass":
			sum.Tests.Passed++
		case "fail":
			sum.Tests.Failed++
			sum.Failures = append(sum.Failures, failureReport{
				Package: k.Pkg, Test: k.Test, Output: res.output,
			})
		case "skip":
			sum.Tests.Skipped++
			if entry := findAllow(allow, k); entry != nil {
				entry.matched = true
				sum.AllowedSkipsUsed++
			} else {
				sum.UnexpectedSkips = append(sum.UnexpectedSkips, skipReport{
					Package: k.Pkg, Test: k.Test, Reason: skipReason(res.output),
				})
			}
		default:
			// A test that started and never reported a terminal action. This
			// happens when the test binary dies mid-run (panic, timeout, OOM),
			// which is exactly when a gate must not report success.
			sum.Tests.Failed++
			sum.Failures = append(sum.Failures, failureReport{
				Package: k.Pkg, Test: k.Test,
				Output: append([]string{"(no terminal result — test binary exited before this test finished)"}, res.output...),
			})
		}
	}
	sum.Tests.Total = sum.Tests.Passed + sum.Tests.Failed + sum.Tests.Skipped

	for pkg, action := range pkgResults {
		sum.Packages.Total++
		if action == "fail" {
			sum.Packages.Failed++
		}
		_ = pkg
	}

	for _, entry := range allow {
		// Conditional entries are exempt from the staleness rule by design: not
		// skipping is a legitimate outcome for them.
		if !entry.matched && !entry.Conditional {
			sum.StaleAllowlist = append(sum.StaleAllowlist, staleReport{
				Package: entry.Pkg, Test: entry.Test, Reason: entry.Reason, Line: entry.Line,
			})
		}
	}
	sort.Slice(sum.StaleAllowlist, func(i, j int) bool {
		return sum.StaleAllowlist[i].Line < sum.StaleAllowlist[j].Line
	})

	// Rule 1: nothing may fail.
	if sum.Tests.Failed > 0 {
		sum.Violations = append(sum.Violations,
			fmt.Sprintf("%d test(s) failed", sum.Tests.Failed))
	}
	// Rule 2: every package must build. A build failure can leave zero test
	// events for a package, so this cannot be inferred from test results.
	if len(buildFailures) > 0 {
		sum.Violations = append(sum.Violations,
			fmt.Sprintf("%d package(s) failed to build", len(buildFailures)))
	}
	// Rule 3: no unapproved skips. This is the rule that makes the gate mean
	// what it says.
	if len(sum.UnexpectedSkips) > 0 {
		sum.Violations = append(sum.Violations,
			fmt.Sprintf("%d test(s) skipped without an allowlist entry", len(sum.UnexpectedSkips)))
	}
	// Rule 4: the allowlist must describe reality.
	if len(sum.StaleAllowlist) > 0 && !allowStale {
		sum.Violations = append(sum.Violations,
			fmt.Sprintf("%d stale allowlist entr(ies) — listed but did not skip", len(sum.StaleAllowlist)))
	}
	// Rule 5: a run that executed nothing is not a pass. Guards against an
	// invocation that matched no packages, or a stream that never arrived.
	if sum.Tests.Total == 0 {
		sum.Violations = append(sum.Violations,
			"no tests ran — refusing to report success for an empty run")
	}

	sum.OK = len(sum.Violations) == 0
	return sum
}

func findAllow(allow []*allowEntry, k testKey) *allowEntry {
	for _, entry := range allow {
		if entry.matches(k) {
			return entry
		}
	}
	return nil
}

// skipReason extracts the t.Skip message from a test's captured output. The
// toolchain prints it as an indented "file.go:line: message" line before the
// "--- SKIP" marker.
func skipReason(output []string) string {
	for i := len(output) - 1; i >= 0; i-- {
		line := strings.TrimSpace(output[i])
		if line == "" || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "===") {
			continue
		}
		return line
	}
	return "(no reason reported)"
}

// loadAllowlist reads the committed skip contract.
//
// Format, one entry per line:
//
//	<import-path> <TestName>   # why this skip is acceptable
//	~<import-path> <TestName>  # ...and this one may or may not skip
//
// TestName may end in "/*" to cover a test and all its subtests. A leading "~"
// marks the entry conditional, exempting it from the staleness rule.
//
// The reason is mandatory: an allowlist without reasons becomes a list nobody
// can review, and the whole point is reviewability.
func loadAllowlist(path string) ([]*allowEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("allowlist %s does not exist: the gate requires an explicit "+
				"(possibly empty) skip contract — create it, with a comment explaining any entries", path)
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var entries []*allowEntry
	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}

		conditional := false
		if stripped, ok := strings.CutPrefix(raw, "~"); ok {
			conditional = true
			raw = strings.TrimSpace(stripped)
		}

		spec, reason, hasReason := strings.Cut(raw, "#")
		if !hasReason || strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("%s:%d: entry %q has no reason — append \"# why this skip is acceptable\"",
				path, lineNo, strings.TrimSpace(spec))
		}

		fields := strings.Fields(spec)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: expected \"<import-path> <TestName>  # reason\", got %q",
				path, lineNo, strings.TrimSpace(spec))
		}

		entries = append(entries, &allowEntry{
			Pkg:         fields[0],
			Test:        fields[1],
			Reason:      strings.TrimSpace(reason),
			Conditional: conditional,
			Line:        lineNo,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return entries, nil
}

func writeSummary(path string, sum summary) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating summary directory: %w", err)
		}
	}
	data, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding summary: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing summary: %w", err)
	}
	return nil
}

// report writes the human-facing summary. Failures come first and in full: when
// this runs in CI, this text is the only thing anyone reads.
func report(w io.Writer, sum summary) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "───────────────────────────────────────────────────────────────")
	fmt.Fprintf(w, " %d tests: %d passed, %d failed, %d skipped (%d allowlisted)\n",
		sum.Tests.Total, sum.Tests.Passed, sum.Tests.Failed, sum.Tests.Skipped, sum.AllowedSkipsUsed)
	fmt.Fprintln(w, "───────────────────────────────────────────────────────────────")

	if len(sum.BuildFailures) > 0 {
		fmt.Fprintf(w, "\nBUILD FAILURES (%d) — these packages ran no tests at all:\n", len(sum.BuildFailures))
		for _, pkg := range sum.BuildFailures {
			fmt.Fprintf(w, "  ✗ %s\n", pkg)
		}
	}

	if len(sum.Failures) > 0 {
		fmt.Fprintf(w, "\nFAILED (%d):\n", len(sum.Failures))
		for _, f := range sum.Failures {
			fmt.Fprintf(w, "\n  ✗ %s\n    %s\n", f.Test, f.Package)
			for _, line := range f.Output {
				fmt.Fprintf(w, "      %s\n", line)
			}
		}
	}

	if len(sum.UnexpectedSkips) > 0 {
		fmt.Fprintf(w, "\nSKIPPED WITHOUT APPROVAL (%d):\n", len(sum.UnexpectedSkips))
		fmt.Fprintln(w, "  A skip is a gap in coverage. Either make the test run, or add it to")
		fmt.Fprintln(w, "  the allowlist with a reason — but do not let it pass silently.")
		for _, s := range sum.UnexpectedSkips {
			fmt.Fprintf(w, "\n  ⊘ %s\n    %s\n    %s\n", s.Test, s.Package, s.Reason)
		}
	}

	if len(sum.StaleAllowlist) > 0 {
		fmt.Fprintf(w, "\nSTALE ALLOWLIST ENTRIES (%d):\n", len(sum.StaleAllowlist))
		fmt.Fprintln(w, "  These are listed as expected skips but did not skip. Either the test")
		fmt.Fprintln(w, "  now runs (delete the entry — good news) or it no longer exists.")
		for _, s := range sum.StaleAllowlist {
			fmt.Fprintf(w, "  ? %s %s (line %d)\n", s.Package, s.Test, s.Line)
		}
	}

	fmt.Fprintln(w)
	if sum.OK {
		fmt.Fprintf(w, "✓ GATE PASSED — %d tests ran, %d skips all accounted for\n",
			sum.Tests.Total, sum.Tests.Skipped)
	} else {
		fmt.Fprintln(w, "✗ GATE FAILED")
		for _, v := range sum.Violations {
			fmt.Fprintf(w, "    · %s\n", v)
		}
	}
	fmt.Fprintln(w)
}
