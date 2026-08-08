package posts

import (
	"context"
	"strings"
	"time"
)

// The read side of an admission decision: social.coves.community.post.getStatus
// (docs/PRD_AUTHOR_OWNED_POSTS.md §3.4).
//
// It exists because a rejection is AppView-LOCAL. §3.3 is explicit that a
// submission refused before it was ever accepted writes NO community record —
// spam must not be archived forever in the repo of the community that refused
// it — so there is nothing on the firehose for an author's client to read, and
// "did it get in, and if not, why" has no answer except to ask the host.
//
// It is deliberately its own service rather than a method on Service. Service
// is the write path plus post hydration and is implemented by test doubles all
// over the suite; a status query needs the admissions repository and nothing
// else, and widening the big interface to reach it would make every one of
// those doubles carry a method it has no opinion about.

// PostStatus is one community's answer about one post.
//
// The optional fields are pointers rather than zero strings because their
// absence is meaningful and different from emptiness: a pending post has no
// decision to report, and rendering an empty code would tell an author their
// post was refused for a reason nobody can name.
type PostStatus struct {
	// Status is the admission state (§6.1), verbatim: the same vocabulary the
	// admissions table and the consumer speak, so a client that switches on it
	// is switching on the real state machine and not a display translation.
	Status AdmissionStatus

	// DecisionCode is set for rejected and removed, and only for those. It is
	// the vocabulary of DecisionCode — both the codes a community publishes in
	// a removal record and the admission-time codes that never reach a repo.
	DecisionCode *string

	// DecisionAt is when the decision above was made.
	DecisionAt *time.Time

	// AcceptanceURI names the live community acceptance record, so a client can
	// go read the signed attestation rather than taking this AppView's word for
	// it. Set only while an acceptance stands.
	AcceptanceURI *string
}

// GetStatusRequest names one subject: which community's answer, about which
// post.
//
// Both halves are required and neither has a default. A post can hold
// independent decisions from several communities (§2, forks), so "the status of
// this post" is not a question with one answer, and a request that omitted the
// community would have to invent one.
type GetStatusRequest struct {
	PostURI      string
	CommunityDID string
}

// StatusService answers getStatus.
type StatusService interface {
	// GetStatus returns one community's decision about one post, or ErrNotFound
	// when the community has never seen it.
	GetStatus(ctx context.Context, req GetStatusRequest) (*PostStatus, error)
}

type statusService struct {
	admissions AdmissionRepository
}

// NewStatusService wires the status query over the admissions store.
func NewStatusService(admissions AdmissionRepository) StatusService {
	return &statusService{admissions: admissions}
}

// GetStatus reads one community's decision about one post.
//
// Both halves of the subject are required rather than defaulted, because a
// post genuinely carries independent decisions from several communities (§2)
// and answering about whichever row was found first would report one
// community's verdict as though it were another's.
func (s *statusService) GetStatus(ctx context.Context, req GetStatusRequest) (*PostStatus, error) {
	if strings.TrimSpace(req.PostURI) == "" {
		return nil, NewValidationError("post", "post URI is required")
	}
	if strings.TrimSpace(req.CommunityDID) == "" {
		return nil, NewValidationError("community", "community DID is required")
	}

	admission, err := s.admissions.Get(ctx, req.CommunityDID, req.PostURI)
	if err != nil {
		// ErrNotFound travels out unchanged: a subject the community has never
		// been offered is a genuine 404, not a status to invent. Reporting it
		// as `pending` would promise the author that somebody is going to
		// decide.
		return nil, err
	}

	status := &PostStatus{
		Status: admission.Status,
		// The live acceptance record, and only while one stands. The repository
		// clears these columns on removal and never sets them on a rejection,
		// so this is the acceptance a caller can actually go and read.
		AcceptanceURI: admission.AcceptanceURI,
	}

	// The decision fields are gated on the status rather than copied blind.
	// They describe a REFUSAL, and the two statuses above are the only ones a
	// refusal produces; surfacing a code beside `pending` would tell an author
	// their post was refused while it is still waiting.
	if admission.Status == AdmissionStatusRejected || admission.Status == AdmissionStatusRemoved {
		status.DecisionCode = admission.DecisionCode
		status.DecisionAt = admission.DecisionAt
	}

	return status, nil
}
