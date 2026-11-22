// Package classificationadmin serves the instance admin classification review endpoints.
//
// Probabilities, thresholds and ratios cross the wire as integer parts per
// million, because lexicons have no floating point type; the service works in
// float64 between 0 and 1.
package classificationadmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/classification"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// ReviewService is the classification review service the handlers call.
type ReviewService interface {
	ListResults(ctx context.Context, filter classification.ResultFilter) (classification.ResultPage, error)
	RecordVerdict(ctx context.Context, request classification.VerdictRequest) (classification.Verdict, error)
	Precision(ctx context.Context, request classification.PrecisionRequest) (classification.PrecisionReport, error)
}

// partsPerMillion is the wire scale of a probability, threshold or ratio.
const partsPerMillion = 1_000_000

// errInvalidParameter marks a request the handler refuses before calling the
// service; it answers 400 InvalidRequest.
var errInvalidParameter = errors.New("invalid parameter")

// ListResultsHandler serves social.coves.moderation.listClassificationResults.
type ListResultsHandler struct{ service ReviewService }

// NewListResultsHandler builds the list handler.
func NewListResultsHandler(service ReviewService) *ListResultsHandler {
	return &ListResultsHandler{service: service}
}

func (h *ListResultsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	filter, err := decodeResultFilter(r.URL.Query())
	if err != nil {
		writeReviewError(w, "listClassificationResults", err)
		return
	}
	page, err := h.service.ListResults(r.Context(), filter)
	if err != nil {
		writeReviewError(w, "listClassificationResults", err)
		return
	}
	output := resultPageOutput{Results: make([]resultView, 0, len(page.Results)), Cursor: page.Cursor}
	for _, result := range page.Results {
		output.Results = append(output.Results, newResultView(result))
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

// RecordVerdictHandler serves social.coves.moderation.recordClassificationVerdict.
type RecordVerdictHandler struct{ service ReviewService }

// NewRecordVerdictHandler builds the verdict handler.
func NewRecordVerdictHandler(service ReviewService) *RecordVerdictHandler {
	return &RecordVerdictHandler{service: service}
}

func (h *RecordVerdictHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Content-Type must be application/json")
		return
	}
	var input struct {
		ResultID *int64 `json:"resultId"`
		Answers  []struct {
			Question string `json:"question"`
			Present  *bool  `json:"present"`
		} `json:"answers"`
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
	if input.ResultID == nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "resultId is required")
		return
	}
	present := make(map[string]bool, len(input.Answers))
	for _, answer := range input.Answers {
		if answer.Present == nil {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "every answer needs a boolean present")
			return
		}
		if _, repeated := present[answer.Question]; repeated {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "each question may be answered once")
			return
		}
		present[answer.Question] = *answer.Present
	}
	adminDID := middleware.GetUserDID(r)
	if adminDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	verdict, err := h.service.RecordVerdict(r.Context(), classification.VerdictRequest{
		ResultID: *input.ResultID, AdminDID: adminDID, Present: present,
	})
	if err != nil {
		writeReviewError(w, "recordClassificationVerdict", err)
		return
	}
	questions := make([]string, 0, len(verdict.Present))
	for question := range verdict.Present {
		questions = append(questions, question)
	}
	sort.Strings(questions)
	output := verdictOutput{
		ResultID:   verdict.ResultID,
		AdminDID:   verdict.AdminDID,
		Answers:    make([]answerView, 0, len(questions)),
		RecordedAt: formatDatetime(verdict.RecordedAt),
	}
	for _, question := range questions {
		output.Answers = append(output.Answers, answerView{Question: question, Present: verdict.Present[question]})
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

// PrecisionHandler serves social.coves.moderation.getClassificationPrecision.
type PrecisionHandler struct{ service ReviewService }

// NewPrecisionHandler builds the precision handler.
func NewPrecisionHandler(service ReviewService) *PrecisionHandler {
	return &PrecisionHandler{service: service}
}

func (h *PrecisionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	request, err := decodePrecisionRequest(r.URL.Query())
	if err != nil {
		writeReviewError(w, "getClassificationPrecision", err)
		return
	}
	report, err := h.service.Precision(r.Context(), request)
	if err != nil {
		writeReviewError(w, "getClassificationPrecision", err)
		return
	}
	output := precisionOutput{
		ModelID:           report.ModelID,
		PolicyVersion:     report.PolicyVersion,
		Threshold:         toPartsPerMillion(report.Threshold),
		VerdictedCount:    report.VerdictedCount,
		UnreviewableCount: report.UnreviewableCount,
		Questions:         make([]questionPrecisionView, 0, len(report.Questions)),
	}
	for _, question := range report.Questions {
		output.Questions = append(output.Questions, questionPrecisionView{
			Question:     question.Question,
			AtOrAbove:    question.AtOrAbove,
			Present:      question.Present,
			TruePositive: question.TruePositive,
			Precision:    optionalPartsPerMillion(question.Precision),
			Recall:       optionalPartsPerMillion(question.Recall),
		})
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

// decodeResultFilter reads the list query parameters. A parameter that does
// not decode is refused here; range rules the service owns (limit,
// minProbability without question, subject kind) are left to it.
func decodeResultFilter(values url.Values) (classification.ResultFilter, error) {
	var filter classification.ResultFilter
	var subjectKind string
	for name, target := range map[string]*string{
		"question":      &filter.Question,
		"subjectKind":   &subjectKind,
		"community":     &filter.CommunityDID,
		"modelId":       &filter.ModelID,
		"policyVersion": &filter.PolicyVersion,
		"cursor":        &filter.Cursor,
	} {
		value, err := singleParameter(values, name)
		if err != nil {
			return filter, err
		}
		*target = value
	}
	filter.SubjectKind = classification.SubjectKind(subjectKind)

	minProbability, err := partsPerMillionParameter(values, "minProbability")
	if err != nil {
		return filter, err
	}
	filter.MinProbability = minProbability

	if filter.Since, err = datetimeParameter(values, "since"); err != nil {
		return filter, err
	}
	if filter.Until, err = datetimeParameter(values, "until"); err != nil {
		return filter, err
	}

	limit, err := singleParameter(values, "limit")
	if err != nil {
		return filter, err
	}
	if limit != "" {
		if filter.Limit, err = strconv.Atoi(limit); err != nil {
			return filter, fmt.Errorf("%w: limit must be an integer", errInvalidParameter)
		}
	}
	return filter, nil
}

func decodePrecisionRequest(values url.Values) (classification.PrecisionRequest, error) {
	var request classification.PrecisionRequest
	var err error
	if request.ModelID, err = singleParameter(values, "modelId"); err != nil {
		return request, err
	}
	if request.PolicyVersion, err = singleParameter(values, "policyVersion"); err != nil {
		return request, err
	}
	threshold, err := partsPerMillionParameter(values, "threshold")
	if err != nil {
		return request, err
	}
	if threshold == nil {
		return request, fmt.Errorf("%w: threshold is required", errInvalidParameter)
	}
	request.Threshold = *threshold
	return request, nil
}

// singleParameter returns a query parameter's one value, or "" when absent.
// A repeated or empty parameter is refused.
func singleParameter(values url.Values, name string) (string, error) {
	entries, present := values[name]
	if !present {
		return "", nil
	}
	if len(entries) > 1 {
		return "", fmt.Errorf("%w: repeated %s", errInvalidParameter, name)
	}
	if entries[0] == "" {
		return "", fmt.Errorf("%w: empty %s", errInvalidParameter, name)
	}
	return entries[0], nil
}

// partsPerMillionParameter decodes an integer 0..1000000 parameter to a
// probability between 0 and 1, or nil when absent.
func partsPerMillionParameter(values url.Values, name string) (*float64, error) {
	text, err := singleParameter(values, name)
	if err != nil || text == "" {
		return nil, err
	}
	parts, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parts < 0 || parts > partsPerMillion {
		return nil, fmt.Errorf("%w: %s must be an integer from 0 to %d", errInvalidParameter, name, partsPerMillion)
	}
	probability := float64(parts) / partsPerMillion
	return &probability, nil
}

// datetimeParameter decodes an atproto datetime parameter, or the zero time
// when absent.
func datetimeParameter(values url.Values, name string) (time.Time, error) {
	text, err := singleParameter(values, name)
	if err != nil || text == "" {
		return time.Time{}, err
	}
	parsed, err := syntax.ParseDatetime(text)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be a datetime", errInvalidParameter, name)
	}
	return parsed.Time(), nil
}

func toPartsPerMillion(value float64) int64 {
	return int64(math.Round(value * partsPerMillion))
}

func optionalPartsPerMillion(value *float64) *int64 {
	if value == nil {
		return nil
	}
	parts := toPartsPerMillion(*value)
	return &parts
}

func formatDatetime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

// reviewFailureCodes names the fixed log code of each operation's internal
// failure.
var reviewFailureCodes = map[string]string{
	"listClassificationResults":   "classification_review_list_failed",
	"recordClassificationVerdict": "classification_review_verdict_failed",
	"getClassificationPrecision":  "classification_review_precision_failed",
}

// writeReviewError answers a review error. Every service refusal is 400 under
// its own name; anything else is logged and answers 500.
func writeReviewError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, errInvalidParameter):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case errors.Is(err, classification.ErrInvalidRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case errors.Is(err, classification.ErrInvalidCursor):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidCursor", "Invalid classification result cursor")
	case errors.Is(err, classification.ErrResultNotFound):
		xrpc.WriteError(w, http.StatusBadRequest, "ResultNotFound", "Classification result not found")
	case errors.Is(err, classification.ErrUnreviewable):
		xrpc.WriteError(w, http.StatusBadRequest, "Unreviewable", "The subject was edited or deleted after it was scored")
	default:
		// Invariant: wrapped review errors carry only result IDs and fixed
		// text, never URIs, DIDs, content or probabilities, so the error is
		// logged under a fixed code; the response body stays generic.
		slog.Error("classification review failure",
			"error_code", reviewFailureCodes[operation], "operation", operation, "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}

type resultPageOutput struct {
	Results []resultView `json:"results"`
	Cursor  string       `json:"cursor,omitempty"`
}

type resultView struct {
	ID                 int64             `json:"id"`
	SubjectKind        string            `json:"subjectKind"`
	SubjectURI         string            `json:"subjectUri"`
	ScoredCID          string            `json:"scoredCid"`
	CommunityDID       string            `json:"communityDid"`
	AuthorDID          string            `json:"authorDid"`
	ModelID            string            `json:"modelId"`
	PolicyVersion      string            `json:"policyVersion"`
	Probabilities      []probabilityView `json:"probabilities"`
	ImagesSent         int               `json:"imagesSent"`
	ImagesSkipped      int               `json:"imagesSkipped"`
	TextTruncated      bool              `json:"textTruncated"`
	Partial            bool              `json:"partial"`
	CreatedAt          string            `json:"createdAt"`
	ScoredCIDIsCurrent bool              `json:"scoredCidIsCurrent"`
	Subject            subjectView       `json:"subject"`
}

type probabilityView struct {
	Question    string `json:"question"`
	Probability int64  `json:"probability"`
}

type subjectView struct {
	Visibility string               `json:"visibility"`
	Deleted    bool                 `json:"deleted"`
	CurrentCID string               `json:"currentCid,omitempty"`
	Post       *postFieldsView      `json:"post,omitempty"`
	Comment    *commentFieldsView   `json:"comment,omitempty"`
	Community  *communityFieldsView `json:"community,omitempty"`
	Images     []imageRefView       `json:"images"`
}

type postFieldsView struct {
	Title         string `json:"title"`
	Body          string `json:"body"`
	ExternalURI   string `json:"externalUri"`
	ExternalTitle string `json:"externalTitle"`
}

type commentFieldsView struct {
	Body string `json:"body"`
}

type communityFieldsView struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

type imageRefView struct {
	OwnerDID string `json:"ownerDid"`
	CID      string `json:"cid"`
}

func newResultView(result classification.ResultView) resultView {
	questions := make([]string, 0, len(result.Probabilities))
	for question := range result.Probabilities {
		questions = append(questions, question)
	}
	sort.Strings(questions)
	probabilities := make([]probabilityView, 0, len(questions))
	for _, question := range questions {
		probabilities = append(probabilities, probabilityView{
			Question: question, Probability: toPartsPerMillion(result.Probabilities[question]),
		})
	}
	return resultView{
		ID:                 result.ID,
		SubjectKind:        string(result.SubjectKind),
		SubjectURI:         result.SubjectURI,
		ScoredCID:          result.ScoredCID,
		CommunityDID:       result.CommunityDID,
		AuthorDID:          result.AuthorDID,
		ModelID:            result.ModelID,
		PolicyVersion:      result.PolicyVersion,
		Probabilities:      probabilities,
		ImagesSent:         result.ImagesSent,
		ImagesSkipped:      result.ImagesSkipped,
		TextTruncated:      result.TextTruncated,
		Partial:            result.Partial,
		CreatedAt:          formatDatetime(result.CreatedAt),
		ScoredCIDIsCurrent: result.ScoredCIDIsCurrent,
		Subject:            newSubjectView(result.Subject),
	}
}

func newSubjectView(subject classification.SubjectView) subjectView {
	view := subjectView{
		Visibility: wireVisibility(subject.Visibility),
		Deleted:    subject.Deleted,
		CurrentCID: subject.CurrentCID,
		Images:     make([]imageRefView, 0, len(subject.Images)),
	}
	if subject.Post != nil {
		view.Post = &postFieldsView{
			Title: subject.Post.Title, Body: subject.Post.Body,
			ExternalURI: subject.Post.ExternalURI, ExternalTitle: subject.Post.ExternalTitle,
		}
	}
	if subject.Comment != nil {
		view.Comment = &commentFieldsView{Body: subject.Comment.Body}
	}
	if subject.Community != nil {
		view.Community = &communityFieldsView{
			Name: subject.Community.Name, DisplayName: subject.Community.DisplayName,
			Description: subject.Community.Description,
		}
	}
	for _, image := range subject.Images {
		view.Images = append(view.Images, imageRefView{OwnerDID: image.OwnerDID, CID: image.CID})
	}
	return view
}

// wireVisibility is a visibility's lowerCamel lexicon value.
func wireVisibility(visibility classification.SubjectVisibility) string {
	switch visibility {
	case classification.VisibilityAdmissionRefused:
		return "admissionRefused"
	case classification.VisibilityAwaitingAdmission:
		return "awaitingAdmission"
	default:
		return string(visibility)
	}
}

type answerView struct {
	Question string `json:"question"`
	Present  bool   `json:"present"`
}

type verdictOutput struct {
	ResultID   int64        `json:"resultId"`
	AdminDID   string       `json:"adminDid"`
	Answers    []answerView `json:"answers"`
	RecordedAt string       `json:"recordedAt"`
}

type precisionOutput struct {
	ModelID           string                  `json:"modelId"`
	PolicyVersion     string                  `json:"policyVersion"`
	Threshold         int64                   `json:"threshold"`
	VerdictedCount    int                     `json:"verdictedCount"`
	UnreviewableCount int                     `json:"unreviewableCount"`
	Questions         []questionPrecisionView `json:"questions"`
}

type questionPrecisionView struct {
	Question     string `json:"question"`
	AtOrAbove    int    `json:"atOrAbove"`
	Present      int    `json:"present"`
	TruePositive int    `json:"truePositive"`
	Precision    *int64 `json:"precision,omitempty"`
	Recall       *int64 `json:"recall,omitempty"`
}
