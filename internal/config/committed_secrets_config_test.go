package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// committedEnvValues parses simple KEY=VALUE lines from a committed env file
// at the repository root. Comments and blank lines are ignored; one pair of
// surrounding quotes is stripped from values, and an unquoted value loses a
// trailing " #..." inline comment.
func committedEnvValues(t *testing.T, name string) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("open committed env file %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close committed env file %s: %v", name, err)
		}
	})

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export "))
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' || first == '\'') && first == last {
				value = value[1 : len(value)-1]
			} else if i := strings.IndexAny(value, " \t"); i >= 0 {
				if rest := strings.TrimLeft(value[i:], " \t"); strings.HasPrefix(rest, "#") {
					value = value[:i]
				}
			}
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read committed env file %s: %v", name, err)
	}
	return values
}

// TestLoad_ProductionRejectsCommittedSecrets pins that production refuses
// secrets whose values are committed publicly in the repository, and
// Cloudflare's published Turnstile test secrets, while dev keeps accepting
// them and production keeps accepting real (or absent) values.
func TestLoad_ProductionRejectsCommittedSecrets(t *testing.T) {
	type row struct {
		name     string
		dev      bool
		variable string
		value    string
		wantErr  bool
		// wantMessage must appear in the error of a wantErr row.
		wantMessage string
	}

	var rows []row

	// Row group 1: Cloudflare's published Turnstile test secrets in production.
	turnstileTestSecrets := []struct{ label, value string }{
		{"always-passes test secret", "1x0000000000000000000000000000000AA"},
		{"always-fails test secret", "2x0000000000000000000000000000000AA"},
		{"token-already-spent test secret", "3x0000000000000000000000000000000AA"},
	}
	for _, s := range turnstileTestSecrets {
		rows = append(rows, row{
			name:     "production TURNSTILE_SECRET_KEY Cloudflare " + s.label,
			variable: "TURNSTILE_SECRET_KEY",
			value:    s.value,
			wantErr:  true,

			wantMessage: "Cloudflare's published test secrets",
		})
	}

	// Row group 2: secrets committed in the repository's env files.
	committedFiles := []struct {
		file     string
		required []string
	}{
		{".env.dev", []string{"ENCRYPTION_KEY", "OAUTH_SEAL_SECRET"}},
		{".env.ci", []string{"ENCRYPTION_KEY", "OAUTH_SEAL_SECRET"}},
		{".env.dev.example", []string{"ENCRYPTION_KEY"}},
	}
	committed := map[string]map[string]string{}
	for _, cf := range committedFiles {
		values := committedEnvValues(t, cf.file)
		committed[cf.file] = values
		for _, variable := range []string{"ENCRYPTION_KEY", "OAUTH_SEAL_SECRET"} {
			value, ok := values[variable]
			if !ok || value == "" {
				for _, required := range cf.required {
					if required == variable {
						t.Fatalf("committed env file %s no longer defines %s; update this test's source list", cf.file, variable)
					}
				}
				continue
			}
			rows = append(rows, row{
				name:     "production " + variable + " from " + cf.file,
				variable: variable,
				value:    value,
				wantErr:  true,

				wantMessage: "published in the repository",
			})
		}
	}

	// Row group 2b: base64.StdEncoding ignores embedded newlines, so a
	// committed value split across lines decodes to the same committed key
	// and must still be refused.
	for _, variable := range []string{"ENCRYPTION_KEY", "OAUTH_SEAL_SECRET"} {
		value := committed[".env.dev"][variable]
		split := value[:len(value)/2] + "\n" + value[len(value)/2:]
		rows = append(rows, row{
			name:     "production " + variable + " from .env.dev with an embedded newline",
			variable: variable,
			value:    split,
			wantErr:  true,

			wantMessage: "published in the repository",
		})
	}

	// Row group 3: dev keeps accepting the same committed values.
	rows = append(rows,
		row{
			name:     "dev TURNSTILE_SECRET_KEY Cloudflare always-passes test secret",
			dev:      true,
			variable: "TURNSTILE_SECRET_KEY",
			value:    turnstileTestSecrets[0].value,
		},
		row{
			name:     "dev ENCRYPTION_KEY from .env.dev",
			dev:      true,
			variable: "ENCRYPTION_KEY",
			value:    committed[".env.dev"]["ENCRYPTION_KEY"],
		},
		row{
			name:     "dev OAUTH_SEAL_SECRET from .env.dev",
			dev:      true,
			variable: "OAUTH_SEAL_SECRET",
			value:    committed[".env.dev"]["OAUTH_SEAL_SECRET"],
		},
	)

	// Row group 4: production keeps accepting a real-shaped or absent secret.
	rows = append(rows,
		row{
			name:     "production TURNSTILE_SECRET_KEY real-shaped value",
			variable: "TURNSTILE_SECRET_KEY",
			value:    "0x4AAAAAAAk7Qz3bN9vR2mX5pL8wT1yH6c",
		},
		row{
			name:     "production TURNSTILE_SECRET_KEY empty",
			variable: "TURNSTILE_SECRET_KEY",
			value:    "",
		},
	)

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			if tc.dev {
				t.Setenv("IS_DEV_ENV", "true")
			} else {
				prodEnv(t)
			}
			t.Setenv(tc.variable, tc.value)

			_, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() accepted the committed %s value; want an error naming %s", tc.variable, tc.variable)
				}
				if !strings.Contains(err.Error(), tc.variable) {
					t.Fatalf("Load() error does not name %s", tc.variable)
				}
				if !strings.Contains(err.Error(), tc.wantMessage) {
					t.Fatalf("Load() error for %s does not say %q", tc.variable, tc.wantMessage)
				}
				if tc.value != "" && strings.Contains(err.Error(), tc.value) {
					t.Fatalf("Load() error echoes the rejected %s value", tc.variable)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() returned an error for %s: %v", tc.variable, err)
			}
		})
	}
}
