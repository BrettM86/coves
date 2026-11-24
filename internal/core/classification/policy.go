package classification

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// Question is one classifier question. One with RequiresImage is asked only
// in requests that carry an image; the others are asked in every request.
type Question struct {
	Key           string `json:"key"`
	Type          string `json:"type"`
	Instructions  string `json:"instructions"`
	RequiresImage bool   `json:"requiresImage"`
}

// Policy is a versioned set of classifier questions.
type Policy struct {
	Version   string     `json:"version"`
	Questions []Question `json:"questions"`
}

//go:embed policy/v3.json
var policyV3 []byte

// LoadPolicy returns the current classification policy.
func LoadPolicy() (Policy, error) {
	var policy Policy
	if err := json.Unmarshal(policyV3, &policy); err != nil {
		return Policy{}, fmt.Errorf("decoding classification policy: %w", err)
	}
	if policy.Version == "" || len(policy.Questions) == 0 {
		return Policy{}, fmt.Errorf("classification policy has no version or no questions")
	}
	for _, question := range policy.Questions {
		if question.Key == "" || question.Type == "" || question.Instructions == "" {
			return Policy{}, fmt.Errorf("classification policy %s has an incomplete question %q", policy.Version, question.Key)
		}
	}
	return policy, nil
}
