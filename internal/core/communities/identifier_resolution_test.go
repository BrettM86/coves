package communities

import (
	"context"
	"errors"
	"fmt"
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

	pairLookups [][2]string
}

// GetByNameAndOrigin answers from the seeded rows with the repository's
// contract: no match is a miss, two matches is ambiguity.
func (r *stubHandleRepo) GetByNameAndOrigin(_ context.Context, name, origin string) (*Community, error) {
	r.pairLookups = append(r.pairLookups, [2]string{name, origin})
	var match *Community
	for _, c := range r.byDID {
		if c.Name == name && c.Origin == origin {
			if match != nil {
				return nil, ErrAmbiguousCommunity
			}
			match = c
		}
	}
	if match == nil {
		return nil, ErrCommunityNotFound
	}
	return match, nil
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

// TestResolveCommunityIdentifier_ScopedForms pins the parse precedence for the
// name@origin family: a leading "!" or a bare "@" is stripped, a local origin
// takes the handle fast path, a remote origin goes to the (name, origin)
// lookup, and a bare name means "on this instance".
func TestResolveCommunityIdentifier_ScopedForms(t *testing.T) {
	local := &Community{DID: "did:plc:local123", Handle: "c-gaming.coves.social", Name: "gaming"}
	bridged := &Community{DID: "did:plc:bridged456", Handle: "comicstrips.lemmy-world.tdpl.io", Name: "comicstrips", Origin: "lemmy.world"}
	// A federated Coves community indexed before the origin column existed:
	// EffectiveOrigin advertises othercoves.social for it, so that has to
	// resolve, and the only thing that can find it is its handle.
	federated := &Community{DID: "did:plc:federated789", Handle: "c-gaming.othercoves.social", Name: "gaming"}

	tests := []struct {
		name            string
		identifier      string
		wantDID         string
		wantHandleLooks []string
		wantPairLooks   [][2]string
	}{
		{
			name:            "bare name is a local community",
			identifier:      "gaming",
			wantDID:         local.DID,
			wantHandleLooks: []string{"c-gaming.coves.social"},
		},
		{
			name:            "bare name is lower-cased",
			identifier:      "Gaming",
			wantDID:         local.DID,
			wantHandleLooks: []string{"c-gaming.coves.social"},
		},
		{
			name:            "!name@local takes the handle fast path",
			identifier:      "!gaming@coves.social",
			wantDID:         local.DID,
			wantHandleLooks: []string{"c-gaming.coves.social"},
		},
		{
			name:            "name@local without the bang takes the same path",
			identifier:      "gaming@Coves.Social",
			wantDID:         local.DID,
			wantHandleLooks: []string{"c-gaming.coves.social"},
		},
		{
			name:          "name@remote resolves by (name, origin)",
			identifier:    "comicstrips@lemmy.world",
			wantDID:       bridged.DID,
			wantPairLooks: [][2]string{{"comicstrips", "lemmy.world"}},
		},
		{
			name:          "!name@remote resolves by (name, origin)",
			identifier:    "!ComicStrips@Lemmy.World",
			wantDID:       bridged.DID,
			wantPairLooks: [][2]string{{"comicstrips", "lemmy.world"}},
		},
		{
			name:            "DNS handle of a bridged community is still a handle lookup",
			identifier:      "comicstrips.lemmy-world.tdpl.io",
			wantDID:         bridged.DID,
			wantHandleLooks: []string{"comicstrips.lemmy-world.tdpl.io"},
		},
		{
			name:            "name@remote with no stored pair falls back to the c-{name}.{origin} handle",
			identifier:      "gaming@othercoves.social",
			wantDID:         federated.DID,
			wantPairLooks:   [][2]string{{"gaming", "othercoves.social"}},
			wantHandleLooks: []string{"c-gaming.othercoves.social"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newStubRepo(local, bridged, federated)
			svc := &communityService{repo: repo, instanceDomain: "coves.social"}

			did, err := svc.ResolveCommunityIdentifier(context.Background(), tt.identifier)
			if err != nil {
				t.Fatalf("ResolveCommunityIdentifier(%q) returned error: %v", tt.identifier, err)
			}
			if did != tt.wantDID {
				t.Errorf("ResolveCommunityIdentifier(%q) = %q, want %q", tt.identifier, did, tt.wantDID)
			}
			if got, want := fmt.Sprint(repo.handleLookups), fmt.Sprint(tt.wantHandleLooks); got != want {
				t.Errorf("handle lookups = %v, want %v", got, want)
			}
			if got, want := fmt.Sprint(repo.pairLookups), fmt.Sprint(tt.wantPairLooks); got != want {
				t.Errorf("(name, origin) lookups = %v, want %v", got, want)
			}

			// GetCommunity carries its own copy of the dispatch; it must agree.
			got, err := svc.GetCommunity(context.Background(), tt.identifier)
			if err != nil {
				t.Fatalf("GetCommunity(%q) returned error: %v", tt.identifier, err)
			}
			if got.DID != tt.wantDID {
				t.Errorf("GetCommunity(%q).DID = %q, want %q", tt.identifier, got.DID, tt.wantDID)
			}
		})
	}
}

func TestResolveCommunityIdentifier_ScopedMisses(t *testing.T) {
	bridged := &Community{DID: "did:plc:bridged456", Handle: "comicstrips.lemmy-world.tdpl.io", Name: "comicstrips", Origin: "lemmy.world"}

	t.Run("unknown remote origin is not found", func(t *testing.T) {
		repo := newStubRepo(bridged)
		svc := &communityService{repo: repo, instanceDomain: "coves.social"}
		_, err := svc.ResolveCommunityIdentifier(context.Background(), "comicstrips@lemmy.ml")
		if !errors.Is(err, ErrCommunityNotFound) {
			t.Fatalf("error = %v, want it to wrap ErrCommunityNotFound", err)
		}
		if IsValidationError(err) {
			t.Error("an unknown origin is a miss, not malformed input")
		}
	})

	t.Run("unknown bare name is not found", func(t *testing.T) {
		repo := newStubRepo(bridged)
		svc := &communityService{repo: repo, instanceDomain: "coves.social"}
		_, err := svc.ResolveCommunityIdentifier(context.Background(), "nope")
		if !errors.Is(err, ErrCommunityNotFound) {
			t.Fatalf("error = %v, want it to wrap ErrCommunityNotFound", err)
		}
	})

	t.Run("remote origin that is not a public hostname is rejected", func(t *testing.T) {
		repo := newStubRepo(bridged)
		svc := &communityService{repo: repo, instanceDomain: "coves.social"}
		_, err := svc.ResolveCommunityIdentifier(context.Background(), "comicstrips@localhost")
		if !IsValidationError(err) {
			t.Fatalf("error = %v, want a validation error", err)
		}
		if len(repo.pairLookups) != 0 {
			t.Errorf("(name, origin) lookups = %v, want none for a refused origin", repo.pairLookups)
		}
	})

	t.Run("two rows with the same pair are reported as ambiguous", func(t *testing.T) {
		twin := &Community{DID: "did:plc:twin789", Handle: "comicstrips-2.lemmy-world.tdpl.io", Name: "comicstrips", Origin: "lemmy.world"}
		repo := newStubRepo(bridged, twin)
		svc := &communityService{repo: repo, instanceDomain: "coves.social"}

		_, err := svc.ResolveCommunityIdentifier(context.Background(), "comicstrips@lemmy.world")
		if !errors.Is(err, ErrAmbiguousCommunity) {
			t.Fatalf("error = %v, want it to wrap ErrAmbiguousCommunity", err)
		}
		if errors.Is(err, ErrCommunityNotFound) {
			t.Error("ambiguity was misreported as not found")
		}
		if _, err := svc.GetCommunity(context.Background(), "comicstrips@lemmy.world"); !errors.Is(err, ErrAmbiguousCommunity) {
			t.Errorf("GetCommunity error = %v, want it to wrap ErrAmbiguousCommunity", err)
		}
	})

	t.Run("repository failure on the pair lookup is not flattened", func(t *testing.T) {
		dbDown := errors.New("connection refused")
		repo := &failingPairRepo{stubHandleRepo: newStubRepo(), err: dbDown}
		svc := &communityService{repo: repo, instanceDomain: "coves.social"}
		_, err := svc.ResolveCommunityIdentifier(context.Background(), "comicstrips@lemmy.world")
		if !errors.Is(err, dbDown) {
			t.Fatalf("error = %v, want it to wrap the repository error", err)
		}
		if errors.Is(err, ErrCommunityNotFound) {
			t.Error("repository failure was misreported as ErrCommunityNotFound")
		}
	})
}

type failingPairRepo struct {
	*stubHandleRepo
	err error
}

func (r *failingPairRepo) GetByNameAndOrigin(context.Context, string, string) (*Community, error) {
	return nil, r.err
}

func TestOriginFromHandle(t *testing.T) {
	for handle, want := range map[string]string{
		"c-gaming.coves.social":           "coves.social",
		"c-test.dev.coves.social":         "dev.coves.social",
		"comicstrips.lemmy-world.tdpl.io": "",
		"c-":                              "",
		"c-.coves.social":                 "",
		"c-gaming":                        "",
		"c-gaming.":                       "",
		"":                                "",
	} {
		if got := OriginFromHandle(handle); got != want {
			t.Errorf("OriginFromHandle(%q) = %q, want %q", handle, got, want)
		}
	}

	// The view converters fall back to the derived origin for native rows
	// indexed before the column existed, and keep a stored origin as-is.
	legacy := &Community{Handle: "c-gaming.coves.social", Name: "gaming"}
	if got := legacy.ToCommunityView().Origin; got != "coves.social" {
		t.Errorf("legacy native row: view origin = %q, want coves.social", got)
	}
	if got := legacy.ToCommunityViewDetailed().Origin; got != "coves.social" {
		t.Errorf("legacy native row: detailed origin = %q, want coves.social", got)
	}
	stored := &Community{Handle: "comicstrips.lemmy-world.tdpl.io", Name: "comicstrips", Origin: "lemmy.world"}
	if got := stored.ToCommunityView().Origin; got != "lemmy.world" {
		t.Errorf("stored origin = %q, want lemmy.world", got)
	}
	unknown := &Community{Handle: "comicstrips.lemmy-world.tdpl.io", Name: "comicstrips"}
	if got := unknown.ToCommunityView().Origin; got != "" {
		t.Errorf("bridged row without origin: view origin = %q, want empty (not derivable)", got)
	}
}
