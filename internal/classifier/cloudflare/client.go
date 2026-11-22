// Package cloudflare classifies content with Cloudflare Workers AI.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"Coves/internal/core/classification"
)

// baseURL is the Cloudflare v4 API root.
const baseURL = "https://api.cloudflare.com/client/v4"

// maxResponseBytes bounds how much of a response body the client reads.
const maxResponseBytes = 1 << 20

// Config holds the Workers AI account and model settings.
type Config struct {
	AccountID    string
	APIToken     string `json:"-"`
	TextModelID  string
	ImageModelID string
}

// String redacts the API token when a Config is formatted with %v or %+v.
func (c Config) String() string {
	return fmt.Sprintf("{AccountID:%q APIToken:[REDACTED] TextModelID:%q ImageModelID:%q}", c.AccountID, c.TextModelID, c.ImageModelID)
}

// GoString redacts the API token when a Config is formatted with %#v.
func (c Config) GoString() string { return c.String() }

// Client is a Workers AI classifier.
type Client struct {
	cfg        Config
	httpClient *http.Client
}

// NewClient returns a Workers AI classifier.
func NewClient(cfg Config, httpClient *http.Client) *Client {
	return &Client{cfg: cfg, httpClient: httpClient}
}

// String redacts the API token when a Client is formatted with %v or %+v.
func (c *Client) String() string {
	return fmt.Sprintf("cloudflare.Client%s", c.cfg.String())
}

// GoString redacts the API token when a Client is formatted with %#v.
func (c *Client) GoString() string { return c.String() }

type questionBody struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

type requestBody struct {
	Model     string                  `json:"model"`
	State     string                  `json:"state"`
	Questions map[string]questionBody `json:"questions"`
	// Images are data URLs; a text-only request has none and no images key.
	Images []string `json:"images,omitempty"`
}

type answer struct {
	Noul *float64 `json:"noul"`
}

type envelope struct {
	Success bool `json:"success"`
	Result  *struct {
		Answers map[string]answer `json:"answers"`
		Usage   struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"result"`
}

// Classify implements classification.Classifier. Every failure is a
// *classification.ProviderError; its text never carries the token, the
// response body or the transport error's text. Content with images goes to
// the image model, each image sent as a data URL; text-only content goes to
// the text model.
func (c *Client) Classify(ctx context.Context, content classification.Content, questions []classification.Question) (classification.Classification, error) {
	modelID := c.cfg.TextModelID
	if len(content.Images) > 0 {
		modelID = c.cfg.ImageModelID
	}
	body := requestBody{
		Model:     modelID[strings.LastIndex(modelID, "/")+1:],
		State:     content.Text,
		Questions: make(map[string]questionBody, len(questions)),
	}
	for _, question := range questions {
		body.Questions[question.Key] = questionBody{Type: question.Type, Instructions: question.Instructions}
	}
	for _, image := range content.Images {
		body.Images = append(body.Images, "data:"+image.ContentType+";base64,"+base64.StdEncoding.EncodeToString(image.Data))
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return classification.Classification{}, fmt.Errorf("encoding Workers AI request: %w", err)
	}

	endpoint := baseURL + "/accounts/" + url.PathEscape(c.cfg.AccountID) + "/ai/run/" + modelID
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return classification.Classification{}, &classification.ProviderError{Code: classification.ErrorCodeTransport}
	}
	request.Header.Set("Authorization", "Bearer "+c.cfg.APIToken)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		// The transport error's text may echo request details, so it is dropped.
		return classification.Classification{}, &classification.ProviderError{Code: classification.ErrorCodeTransport}
	}
	defer response.Body.Close()

	status := response.StatusCode
	switch {
	case status == http.StatusTooManyRequests:
		return classification.Classification{}, &classification.ProviderError{Code: classification.ErrorCodeRateLimited, HTTPStatus: status}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return classification.Classification{}, &classification.ProviderError{Code: classification.ErrorCodeProviderAuth, HTTPStatus: status}
	case status < 200 || status > 299:
		return classification.Classification{}, &classification.ProviderError{Code: classification.ErrorCodeProviderError, HTTPStatus: status}
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return classification.Classification{}, &classification.ProviderError{Code: classification.ErrorCodeTransport, HTTPStatus: status}
	}
	badResponse := &classification.ProviderError{Code: classification.ErrorCodeBadResponse, HTTPStatus: status}
	var decoded envelope
	if err := json.Unmarshal(raw, &decoded); err != nil || !decoded.Success || decoded.Result == nil {
		return classification.Classification{}, badResponse
	}

	probabilities := make(map[string]float64, len(questions))
	for _, question := range questions {
		answer, ok := decoded.Result.Answers[question.Key]
		if !ok || answer.Noul == nil || *answer.Noul < 0 || *answer.Noul > 1 {
			return classification.Classification{}, badResponse
		}
		probabilities[question.Key] = *answer.Noul
	}
	return classification.Classification{
		ModelID:       modelID,
		Probabilities: probabilities,
		InputTokens:   decoded.Result.Usage.InputTokens,
	}, nil
}
