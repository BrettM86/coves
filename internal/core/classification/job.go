package classification

import (
	"context"
	"time"
)

// SubjectKind names what kind of record a classification job is about.
type SubjectKind string

// SubjectKindPost is a post subject.
const SubjectKindPost SubjectKind = "post"

// SubjectKindComment is a comment subject.
const SubjectKindComment SubjectKind = "comment"

// SubjectKindCommunity is a community subject.
const SubjectKindCommunity SubjectKind = "community"

// Job statuses. Queued, deferred and retrying jobs are non-terminal;
// classified, closed and failed jobs are terminal.
const (
	StatusQueued     = "queued"
	StatusDeferred   = "deferred"
	StatusRetrying   = "retrying"
	StatusClassified = "classified"
	StatusClosed     = "closed"
	StatusFailed     = "failed"
)

// Evaluation is the policy and models a subject is classified under; with the
// subject URI and record CID it is a job's identity.
type Evaluation struct {
	PolicyVersion string
	TextModelID   string
	ImageModelID  string
}

// NewJob is a classification job to enqueue.
type NewJob struct {
	SubjectURI   string
	SubjectKind  SubjectKind
	RecordCID    string
	AuthorDID    string
	CommunityDID string
	Evaluation   Evaluation
}

// ClaimedJob is a due job the worker claimed. NextAttemptAt is the job's
// next_attempt_at before the claim pushed it forward as a lease, so
// ReleaseClaim can restore it. DeferralCount is how many times in a row the
// job was deferred awaiting admission.
type ClaimedJob struct {
	ID            int64
	SubjectURI    string
	SubjectKind   SubjectKind
	RecordCID     string
	AuthorDID     string
	CommunityDID  string
	Evaluation    Evaluation
	Status        string
	AttemptCount  int
	DeferralCount int
	NextAttemptAt time.Time
}

// SubjectVisibility is whether a subject may be classified now.
type SubjectVisibility string

const (
	// VisibilityMissing means no indexed record exists for the subject URI.
	VisibilityMissing SubjectVisibility = "missing"
	// VisibilityHidden means the subject is hidden from an anonymous viewer
	// whatever its admission: deleted, under an active removal decision, by an
	// account restricted on the instance (account ban or PDS account status),
	// or in a community the instance removed.
	VisibilityHidden SubjectVisibility = "hidden"
	// VisibilityAdmissionRefused means the subject's community rejected or
	// removed it.
	VisibilityAdmissionRefused SubjectVisibility = "admission_refused"
	// VisibilityAwaitingAdmission means the subject's community has not yet
	// accepted its current record: admission pending or pending
	// re-acceptance, an acceptance pinned to another CID, or no admission row
	// for a postv2 record.
	VisibilityAwaitingAdmission SubjectVisibility = "awaiting_admission"
	// VisibilityVisible means the subject is publicly visible.
	VisibilityVisible SubjectVisibility = "visible"
)

// PostSubject is the indexed post a job is about. CID and Fields are empty
// when Visibility is VisibilityMissing.
type PostSubject struct {
	Visibility SubjectVisibility
	CID        string
	Fields     PostFields
	// ImageRefs are the post's images to classify; ImagesSkipped counts
	// image slots that yield no ref.
	ImageRefs     []ImageRef
	ImagesSkipped int
}

// CommentSubject is the indexed comment a job is about. CommunityDID is the
// community of the post the comment's parent chain reaches, empty when that
// post is not indexed yet. CID and Body are empty when Visibility is
// VisibilityMissing.
type CommentSubject struct {
	Visibility   SubjectVisibility
	CID          string
	CommunityDID string
	Body         string
	// ImageRefs are the comment's images to classify; ImagesSkipped counts
	// image slots that yield no ref.
	ImageRefs     []ImageRef
	ImagesSkipped int
}

// CommunitySubject is the indexed community a job is about. CID and Fields
// are empty when Visibility is VisibilityMissing.
type CommunitySubject struct {
	Visibility SubjectVisibility
	CID        string
	Fields     CommunityFields
	// ImageRefs are the community's avatar and banner; ImagesSkipped counts
	// image slots that yield no ref.
	ImageRefs     []ImageRef
	ImagesSkipped int
}

// Failure is a failed classification request to record on a job: the
// attempt count goes up by one and last_error_code becomes ErrorCode. A Final
// failure ends the job failed; otherwise it is retrying, due at RetryAt.
type Failure struct {
	ErrorCode string
	Final     bool
	RetryAt   time.Time
}

// Result is a classification to store for a job. The subject identity and
// policy version come from the job.
type Result struct {
	ModelID       string
	Probabilities map[string]float64
	ImagesSent    int
	ImagesSkipped int
	TextTruncated bool
	// AlertQuestions are the question keys whose score alert thresholds the
	// result meets.
	AlertQuestions []string
	// Partial marks a result stored from the answered batches of a job that
	// failed at the attempt limit; the images never sent count as skipped.
	Partial bool
}

// Repository is the classification queue the worker reads and writes. Every
// method that changes a job acts only on a non-terminal job.
type Repository interface {
	// ClaimDue claims up to limit non-terminal jobs under evaluation due at
	// now (next_attempt_at <= now), skipping jobs another claim holds, and
	// pushes their next_attempt_at to now+lease so they are not claimed again
	// while in flight. Jobs under another evaluation are left alone.
	ClaimDue(ctx context.Context, evaluation Evaluation, now time.Time, limit int, lease time.Duration) ([]ClaimedJob, error)
	// LoadPostSubject reads the indexed post at subjectURI and its visibility.
	LoadPostSubject(ctx context.Context, subjectURI string) (PostSubject, error)
	// LoadCommentSubject reads the indexed comment at subjectURI, its
	// visibility and the community its parent chain resolves to.
	LoadCommentSubject(ctx context.Context, subjectURI string) (CommentSubject, error)
	// LoadCommunitySubject reads the indexed community communityDID and its
	// visibility.
	LoadCommunitySubject(ctx context.Context, communityDID string) (CommunitySubject, error)
	// Close ends the job closed with reason (CloseReasonSuperseded or
	// CloseReasonNotVisible).
	Close(ctx context.Context, jobID int64, reason string) error
	// Defer makes the job deferred, due at until, and counts the deferral in
	// deferral_count; attempt_count is unchanged.
	Defer(ctx context.Context, jobID int64, until time.Time) error
	// ReleaseClaim gives a claimed job back unchanged: its status, attempt
	// count and pre-claim next_attempt_at.
	ReleaseClaim(ctx context.Context, job ClaimedJob) error
	// RecordFailure records a failed request on the job. A Final failure also
	// discards the job's stored batches, in the same statement, and reports
	// how many it discarded.
	RecordFailure(ctx context.Context, jobID int64, failure Failure) (batchesDiscarded int, err error)
	// ReserveRequest counts one request for modelID on the UTC date of now
	// against dailyCap, which caps the day's requests across all models
	// together, reporting false when the day's cap is reached.
	ReserveRequest(ctx context.Context, modelID string, now time.Time, dailyCap int) (bool, error)
	// ReleaseRequest gives back one request reserved for modelID on the UTC
	// date of reservedAt, never taking the count below zero; it does nothing
	// when the date has no usage row for the model.
	ReleaseRequest(ctx context.Context, modelID string, reservedAt time.Time) error
	// StoreResult stores the result and marks the job classified in one
	// transaction, writing the claimed job's CommunityDID to the result and
	// the job, reporting false (and storing nothing) when the job no
	// longer exists or is already terminal, as after account erasure. A
	// partial result the job already holds (it was revived) is replaced in
	// place, keeping its ID and verdicts.
	StoreResult(ctx context.Context, job ClaimedJob, result Result) (bool, error)
	// StorePartialResult records the job's final failure and stores result,
	// marked partial, in one transaction, under the same rules as
	// StoreResult: the job ends failed with the failure counted, and its
	// stored batches are discarded.
	StorePartialResult(ctx context.Context, job ClaimedJob, result Result, failure Failure) (bool, error)
	// LoadBatchProgress reads the job's completed image batches, ordered by
	// Index.
	LoadBatchProgress(ctx context.Context, jobID int64) ([]BatchProgress, error)
	// StoreBatchProgress stores one completed batch and adds its InputTokens
	// to its ModelID's usage on the UTC date of now, in one transaction. The
	// tokens are added even when the job is no longer non-terminal, since the
	// request was sent; the batch is then not stored and it reports false.
	StoreBatchProgress(ctx context.Context, jobID int64, progress BatchProgress, now time.Time) (bool, error)
}
