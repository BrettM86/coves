package classification

import (
	"context"
	"errors"
	"fmt"
)

// Close reasons, stored in a closed job's close_reason.
const (
	// CloseReasonSuperseded means the subject's record CID changed.
	CloseReasonSuperseded = "superseded"
	// CloseReasonNotVisible means the subject is missing or hidden.
	CloseReasonNotVisible = "not_visible"
)

// Provider error codes, carried by ProviderError and stored in a job's
// last_error_code.
const (
	// ErrorCodeTransport means the request did not get an HTTP response.
	ErrorCodeTransport = "transport"
	// ErrorCodeRateLimited means the provider answered 429.
	ErrorCodeRateLimited = "rate_limited"
	// ErrorCodeProviderError means the provider answered 5xx or another
	// non-2xx status other than 401, 403 and 429.
	ErrorCodeProviderError = "provider_error"
	// ErrorCodeBadResponse means a 2xx answer was not a valid classification.
	ErrorCodeBadResponse = "bad_response"
	// ErrorCodeProviderAuth means the provider refused the API token (HTTP
	// 401 or 403). It is not retried as a job failure: the worker spends no
	// attempt and pauses claiming instead.
	ErrorCodeProviderAuth = "provider_auth"
)

// Classifier asks a provider the policy questions about content.
type Classifier interface {
	Classify(ctx context.Context, content Content, questions []Question) (Classification, error)
}

// Classification is a provider's answer for one subject.
type Classification struct {
	ModelID       string
	Probabilities map[string]float64
	InputTokens   int
}

// ProviderError is a failed classification request. Its message names only
// the code and the HTTP status (0 when there was no response), never response
// bodies, transport error text or credentials.
type ProviderError struct {
	Code       string
	HTTPStatus int
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("classifier provider error: code=%s status=%d", e.Code, e.HTTPStatus)
}

// ErrorCodeOf returns the provider error code carried by err, or "".
func ErrorCodeOf(err error) string {
	var providerError *ProviderError
	if errors.As(err, &providerError) {
		return providerError.Code
	}
	return ""
}
