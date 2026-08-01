package communities

import (
	"context"
	"errors"
	"testing"
)

// stubHandleRepo implements only the lookups ResolveCommunityIdentifier needs.
// The embedded nil Repository panics loudly if resolution ever reaches for
// anything else, which keeps the stub honest as the interface grows.
type stubHandleRepo struct {
	Repository
	byHandle map[string]*Community
	byDID    map[string]*Community

	handleLookups []string
	getByHandleFn func(handle string) (*Community, error)
}

func (r *stubHandleRepo) GetByHandle(_ context.Context, handle string) (*Community, error) {
	r.handleLookups = append(r.handleLookups, handle)
	if r.getByHandleFn != nil {
		return r.getByHandleFn(handle)
	}
	if c, ok := r.byHandle[handle]; ok {
		return c, nil
	}
	return nil, ErrCommunityNotFound
}

func (r *stubHandleRepo) GetByDID(_ context.Context, did string) (*Community, error) {
	if c, ok := r.byDID[did]; ok {
		return c, nil
	}
	return nil, ErrCommunityNotFound
}

func newStubRepo(communities ...*Community) *stubHandleRepo {
	repo := &stubHandleRepo{
		byHandle: make(map[string]*Community, len(communities)),
		byDID:    make(map[string]*Community, len(communities)),
	}
	for _, c := range communities {
		repo.byHandle[c.Handle] = c
		repo.byDID[c.DID] = c
	}
	return repo
}

func TestResolveCommunityIdentifier_HandleForms(t *testing.T) {
	// A community provisioned on this instance: handle carries the c- prefix.
	local := &Community{DID: "did:plc:local123", Handle: "c-gardening.coves.social", Name: "gardening"}
	// A bridged community: handle is the source platform's, no prefix.
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

			did, err := svc.ResolveCommunityIdentifier(context.Background(), tt.identifier)
			if err != nil {
				t.Fatalf("ResolveCommunityIdentifier(%q) returned error: %v", tt.identifier, err)
			}
			if did != tt.wantDID {
				t.Errorf("ResolveCommunityIdentifier(%q) = %q, want %q", tt.identifier, did, tt.wantDID)
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

func TestResolveCommunityIdentifier_UnknownHandle(t *testing.T) {
	repo := newStubRepo()
	svc := &communityService{repo: repo, instanceDomain: "coves.social"}

	_, err := svc.ResolveCommunityIdentifier(context.Background(), "nope.coves.social")
	if !errors.Is(err, ErrCommunityNotFound) {
		t.Fatalf("error = %v, want it to wrap ErrCommunityNotFound", err)
	}
	// Both forms must be tried before reporting the miss.
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

// A database failure must surface as itself, not be reported as "not found" —
// and must not trigger the prefixed retry, which would mask the outage.
func TestResolveCommunityIdentifier_RepoErrorIsNotSwallowed(t *testing.T) {
	dbDown := errors.New("connection refused")
	repo := newStubRepo()
	repo.getByHandleFn = func(string) (*Community, error) { return nil, dbDown }
	svc := &communityService{repo: repo, instanceDomain: "coves.social"}

	_, err := svc.ResolveCommunityIdentifier(context.Background(), "gardening.coves.social")
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
