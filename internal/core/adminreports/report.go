package adminreports

import (
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Report represents an admin report in the AppView database
// Reports are created by users to flag serious content for admin review
type Report struct {
	ID              int64      `json:"id" db:"id"`
	ReporterDID     string     `json:"reporterDid" db:"reporter_did"`
	TargetURI       string     `json:"targetUri" db:"target_uri"`
	TargetType      TargetType `json:"targetType" db:"target_type"`
	Reason          Reason     `json:"reason" db:"reason"`
	Explanation     string     `json:"explanation,omitempty" db:"explanation"`
	Status          Status     `json:"status" db:"status"`
	ResolvedBy      *string    `json:"resolvedBy,omitempty" db:"resolved_by"`
	ResolutionNotes *string    `json:"resolutionNotes,omitempty" db:"resolution_notes"`
	CreatedAt       time.Time  `json:"createdAt" db:"created_at"`
	ResolvedAt      *time.Time `json:"resolvedAt,omitempty" db:"resolved_at"`
}

// Reason represents the category of an admin report
type Reason string

// Valid reason values for admin reports
const (
	ReasonCSAM       Reason = "csam"
	ReasonDoxing     Reason = "doxing"
	ReasonHarassment Reason = "harassment"
	ReasonSpam       Reason = "spam"
	ReasonIllegal    Reason = "illegal"
	ReasonOther      Reason = "other"
)

// Status represents the processing status of an admin report
type Status string

// Valid status values for admin reports
const (
	StatusOpen      Status = "open"
	StatusReviewing Status = "reviewing"
	StatusResolved  Status = "resolved"
	StatusDismissed Status = "dismissed"
)

// TargetType represents the type of content being reported
type TargetType string

// Valid target types for admin reports
const (
	TargetTypePost    TargetType = "post"
	TargetTypeComment TargetType = "comment"
)

// ValidReasons returns all valid reason values
func ValidReasons() []Reason {
	return []Reason{ReasonCSAM, ReasonDoxing, ReasonHarassment, ReasonSpam, ReasonIllegal, ReasonOther}
}

// ValidStatuses returns all valid status values
func ValidStatuses() []Status {
	return []Status{StatusOpen, StatusReviewing, StatusResolved, StatusDismissed}
}

// ValidTargetTypes returns all valid target type values
func ValidTargetTypes() []TargetType {
	return []TargetType{TargetTypePost, TargetTypeComment}
}

// IsValidReason checks if a reason value is valid
func IsValidReason(reason string) bool {
	for _, r := range ValidReasons() {
		if string(r) == reason {
			return true
		}
	}
	return false
}

// IsValidStatus checks if a status value is valid
func IsValidStatus(status string) bool {
	for _, s := range ValidStatuses() {
		if string(s) == status {
			return true
		}
	}
	return false
}

// IsValidTargetType checks if a target type value is valid
func IsValidTargetType(targetType string) bool {
	for _, t := range ValidTargetTypes() {
		if string(t) == targetType {
			return true
		}
	}
	return false
}

// MaxExplanationLength is the maximum number of characters allowed in an explanation
const MaxExplanationLength = 1000

// SubmitReportRequest contains the data needed to submit a new report
type SubmitReportRequest struct {
	// ReporterDID is the DID of the user submitting the report
	ReporterDID string

	// TargetURI is the AT Protocol URI of the content being reported
	TargetURI string

	// Reason is the category of the report
	Reason string

	// Explanation is an optional description of the issue
	Explanation string
}

// atURIPattern validates AT Protocol URIs with proper structure:
// at://did:plc:xxx/collection/rkey or at://did:web:xxx/collection/rkey
// Note: This validation focuses on structure rather than strict DID format validation,
// which is the responsibility of the PDS. The pattern allows alphanumeric DID identifiers.
var atURIPattern = regexp.MustCompile(`^at://did:(plc:[a-zA-Z0-9]+|web:[a-zA-Z0-9.-]+)/[a-zA-Z0-9.]+/[a-zA-Z0-9_-]+$`)

// Validate validates the SubmitReportRequest and returns an error if invalid
func (r *SubmitReportRequest) Validate() error {
	// Validate reporter DID
	if r.ReporterDID == "" {
		return ErrReporterRequired
	}

	// Validate reason is one of the allowed values
	if !IsValidReason(r.Reason) {
		return ErrInvalidReason
	}

	// Validate target URI is a proper AT Protocol URI
	if !isValidATURI(r.TargetURI) {
		return ErrInvalidTarget
	}

	// Validate explanation length (max 1000 characters, using proper character counting)
	if utf8.RuneCountInString(r.Explanation) > MaxExplanationLength {
		return ErrExplanationTooLong
	}

	return nil
}

// isValidATURI validates that the URI is a proper AT Protocol URI
// AT Protocol URIs have the format: at://did:plc:xxx/collection/rkey
// or at://did:web:xxx/collection/rkey
func isValidATURI(uri string) bool {
	// Check basic prefix
	if !strings.HasPrefix(uri, "at://") {
		return false
	}

	// Validate the full URI pattern
	return atURIPattern.MatchString(uri)
}

// determineTargetType determines whether the target is a post or comment based on the URI
// AT Protocol URIs have the format: at://did:plc:xxx/collection/rkey
// For Coves, the collection will contain "post" or "comment"
func determineTargetType(uri string) TargetType {
	// Check if the URI contains common post or comment collection patterns
	lowerURI := strings.ToLower(uri)

	if strings.Contains(lowerURI, "comment") {
		return TargetTypeComment
	}

	// Log when defaulting to post for unknown target types
	if !strings.Contains(lowerURI, "post") {
		log.Printf("[ADMIN_REPORT] Unknown target type in URI, defaulting to post: %s", uri)
	}

	return TargetTypePost
}

// NewReport creates a new Report from a validated SubmitReportRequest
// This constructor ensures that reports are created with proper defaults and validation
// The request must be validated before calling this function
func NewReport(req SubmitReportRequest) (*Report, error) {
	// Validate the request first
	if err := req.Validate(); err != nil {
		return nil, err
	}

	return &Report{
		ReporterDID: req.ReporterDID,
		TargetURI:   req.TargetURI,
		TargetType:  determineTargetType(req.TargetURI),
		Reason:      Reason(req.Reason),
		Explanation: req.Explanation,
		Status:      StatusOpen,
		CreatedAt:   time.Now().UTC(),
	}, nil
}
