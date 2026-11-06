package users

import (
	"context"
	"fmt"
	"time"

	"Coves/internal/core/accountbans"
)

// AccountStatusSuspended is the accountStatus of a banned account's profile
// as every other viewer reads it.
const AccountStatusSuspended = "suspended"

// WithAccountBans installs the instance account-ban read used by GetProfile.
// Without it no profile reads as suspended and none carries a ban notice.
func WithAccountBans(reader accountbans.Reader) UserServiceOption {
	return func(s *userService) { s.accountBans = reader }
}

// accountBanInForce returns the account's instance ban when it is in force on
// the server's clock, or nil. A lookup error fails the read rather than serve
// a banned account's profile.
func (s *userService) accountBanInForce(ctx context.Context, did string) (*accountbans.Ban, error) {
	if s.accountBans == nil {
		return nil, nil
	}
	ban, err := s.accountBans.ActiveBan(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("check account ban: %w", err)
	}
	if !ban.InForce(time.Now()) {
		return nil, nil
	}
	return ban, nil
}

// suspendedProfile is the profile of a banned account as every other viewer
// reads it, like Bluesky's takedown: identity (did, handle, createdAt) and the
// suspended status, with zero activity and no display name, description,
// avatar, banner or origin.
func suspendedProfile(profile *ProfileViewDetailed) *ProfileViewDetailed {
	return &ProfileViewDetailed{
		DID:           profile.DID,
		Handle:        profile.Handle,
		CreatedAt:     profile.CreatedAt,
		Stats:         &ProfileStats{},
		AccountStatus: AccountStatusSuspended,
	}
}
