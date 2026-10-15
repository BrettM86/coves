package jetstream

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeUserProfileOrigin(t *testing.T) {
	t.Parallel()

	usernameAtLimit := strings.Repeat("é", 128)
	usernameOverLimit := strings.Repeat("é", 129)
	softwareAtLimit := strings.Repeat("é", 32)
	softwareOverLimit := strings.Repeat("é", 33)
	usernameASCIIOverLimit := strings.Repeat("a", 257)
	softwareASCIIOverLimit := strings.Repeat("a", 65)
	require.Equal(t, 256, len(usernameAtLimit))
	require.Equal(t, 258, len(usernameOverLimit))
	require.Equal(t, 64, len(softwareAtLimit))
	require.Equal(t, 66, len(softwareOverLimit))
	require.Equal(t, 257, len(usernameASCIIOverLimit))
	require.Equal(t, 65, len(softwareASCIIOverLimit))
	softwareLowercaseGrowsOverLimit := strings.Repeat("\u023a", 32)
	require.Equal(t, 64, len(softwareLowercaseGrowsOverLimit))
	require.Equal(t, 96, len(strings.ToLower(softwareLowercaseGrowsOverLimit)))
	originURLOverLimit := "https://lemmy.world/u/" + strings.Repeat("a", 8192)

	for _, tc := range []struct {
		name string
		raw  userProfileOrigin
		want userProfileOrigin
	}{
		{
			name: "HTTP origin URL is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "http://lemmy.world/u/a"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "foreign host origin URL is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "https://evil.example/u/a"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "malformed origin URL is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "not a url"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "HTTPS URL on origin host is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "https://lemmy.world/u/Alice_B"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "https://lemmy.world/u/Alice_B"},
		},
		{
			name: "uppercase scheme origin URL is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "HTTPS://lemmy.world/u/a"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "origin URL with a space is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "https://lemmy.world/u/a b"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "origin URL with a non-ASCII letter is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "https://lemmy.world/u/\u00c5lice"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "origin URL with a no-break space is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "https://lemmy.world/u/a\u00a0b"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "origin URL over 8192 bytes is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: originURLOverLimit},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "origin URL with a Kelvin sign lookalike host is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "kbin.social", OriginUsername: "Alice_B", OriginURL: "https://\u212abin.social/u/a"},
			want: userProfileOrigin{Origin: "kbin.social", OriginUsername: "Alice_B"},
		},
		{
			name: "origin URL with userinfo is dropped but the pair is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B", OriginURL: "https://evil.example%2F@lemmy.world/u/a"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "software that grows past 64 bytes when lowercased is dropped",
			raw:  userProfileOrigin{OriginSoftware: softwareLowercaseGrowsOverLimit},
		},
		{
			name: "URL without a kept pair is dropped",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginURL: "https://lemmy.world/u/Alice_B"},
		},
		{
			name: "username without origin is dropped",
			raw:  userProfileOrigin{OriginUsername: "Alice_B"},
		},
		{
			name: "invalid origin host drops pair",
			raw:  userProfileOrigin{Origin: "not a host!", OriginUsername: "Alice_B"},
		},
		{
			name: "empty username drops pair",
			raw:  userProfileOrigin{Origin: "lemmy.world"},
		},
		{
			name: "257 byte username drops pair",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: usernameASCIIOverLimit},
		},
		{
			name: "258 byte username drops pair",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: usernameOverLimit},
		},
		{
			name: "256 byte multibyte username is kept",
			raw:  userProfileOrigin{Origin: "lemmy.world", OriginUsername: usernameAtLimit},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: usernameAtLimit},
		},
		{
			name: "origin host is lowercased without changing username",
			raw:  userProfileOrigin{Origin: "LEMMY.WORLD", OriginUsername: "Alice_B"},
			want: userProfileOrigin{Origin: "lemmy.world", OriginUsername: "Alice_B"},
		},
		{
			name: "software is lowercased",
			raw:  userProfileOrigin{OriginSoftware: "Lemmy"},
			want: userProfileOrigin{OriginSoftware: "lemmy"},
		},
		{
			name: "software is trimmed and lowercased",
			raw:  userProfileOrigin{OriginSoftware: " Lemmy "},
			want: userProfileOrigin{OriginSoftware: "lemmy"},
		},
		{
			name: "65 byte software is dropped",
			raw:  userProfileOrigin{OriginSoftware: softwareASCIIOverLimit},
		},
		{
			name: "blank software is dropped",
			raw:  userProfileOrigin{OriginSoftware: "   "},
		},
		{
			name: "66 byte software is dropped",
			raw:  userProfileOrigin{OriginSoftware: softwareOverLimit},
		},
		{
			name: "64 byte multibyte software is kept independently",
			raw:  userProfileOrigin{OriginSoftware: softwareAtLimit},
			want: userProfileOrigin{OriginSoftware: softwareAtLimit},
		},
		{
			name: "software is kept without origin pair",
			raw:  userProfileOrigin{OriginSoftware: "lemmy"},
			want: userProfileOrigin{OriginSoftware: "lemmy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, sanitizeUserProfileOrigin(tc.raw))
		})
	}
}

func TestHandleUnderPDSRegistrableDomain(t *testing.T) {
	for _, tc := range []struct {
		name, handle, pdsURL string
		want                 bool
	}{
		{"bridge handle under the PDS zone", "alice-b.lemmy-world.ap.tdpl.io", "https://tdpl.io", true},
		{"PDS on a subdomain of the zone", "alice-b.lemmy-world.ap.tdpl.io", "https://pds.tdpl.io", true},
		{"handle case is ignored", "Alice-B.AP.TDPL.IO", "https://tdpl.io", true},
		{"foreign handle", "alice.example.com", "https://tdpl.io", false},
		{"zone used as a subdomain of another domain", "alice.tdpl.io.evil.example", "https://tdpl.io", false},
		{"sibling under the same public suffix", "alice.other.io", "https://tdpl.io", false},
		{"unparseable PDS URL", "alice.tdpl.io", "://", false},
		{"empty handle", "", "https://tdpl.io", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := handleUnderPDSRegistrableDomain(tc.handle, tc.pdsURL); got != tc.want {
				t.Errorf("handleUnderPDSRegistrableDomain(%q, %q) = %v, want %v", tc.handle, tc.pdsURL, got, tc.want)
			}
		})
	}
}

func TestSanitizeOriginSoftware(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, raw, want string
	}{
		{"64 bytes of two-byte letters is kept unchanged", "éééééééééééééééééééééééééééééééé", "éééééééééééééééééééééééééééééééé"},
		{"66 bytes of two-byte letters is dropped", "ééééééééééééééééééééééééééééééééé", ""},
		{"surrounding spaces are trimmed and the value lowercased", "  Lemmy ", "lemmy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, sanitizeOriginSoftware(tc.raw))
		})
	}
}
