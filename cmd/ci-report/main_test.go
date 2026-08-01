package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gotestJSON builds a -json stream from terse specs so each test reads as the
// scenario it covers rather than as a wall of JSON.
//
// Each spec is "pkg|test|action" for a test event, "pkg||action" for a
// package-level event, or "buildfail|importpath" for a build failure. An
// optional fourth field is an output line attached to the test.
func gotestJSON(t *testing.T, specs ...string) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, spec := range specs {
		parts := strings.Split(spec, "|")
		if parts[0] == "buildfail" {
			if err := enc.Encode(event{Action: "build-fail", ImportPath: parts[1]}); err != nil {
				t.Fatalf("encoding build-fail event: %v", err)
			}
			continue
		}
		pkg, test, action := parts[0], parts[1], parts[2]
		if len(parts) > 3 && parts[3] != "" {
			if err := enc.Encode(event{Action: "output", Package: pkg, Test: test, Output: parts[3] + "\n"}); err != nil {
				t.Fatalf("encoding output event: %v", err)
			}
		}
		if action != "" {
			if err := enc.Encode(event{Action: action, Package: pkg, Test: test}); err != nil {
				t.Fatalf("encoding %s event: %v", action, err)
			}
		}
	}
	return b.String()
}

// writeAllowlist writes an allowlist to a temp file and returns its path.
func writeAllowlist(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "allowed_skips.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing allowlist: %v", err)
	}
	return path
}

func TestGateOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		stream    []string
		allowlist string
		wantCode  int
		// wantViolation is a substring expected in the violation list; empty
		// means the gate must pass with none.
		wantViolation string
	}{
		{
			name:      "all passing is a pass",
			stream:    []string{"pkg/a|TestOne|pass", "pkg/a|TestTwo|pass", "pkg/a||pass"},
			allowlist: "# nothing tolerated\n",
			wantCode:  0,
		},
		{
			name: "an unapproved skip fails the gate",
			stream: []string{
				"pkg/a|TestOne|pass",
				// A verbatim skip message from the era this gate exists to end.
				// It is input text for the parser, not an address: ci-report
				// never dials anything, and blunting the sample would make the
				// fixture less like the output it has to survive.
				"pkg/a|TestPDS|skip|    blob_test.go:49: PDS not running at http://localhost:3001", // coves:allow-host-literal: quoted skip message parsed as text; ci-report opens no connections
			},
			allowlist:     "# nothing tolerated\n",
			wantCode:      1,
			wantViolation: "skipped without an allowlist entry",
		},
		{
			name: "an allowlisted skip is tolerated",
			stream: []string{
				"pkg/a|TestOne|pass",
				"pkg/a|TestRealHandles|skip|    identity_test.go:363: set TEST_REAL_HANDLES=1",
			},
			allowlist: "pkg/a TestRealHandles  # opt-in, reaches the public internet\n",
			wantCode:  0,
		},
		{
			name: "a wildcard entry covers subtests",
			stream: []string{
				"pkg/a|TestOne|pass",
				"pkg/a|TestNet|skip|opt-in",
				"pkg/a|TestNet/resolves_handle|skip|opt-in",
				"pkg/a|TestNet/resolves_did|skip|opt-in",
			},
			allowlist: "pkg/a TestNet/*  # opt-in, reaches the public internet\n",
			wantCode:  0,
		},
		{
			name:          "a wildcard entry does not cover a different test with the same prefix",
			stream:        []string{"pkg/a|TestOne|pass", "pkg/a|TestNetworking|skip|why"},
			allowlist:     "pkg/a TestNet/*  # opt-in\n",
			wantCode:      1,
			wantViolation: "skipped without an allowlist entry",
		},
		{
			name:          "a stale allowlist entry fails the gate",
			stream:        []string{"pkg/a|TestOne|pass"},
			allowlist:     "pkg/a TestGone  # was opt-in\n",
			wantCode:      1,
			wantViolation: "stale allowlist",
		},
		{
			name:          "a failing test fails the gate",
			stream:        []string{"pkg/a|TestOne|fail|    a_test.go:10: want 1 got 2"},
			allowlist:     "",
			wantCode:      1,
			wantViolation: "test(s) failed",
		},
		{
			name:          "a build failure fails the gate even with no test events",
			stream:        []string{"buildfail|Coves/internal/broken"},
			allowlist:     "",
			wantCode:      1,
			wantViolation: "failed to build",
		},
		{
			name:          "an empty run is not a pass",
			stream:        []string{},
			allowlist:     "",
			wantCode:      1,
			wantViolation: "no tests ran",
		},
		{
			name: "a test with no terminal result counts as a failure",
			// The shape of a test binary dying mid-run: output arrives, then
			// nothing. Reporting this as a pass is the worst thing the gate
			// could do, so it is pinned here.
			stream:        []string{"pkg/a|TestOne|pass", "pkg/a|TestPanics||    panic: nil map write"},
			allowlist:     "",
			wantCode:      1,
			wantViolation: "test(s) failed",
		},
		{
			name: "a package-level skip is not a test skip",
			// "no test files" arrives as a package-level skip with no Test
			// field. Counting it would make the gate demand an allowlist entry
			// for every package that has no tests.
			stream:    []string{"pkg/a|TestOne|pass", "pkg/b||skip"},
			allowlist: "",
			wantCode:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stream := gotestJSON(t, tc.stream...)
			allowPath := writeAllowlist(t, tc.allowlist)
			summaryPath := filepath.Join(t.TempDir(), "summary.json")

			var out strings.Builder
			code, err := run(strings.NewReader(stream), &out, allowPath, summaryPath, false)
			if err != nil {
				t.Fatalf("run returned an error: %v", err)
			}
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\n--- report ---\n%s", code, tc.wantCode, out.String())
			}

			data, err := os.ReadFile(summaryPath)
			if err != nil {
				t.Fatalf("reading summary: %v", err)
			}
			var sum summary
			if err := json.Unmarshal(data, &sum); err != nil {
				t.Fatalf("summary is not valid JSON: %v", err)
			}
			if sum.OK != (tc.wantCode == 0) {
				t.Errorf("summary.ok = %v, want %v", sum.OK, tc.wantCode == 0)
			}

			joined := strings.Join(sum.Violations, "; ")
			if tc.wantViolation == "" {
				if len(sum.Violations) != 0 {
					t.Errorf("expected no violations, got: %s", joined)
				}
			} else if !strings.Contains(joined, tc.wantViolation) {
				t.Errorf("violations %q do not mention %q", joined, tc.wantViolation)
			}
		})
	}
}

func TestConditionalEntries(t *testing.T) {
	// A "~" entry covers a test whose skip depends on something outside the
	// stack — a third-party URL being reachable. It must be tolerated both when
	// the test skips and when it runs, or the gate flakes on someone else's
	// outage.
	t.Run("tolerated when the test skips", func(t *testing.T) {
		stream := gotestJSON(t, "pkg/a|TestOne|pass", "pkg/a|TestUnfurl|skip|network unreachable")
		allowPath := writeAllowlist(t, "~pkg/a TestUnfurl  # reaches a third-party URL\n")
		code, err := run(strings.NewReader(stream), &strings.Builder{}, allowPath, "", false)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	})

	t.Run("tolerated when the test runs", func(t *testing.T) {
		stream := gotestJSON(t, "pkg/a|TestOne|pass", "pkg/a|TestUnfurl|pass")
		allowPath := writeAllowlist(t, "~pkg/a TestUnfurl  # reaches a third-party URL\n")
		var out strings.Builder
		code, err := run(strings.NewReader(stream), &out, allowPath, "", false)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (a conditional entry is never stale)\n%s", code, out.String())
		}
		if strings.Contains(out.String(), "STALE ALLOWLIST") {
			t.Error("a conditional entry must not be reported as stale")
		}
	})

	t.Run("a non-conditional entry is still held to staleness", func(t *testing.T) {
		// The contrast that makes the marker meaningful: without "~", a test
		// that stops skipping must fail the gate so the entry gets removed.
		stream := gotestJSON(t, "pkg/a|TestOne|pass", "pkg/a|TestStructural|pass")
		allowPath := writeAllowlist(t, "pkg/a TestStructural  # defs-only lexicon file\n")
		code, err := run(strings.NewReader(stream), &strings.Builder{}, allowPath, "", false)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
	})
}

func TestStaleAllowlistCanBeDowngraded(t *testing.T) {
	// -allow-stale exists for the transitional case: regenerating the allowlist
	// after moving tests between tiers. It must report the staleness either way.
	stream := gotestJSON(t, "pkg/a|TestOne|pass")
	allowPath := writeAllowlist(t, "pkg/a TestGone  # was opt-in\n")
	summaryPath := filepath.Join(t.TempDir(), "summary.json")

	var out strings.Builder
	code, err := run(strings.NewReader(stream), &out, allowPath, summaryPath, true)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 with allowStale", code)
	}
	if !strings.Contains(out.String(), "STALE ALLOWLIST") {
		t.Error("stale entries must still be reported even when downgraded")
	}
}

func TestAllowlistRequiresReason(t *testing.T) {
	// An allowlist whose entries carry no justification is a list nobody can
	// review, which defeats the point of having one.
	allowPath := writeAllowlist(t, "pkg/a TestNoReason\n")
	_, err := run(strings.NewReader(""), &strings.Builder{}, allowPath, "", false)
	if err == nil {
		t.Fatal("expected an error for an entry with no reason")
	}
	if !strings.Contains(err.Error(), "no reason") {
		t.Errorf("error %q should explain that a reason is required", err)
	}
}

func TestAllowlistMalformedEntry(t *testing.T) {
	allowPath := writeAllowlist(t, "just-one-field  # reason\n")
	_, err := run(strings.NewReader(""), &strings.Builder{}, allowPath, "", false)
	if err == nil {
		t.Fatal("expected an error for a malformed entry")
	}
	if !strings.Contains(err.Error(), "import-path") {
		t.Errorf("error %q should show the expected format", err)
	}
}

func TestMissingAllowlistIsAnError(t *testing.T) {
	// Silently treating an absent allowlist as "tolerate nothing" would be
	// defensible, but a missing file more often means a broken invocation, and
	// the gate must not grade itself when it is misconfigured.
	_, err := run(strings.NewReader(""), &strings.Builder{},
		filepath.Join(t.TempDir(), "absent.txt"), "", false)
	if err == nil {
		t.Fatal("expected an error for a missing allowlist")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q should say the file is missing", err)
	}
}

func TestSkipReasonExtraction(t *testing.T) {
	tests := []struct {
		name   string
		output []string
		want   string
	}{
		{
			name:   "picks the message line, not the SKIP marker",
			output: []string{"=== RUN   TestX", "    x_test.go:49: PDS not running", "--- SKIP: TestX (0.00s)"},
			want:   "x_test.go:49: PDS not running",
		},
		{
			name:   "reports absence rather than an empty string",
			output: []string{"=== RUN   TestX", "--- SKIP: TestX (0.00s)"},
			want:   "(no reason reported)",
		},
		{
			name:   "handles no output at all",
			output: nil,
			want:   "(no reason reported)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipReason(tc.output); got != tc.want {
				t.Errorf("skipReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPassingTestOutputIsReleased(t *testing.T) {
	// Retaining output for passing tests would make memory scale with suite
	// size rather than with the number of problems.
	stream := gotestJSON(t, "pkg/a|TestOne|pass|some chatty output")
	results, _, _, err := parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, ok := results[testKey{Pkg: "pkg/a", Test: "TestOne"}]
	if !ok {
		t.Fatal("expected a result for TestOne")
	}
	if len(res.output) != 0 {
		t.Errorf("passing test retained %d output lines, want 0", len(res.output))
	}
}

func TestOutputIsBounded(t *testing.T) {
	// A failing test that printed thousands of lines must not be able to hold
	// all of them in memory.
	specs := []string{}
	for i := 0; i < retainedOutputLines*3; i++ {
		specs = append(specs, "pkg/a|TestNoisy||line")
	}
	specs = append(specs, "pkg/a|TestNoisy|fail")
	results, _, _, err := parse(strings.NewReader(gotestJSON(t, specs...)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := results[testKey{Pkg: "pkg/a", Test: "TestNoisy"}]
	if len(res.output) > retainedOutputLines {
		t.Errorf("retained %d lines, want at most %d", len(res.output), retainedOutputLines)
	}
}

func TestNonJSONLinesAreTolerated(t *testing.T) {
	// The toolchain and a panicking test process both write unwrapped text to
	// stdout. That must not derail parsing of the surrounding events.
	stream := "warning: something from the toolchain\n" +
		gotestJSON(t, "pkg/a|TestOne|pass") +
		"panic: unrelated noise\n"
	results, _, _, err := parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results, want 1", len(results))
	}
}
