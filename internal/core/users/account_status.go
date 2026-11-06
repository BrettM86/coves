package users

import (
	"context"
	"fmt"

	"Coves/internal/core/accountstatus"
)

// WithAccountStatusRestrictions installs the PDS-confirmed account status read
// used by GetProfile. Without it a takendown or suspended account's profile
// reads normally.
func WithAccountStatusRestrictions(reader accountstatus.Reader) UserServiceOption {
	return func(s *userService) { s.accountStatuses = reader }
}

// accountStatusRestricted reports whether the account has a stored takendown
// or suspended status. A lookup error fails the read rather than serve a
// restricted account's profile.
func (s *userService) accountStatusRestricted(ctx context.Context, did string) (bool, error) {
	if s.accountStatuses == nil {
		return false, nil
	}
	_, found, err := s.accountStatuses.Restriction(ctx, did)
	if err != nil {
		return false, fmt.Errorf("check account status: %w", err)
	}
	return found, nil
}
