package classificationadmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/classification"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// ReenqueueService is the re-enqueue service the enqueue handler calls.
type ReenqueueService interface {
	Reenqueue(ctx context.Context, request classification.ReenqueueRequest) (classification.ReenqueueReport, error)
}

// SummaryService is the summary service the summary handler calls.
type SummaryService interface {
	Summary(ctx context.Context, request classification.SummaryRequest) (classification.CommunitySummary, error)
}

// EnqueueHandler serves social.coves.moderation.enqueueClassification.
type EnqueueHandler struct{ service ReenqueueService }

// NewEnqueueHandler builds the enqueue handler.
func NewEnqueueHandler(service ReenqueueService) *EnqueueHandler {
	return &EnqueueHandler{service: service}
}

func (h *EnqueueHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Content-Type must be application/json")
		return
	}
	var input struct {
		Target string  `json:"target"`
		Since  *string `json:"since"`
		Until  *string `json:"until"`
	}
	if err := reqbody.DecodeJSON(w, r, reqbody.LimitMedium, &input); err != nil {
		var tooLarge *reqbody.TooLargeError
		if errors.As(err, &tooLarge) {
			xrpc.WriteError(w, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Request body too large")
			return
		}
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}
	request := classification.ReenqueueRequest{Target: classification.ReenqueueTarget(input.Target)}
	if request.Since, err = optionalDatetime(input.Since, "since"); err != nil {
		writeOperationsError(w, "enqueueClassification", err)
		return
	}
	if request.Until, err = optionalDatetime(input.Until, "until"); err != nil {
		writeOperationsError(w, "enqueueClassification", err)
		return
	}
	report, err := h.service.Reenqueue(r.Context(), request)
	if err != nil {
		writeOperationsError(w, "enqueueClassification", err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, enqueueOutput{
		Created: report.Created, Revived: report.Revived, AlreadyPresent: report.AlreadyPresent,
	})
}

// SummaryHandler serves social.coves.moderation.getCommunityClassificationSummary.
type SummaryHandler struct{ service SummaryService }

// NewSummaryHandler builds the summary handler.
func NewSummaryHandler(service SummaryService) *SummaryHandler {
	return &SummaryHandler{service: service}
}

func (h *SummaryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	var request classification.SummaryRequest
	var err error
	if request.CommunityDID, err = singleParameter(values, "community"); err != nil {
		writeOperationsError(w, "getCommunityClassificationSummary", err)
		return
	}
	if request.Question, err = singleParameter(values, "question"); err != nil {
		writeOperationsError(w, "getCommunityClassificationSummary", err)
		return
	}
	minProbability, err := partsPerMillionParameter(values, "minProbability")
	if err != nil {
		writeOperationsError(w, "getCommunityClassificationSummary", err)
		return
	}
	if minProbability == nil {
		writeOperationsError(w, "getCommunityClassificationSummary",
			fmt.Errorf("%w: minProbability is required", errInvalidParameter))
		return
	}
	request.MinProbability = *minProbability

	summary, err := h.service.Summary(r.Context(), request)
	if err != nil {
		writeOperationsError(w, "getCommunityClassificationSummary", err)
		return
	}
	output := summaryOutput{
		Community:      summary.CommunityDID,
		Question:       summary.Question,
		MinProbability: toPartsPerMillion(summary.MinProbability),
		Since:          formatDatetime(summary.Since),
		Until:          formatDatetime(summary.Until),
		Posts:          kindSummaryView{Scored: summary.Posts.Scored, Above: summary.Posts.Above},
		Comments:       kindSummaryView{Scored: summary.Comments.Scored, Above: summary.Comments.Above},
		FlagEvents:     make([]flagEventView, 0, len(summary.FlagEvents)),
	}
	if share, defined := summary.Share(); defined {
		parts := toPartsPerMillion(share)
		output.Share = &parts
	}
	for _, event := range summary.FlagEvents {
		output.FlagEvents = append(output.FlagEvents, flagEventView{
			OldNSFW: event.OldNSFW, NewNSFW: event.NewNSFW,
			OccurredAt: formatDatetime(event.OccurredAt), Delivered: event.Delivered,
		})
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

// optionalDatetime decodes an atproto datetime body field, or the zero time
// when absent.
func optionalDatetime(text *string, name string) (time.Time, error) {
	if text == nil {
		return time.Time{}, nil
	}
	parsed, err := syntax.ParseDatetime(*text)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be a datetime", errInvalidParameter, name)
	}
	return parsed.Time(), nil
}

// operationsFailureCodes names the fixed log code of each operation's
// internal failure.
var operationsFailureCodes = map[string]string{
	"enqueueClassification":             "classification_enqueue_failed",
	"getCommunityClassificationSummary": "classification_summary_failed",
}

// writeOperationsError answers an operations error. Every service refusal is
// 400 under its own name; anything else is logged and answers 500.
func writeOperationsError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, errInvalidParameter), errors.Is(err, classification.ErrInvalidRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case errors.Is(err, classification.ErrClassifierNotConfigured):
		xrpc.WriteError(w, http.StatusBadRequest, "ClassifierNotConfigured", "The content classifier is not configured")
	case errors.Is(err, classification.ErrCommunityNotFound):
		xrpc.WriteError(w, http.StatusBadRequest, "CommunityNotFound", "Community not found")
	default:
		// Invariant: wrapped operations errors carry only fixed text, never
		// URIs, DIDs, content or probabilities, so the error is logged under
		// a fixed code; the response body stays generic.
		slog.Error("classification operations failure",
			"error_code", operationsFailureCodes[operation], "operation", operation, "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}

type enqueueOutput struct {
	Created        int `json:"created"`
	Revived        int `json:"revived"`
	AlreadyPresent int `json:"alreadyPresent"`
}

type kindSummaryView struct {
	Scored int `json:"scored"`
	Above  int `json:"above"`
}

type flagEventView struct {
	OldNSFW    bool   `json:"oldNsfw"`
	NewNSFW    bool   `json:"newNsfw"`
	OccurredAt string `json:"occurredAt"`
	Delivered  bool   `json:"delivered"`
}

type summaryOutput struct {
	Community      string          `json:"community"`
	Question       string          `json:"question"`
	MinProbability int64           `json:"minProbability"`
	Since          string          `json:"since"`
	Until          string          `json:"until"`
	Posts          kindSummaryView `json:"posts"`
	Comments       kindSummaryView `json:"comments"`
	Share          *int64          `json:"share,omitempty"`
	FlagEvents     []flagEventView `json:"flagEvents"`
}
