package classification_test

import (
	"testing"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadPolicy pins the v3 policy: its version and the six question keys in
// order, each a noul question with instructions.
func TestLoadPolicy(t *testing.T) {
	policy, err := classification.LoadPolicy()
	require.NoError(t, err)

	assert.Equal(t, "v3", policy.Version)

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

// Only sexual_minors requires an image: a request without one is not asked
// it (B1).
func TestLoadPolicy_OnlySexualMinorsRequiresAnImage(t *testing.T) {
	policy, err := classification.LoadPolicy()
	require.NoError(t, err)

	requiresImage := map[string]bool{}
	for _, question := range policy.Questions {
		requiresImage[question.Key] = question.RequiresImage
	}
	assert.Equal(t, map[string]bool{
		"sexual_explicit":   false,
		"sexual_suggestive": false,
		"sexual_minors":     true,
		"graphic_violence":  false,
		"hate_harassment":   false,
		"spam_scam":         false,
	}, requiresImage)
}
