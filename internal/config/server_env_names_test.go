package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_ServerIdentityEnvCharacterization pins how Load maps the server
// identity environment variables onto the config today.
func TestLoad_ServerIdentityEnvCharacterization(t *testing.T) {
	tests := []struct {
		name           string
		env            map[string]string
		wantDID        string
		wantDomain     string
		wantHandle     string
		wantPassword   string
		wantAtprotoKey string
	}{
		{
			name:       "all blank uses the coves.social defaults",
			env:        map[string]string{},
			wantDID:    "did:web:coves.social",
			wantDomain: "coves.social",
		},
		{
			name:         "PDS_INSTANCE_HANDLE and PDS_INSTANCE_PASSWORD",
			env:          map[string]string{"PDS_INSTANCE_HANDLE": "instance.example.social", "PDS_INSTANCE_PASSWORD": "instance-password-value"},
			wantDID:      "did:web:coves.social",
			wantDomain:   "coves.social",
			wantHandle:   "instance.example.social",
			wantPassword: "instance-password-value",
		},
		{
			name:       "INSTANCE_DID did:plc with INSTANCE_DOMAIN",
			env:        map[string]string{"INSTANCE_DID": "did:plc:abc123", "INSTANCE_DOMAIN": "example.social"},
			wantDID:    "did:plc:abc123",
			wantDomain: "example.social",
		},
		{
			name:       "INSTANCE_DID did:web derives the domain",
			env:        map[string]string{"INSTANCE_DID": "did:web:example.social"},
			wantDID:    "did:web:example.social",
			wantDomain: "example.social",
		},
		{
			name:           "INSTANCE_ATPROTO_PUBLIC_KEY",
			env:            map[string]string{"INSTANCE_ATPROTO_PUBLIC_KEY": instanceAtprotoPublicMultibase},
			wantDID:        "did:web:coves.social",
			wantDomain:     "coves.social",
			wantAtprotoKey: instanceAtprotoPublicMultibase,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ClearEnvForTest(t)
			prodEnv(t)
			// prodEnv pins SERVER_DID; blank it so each row starts from the default.
			t.Setenv("SERVER_DID", "")
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, tc.wantDID, cfg.Instance.DID)
			assert.Equal(t, tc.wantDomain, cfg.Instance.Domain)
			assert.Equal(t, tc.wantHandle, cfg.PDS.ServerHandle)
			assert.Equal(t, tc.wantPassword, cfg.PDS.ServerRepoPassword)
			assert.Equal(t, tc.wantAtprotoKey, cfg.Instance.AtprotoPublicKey)
		})
	}
}

// serverEnvNamePair is one renamed server identity variable: the new name, the
// deprecated old name it falls back to, and the config field both land in.
type serverEnvNamePair struct {
	newName string
	oldName string
	// field reads the resolved value from the loaded config.
	field func(cfg *Config) string
	// base is extra environment every row of this pair needs (did:plc for the
	// domain pair, since did:web derives the domain and ignores the variable).
	base map[string]string
	// blankBase replaces base for the both-blank row, and wantBlank is the
	// field value Load produces then.
	blankBase map[string]string
	wantBlank string
	// newValue and oldValue are two distinct valid values.
	newValue string
	oldValue string
}

func serverEnvNamePairs() []serverEnvNamePair {
	return []serverEnvNamePair{
		{
			newName:  "SERVER_HANDLE",
			oldName:  "PDS_INSTANCE_HANDLE",
			field:    func(cfg *Config) string { return cfg.PDS.ServerHandle },
			newValue: "newname-handle.example.social",
			oldValue: "oldname-handle.example.social",
		},
		{
			newName:  "SERVER_REPO_PASSWORD",
			oldName:  "PDS_INSTANCE_PASSWORD",
			field:    func(cfg *Config) string { return cfg.PDS.ServerRepoPassword },
			newValue: "newname-repo-password-value",
			oldValue: "oldname-repo-password-value",
		},
		{
			newName:   "SERVER_DID",
			oldName:   "INSTANCE_DID",
			field:     func(cfg *Config) string { return cfg.Instance.DID },
			wantBlank: "did:web:coves.social",
			newValue:  "did:web:newname.example.social",
			oldValue:  "did:web:oldname.example.social",
		},
		{
			newName:   "SERVER_DOMAIN",
			oldName:   "INSTANCE_DOMAIN",
			field:     func(cfg *Config) string { return cfg.Instance.Domain },
			base:      map[string]string{"SERVER_DID": "did:plc:abc123"},
			blankBase: map[string]string{},
			wantBlank: "coves.social",
			newValue:  "newname.example.social",
			oldValue:  "oldname.example.social",
		},
		{
			newName:  "SERVER_ATPROTO_PUBLIC_KEY",
			oldName:  "INSTANCE_ATPROTO_PUBLIC_KEY",
			field:    func(cfg *Config) string { return cfg.Instance.AtprotoPublicKey },
			newValue: instanceAtprotoPublicMultibase,
			oldValue: labelSigningKeyPublicMultibase,
		},
	}
}

// captureSlogJSON routes slog.Default through a JSON handler over a buffer for
// the rest of the test. Tests using t.Setenv are never parallel, so swapping
// the process-wide default is safe.
func captureSlogJSON(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// warnRecordsMentioning returns the raw JSON line of every WARN record whose
// message or attributes contain name.
func warnRecordsMentioning(t *testing.T, buf *bytes.Buffer, name string) []string {
	t.Helper()
	var matches []string
	scanner := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), "log line is not a JSON record: %s", line)
		if record["level"] != "WARN" {
			continue
		}
		if strings.Contains(line, name) {
			matches = append(matches, line)
		}
	}
	require.NoError(t, scanner.Err())
	return matches
}

// TestLoad_ServerEnvNamesWithDeprecatedFallback pins the renamed server
// identity variables: the new name wins, the old name still works but logs
// one deprecation warning naming both variables and never the value, and a
// blank value counts as unset. Both names set to the same value logs no
// warning: production sets both spellings to identical values during the
// transition so an older image still boots, and a warning on every boot would
// bury the actionable ones.
func TestLoad_ServerEnvNamesWithDeprecatedFallback(t *testing.T) {
	type caseKind int
	const (
		newOnly caseKind = iota
		oldOnly
		bothDifferent
		bothEqual
		bothBlank
		whitespaceNewWithOld
	)
	cases := []struct {
		name string
		kind caseKind
	}{
		{"B1 new only", newOnly},
		{"B2 old only", oldOnly},
		{"B3 both set and different", bothDifferent},
		{"B4 both set and equal", bothEqual},
		{"B5 both blank", bothBlank},
		{"B6 whitespace new with old set", whitespaceNewWithOld},
	}
	for _, pair := range serverEnvNamePairs() {
		for _, tc := range cases {
			t.Run(pair.newName+"/"+tc.name, func(t *testing.T) {
				ClearEnvForTest(t)
				prodEnv(t)
				// prodEnv pins SERVER_DID; blank both DID names so each row
				// starts from the default.
				t.Setenv("INSTANCE_DID", "")
				t.Setenv("SERVER_DID", "")
				base := pair.base
				if tc.kind == bothBlank && pair.blankBase != nil {
					base = pair.blankBase
				}
				for key, value := range base {
					t.Setenv(key, value)
				}

				var want string
				var values []string
				switch tc.kind {
				case newOnly:
					t.Setenv(pair.newName, pair.newValue)
					want = pair.newValue
				case oldOnly:
					t.Setenv(pair.oldName, pair.oldValue)
					want, values = pair.oldValue, []string{pair.oldValue}
				case bothDifferent:
					t.Setenv(pair.newName, pair.newValue)
					t.Setenv(pair.oldName, pair.oldValue)
					want, values = pair.newValue, []string{pair.newValue, pair.oldValue}
				case bothEqual:
					t.Setenv(pair.newName, pair.newValue)
					t.Setenv(pair.oldName, pair.newValue)
					want = pair.newValue
				case bothBlank:
					t.Setenv(pair.newName, "")
					t.Setenv(pair.oldName, "")
					want = pair.wantBlank
				case whitespaceNewWithOld:
					t.Setenv(pair.newName, "   ")
					t.Setenv(pair.oldName, pair.oldValue)
					want, values = pair.oldValue, []string{pair.oldValue}
				}

				logs := captureSlogJSON(t)
				cfg, err := Load()
				require.NoError(t, err)
				assert.Equal(t, want, pair.field(cfg), "resolved value")

				oldWarnings := warnRecordsMentioning(t, logs, pair.oldName)
				switch tc.kind {
				case newOnly:
					assert.Empty(t, oldWarnings, "no warning may mention %s when only %s is set", pair.oldName, pair.newName)
				case bothBlank:
					assert.Empty(t, oldWarnings, "no warning may mention %s when both are blank", pair.oldName)
					assert.Empty(t, warnRecordsMentioning(t, logs, pair.newName),
						"no warning may mention %s when both are blank", pair.newName)
				case bothEqual:
					assert.Empty(t, oldWarnings, "no warning may mention %s when both names hold the same value; log:\n%s", pair.oldName, logs.String())
					assert.Empty(t, warnRecordsMentioning(t, logs, pair.newName),
						"no warning may mention %s when both names hold the same value", pair.newName)
				default:
					require.Len(t, oldWarnings, 1, "want exactly one deprecation warning mentioning %s; log:\n%s", pair.oldName, logs.String())
					warning := oldWarnings[0]
					assert.Contains(t, warning, pair.newName, "the deprecation warning must name the new variable")
					for _, value := range values {
						assert.NotContains(t, warning, value, "the deprecation warning must not contain the configured value")
					}
				}
			})
		}
	}
}

// TestClearEnvForTestClearsServerEnvNames pins that the renamed server
// identity variables and their deprecated names are cleared, so a developer's shell cannot leak them into
// a test that calls Load.
func TestClearEnvForTestClearsServerEnvNames(t *testing.T) {
	names := []string{
		"SERVER_HANDLE", "SERVER_REPO_PASSWORD", "SERVER_DID", "SERVER_DOMAIN", "SERVER_ATPROTO_PUBLIC_KEY",
		"PDS_INSTANCE_HANDLE", "PDS_INSTANCE_PASSWORD", "INSTANCE_DID", "INSTANCE_DOMAIN", "INSTANCE_ATPROTO_PUBLIC_KEY",
	}
	for _, name := range names {
		t.Setenv(name, "set")
	}
	ClearEnvForTest(t)
	for _, name := range names {
		assert.Empty(t, os.Getenv(name), "%s must be cleared by ClearEnvForTest", name)
	}
}
