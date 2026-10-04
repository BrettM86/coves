package moderation_test

import (
	"testing"

	"Coves/internal/core/moderation"
	"github.com/stretchr/testify/assert"
)

func TestAllowlistAuthorityDefaultsToNobody(t *testing.T) {
	for _, test := range []struct {
		name string
		dids []string
	}{
		{name: "nil list"},
		{name: "empty list", dids: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority := moderation.NewAllowlistAuthority(test.dids)
			for _, did := range []string{"", "did:plc:a"} {
				assert.False(t, authority.IsInstanceAdmin(did), "DID %q must not be an admin", did)
			}
		})
	}
}

func TestAllowlistAuthorityMatchesExactDID(t *testing.T) {
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:alice"})
	for _, test := range []struct {
		name string
		did  string
		want bool
	}{
		{name: "listed DID", did: "did:plc:alice", want: true},
		{name: "unlisted DID", did: "did:plc:bob"},
		{name: "different case", did: "did:plc:Alice"},
		{name: "trailing whitespace", did: "did:plc:alice "},
		{name: "prefix", did: "did:plc:alic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, authority.IsInstanceAdmin(test.did))
		})
	}
}
