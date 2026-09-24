package moderation

// NewAllowlistAuthority builds an Authority from the configured admin DIDs.
func NewAllowlistAuthority(dids []string) Authority {
	allowed := make(map[string]struct{}, len(dids))
	for _, did := range dids {
		allowed[did] = struct{}{}
	}
	return allowlistAuthority{allowed: allowed}
}

type allowlistAuthority struct {
	allowed map[string]struct{}
}

func (a allowlistAuthority) IsInstanceAdmin(did string) bool {
	_, ok := a.allowed[did]
	return ok
}
