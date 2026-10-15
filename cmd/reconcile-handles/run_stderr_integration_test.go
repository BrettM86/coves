//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_PrintsPLCURLAndEachFailedRowsCause: in a normal run and in a dry
// run, run prints the PLC directory the resolvers use once on stderr, and
// exactly one stderr cause line per failed row, carrying the row's error.
func TestRun_PrintsPLCURLAndEachFailedRowsCause(t *testing.T) {
	t.Parallel()
	const plcURL = "http://plc.reconcile.test"
	cases := []struct {
		name string
		args []string
	}{
		{name: "normal run", args: []string{"-trusted-hosts", bridgeHost}},
		{name: "dry run", args: []string{"-dry-run", "-trusted-hosts", bridgeHost}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			failingDID := uniqueDID(t, "failing")
			seedUser(t, db, failingDID, "carl.bridge.test", bridgeHost)
			uncached := newScriptedIdentityResolver(map[string]scriptedAnswer{
				failingDID: {err: errors.New("scripted resolution failure: directory unreachable")},
			})
			dependencies := newSweepDependencies(t, db, uncached)
			dependencies.PLCURL = plcURL
			open := func(context.Context, func(string) string, int) (sweepDependencies, func(), error) {
				return dependencies, func() {}, nil
			}
			getenv := func(string) string { return "" }
			var stdout, stderr bytes.Buffer

			status := run(context.Background(), tc.args, getenv, &stdout, &stderr, open)

			assert.Equal(t, 1, strings.Count(stderr.String(), "plc_url="+plcURL+"\n"), "plc_url lines; stderr:\n%s", stderr.String())
			var causeLines []string
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.Contains(line, failingDID) {
					causeLines = append(causeLines, line)
				}
			}
			require.Len(t, causeLines, 1, "stderr lines naming the failed row; stderr:\n%s", stderr.String())
			assert.True(t, strings.HasPrefix(causeLines[0],
				"reconcile-handles: cause kind=user did="+failingDID+" reason=resolve-error: "), "cause line: %s", causeLines[0])
			assert.Contains(t, causeLines[0], "scripted resolution failure: directory unreachable", "cause line")
			assert.NotZero(t, status, "exit status")
		})
	}
}
