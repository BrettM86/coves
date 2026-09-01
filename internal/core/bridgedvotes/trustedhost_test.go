package bridgedvotes_test

import (
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"

	"github.com/stretchr/testify/require"
)

func TestParseTrustedHostAcceptsSchemeAndHostOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want string
	}{
		{raw: "https://tdpl.io", want: "https://tdpl.io"},
		{raw: "https://tdpl.io/", want: "https://tdpl.io"},
		{raw: "  HTTPS://TDPL.IO:443/  ", want: "https://tdpl.io"},
		{raw: "http://bridge.internal:8080", want: "http://bridge.internal:8080"},
		{raw: "https://[2001:db8::1]:8443", want: "https://[2001:db8::1]:8443"},
	}
	for _, test := range tests {
		host, err := bridgedvotes.ParseTrustedHost(test.raw)
		require.NoError(t, err, test.raw)
		require.Equal(t, test.want, host.String(), test.raw)
		require.False(t, host.IsZero())
		require.Equal(t, bridgedvotes.NormalizeHost(test.raw), host.String(),
			"the dial form must be the same key trust matching uses for %q", test.raw)
	}
}

func TestParseTrustedHostRejectsAnythingBeyondSchemeAndHost(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":                 "",
		"schemeless":            "tdpl.io",
		"non-http scheme":       "ftp://tdpl.io",
		"no host":               "https://",
		"credentials":           "https://user:secret@tdpl.io",
		"path":                  "https://tdpl.io/pds",
		"query":                 "https://tdpl.io?x=1",
		"fragment":              "https://tdpl.io#top",
		"port out of range":     "https://tdpl.io:70000",
		"nested userinfo trick": "https://user@evil.example@tdpl.io",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host, err := bridgedvotes.ParseTrustedHost(raw)
			require.Error(t, err)
			require.True(t, host.IsZero())
			if raw != "" {
				require.Contains(t, err.Error(), raw, "the error must name the offending entry")
			}
		})
	}
}

func TestParseAsOfSharedHygiene(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	got, err := bridgedvotes.ParseAsOf("2026-08-31T02:04:01.080Z", now)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 8, 31, 2, 4, 1, 80_000_000, time.UTC), got)

	got, err = bridgedvotes.ParseAsOf(now.Add(bridgedvotes.MaxAsOfSkew).Format(time.RFC3339), now)
	require.NoError(t, err, "a stamp exactly at the skew allowance is accepted")
	require.True(t, got.Equal(now.Add(bridgedvotes.MaxAsOfSkew)))

	_, err = bridgedvotes.ParseAsOf(now.Add(bridgedvotes.MaxAsOfSkew+time.Second).Format(time.RFC3339), now)
	require.Error(t, err, "one second past the allowance is a clock fault or a hostile stamp")

	_, err = bridgedvotes.ParseAsOf("0001-01-01T00:00:00Z", now)
	require.Error(t, err, "the zero time parses but would defeat the >= guard")

	_, err = bridgedvotes.ParseAsOf("not-a-time", now)
	require.Error(t, err)
}
