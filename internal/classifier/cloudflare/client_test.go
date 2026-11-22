package cloudflare_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"testing"

	"Coves/internal/classifier/cloudflare"
	"Coves/internal/classifier/cloudflare/cloudflaretest"
	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testQuestions = []classification.Question{
	{Key: "sexual_explicit", Type: "noul", Instructions: "Is the content sexually explicit?"},
	{Key: "spam_scam", Type: "noul", Instructions: "Is the content spam or a scam?"},
}

// TestClientClassifyRequestAndResponse pins the Workers AI request the client
// sends and how it reads the envelope response (B3).
func TestClientClassifyRequestAndResponse(t *testing.T) {
	transport := cloudflaretest.NewTransport(cloudflaretest.Canned(map[string]float64{
		"sexual_explicit": 0.12,
		"spam_scam":       0.87,
	}, 123))
	client := cloudflare.NewClient(cloudflare.Config{
		AccountID:   "acct-123",
		APIToken:    "token-abc",
		TextModelID: "@cf/cloudflare/clef-flash",
	}, transport.Client())

	result, err := client.Classify(context.Background(),
		classification.Content{Text: "Harbor seals\n\nSpotted three on the rocks."}, testQuestions)
	require.NoError(t, err)

	assert.Equal(t, classification.Classification{
		ModelID:       "@cf/cloudflare/clef-flash",
		Probabilities: map[string]float64{"sexual_explicit": 0.12, "spam_scam": 0.87},
		InputTokens:   123,
	}, result)

	requests := transport.Requests()
	require.Len(t, requests, 1)
	request := requests[0]
	assert.Equal(t, http.MethodPost, request.Method)
	assert.Equal(t, "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/run/@cf/cloudflare/clef-flash", request.URL)
	assert.Equal(t, "Bearer token-abc", request.Header.Get("Authorization"))
	assert.Equal(t, "application/json", request.Header.Get("Content-Type"))

	headerNames := make([]string, 0, len(request.Header))
	for name := range request.Header {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	assert.Equal(t, []string{"Authorization", "Content-Type"}, headerNames)

	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(request.Body, &top))
	topKeys := make([]string, 0, len(top))
	for key := range top {
		topKeys = append(topKeys, key)
	}
	sort.Strings(topKeys)
	assert.Equal(t, []string{"model", "questions", "state"}, topKeys)

	var body struct {
		Model     string                       `json:"model"`
		State     string                       `json:"state"`
		Questions map[string]map[string]string `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(request.Body, &body))
	assert.Equal(t, "clef-flash", body.Model)
	assert.Equal(t, "Harbor seals\n\nSpotted three on the rocks.", body.State)
	assert.Equal(t, map[string]map[string]string{
		"sexual_explicit": {"type": "noul", "instructions": "Is the content sexually explicit?"},
		"spam_scam":       {"type": "noul", "instructions": "Is the content spam or a scam?"},
	}, body.Questions)
}

const tokenSentinel = "cf-workers-ai-token-SENTINEL-91c4"

// TestClientClassifyErrors maps each failure to its provider error code and
// checks the API token never reaches the error, the client's formatting or
// the default logger (B4).
func TestClientClassifyErrors(t *testing.T) {
	bothAnswers := map[string]any{
		"sexual_explicit": map[string]any{"type": "noul", "noul": 0.12},
		"spam_scam":       map[string]any{"type": "noul", "noul": 0.87},
	}
	withAnswer := func(key string, answer any) map[string]any {
		answers := map[string]any{}
		for k, v := range bothAnswers {
			answers[k] = v
		}
		answers[key] = answer
		return answers
	}

	for _, test := range []struct {
		name     string
		respond  cloudflaretest.Responder
		wantCode string
	}{
		{
			name: "transport error",
			respond: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed for Bearer " + tokenSentinel)
			},
			wantCode: "transport",
		},
		{
			name:     "server error",
			respond:  cloudflaretest.Status(http.StatusInternalServerError, []byte(`{"errors":[{"message":"bad token `+tokenSentinel+`"}]}`)),
			wantCode: "provider_error",
		},
		{
			name:     "unauthorized token",
			respond:  cloudflaretest.Status(http.StatusUnauthorized, []byte(`{"errors":[{"message":"bad token `+tokenSentinel+`"}]}`)),
			wantCode: "provider_auth",
		},
		{
			name:     "forbidden token",
			respond:  cloudflaretest.Status(http.StatusForbidden, []byte(`{"errors":[{"message":"no scope `+tokenSentinel+`"}]}`)),
			wantCode: "provider_auth",
		},
		{
			name:     "rate limited",
			respond:  cloudflaretest.Status(http.StatusTooManyRequests, []byte(`{"errors":[{"message":"slow down `+tokenSentinel+`"}]}`)),
			wantCode: "rate_limited",
		},
		{
			name:     "undecodable JSON",
			respond:  cloudflaretest.Status(http.StatusOK, []byte(`{"result":`)),
			wantCode: "bad_response",
		},
		{
			name:     "success false",
			respond:  cloudflaretest.Status(http.StatusOK, cloudflaretest.EnvelopeWithAnswers(bothAnswers, 123, false)),
			wantCode: "bad_response",
		},
		{
			name: "missing answer for one key",
			respond: cloudflaretest.Status(http.StatusOK, cloudflaretest.EnvelopeWithAnswers(map[string]any{
				"sexual_explicit": map[string]any{"type": "noul", "noul": 0.12},
			}, 123, true)),
			wantCode: "bad_response",
		},
		{
			name:     "noul absent",
			respond:  cloudflaretest.Status(http.StatusOK, cloudflaretest.EnvelopeWithAnswers(withAnswer("spam_scam", map[string]any{"type": "noul"}), 123, true)),
			wantCode: "bad_response",
		},
		{
			name:     "noul null",
			respond:  cloudflaretest.Status(http.StatusOK, cloudflaretest.EnvelopeWithAnswers(withAnswer("spam_scam", map[string]any{"type": "noul", "noul": nil}), 123, true)),
			wantCode: "bad_response",
		},
		{
			name:     "noul above one",
			respond:  cloudflaretest.Status(http.StatusOK, cloudflaretest.EnvelopeWithAnswers(withAnswer("spam_scam", map[string]any{"type": "noul", "noul": 1.5}), 123, true)),
			wantCode: "bad_response",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			transport := cloudflaretest.NewTransport(test.respond)
			client := cloudflare.NewClient(cloudflare.Config{
				AccountID:   "acct-123",
				APIToken:    tokenSentinel,
				TextModelID: "@cf/cloudflare/clef-flash",
			}, transport.Client())

			_, err := client.Classify(context.Background(),
				classification.Content{Text: "Harbor seals"}, testQuestions)
			require.Error(t, err)
			assert.Equal(t, test.wantCode, classification.ErrorCodeOf(err))

			assert.NotContains(t, err.Error(), tokenSentinel)
			assert.NotContains(t, fmt.Sprintf("%+v", err), tokenSentinel)
			assert.NotContains(t, fmt.Sprintf("%+v", client), tokenSentinel)
			assert.NotContains(t, logs.String(), tokenSentinel)
		})
	}
}

// TestClientClassifyWithImages pins an image request: it goes to the
// configured image model with the images as data URLs beside the text, and a
// text-only request carries no images key (R3, R4).
func TestClientClassifyWithImages(t *testing.T) {
	for _, test := range []struct {
		name       string
		images     []classification.Image
		wantURL    string
		wantModel  string
		wantImages []string
		wantKeys   []string
		wantResult string
	}{
		{
			name: "two images",
			images: []classification.Image{
				{ContentType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
				{ContentType: "image/jpeg", Data: []byte{0xFF, 0xD8, 0xFF, 0xE0}},
			},
			wantURL:    "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/run/@cf/acme/clef",
			wantModel:  "clef",
			wantImages: []string{"data:image/png;base64,iVBORw==", "data:image/jpeg;base64,/9j/4A=="},
			wantKeys:   []string{"images", "model", "questions", "state"},
			wantResult: "@cf/acme/clef",
		},
		{
			name:       "text only",
			wantURL:    "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/run/@cf/cloudflare/clef-flash",
			wantModel:  "clef-flash",
			wantKeys:   []string{"model", "questions", "state"},
			wantResult: "@cf/cloudflare/clef-flash",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := cloudflaretest.NewTransport(cloudflaretest.Canned(map[string]float64{
				"sexual_explicit": 0.12,
				"spam_scam":       0.87,
			}, 456))
			client := cloudflare.NewClient(cloudflare.Config{
				AccountID:    "acct-123",
				APIToken:     "token-abc",
				TextModelID:  "@cf/cloudflare/clef-flash",
				ImageModelID: "@cf/acme/clef",
			}, transport.Client())

			result, err := client.Classify(context.Background(),
				classification.Content{Text: "Harbor seals\n\nSpotted three on the rocks.", Images: test.images}, testQuestions)
			require.NoError(t, err)
			assert.Equal(t, test.wantResult, result.ModelID)

			requests := transport.Requests()
			require.Len(t, requests, 1)
			assert.Equal(t, test.wantURL, requests[0].URL)

			var top map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(requests[0].Body, &top))
			keys := make([]string, 0, len(top))
			for key := range top {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			assert.Equal(t, test.wantKeys, keys)

			var body struct {
				Model  string   `json:"model"`
				State  string   `json:"state"`
				Images []string `json:"images"`
			}
			require.NoError(t, json.Unmarshal(requests[0].Body, &body))
			assert.Equal(t, test.wantModel, body.Model)
			assert.Equal(t, "Harbor seals\n\nSpotted three on the rocks.", body.State)
			assert.Equal(t, test.wantImages, body.Images)
		})
	}
}
