package communities_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
)

// The two functions that turn a community row into what a client receives.
//
// ToCommunityView and ToCommunityViewDetailed are the last thing that happens
// before a community is serialised onto the wire, and between them they copy
// twenty-two fields by hand. That is a transposition waiting to happen —
// member_count landing in subscriberCount, the banner hydrated with the avatar's
// CID — and none of it is visible from a service test, because both views were
// at zero coverage before this file: the API contracts observe the JSON, and the
// T1 service tests stop at the domain struct.
//
// Two things beyond the field mapping matter here:
//
//   - Which fields the LIST view deliberately omits. CommunityView carries no
//     description, no banner, no creator and no content warnings, because it is
//     rendered dozens at a time. A field quietly added to it is a payload
//     regression nothing else would catch.
//   - Which image preset each view asks for. The list view asks for
//     avatar_small (24px) and the detail view for avatar (80px) and banner. With
//     the image proxy disabled — the default, and what every other test in this
//     binary runs under — all three collapse to the same PDS blob URL, so the
//     distinction is only observable with the proxy on. That is the one test
//     below that is not parallel; see its comment.

// fieldNamesOf lists a struct's exported field names in declaration order. It
// is how the two tests below assert a SHAPE rather than a list of values: "this
// view carries exactly these fields" is the claim, and enumerating the fields
// one assert at a time proves only that the ones someone thought of are present.
func fieldNamesOf(view any) []string {
	structType := reflect.TypeOf(view)
	for structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	names := make([]string, 0, structType.NumField())
	for i := 0; i < structType.NumField(); i++ {
		names = append(names, structType.Field(i).Name)
	}
	return names
}

func fullCommunity() *communities.Community {
	return &communities.Community{
		ID:                     7,
		DID:                    "did:plc:viewcommunity00000000",
		Handle:                 "c-gardening.coves.example",
		Name:                   "gardening",
		DisplayName:            "Gardening",
		Description:            "things that grow",
		DescriptionFacets:      []byte(`[{"index":{"byteStart":0,"byteEnd":6}}]`),
		AvatarCID:              "bafyreiavatarcid",
		BannerCID:              "bafyreibannercid",
		PDSURL:                 "https://pds.invalid",
		OwnerDID:               "did:web:coves.example",
		CreatedByDID:           "did:plc:creator00000000000000",
		HostedByDID:            "did:web:coves.example",
		Visibility:             "unlisted",
		ModerationType:         "moderated",
		ContentWarnings:        []string{"spoilers"},
		AllowExternalDiscovery: true,
		SubscriberCount:        11,
		MemberCount:            22,
		PostCount:              33,
		CreatedAt:              time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		UpdatedAt:              time.Date(2026, 4, 4, 5, 6, 7, 0, time.UTC),
		RecordURI:              "at://did:plc:viewcommunity00000000/social.coves.community.profile/self",
		RecordCID:              "bafyreirecordcid",
	}
}

func TestToCommunityView_CarriesTheListFields(t *testing.T) {
	t.Parallel()
	community := fullCommunity()

	view := community.ToCommunityView()

	assert.Equal(t, community.DID, view.DID)
	assert.Equal(t, community.Handle, view.Handle)
	assert.Equal(t, community.Name, view.Name)
	assert.Equal(t, community.DisplayName, view.DisplayName)
	assert.Equal(t, "!gardening@coves.example", view.DisplayHandle)
	assert.Equal(t, community.Visibility, view.Visibility)

	// The three counters are the classic transposition: they are all ints, all
	// plausible in each other's slots, and wrong values look like real data.
	assert.Equal(t, 11, view.SubscriberCount)
	assert.Equal(t, 22, view.MemberCount)
	assert.Equal(t, 33, view.PostCount)

	assert.Contains(t, view.Avatar, community.AvatarCID,
		"the avatar must be served as a URL naming the community's own avatar blob. Asserted as "+
			"Contains rather than as an exact URL because the shape depends on whether the image "+
			"proxy is configured, and pinning either shape would make this a test of one env var")
	assert.NotContains(t, view.Avatar, community.BannerCID,
		"the list view hydrated the banner into the avatar slot")
}

func TestToCommunityView_OmitsTheDetailOnlyFields(t *testing.T) {
	t.Parallel()

	// Asserted structurally rather than field by field: the list view is the
	// payload rendered dozens at a time, so what it does NOT carry is the
	// contract. A new field appearing here fails this test and forces the
	// question of whether every list row should pay for it.
	view := fullCommunity().ToCommunityView()

	assert.Equal(t, []string{
		"DID", "Handle", "Name", "DisplayName", "DisplayHandle", "Avatar",
		"Visibility", "SubscriberCount", "MemberCount", "PostCount", "Viewer",
	}, fieldNamesOf(view),
		"CommunityView's shape changed. If the new field belongs in a list of communities, add it "+
			"here; if it is detail-only, it belongs on CommunityViewDetailed instead")
}

func TestToCommunityViewDetailed_CarriesEverything(t *testing.T) {
	t.Parallel()
	community := fullCommunity()

	view := community.ToCommunityViewDetailed()

	assert.Equal(t, community.DID, view.DID)
	assert.Equal(t, community.Handle, view.Handle)
	assert.Equal(t, community.Name, view.Name)
	assert.Equal(t, community.DisplayName, view.DisplayName)
	assert.Equal(t, "!gardening@coves.example", view.DisplayHandle)
	assert.Equal(t, community.Description, view.Description)
	assert.Equal(t, community.CreatedByDID, view.CreatedByDID)
	assert.Equal(t, community.HostedByDID, view.HostedByDID,
		"hostedBy is what tells a federated reader which instance is answerable for this community")
	assert.Equal(t, community.Visibility, view.Visibility)
	assert.Equal(t, community.ModerationType, view.ModerationType)
	assert.Equal(t, community.ContentWarnings, view.ContentWarnings)
	assert.Equal(t, community.CreatedAt, view.CreatedAt)
	assert.True(t, view.AllowExternalDiscovery)
	assert.Equal(t, 11, view.SubscriberCount)
	assert.Equal(t, 22, view.MemberCount)
	assert.Equal(t, 33, view.PostCount)

	assert.Contains(t, view.Avatar, community.AvatarCID)
	assert.Contains(t, view.Banner, community.BannerCID)
	assert.NotContains(t, view.Banner, community.AvatarCID,
		"the avatar and the banner are two different blobs and hydrating one from the other's CID "+
			"produces a page whose header is a 24-pixel icon stretched across it")
	assert.NotEqual(t, view.Avatar, view.Banner)
}

func TestCommunityViews_OmitTheSecretsTheRowCarries(t *testing.T) {
	t.Parallel()

	// A Community row holds the community's PDS password, access token, refresh
	// token and signing key. The views are what gets serialised to anonymous
	// callers, so the important property is not which fields they copy but that
	// these four are not among them. The struct tags on Community say `json:"-"`
	// for all of them, but a view is a different struct and inherits nothing.
	list := fieldNamesOf(fullCommunity().ToCommunityView())
	detailed := fieldNamesOf(fullCommunity().ToCommunityViewDetailed())

	for _, secret := range []string{
		"PDSPassword", "PDSAccessToken", "PDSRefreshToken", "SigningKeyPEM", "RotationKeyPEM",
		"PDSEmail", "PDSURL",
	} {
		assert.NotContainsf(t, list, secret, "CommunityView must not carry %s", secret)
		assert.NotContainsf(t, detailed, secret, "CommunityViewDetailed must not carry %s", secret)
	}
}

func TestCommunityViews_HandleAMissingImage(t *testing.T) {
	t.Parallel()
	community := fullCommunity()
	community.AvatarCID = ""
	community.BannerCID = ""

	list := community.ToCommunityView()
	detailed := community.ToCommunityViewDetailed()

	// Both view fields are `omitempty`, so an empty string disappears from the
	// JSON entirely and the client falls back to its placeholder. A URL built
	// from an empty CID would instead be a link that 404s, which renders as a
	// broken image rather than a default one.
	assert.Empty(t, list.Avatar, "a community with no avatar must produce no avatar URL")
	assert.Empty(t, detailed.Avatar)
	assert.Empty(t, detailed.Banner)
}

func TestGetDisplayHandle(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		handle string
		want   string
	}{
		{"a canonical handle", "c-gaming.coves.social", "!gaming@coves.social"},
		{"a multi-label TLD", "c-gaming.coves.co.uk", "!gaming@coves.co.uk"},
		{"a subdomained instance", "c-test.dev.coves.social", "!test@dev.coves.social"},
		{"a name containing hyphens", "c-book-club.coves.social", "!book-club@coves.social"},

		// The fallbacks. Each returns the raw handle rather than a mangled !
		// form, because a handle this function cannot parse is still the only
		// name the community has, and rendering "!@" is worse than rendering
		// the DNS name.
		{"a handle with no c- prefix", "gaming.coves.social", "gaming.coves.social"},
		{"a handle that is only the prefix", "c-gaming", "c-gaming"},
		{"a handle whose name is empty", "c-.coves.social", "c-.coves.social"},
		{"an empty handle", "", ""},

		// "c-" alone has the prefix, nothing after it, and therefore no dot:
		// the no-dot branch catches it before the empty-name branch can.
		{"the bare prefix", "c-", "c-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			community := &communities.Community{Handle: tc.handle}
			assert.Equal(t, tc.want, community.GetDisplayHandle())
		})
	}
}

// TestCommunityViews_ImageProxyPresets is the only non-parallel test in this
// package's unit build, and it has to be.
//
// The preset each view asks for is unobservable with the image proxy disabled:
// blobs.HydrateImageURL ignores the preset entirely and returns the PDS blob URL
// for all three. Turning the proxy on is the only way to see the difference, and
// the switch is a package-level variable in blobs behind a write-once latch —
// process-global, with no injection seam. So this test owns that global for its
// duration.
//
// It is safe here for a reason that is worth writing down rather than assuming:
// Go runs every non-parallel top-level test to completion before resuming any
// paused parallel one, so nothing else in this binary reads the config while
// this test is writing it. Nothing else in the package reads it at all today —
// the config's other consumers are in comments, users and db/postgres, which are
// separate test binaries. If a parallel test in THIS package ever starts
// hydrating an image URL, this test and that one will race, and the fix is a
// config seam on the view functions rather than a mutex here.
func TestCommunityViews_ImageProxyPresets(t *testing.T) {
	blobs.ResetImageURLConfigForTesting()
	t.Cleanup(blobs.ResetImageURLConfigForTesting)

	blobs.SetImageURLConfig(blobs.ImageURLConfig{
		ProxyEnabled: true,
		ProxyBaseURL: "https://images.invalid",
	})
	require.True(t, blobs.GetImageURLConfig().ProxyEnabled,
		"the config did not take; every assertion below would silently fall back to the direct PDS URL "+
			"and pass for the wrong reason")

	community := fullCommunity()
	list := community.ToCommunityView()
	detailed := community.ToCommunityViewDetailed()

	// The presets are sizes. A list rendering 50 communities at the 80-pixel
	// preset fetches 50 images an order of magnitude larger than it displays,
	// and the only place that decision is made is the string literal in these
	// two functions.
	assert.Contains(t, list.Avatar, "/avatar_small/",
		"the list view must request the 24-pixel preset")
	assert.Contains(t, detailed.Avatar, "/avatar/")
	assert.NotContains(t, detailed.Avatar, "avatar_small",
		"the detail view requested the list view's thumbnail; a community page would show a 24-pixel "+
			"avatar scaled up")
	assert.Contains(t, detailed.Banner, "/banner/")

	assert.Contains(t, list.Avatar, "https://images.invalid",
		"with the proxy enabled the URL must point at the proxy, not at the PDS: serving PDS blob "+
			"URLs directly is what the proxy exists to stop")
	assert.NotContains(t, list.Avatar, community.PDSURL)
}
