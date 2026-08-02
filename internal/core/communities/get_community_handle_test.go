package communities

import (
	"context"
	"errors"
	"testing"
)

// GetCommunity is the endpoint backing social.coves.community.get, the lookup
// clients hit when opening /c/{handle}. It resolves handles independently of
// ResolveCommunityIdentifier, so the prefix fallback has to be exercised here
// too — covering only the resolver left this path 404ing on the prefix-free
// handles clients actually link to.
func TestGetCommunity_HandleForms(t *testing.T) {
	local := &Community{DID: "did:plc:local123", Handle: "c-gardening.coves.social", Name: "gardening"}
	bridged := &Community{DID: "did:plc:bridged456", Handle: "selfhosted.lemmy-world.tdpl.io", Name: "selfhosted"}

	tests := []struct {
		name        string
		identifier  string
		wantDID     string
		wantLookups []string
	}{
		{
			name:        "prefixed handle resolves on the first lookup",
			identifier:  "c-gardening.coves.social",
			wantDID:     local.DID,
			wantLookups: []string{"c-gardening.coves.social"},
		},
		{
			name:        "bare handle falls back to the prefixed form",
			identifier:  "gardening.coves.social",
			wantDID:     local.DID,
			wantLookups: []string{"gardening.coves.social", "c-gardening.coves.social"},
		},
		{
			name:        "bridged handle resolves without a prefixed retry",
			identifier:  "selfhosted.lemmy-world.tdpl.io",
			wantDID:     bridged.DID,
			wantLookups: []string{"selfhosted.lemmy-world.tdpl.io"},
		},
		{
			name:        "at-identifier prefix is stripped before lookup",
			identifier:  "@gardening.coves.social",
			wantDID:     local.DID,
			wantLookups: []string{"gardening.coves.social", "c-gardening.coves.social"},
		},
		{
			name:        "handle is lowercased before lookup",
			identifier:  "Gardening.Coves.Social",
			wantDID:     local.DID,
			wantLookups: []string{"gardening.coves.social", "c-gardening.coves.social"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newStubRepo(local, bridged)
			svc := &communityService{repo: repo, instanceDomain: "coves.social"}

			community, err := svc.GetCommunity(context.Background(), tt.identifier)
			if err != nil {
				t.Fatalf("GetCommunity(%q) returned error: %v", tt.identifier, err)
			}
			if community.DID != tt.wantDID {
				t.Errorf("GetCommunity(%q).DID = %q, want %q", tt.identifier, community.DID, tt.wantDID)
			}
			if len(repo.handleLookups) != len(tt.wantLookups) {
				t.Fatalf("handle lookups = %v, want %v", repo.handleLookups, tt.wantLookups)
			}
			for i, want := range tt.wantLookups {
				if repo.handleLookups[i] != want {
					t.Errorf("handle lookup %d = %q, want %q", i, repo.handleLookups[i], want)
				}
			}
		})
	}
}

func TestGetCommunity_UnknownHandle(t *testing.T) {
	repo := newStubRepo()
	svc := &communityService{repo: repo, instanceDomain: "coves.social"}

	_, err := svc.GetCommunity(context.Background(), "nope.coves.social")
	if !errors.Is(err, ErrCommunityNotFound) {
		t.Fatalf("error = %v, want it to wrap ErrCommunityNotFound", err)
	}
	want := []string{"nope.coves.social", "c-nope.coves.social"}
	if len(repo.handleLookups) != len(want) {
		t.Fatalf("handle lookups = %v, want %v", repo.handleLookups, want)
	}
	for i, w := range want {
		if repo.handleLookups[i] != w {
			t.Errorf("handle lookup %d = %q, want %q", i, repo.handleLookups[i], w)
		}
	}
}

// A database failure must surface as itself rather than as a missing community,
// and must not trigger the prefixed retry, which would double the load on an
// already-failing database.
func TestGetCommunity_RepoErrorIsNotSwallowed(t *testing.T) {
	dbDown := errors.New("connection refused")
	repo := newStubRepo()
	repo.getByHandleFn = func(string) (*Community, error) { return nil, dbDown }
	svc := &communityService{repo: repo, instanceDomain: "coves.social"}

	_, err := svc.GetCommunity(context.Background(), "gardening.coves.social")
	if !errors.Is(err, dbDown) {
		t.Fatalf("error = %v, want it to wrap the repository error", err)
	}
	if errors.Is(err, ErrCommunityNotFound) {
		t.Error("repository failure was misreported as ErrCommunityNotFound")
	}
	if len(repo.handleLookups) != 1 {
		t.Errorf("handle lookups = %v, want a single attempt with no prefixed retry", repo.handleLookups)
	}
}
