package classification

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// Question is one classifier question asked of every subject.
type Question struct {
	Key          string `json:"key"`
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

// Policy is a versioned set of classifier questions.
type Policy struct {
	Version   string     `json:"version"`
	Questions []Question `json:"questions"`
}

//go:embed policy/v2.json
var policyV2 []byte

// LoadPolicy returns the current classification policy.
func LoadPolicy() (Policy, error) {
	var policy Policy
	if err := json.Unmarshal(policyV2, &policy); err != nil {
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
