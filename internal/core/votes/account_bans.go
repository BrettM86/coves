package votes

import (
	"context"
	"fmt"
	"time"

	"Coves/internal/core/accountbans"
)

// WithAccountBans installs the instance account-ban read. With it, a voter
// whose account is banned is refused with ErrAccountBanned before anything reaches
// their PDS.
func WithAccountBans(reader accountbans.Reader) ServiceOption {
	return func(s *voteService) { s.accountBans = reader }
}

// refuseAccountBanned returns ErrAccountBanned when accountDID has an instance
// account ban in force. A lookup error fails the request.
func (s *voteService) refuseAccountBanned(ctx context.Context, accountDID string) error {
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
