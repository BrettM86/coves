package classification_test

import (
	"testing"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadPolicy pins the v1 policy: its version and the six question keys in
// order, each a noul question with instructions (B1).
func TestLoadPolicy(t *testing.T) {
	policy, err := classification.LoadPolicy()
	require.NoError(t, err)

	assert.Equal(t, "v1", policy.Version)

	keys := make([]string, 0, len(policy.Questions))
	for _, question := range policy.Questions {
		keys = append(keys, question.Key)
	}
	require.Equal(t, []string{
		"sexual_explicit",
		"sexual_suggestive",
		"sexual_minors",
		"graphic_violence",
		"hate_harassment",
		"spam_scam",
	}, keys)

	for _, question := range policy.Questions {
		assert.Equal(t, "noul", question.Type, "question %s type", question.Key)
		assert.NotEmpty(t, question.Instructions, "question %s instructions", question.Key)
	}
}
