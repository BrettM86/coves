package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRun_RefusesBeforeOpeningOrFailsOnOpenError: invalid flag combinations
// and a missing trusted-host list are refused with a non-zero exit before
// anything is opened; an open failure after valid settings exits non-zero.
func TestRun_RefusesBeforeOpeningOrFailsOnOpenError(t *testing.T) {
	t.Parallel()
	const hostsVariable = "TRUSTED_BRIDGE_PDS_HOSTS"
	hostsSet := map[string]string{hostsVariable: "https://bridge.test"}

	cases := []struct {
		name           string
		args           []string
		env            map[string]string
		openErr        error
		wantOpenCalled bool
		wantStderr     []string
	}{
		{
			name:       "dry run with cache clear",
			args:       []string{"-dry-run", "-clear-identity-cache"},
			env:        hostsSet,
			wantStderr: []string{"-dry-run", "-clear-identity-cache"},
		},
		{
			name:       "no trusted hosts configured",
			args:       nil,
			env:        map[string]string{},
			wantStderr: []string{hostsVariable},
		},
		{
			name:       "trusted-hosts flag with only empty entries",
			args:       []string{"-trusted-hosts", " , "},
			env:        map[string]string{},
			wantStderr: []string{hostsVariable},
		},
		{
			name:           "open fails",
			args:           nil,
			env:            hostsSet,
			openErr:        errors.New("open database: connection refused"),
			wantOpenCalled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			openCalled := false
			open := func(context.Context, func(string) string, int) (sweepDependencies, func(), error) {
				openCalled = true
				return sweepDependencies{}, func() {}, tc.openErr
			}
			getenv := func(key string) string { return tc.env[key] }
			var stdout, stderr bytes.Buffer

			status := run(context.Background(), tc.args, getenv, &stdout, &stderr, open)

			assert.NotZero(t, status, "exit status")
			assert.Equal(t, tc.wantOpenCalled, openCalled, "open called")
			for _, want := range tc.wantStderr {
				assert.Contains(t, stderr.String(), want, "stderr")
			}
		})
	}
}
