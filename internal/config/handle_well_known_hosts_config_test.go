// coves:allow-host-literal-file: this file tests the HANDLE_WELL_KNOWN_HOSTS parser, and every host:port here is a parser INPUT or an expected parse result — the documented value IS localhost:3001/3011 in .env.ci — matched as a string and never dialled.
package config

import (
	"strings"
	"testing"
)

// HANDLE_WELL_KNOWN_HOSTS: where handle verification is allowed to look when
// DNS cannot answer.
//
// atProto verifies a handle by resolving it back to the DID that declares it —
// a DNS TXT lookup, or a GET https://<handle>/.well-known/atproto-did. The
// hermetic CI stack has neither a DNS server nor a certificate authority, so
// that leg can never complete there and every identity resolves to the reserved
// handle.invalid placeholder. This variable names the hosts that will answer for
// a given handle suffix, turning verification back into something an in-network
// stack can do. A PDS serves that endpoint for every handle it hosts, keyed on
// the Host header, so the answer is authoritative rather than assumed.
//
// # WHY THE LEADING DOT IS MANDATORY
//
// The suffix is matched against a hostname, and `pds2.test` as a suffix also
// matches `evilpds2.test` — a different domain, owned by somebody else, whose
// handle verification would be redirected to a host we chose. Requiring
// `.pds2.test` makes the match land on a label boundary. It is the same
// dot-boundary rule admitCommunityOrigin enforces for origins, and it is
// refused at load rather than silently normalised because a config value that
// quietly means something other than what was typed is worse than one that
// stops the boot.
//
// # WHY IT IS AN ERROR OUTSIDE DEV RATHER THAN A WARNING
//
// TURNSTILE_SITEVERIFY_URL, the closest precedent in this file, is ignored with
// a warning outside dev — and that is right for it, because ignoring it falls
// back to Cloudflare, which is the safe answer. There is no safe fallback here.
// identity.NewResolver PANICS when it is handed well-known hosts without the
// private-address hatch, and cmd/server only passes that hatch when IsDevEnv, so
// the combination this variable creates in production is a panic during resolver
// construction — deep in wiring, with a stack trace instead of an explanation.
// Refusing at load turns that into one readable line naming the variable.
//
// A warning would be worse than either. The operator would be left with a
// resolver that verifies no handles in the namespace they thought they had
// configured, and nothing but a log line from boot to connect the two.
func TestHandleWellKnownHostsConfig(t *testing.T) {
	t.Run("unset means no redirection at all", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned unexpected error: %v", err)
		}

		// Production's shape, and it must stay the shape a caller gets by
		// writing nothing: identity.PrivateHostOptions(false) returns no
		// options at all, and this must likewise contribute nothing.
		if len(cfg.Identity.WellKnownHosts) != 0 {
			t.Errorf("Identity.WellKnownHosts = %v, want empty when HANDLE_WELL_KNOWN_HOSTS is unset",
				cfg.Identity.WellKnownHosts)
		}
	})

	t.Run("parses the documented multi-entry form", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		// Spacing around the entries and around the separator is what a
		// human editing a .env file leaves behind, and a map keyed on
		// " .pds2.test" matches no handle ever.
		t.Setenv("HANDLE_WELL_KNOWN_HOSTS", " .local.coves.dev=localhost:3001 , .pds2.test=localhost:3011 ")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned unexpected error: %v", err)
		}

		want := map[string]string{
			".local.coves.dev": "localhost:3001",
			".pds2.test":       "localhost:3011",
		}
		if len(cfg.Identity.WellKnownHosts) != len(want) {
			t.Fatalf("Identity.WellKnownHosts = %v, want %v", cfg.Identity.WellKnownHosts, want)
		}
		for suffix, host := range want {
			if got := cfg.Identity.WellKnownHosts[suffix]; got != host {
				t.Errorf("Identity.WellKnownHosts[%q] = %q, want %q", suffix, got, host)
			}
		}
	})

	t.Run("rejects a suffix that does not start with a dot", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("HANDLE_WELL_KNOWN_HOSTS", "pds2.test=localhost:3011")

		_, err := Load()
		if err == nil {
			t.Fatal("Load() accepted a suffix with no leading dot. `pds2.test` also matches " +
				"`evilpds2.test`, so handle verification for a domain we do not own would be " +
				"redirected to a host we chose")
		}
		if !strings.Contains(err.Error(), "HANDLE_WELL_KNOWN_HOSTS") {
			t.Errorf("Load() error = %q, want it to name HANDLE_WELL_KNOWN_HOSTS", err.Error())
		}
	})

	t.Run("rejects a malformed entry", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			value string
			why   string
		}{
			{
				name:  "no separator",
				value: ".pds2.test",
				why:   "a suffix with nowhere to send the request configures nothing",
			},
			{
				name:  "empty host",
				value: ".pds2.test=",
				why:   "an empty host builds the URL http:///.well-known/atproto-did, which fails at every resolution",
			},
			{
				name:  "empty suffix",
				value: "=localhost:3011",
				why:   "an empty suffix is a suffix of every hostname, so this redirects ALL handle verification",
			},
			{
				name:  "one good entry and one malformed",
				value: ".pds2.test=localhost:3011,.local.coves.dev",
				why: "a partial parse is the dangerous outcome: the operator sees the stack half-work " +
					"and has no reason to suspect the second entry was dropped",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				clearEnv(t)
				t.Setenv("IS_DEV_ENV", "true")
				t.Setenv("HANDLE_WELL_KNOWN_HOSTS", tc.value)

				_, err := Load()
				if err == nil {
					t.Fatalf("Load() accepted HANDLE_WELL_KNOWN_HOSTS=%q: %s", tc.value, tc.why)
				}
				if !strings.Contains(err.Error(), "HANDLE_WELL_KNOWN_HOSTS") {
					t.Errorf("Load() error = %q, want it to name HANDLE_WELL_KNOWN_HOSTS", err.Error())
				}
			})
		}
	})

	t.Run("lowercases the suffix", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("HANDLE_WELL_KNOWN_HOSTS", ".PDS2.TEST=localhost:3011")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned unexpected error: %v", err)
		}

		// DNS names are case-insensitive, and every consumer of this map
		// compares against a hostname that has already been lowered — the
		// rewrite transport lowers the handle before matching, and indigo
		// normalises before checking SkipDNSDomainSuffixes. A key that kept its
		// case would therefore match nothing at all, and would do it silently:
		// handle verification for the namespace the operator configured would
		// simply carry on failing, with a correct-looking value in the env file.
		if got := cfg.Identity.WellKnownHosts[".pds2.test"]; got != "localhost:3011" {
			t.Errorf("Identity.WellKnownHosts = %v, want the suffix lowercased to %q",
				cfg.Identity.WellKnownHosts, ".pds2.test")
		}
	})

	t.Run("rejects a duplicate suffix", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("HANDLE_WELL_KNOWN_HOSTS", ".pds2.test=localhost:3011,.pds2.test=localhost:3012")

		_, err := Load()
		if err == nil {
			t.Fatal("Load() accepted the same suffix twice. Map assignment keeps whichever entry was " +
				"parsed last and drops the other without a word, so an operator who edited one line " +
				"and forgot the duplicate below it gets a stack that half-works")
		}
		if !strings.Contains(err.Error(), "HANDLE_WELL_KNOWN_HOSTS") {
			t.Errorf("Load() error = %q, want it to name HANDLE_WELL_KNOWN_HOSTS", err.Error())
		}
	})

	t.Run("rejects overlapping suffixes", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("HANDLE_WELL_KNOWN_HOSTS", ".test=localhost:3011,.pds2.test=localhost:3012")

		_, err := Load()
		if err == nil {
			t.Fatal("Load() accepted two suffixes where one contains the other. `alice.pds2.test` " +
				"matches both, so which host answers for it depends on a rule the operator cannot see " +
				"in the config — and the resolver has no way to report that it chose")
		}

		// Both suffixes must be named, because the whole difficulty is knowing
		// WHICH PAIR overlaps: an operator with six entries and a message
		// naming one of them has learned almost nothing.
		//
		// The quotes in these needles are load-bearing and are not decoration.
		// ".test" is a plain substring of ".pds2.test", so a bare
		// Contains(".test") would be satisfied by a message that mentioned only
		// the longer suffix and never noticed the shorter one. Matching the
		// %q-quoted form pins the shorter suffix appearing as a value in its own
		// right.
		for _, needle := range []string{`".test"`, `".pds2.test"`} {
			if !strings.Contains(err.Error(), needle) {
				t.Errorf("Load() error = %q, want it to name %s: an overlap is a relationship between "+
					"two entries, and a message naming one of them does not say which pair to fix",
					err.Error(), needle)
			}
		}
		if !strings.Contains(err.Error(), "HANDLE_WELL_KNOWN_HOSTS") {
			t.Errorf("Load() error = %q, want it to name HANDLE_WELL_KNOWN_HOSTS", err.Error())
		}
	})

	t.Run("rejects a host that is not a host:port", func(t *testing.T) {
		// The value is dialled, not fetched: the rewrite transport puts it in
		// URL.Host and supplies the scheme and path itself. So anything that
		// looks like a URL is a misunderstanding of the format that will fail
		// at every resolution, long after boot, as handles that will not verify.
		for _, tc := range []struct {
			name  string
			value string
			why   string
		}{
			{
				name:  "a scheme",
				value: ".pds2.test=http://localhost:3011",
				why:   "the transport supplies the scheme; this one would end up inside the authority",
			},
			{
				name:  "a path",
				value: ".pds2.test=localhost:3011/pds",
				why: "the transport supplies /.well-known/atproto-did. Worth its own case because " +
					"net.SplitHostPort ACCEPTS this — it splits on the last colon and hands back the " +
					"port \"3011/pds\" with no error — so a check that only calls SplitHostPort passes it",
			},
			{
				name:  "no port",
				value: ".pds2.test=localhost",
				why:   "there is no default port to fall back on: the transport dials exactly this string",
			},
			// net.SplitHostPort does not look at the port beyond splitting on
			// the last colon, so each of these produces a host:port pair it
			// reports as valid and the dialler then refuses. Every mapped handle
			// resolves handle.invalid, forever, from a config file that looks
			// right — and because an unverifiable handle is TRANSIENT by design,
			// the events retry rather than dead-letter, so nothing ever surfaces
			// the cause.
			{
				name:  "a non-numeric port",
				value: ".pds2.test=localhost:notaport",
				why:   "SplitHostPort returns port \"notaport\" with no error; only the dial fails, per resolution",
			},
			{
				name:  "port zero",
				value: ".pds2.test=localhost:0",
				why:   "port 0 means \"any free port\" to a listener and nothing at all to a dialler",
			},
			{
				name:  "a port above the 16-bit range",
				value: ".pds2.test=localhost:99999",
				why:   "there is no such port: TCP port numbers are 16-bit, so the ceiling is 65535",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				clearEnv(t)
				t.Setenv("IS_DEV_ENV", "true")
				t.Setenv("HANDLE_WELL_KNOWN_HOSTS", tc.value)

				_, err := Load()
				if err == nil {
					t.Fatalf("Load() accepted HANDLE_WELL_KNOWN_HOSTS=%q: %s", tc.value, tc.why)
				}
				if !strings.Contains(err.Error(), "HANDLE_WELL_KNOWN_HOSTS") {
					t.Errorf("Load() error = %q, want it to name HANDLE_WELL_KNOWN_HOSTS", err.Error())
				}
			})
		}
	})

	t.Run("refuses the variable outside dev", func(t *testing.T) {
		clearEnv(t)
		// IS_DEV_ENV deliberately unset: this is production's reading of the
		// environment, and the whole point is that production cannot enable
		// this however the variable is spelled.
		t.Setenv("HANDLE_WELL_KNOWN_HOSTS", ".pds2.test=localhost:3011")

		_, err := Load()
		if err == nil {
			t.Fatal("Load() accepted HANDLE_WELL_KNOWN_HOSTS outside dev")
		}
		// Load outside dev already fails for unrelated reasons — OAUTH_SEAL_SECRET
		// is required in production, among others — so "an error came back"
		// proves nothing here. The message naming this variable is the whole
		// assertion.
		if !strings.Contains(err.Error(), "HANDLE_WELL_KNOWN_HOSTS") {
			t.Errorf("Load() error = %q, want it to name HANDLE_WELL_KNOWN_HOSTS. Without that the "+
				"operator gets a panic from identity.NewResolver during wiring instead of a config "+
				"error, and nothing connects the crash to the variable that caused it", err.Error())
		}
	})
}
