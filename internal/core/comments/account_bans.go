package comments

import (
	"context"
	"fmt"
	"time"

	"Coves/internal/core/accountbans"
)

// WithAccountBans installs the instance account-ban read. With it, a
// commenter whose account is banned is refused with ErrAccountBanned before anything
// is written to their PDS.
func WithAccountBans(reader accountbans.Reader) CommentServiceOption {
	return func(s *commentService) { s.accountBans = reader }
}

// refuseAccountBanned returns ErrAccountBanned when accountDID has an instance
// account ban in force. A lookup error fails the request.
func (s *commentService) refuseAccountBanned(ctx context.Context, accountDID string) error {
	if s.accountBans == nil {
		return nil
	}
	ban, err := s.accountBans.ActiveBan(ctx, accountDID)
	if err != nil {
		return fmt.Errorf("check account ban: %w", err)
	}
	if ban.InForce(time.Now()) {
		return ErrAccountBanned
	}
	return nil
}

// WithInstanceDID names this instance's moderation authority for the
// placeholders of a banned account's comments.
func WithInstanceDID(instanceDID string) CommentServiceOption {
	return func(s *commentService) { s.instanceDID = instanceDID }
}
