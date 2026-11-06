package votes

import (
	"context"
	"fmt"
	"time"

	"Coves/internal/core/communities"
)

// ServiceOption configures a vote service.
type ServiceOption func(*voteService)

// WithCommunityBans installs the community ban index. With it, a voter banned
// in the community of the subject (a post, or the root post of a comment's
// thread) is refused with ErrBanned before anything reaches their PDS.
func WithCommunityBans(index communities.CommunityBanIndex) ServiceOption {
	return func(s *voteService) { s.communityBans = index }
}

// refuseBannedVoter returns ErrBanned when voterDID has an active ban in the
// community of subjectURI. An unindexed subject is admitted, since vote create
// deliberately does not require the subject to exist; index errors fail the
// request.
// An instance account ban in force refuses first, whatever the subject. A
// resolved community under an instance removal refuses with
// ErrCommunityRemoved before the community ban is read.
func (s *voteService) refuseBannedVoter(ctx context.Context, voterDID, subjectURI string) error {
	if err := s.refuseAccountBanned(ctx, voterDID); err != nil {
		return err
	}
	if s.communityBans == nil {
		return nil
	}
	communityDID, found, err := s.communityBans.ContentCommunity(ctx, subjectURI)
	if err != nil {
		return fmt.Errorf("resolve community for ban check: %w", err)
	}
	if !found {
		return nil
	}
	if err := s.refuseRemovedCommunity(ctx, communityDID); err != nil {
		return err
	}
	ban, err := communities.ActiveCommunityBan(ctx, s.communityBans, communityDID, voterDID, time.Now())
	if err != nil {
		return fmt.Errorf("check community ban: %w", err)
	}
	if ban != nil {
		return ErrBanned
	}
	return nil
}

// WithCommunityRemovals installs the community removal reader. With it (and
// WithCommunityBans, which resolves the subject's community), a vote on a
// subject in a community under an instance removal is refused with
// ErrCommunityRemoved before anything reaches the voter's PDS.
func WithCommunityRemovals(reader communities.RemovalReader) ServiceOption {
	return func(s *voteService) { s.communityRemovals = reader }
}

// refuseRemovedCommunity returns ErrCommunityRemoved when communityDID is under
// an instance removal. A reader error refuses the request (fail closed).
func (s *voteService) refuseRemovedCommunity(ctx context.Context, communityDID string) error {
	if s.communityRemovals == nil {
		return nil
	}
	removed, err := s.communityRemovals.CommunityRemoved(ctx, communityDID)
	if err != nil {
		return fmt.Errorf("check community removal: %w", err)
	}
	if removed {
		return ErrCommunityRemoved
	}
	return nil
}
