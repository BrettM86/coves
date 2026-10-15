//go:build integration

package community_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/handlers/community"
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/internal/validation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
)

// TestCommunityViews_ServeOriginDisplayFields covers originUrl and
// originSoftware on social.coves.community.get, list and search: a community
// with both stored carries them, one with neither carries neither key, and
// every view validates against its lexicon def.
func TestCommunityViews_ServeOriginDisplayFields(t *testing.T) {
	t.Parallel()
	repo := postgres.NewCommunityRepository(testkit.DB(t), credentialciphertest.Fixed())
	ctx := context.Background()

	withFieldsDID := fixtures.DID("originviewalpha")
	withoutFieldsDID := fixtures.DID("originviewbeta")
	now := time.Now().UTC()
	for _, seeded := range []*communities.Community{
		{
			DID: withFieldsDID, Handle: "originviewalpha.lemmy.world.ap.bridge.test", Name: "originviewalpha",
			DisplayName: "Origin View Alpha", PDSURL: "https://bridge.test",
			Origin: "lemmy.world", OriginURL: "https://lemmy.world/c/originviewalpha", OriginSoftware: "lemmy",
		},
		{
			DID: withoutFieldsDID, Handle: "originviewbeta.coves.social", Name: "originviewbeta",
			DisplayName: "Origin View Beta",
		},
	} {
		seeded.OwnerDID, seeded.CreatedByDID, seeded.HostedByDID = seeded.DID, fixtures.DID("originviewcreator"), fixtures.InstanceDID()
		seeded.Visibility, seeded.CreatedAt, seeded.UpdatedAt = "public", now, now
		if _, err := repo.Create(ctx, seeded); err != nil {
			t.Fatalf("seed community %s: %v", seeded.DID, err)
		}
	}

	catalog := lexicon.NewBaseCatalog()
	if err := catalog.LoadDirectory("../../../atproto/lexicon"); err != nil {
		t.Fatalf("load lexicons: %v", err)
	}
	service := &repositoryBackedService{repo: repo}

	call := func(t *testing.T, pattern, target string, handler http.HandlerFunc) []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		authenticatedAs("", pattern, handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d: %s", target, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}

	// checkView validates one view against ref and asserts its origin display
	// keys: present with the seeded literals for the community that has them,
	// absent for the one that does not.
	checkView := func(t *testing.T, raw json.RawMessage, ref string) {
		t.Helper()
		decoded, err := atdata.UnmarshalJSON(raw)
		if err != nil {
			t.Fatalf("decode view %s: %v", raw, err)
		}
		if err := validation.ValidateData(catalog, decoded, ref, lexicon.AllowLenientDatetime); err != nil {
			t.Errorf("view %s does not validate as %s: %v", raw, ref, err)
		}
		var view map[string]json.RawMessage
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatalf("decode view keys: %v", err)
		}
		var did string
		if err := json.Unmarshal(view["did"], &did); err != nil {
			t.Fatalf("decode view did: %v", err)
		}
		switch did {
		case withFieldsDID:
			for key, want := range map[string]string{
				"originUrl":      "https://lemmy.world/c/originviewalpha",
				"originSoftware": "lemmy",
			} {
				var got string
				if value, present := view[key]; !present {
					t.Errorf("view of %s has no %s key: %s", did, key, raw)
				} else if err := json.Unmarshal(value, &got); err != nil || got != want {
					t.Errorf("view of %s %s = %s, want %q", did, key, value, want)
				}
			}
		case withoutFieldsDID:
			for _, key := range []string{"originUrl", "originSoftware"} {
				if value, present := view[key]; present {
					t.Errorf("view of %s carries %s = %s, want the key omitted", did, key, value)
				}
			}
		}
	}

	// checkListing asserts both seeded communities are listed and checks each.
	checkListing := func(t *testing.T, body []byte) {
		t.Helper()
		var response struct {
			Communities []json.RawMessage `json:"communities"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("decode listing: %v", err)
		}
		seen := map[string]bool{}
		for _, raw := range response.Communities {
			var view struct {
				DID string `json:"did"`
			}
			if err := json.Unmarshal(raw, &view); err != nil {
				t.Fatalf("decode listed view: %v", err)
			}
			seen[view.DID] = true
			checkView(t, raw, "social.coves.community.defs#communityView")
		}
		for _, did := range []string{withFieldsDID, withoutFieldsDID} {
			if !seen[did] {
				t.Errorf("community %s missing from the listing: %s", did, body)
			}
		}
	}

	t.Run("get", func(t *testing.T) {
		handler := community.NewGetHandler(service, repo).HandleGet
		for _, did := range []string{withFieldsDID, withoutFieldsDID} {
			body := call(t, "/xrpc/social.coves.community.get", "/xrpc/social.coves.community.get?community="+did, handler)
			checkView(t, body, "social.coves.community.defs#communityViewDetailed")
		}
	})

	t.Run("list", func(t *testing.T) {
		checkListing(t, call(t, "/xrpc/social.coves.community.list", "/xrpc/social.coves.community.list?limit=50",
			community.NewListHandler(service, repo).HandleList))
	})

	t.Run("search", func(t *testing.T) {
		checkListing(t, call(t, "/xrpc/social.coves.community.search", "/xrpc/social.coves.community.search?q=originview",
			community.NewSearchHandler(service).HandleSearch))
	})
}
