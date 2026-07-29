//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The user domain's pipeline contracts: the ingestion proof for
// social.coves.actor.profile, including the blob path an avatar travels, and
// the client-facing read surface.
//
// # THIS COLLECTION IS THE PACKAGE DOC'S RECONCILIATION HAZARD, IN PERSON
//
// contracts_test.go warns about code that reads the PDS by itself and can
// therefore satisfy a "did the firehose deliver it" wait with every consumer
// dead. actor.profile is the known instance: users.maybeBackfillProfile spawns
// a detached goroutine at IndexUser time that fetches
// social.coves.actor.profile/self straight from the user's PDS and writes it to
// Postgres. A contract that signs up, writes a profile and waits for it can be
// satisfied entirely by that goroutine.
//
// The guard is to falsify backfill's precondition before asserting anything.
// Backfill only touches a profile that is COMPLETELY empty, and it checks that
// twice — at the spawn site and again immediately before the write, after the
// fetch, specifically so that a firehose event arriving mid-fetch is not
// clobbered. So one arming write makes the row non-empty and disarms it for
// good, and every assertion this contract makes is on a LATER write.
//
// tests/ci/pending_contracts.txt carried that requirement as this collection's
// entry, and it is the reason the arming step below is not optional politeness.
// TestPipelineSmoke does the same thing with two display names; this contract
// goes further, because a display name is the one field that proves the least.
//
// # WHAT AN AVATAR ACTUALLY IS, END TO END
//
// The avatar is where this domain stops being a string round-trip, and it is
// worth writing down because five components have to agree:
//
//  1. The bytes are uploaded to the PDS with com.atproto.repo.uploadBlob, which
//     answers with a blob ref — {$type: "blob", ref: {$link: <CID>}, mimeType,
//     size}. The PDS holds the bytes; the record holds only the reference.
//  2. The profile record embeds that ref under "avatar" (and "banner").
//  3. The user consumer pulls the CID back out with extractBlobCID
//     (community_consumer.go), which insists on $type == "blob" and reads
//     ref.$link, and stores it as users.avatar_cid. A ref it cannot parse is
//     silently left alone — so a malformed blob does not clear an existing
//     avatar, and does not fail the event either.
//  4. getProfile does NOT serve the CID. users.GetProfile hydrates it into a URL
//     with blobs.HydrateImageURL, which in a stack with the image proxy enabled
//     (.env.ci sets IMAGE_PROXY_ENABLED=true) is
//     {proxy}/img/avatar/plain/{did}/{cid}, and with it disabled is the PDS'
//     own com.atproto.sync.getBlob URL. Both forms contain the CID, which is
//     what the assertions below key on — pinning the proxy form would make this
//     contract a test of one config value.
//  5. Following that URL makes the AppView's image proxy fetch the blob back
//     out of the PDS. That last hop is the only part of the chain a CID
//     comparison cannot prove, and it is the part most likely to break silently
//     (a proxy pointed at the wrong host answers a URL that looks perfect), so
//     the contract follows the link it was given.
//
// The file this replaces (tests/integration/user_profile_avatar_e2e_test.go,
// deleted with this commit) advertised all of that across 1,022 lines and
// proved almost none of it: it watched a real firehose event arrive, then
// re-implemented extractBlobCID inside the test body and called
// userService.UpdateProfile itself — swallowing the error — so its final
// assertion was about its own transcription rather than about the consumer. It
// also asserted the com.atproto.sync.getBlob URL form, which the CI stack does
// not produce, and it fetched the avatar URL only to log the status code.
const profileCollection = "social.coves.actor.profile"

// profileImages is the slice of getProfile's response carrying the hydrated
// blob URLs. Kept apart from ProfileView (contracts_test.go) rather than folded
// into it: ProfileView is read by every contract in the package and these two
// fields are this contract's business.
type profileImages struct {
	Avatar string `json:"avatar"`
	Banner string `json:"banner"`
}

// profileWithImages reads the profile fields ProfileView models plus the image
// URLs, in one request.
func (p *pipeline) profileWithImages(ctx context.Context, actor string) (ProfileView, profileImages, error) {
	var both struct {
		ProfileView
		profileImages
	}
	err := p.AppView.Query(ctx, "social.coves.actor.getProfile", url.Values{"actor": {actor}}, &both)
	return both.ProfileView, both.profileImages, err
}

// requireServesImage asserts that an image URL the AppView advertised actually
// serves that image, and is the only place in this package that follows a link
// out of a response body.
//
// # WHAT IT CHECKS AND WHY EACH PART IS THERE
//
// The claim being made is "a client rendering this profile would see a
// picture", and it decomposes into three facts that fail independently:
//
//   - the URL points at THIS AppView. A hydrated URL is built from configured
//     values (blobs.HydrateImageURL, IMAGE_PROXY_BASE_URL), so a misconfigured
//     deployment serves URLs that are perfectly well-formed and point somewhere
//     a client cannot reach. Checked before the fetch, because following it
//     first would silently make the assertion about some other host.
//   - the response is a 200 with a NON-EMPTY body of an image content type.
//     Not merely "2xx": a 204, or a 200 with an empty body, satisfies "did not
//     fail" while serving nothing, and that is a real outcome here rather than
//     a hypothetical — the proxy fetches the blob from the PDS on demand, so
//     everything about whether bytes exist is decided upstream of the status
//     code it returns.
//   - the bytes are reached through the AppView CLIENT, so the request carries
//     this contract's synthetic rate-limit bucket like every other request the
//     contract makes.
//
// What it deliberately does NOT check is that the bytes equal the bytes
// uploaded. The proxy re-encodes and resizes by preset, so a byte comparison
// would be asserting the image pipeline's output rather than its reachability,
// and would fail the day a preset changed.
func requireServesImage(t *testing.T, p *pipeline, kind, rawURL string) {
	t.Helper()

	appview, err := url.Parse(testkit.Endpoints().AppView.BaseURL)
	require.NoError(t, err)
	parsed, err := url.Parse(rawURL)
	require.NoErrorf(t, err, "the %s URL the AppView served is not a URL: %q", kind, rawURL)
	require.Equalf(t, appview.Host, parsed.Host,
		"the %s URL points at %q rather than at the AppView serving it (%q) — a client following "+
			"it would leave the deployment", kind, parsed.Host, appview.Host)

	resp, err := p.AppView.GetBinary(context.Background(), parsed.Path)
	require.NoErrorf(t, err,
		"the %s URL the AppView advertised does not serve: the profile names a blob the image "+
			"path cannot fetch back out of the PDS", kind)
	require.Equalf(t, http.StatusOK, resp.Status,
		"the %s URL answered %d rather than 200", kind, resp.Status)
	require.NotEmptyf(t, resp.Body,
		"the %s URL answered 200 with an EMPTY body: the status says the proxy is fine and the "+
			"client still renders a broken image", kind)
	require.Truef(t, strings.HasPrefix(resp.ContentType, "image/"),
		"the %s URL served content type %q rather than an image/*: a client will not render it, "+
			"and an HTML error page returned with a 200 looks exactly like this", kind, resp.ContentType)
}

// blobRefValue renders a testkit blob reference the way a record embeds it.
//
// testkit.BlobRef marshals correctly on its own, but records here are built as
// map[string]any so that a contract can write a MALFORMED ref too — which is
// one of the cases below — and mixing a typed value into an otherwise untyped
// record makes the two look like different kinds of thing when they are not.
func blobRefValue(ref testkit.BlobRef) map[string]any {
	return map[string]any{
		"$type":    "blob",
		"ref":      map[string]any{"$link": ref.CID()},
		"mimeType": ref.MimeType,
		"size":     ref.Size,
	}
}

// TestActorProfileIngestion is the pipeline proof for user profiles.
//
// coves:ingestion-contract social.coves.actor.profile
//
// Every record is written straight into the account's own repo at rkey "self",
// and every observation is made through social.coves.actor.getProfile:
//
//	arm    → the row becomes non-empty, DISARMING profile backfill (proves nothing)
//	update → the second write's fields are served, and only the firehose could have brought them
//	blob   → an uploaded avatar and banner reach the endpoint as URLs that carry their CIDs,
//	         and those URLs serve the bytes back through the AppView's image proxy
//	replace→ a new avatar changes the URL, because the CID is the content
//	delete → the profile fields are gone, and STAY gone (Holds, §3.4a)
func TestActorProfileIngestion(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "up")

	// Every value is run-scoped, so a row left by an earlier run on a kept PDS
	// volume cannot satisfy a wait for us.
	arming := "arming " + testkit.UniqueID(t)
	proving := "proving " + testkit.UniqueID(t)
	provingBio := "written straight into the repo " + testkit.UniqueID(t)

	writeProfile := func(record map[string]any) {
		t.Helper()
		record["$type"] = profileCollection
		account.PutRecord(t, profileCollection, "self", record)
	}

	awaitProfile := func(description string, accept func(ProfileView, profileImages) bool) (ProfileView, profileImages) {
		t.Helper()
		var view ProfileView
		var images profileImages
		p.Await(t, description, func() (bool, error) {
			v, i, err := p.profileWithImages(context.Background(), account.DID)
			if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
				return done, err
			}
			view, images = v, i
			return accept(v, i), nil
		})
		return view, images
	}

	// ---- arm ----------------------------------------------------------------
	// ASSERTS NOTHING about the pipeline. Its only job is to make the profile
	// row non-empty so that backfill can never write again — this write may
	// legitimately have been delivered by backfill itself, and the contract does
	// not care which path brought it.
	writeProfile(map[string]any{"displayName": arming, "description": "arming write"})
	awaitProfile("the profile row to become non-empty (disarming profile backfill)",
		func(v ProfileView, _ profileImages) bool { return v.DisplayName == arming })

	// ---- update: the first real assertion ------------------------------------
	// Backfill cannot write over a non-empty profile, so the only remaining path
	// from the PDS to this endpoint is firehose → Jetstream → the AppView's own
	// consumers → Postgres.
	writeProfile(map[string]any{"displayName": proving, "description": provingBio})
	view, _ := awaitProfile(
		"the second directly-written profile to reach social.coves.actor.getProfile via the consumers",
		func(v ProfileView, _ profileImages) bool { return v.DisplayName == proving })

	require.Equal(t, proving, view.DisplayName)
	require.Equal(t, provingBio, view.Description,
		"the update path must carry every changed field, not only the display name")
	require.Equal(t, account.DID, view.DID, "the AppView served a different actor than the one that wrote")
	require.Equal(t, account.Handle, view.Handle,
		"the handle comes from the users row the signup created, not from the profile record — "+
			"a profile update must not disturb it")

	// ---- the blob path -------------------------------------------------------
	avatarBytes := testkit.TestPNG(64, 64)
	avatar := account.UploadBlob(t, avatarBytes, "image/png")
	banner := account.UploadBlob(t, testkit.TestJPEG(96, 32), "image/jpeg")
	require.NotEqual(t, avatar.CID(), banner.CID(),
		"two different images must have different CIDs, or the assertions below cannot tell "+
			"the avatar from the banner")

	withImages := "images " + testkit.UniqueID(t)
	writeProfile(map[string]any{
		"displayName": withImages,
		"description": provingBio,
		"avatar":      blobRefValue(avatar),
		"banner":      blobRefValue(banner),
	})
	_, images := awaitProfile("the uploaded avatar to reach the profile endpoint",
		func(_ ProfileView, i profileImages) bool { return i.Avatar != "" })

	// The endpoint serves a URL, not a CID (see the file's opening note), so
	// what is asserted is that the URL is ABOUT the blob that was uploaded. That
	// holds in both hydration modes, which is the point: a contract that pinned
	// the proxy path would fail the day IMAGE_PROXY_ENABLED changed, for no
	// reason connected to the pipeline.
	require.Containsf(t, images.Avatar, avatar.CID(),
		"the avatar URL %q does not name the CID of the blob that was uploaded (%s): the "+
			"consumer either failed to extract the blob ref or extracted the wrong one",
		images.Avatar, avatar.CID())
	require.Containsf(t, images.Banner, banner.CID(),
		"the banner URL %q does not name the uploaded banner's CID (%s)", images.Banner, banner.CID())
	require.Containsf(t, images.Avatar, account.DID,
		"the avatar URL must name the actor whose blob it is, or the proxy cannot fetch it "+
			"from the right repo: %q", images.Avatar)
	require.NotEqual(t, images.Avatar, images.Banner)

	// ---- the URL is not merely well-formed, it serves ------------------------
	// The hop a CID comparison cannot prove: following the link makes the
	// AppView's image proxy fetch the blob back out of the PDS. A proxy pointed
	// at the wrong host, or a PDS that never stored the bytes, produces a URL
	// that passes every assertion above and a 502 here.
	//
	// Requested through the AppView client so the request carries this
	// contract's rate-limit bucket, and by PATH so the assertion cannot be
	// satisfied by a host the tier did not configure — a URL naming some other
	// service would fail here rather than being silently followed. That the host
	// is this AppView is asserted separately, just below.
	requireServesImage(t, p, "avatar", images.Avatar)
	requireServesImage(t, p, "banner", images.Banner)

	// ---- replace: the CID is the content ------------------------------------
	// Different bytes, so necessarily a different CID, so necessarily a
	// different URL. This is the assertion that a re-upload is really re-read
	// rather than cached: an avatar_cid the consumer failed to update leaves the
	// OLD URL being served, which looks entirely healthy.
	replacement := account.UploadBlob(t, testkit.TestPNG(48, 48), "image/png")
	require.NotEqual(t, avatar.CID(), replacement.CID(),
		"the replacement image must differ from the original, or this step proves nothing")

	writeProfile(map[string]any{
		"displayName": withImages,
		"description": provingBio,
		"avatar":      blobRefValue(replacement),
		"banner":      blobRefValue(banner),
	})
	_, images = awaitProfile("the replacement avatar to reach the profile endpoint",
		func(_ ProfileView, i profileImages) bool { return strings.Contains(i.Avatar, replacement.CID()) })

	require.NotContains(t, images.Avatar, avatar.CID(),
		"the profile still names the previous avatar's CID alongside the new one")
	require.Containsf(t, images.Banner, banner.CID(),
		"replacing the avatar cleared or changed the banner (%q), which the record did not ask for",
		images.Banner)

	// The replacement must SERVE, not merely be named. A URL rebuilt around a
	// CID whose bytes never reached the PDS is the exact failure a CID
	// comparison cannot see, and a re-upload is where it would happen.
	requireServesImage(t, p, "replacement avatar", images.Avatar)

	// ---- delete: the profile record goes, the account stays ------------------
	// handleProfileDelete does not delete the user — it clears the profile
	// fields by writing empty strings — so the correct observation is that
	// getProfile still answers 200 for the actor with the fields gone, NOT a
	// 404. Getting this the wrong way round is the easy mistake: community
	// profiles ARE hard-deleted and their endpoint does 404 (see
	// TestCommunityProfileIngestion), and the two collections look alike from
	// the outside.
	account.DeleteExistingRecord(t, profileCollection, "self")

	cleared := func() (bool, error) {
		v, i, err := p.profileWithImages(context.Background(), account.DID)
		if err != nil {
			return false, err
		}
		return v.DisplayName == "" && v.Description == "" && i.Avatar == "" && i.Banner == "", nil
	}
	p.Await(t, "the deleted profile record to clear the served profile", cleared)
	p.Holds(t, "the cleared profile to stay cleared", cleared)

	final, _, err := p.profileWithImages(context.Background(), account.DID)
	require.NoError(t, err, "deleting a profile RECORD must not delete the actor: getProfile "+
		"still answers for an account whose profile was cleared")
	require.Equal(t, account.DID, final.DID)
	require.Equal(t, account.Handle, final.Handle,
		"the handle survives a profile delete — it belongs to the identity, not to the profile record")
}

// TestActorProfileAPIContract covers the client-facing surface of the profile
// endpoints as a third-party client meets it. It carries NO ingestion marker —
// markers are for pipeline proofs (§3.4a).
//
// The authenticated half — social.coves.actor.updateProfile, which is how the
// mobile app writes the record this contract's sibling writes directly — is
// proven at T1 for the reason §3.4b records: nothing outside the browser OAuth
// callback mints a session RequireAuth accepts. That half lives in
// internal/api/handlers/user/update_profile_test.go, which asserts (among the
// size caps and MIME allowlist) that the record handed to the PDS embeds the
// uploaded blob ref in the shape the consumer above parses. What this adds is
// the part no handler test can see: that the shipped binary really routes these
// NSIDs, really guards the write one, and really serves an indexed profile back
// by every identifier a client holds.
func TestActorProfileAPIContract(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "ua")

	displayName := "api contract " + testkit.UniqueID(t)
	account.PutRecord(t, profileCollection, "self", map[string]any{
		"$type": profileCollection, "displayName": displayName, "description": "read back through the client surface",
	})
	p.Await(t, "the profile to be indexed before the client surface is exercised", func() (bool, error) {
		view, err := p.Profile(context.Background(), account.DID)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return done, err
		}
		return view.DisplayName == displayName, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	t.Run("the write endpoint refuses an unauthenticated client", func(t *testing.T) {
		// social.coves.actor.updateProfile is the only actor NSID behind
		// RequireAuth. signup is deliberately NOT here: it is public by design
		// (there is no session yet), and it has its own contract in
		// user_signup_test.go.
		err := p.AppView.Procedure(ctx, "social.coves.actor.updateProfile",
			map[string]any{"displayName": "nope"}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
			"social.coves.actor.updateProfile must answer 401 to a client with no session, answered: %v", err)
	})

	t.Run("a client reads the profile by DID and by handle", func(t *testing.T) {
		// The two identifier forms take different paths: a DID goes straight to
		// the lookup, a handle is resolved first. The handle case doubles as
		// proof that resolution is served from the AppView's own index — the
		// stack is egress-blocked, so a lookup that escaped to DNS could not
		// have answered at all.
		for _, actor := range []string{account.DID, account.Handle} {
			view, err := p.Profile(ctx, actor)
			require.NoErrorf(t, err, "social.coves.actor.getProfile rejected identifier %q", actor)
			require.Equalf(t, account.DID, view.DID, "identifier %q resolved to the wrong actor", actor)
			require.Equal(t, displayName, view.DisplayName)
		}
	})

	t.Run("an unknown actor is an XRPC not-found", func(t *testing.T) {
		// XRPC-shaped, which is what testkit.IsNotFound insists on: a plain 404
		// would mean the route is gone, and every wait in this tier that treats
		// not-found as "not yet" depends on telling those apart.
		//
		// The DID is a literal at the full 24 characters of a real did:plc
		// rather than a generated one, for the reason TestPostAPIContract gives:
		// UniqueID does not promise the base32 alphabet the validator checks,
		// and a malformed identifier would take the 400 path instead of the
		// lookup path under test.
		_, err := p.Profile(ctx, "did:plc:aaaaaaaaneverindexedactr")
		require.Truef(t, testkit.IsNotFound(err), "expected an XRPC not-found, got: %v", err)
		require.True(t, testkit.IsStatus(err, http.StatusNotFound))
	})

	t.Run("a profile write that omits a field leaves it alone", func(t *testing.T) {
		// PINS A DESIGN CHOICE THAT IS NOT OBVIOUS AND IS EASY TO GET WRONG.
		//
		// handleProfileUpdate builds users.UpdateProfileInput from whichever keys
		// the record HAPPENS to carry: an absent displayName leaves the pointer
		// nil, and a nil pointer means "do not touch" rather than "clear". So a
		// record is not a snapshot of the profile — writing {description: "x"}
		// to rkey self does not remove the display name, even though the record
		// in the repo no longer has one.
		//
		// That makes the AppView's view and the PDS record disagree, which is
		// worth knowing about rather than discovering. It is the correct choice
		// for the AppView's own client (updateProfile always sends the full set)
		// and the surprising one for a third-party client doing a partial
		// putRecord. Clearing a field requires writing it EMPTY, which is
		// exactly what handleProfileDelete does and what the ingestion
		// contract's delete step observes.
		onlyDescription := "description only " + testkit.UniqueID(t)
		account.PutRecord(t, profileCollection, "self", map[string]any{
			"$type": profileCollection, "description": onlyDescription,
		})
		p.Await(t, "the field-omitting update to be indexed", func() (bool, error) {
			view, err := p.Profile(context.Background(), account.DID)
			if err != nil {
				return false, err
			}
			return view.Description == onlyDescription, nil
		})

		view, err := p.Profile(ctx, account.DID)
		require.NoError(t, err)
		require.Equal(t, displayName, view.DisplayName,
			"a profile record with no displayName cleared the stored one: the consumer now treats "+
				"a record as a full snapshot, which silently erases fields for any client that "+
				"writes partial records")
	})
}
