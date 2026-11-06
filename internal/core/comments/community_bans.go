package comments

import (
	"context"
	"fmt"
	"time"

	"Coves/internal/core/communities"
)

// CommentServiceOption configures a comment service.
type CommentServiceOption func(*commentService)

// WithCommunityBans installs the community ban index. With it, a commenter
// banned in the community of the thread they reply in is refused with
// ErrBanned before anything is written to their PDS.
func WithCommunityBans(index communities.CommunityBanIndex) CommentServiceOption {
	return func(s *commentService) { s.communityBans = index }
}

// refuseBannedCommenter returns ErrBanned when commenterDID has an active ban
// in the community of the thread being replied to. The parent chain is
// authoritative: when parentURI resolves, that community decides and the
// client-supplied rootURI is ignored. Only when the parent is not indexed yet
// is rootURI resolved, and then only to refuse — a root in an unbanned
// community never admits a reply whose parent resolves elsewhere. Content
// unindexed on both paths is admitted; index errors fail the request.
// An instance account ban in force refuses first, whatever the thread. A
// resolved community under an instance removal refuses with
// ErrCommunityRemoved before the community ban is read.
func (s *commentService) refuseBannedCommenter(ctx context.Context, commenterDID, parentURI, rootURI string) error {
	if err := s.refuseAccountBanned(ctx, commenterDID); err != nil {
		return err
	}
	if s.communityBans == nil {
		return nil
	}
	communityDID, found, err := s.communityBans.ContentCommunity(ctx, parentURI)
	if err != nil {
		return fmt.Errorf("resolve community for ban check: %w", err)
	}
	if !found {
		communityDID, found, err = s.communityBans.ContentCommunity(ctx, rootURI)
		if err != nil {
			return fmt.Errorf("resolve root community for ban check: %w", err)
		}
		if !found {
			return nil
		}
	}
	if err := s.refuseRemovedCommunity(ctx, communityDID); err != nil {
		return err
	}
	ban, err := communities.ActiveCommunityBan(ctx, s.communityBans, communityDID, commenterDID, time.Now())
	if err != nil {
		return fmt.Errorf("check community ban: %w", err)
	}
	if ban != nil {
		return ErrBanned
	}
	return nil
}

// WithCommunityRemovals installs the community removal reader. With it (and
// WithCommunityBans, which resolves the thread's community), a comment in a
// community under an instance removal is refused with ErrCommunityRemoved
// before anything is written to the commenter's PDS.
func WithCommunityRemovals(reader communities.RemovalReader) CommentServiceOption {
	return func(s *commentService) { s.communityRemovals = reader }
}

// refuseRemovedCommunity returns ErrCommunityRemoved when communityDID is under
// an instance removal. A reader error refuses the request (fail closed).
func (s *commentService) refuseRemovedCommunity(ctx context.Context, communityDID string) error {
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
