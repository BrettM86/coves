package bridgedvotes_test

import (
	"testing"

	"Coves/internal/core/bridgedvotes"

	"github.com/stretchr/testify/require"
)

func TestNormalizeHostUsesUnifiedBridgeTrustSemantics(t *testing.T) {
	t.Parallel()

	require.Equal(t, "https://bridge.example", bridgedvotes.NormalizeHost("HTTPS://Bridge.Example:443/"))
	require.Equal(t, "bridge.example", bridgedvotes.NormalizeHost("bridge.example"),
		"schemeless stored values retain BridgeTrust's tolerant normalized fallback")
}
