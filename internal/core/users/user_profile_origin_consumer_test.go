//go:build integration

package users_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
)

type profileOriginCase struct {
	service  users.UserService
	caching  *countingIdentityResolver
	uncached *scriptedIdentityResolver
	consumer *jetstream.UserEventConsumer
	db       *sql.DB
	did      string
}

func newProfileOriginCase(t *testing.T, did, handle, resolvedPDS string, realCache bool) *profileOriginCase {
	t.Helper()
	service, resolver, db := newUserService(t)
	answer := &identity.Identity{DID: did, Handle: handle, PDSURL: resolvedPDS}
	cachingResolver := identity.Resolver(&scriptedIdentityResolver{wantDID: did, answer: answer})
	if realCache {
		cachingResolver = resolver
	}
	caching := &countingIdentityResolver{Resolver: cachingResolver}
	uncached := &scriptedIdentityResolver{wantDID: did, answer: answer}
	return &profileOriginCase{
		service: service, caching: caching, uncached: uncached, db: db, did: did,
		consumer: jetstream.NewUserEventConsumer(service, caching,
			jetstream.WithUserUncachedIdentityResolver(uncached),
			jetstream.WithUserBridgeTrust(jetstream.NewBridgeTrust([]string{"https://bridge.test"}))),
	}
}

func (c *profileOriginCase) seed(t *testing.T, handle, pdsURL, displayName string, withOrigin bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.service.CreateUser(ctx, users.CreateUserRequest{DID: c.did, Handle: handle, PDSURL: pdsURL}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	input := users.UpdateProfileInput{DisplayName: &displayName}
	if withOrigin {
		origin, username, url, software := "lemmy.world", "Alice_B", "https://lemmy.world/u/Alice_B", "lemmy"
		input.Origin, input.OriginUsername, input.OriginURL, input.OriginSoftware = &origin, &username, &url, &software
	}
	if _, err := c.service.UpdateProfile(ctx, c.did, input); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if withOrigin {
		user := c.read(t)
		if got := (profileOriginFields{user.Origin, user.OriginUsername, user.OriginURL, user.OriginSoftware}); got != (profileOriginFields{"lemmy.world", "Alice_B", "https://lemmy.world/u/Alice_B", "lemmy"}) {
			t.Fatalf("seeded origin fields = %+v, want all four stored before delivery", got)
		}
	}
}

func (c *profileOriginCase) seedTrustedCache(t *testing.T, handle string) {
	t.Helper()
	if err := identity.NewPostgresCache(c.db, 24*time.Hour).Set(context.Background(), &identity.Identity{
		DID: c.did, Handle: handle, PDSURL: "https://bridge.test",
		Method: identity.MethodDNS, ResolvedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed cached identity: %v", err)
	}
	got, err := c.caching.Resolve(context.Background(), c.did)
	if err != nil || got == nil || got.DID != c.did || got.Handle != handle || got.PDSURL != "https://bridge.test" {
		t.Fatalf("caching resolver did not serve trusted cached identity: identity=%+v, err=%v", got, err)
	}
	c.caching.resolves = 0
}

func (c *profileOriginCase) deliver(operation string, record map[string]interface{}) error {
	return c.consumer.HandleEvent(context.Background(), &jetstream.JetstreamEvent{
		Did: c.did, Kind: "commit", TimeUS: 0,
		Commit: &jetstream.CommitEvent{
			Operation: operation, Collection: jetstream.CovesProfileCollection,
			RKey: "self", CID: "profile-origin-test", Record: record,
		},
	})
}

func (c *profileOriginCase) read(t *testing.T) *users.User {
	t.Helper()
	user, err := c.service.GetUserByDID(context.Background(), c.did)
	if err != nil {
		t.Fatalf("read stored user: %v", err)
	}
	return user
}

type profileOriginFields struct {
	origin, username, url, software string
}

func (c *profileOriginCase) assertOrigin(t *testing.T, want profileOriginFields) {
	t.Helper()
	user := c.read(t)
	got := profileOriginFields{user.Origin, user.OriginUsername, user.OriginURL, user.OriginSoftware}
	if got != want {
		t.Errorf("stored origin fields = %+v, want %+v", got, want)
	}
}

func TestProfileOriginUnknownTrustedCreateAndReplacement(t *testing.T) {
	c := newProfileOriginCase(t, "did:plc:originb5create", "alice-originb5.bridge.test", "https://bridge.test", false)
	if err := c.deliver("create", map[string]interface{}{
		"displayName": "Bridge Name", "origin": "lemmy.world", "originUsername": "Alice_B",
		"originUrl": "https://lemmy.world/u/Alice_B", "originSoftware": "lemmy",
	}); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	c.assertOrigin(t, profileOriginFields{"lemmy.world", "Alice_B", "https://lemmy.world/u/Alice_B", "lemmy"})
	if c.uncached.resolves != 1 || c.caching.resolves != 0 {
		t.Errorf("create Resolve calls: uncached=%d, caching=%d; want 1, 0", c.uncached.resolves, c.caching.resolves)
	}
	if got := c.read(t).DisplayName; got != "Bridge Name" {
		t.Errorf("displayName = %q, want Bridge Name", got)
	}
	for _, tc := range []struct {
		name         string
		record       map[string]interface{}
		want         profileOriginFields
		wantUncached int
	}{
		{"partial origin and username", map[string]interface{}{"origin": "lemmy.ml", "originUsername": "Bob_C"}, profileOriginFields{"lemmy.ml", "Bob_C", "", ""}, 2},
		{"software only", map[string]interface{}{"originSoftware": "kbin"}, profileOriginFields{"", "", "", "kbin"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.deliver("update", tc.record); err != nil {
				t.Fatalf("update profile: %v", err)
			}
			c.assertOrigin(t, tc.want)
			if c.caching.resolves != 0 || c.uncached.resolves != tc.wantUncached {
				t.Errorf("Resolve calls: uncached=%d, caching=%d; want %d, 0", c.uncached.resolves, c.caching.resolves, tc.wantUncached)
			}
		})
	}
}

func TestProfileOriginNativeUserRejectsFieldsWithoutResolution(t *testing.T) {
	c := newProfileOriginCase(t, "did:plc:originb6native", "native-originb6.bridge.test", "https://bridge.test", false)
	c.seed(t, "native-originb6.bridge.test", "https://pds.example.com", "Before", false)
	if err := c.deliver("update", map[string]interface{}{
		"displayName": "Native Name", "description": "Native bio",
		"origin": "lemmy.world", "originUsername": "Alice_B",
		"originUrl": "https://lemmy.world/u/Alice_B", "originSoftware": "lemmy",
	}); err != nil {
		t.Fatalf("update native profile: %v", err)
	}
	c.assertOrigin(t, profileOriginFields{})
	user := c.read(t)
	if user.DisplayName != "Native Name" || user.Bio != "Native bio" {
		t.Errorf("native displayName/bio = %q/%q, want Native Name/Native bio", user.DisplayName, user.Bio)
	}
	if c.uncached.resolves != 0 || c.caching.resolves != 0 {
		t.Errorf("native Resolve calls: uncached=%d, caching=%d; want 0, 0", c.uncached.resolves, c.caching.resolves)
	}
}

func TestProfileOriginFieldFreeUpdateAndDeleteClear(t *testing.T) {
	for _, tc := range []struct {
		name, did, handle, operation string
		record                       map[string]interface{}
		wantDisplayName              string
	}{
		{"field-free update", "did:plc:originb7update", "alice-originb7update.bridge.test", "update", map[string]interface{}{"displayName": "Renamed"}, "Renamed"},
		{"profile delete", "did:plc:originb7delete", "alice-originb7delete.bridge.test", "delete", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newProfileOriginCase(t, tc.did, tc.handle, "https://bridge.test", false)
			c.seed(t, tc.handle, "https://bridge.test", "Before", true)
			if err := c.deliver(tc.operation, tc.record); err != nil {
				t.Fatalf("deliver profile %s: %v", tc.operation, err)
			}
			c.assertOrigin(t, profileOriginFields{})
			if got := c.read(t).DisplayName; got != tc.wantDisplayName {
				t.Errorf("displayName = %q, want %q", got, tc.wantDisplayName)
			}
			if c.uncached.resolves != 0 || c.caching.resolves != 0 {
				t.Errorf("Resolve calls: uncached=%d, caching=%d; want 0, 0", c.uncached.resolves, c.caching.resolves)
			}
		})
	}
}

func TestProfileOriginMissedMoveIgnoresTrustedCache(t *testing.T) {
	for _, tc := range []struct {
		name, did, handle, operation string
		known                        bool
	}{
		{"known moved user", "did:plc:originb8known", "alice-originb8known.bridge.test", "update", true},
		{"unknown moved user", "did:plc:originb8unknown", "alice-originb8unknown.bridge.test", "create", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newProfileOriginCase(t, tc.did, tc.handle, "https://pds.example.com", true)
			if tc.known {
				c.seed(t, tc.handle, "https://bridge.test", "Before", true)
			}
			c.seedTrustedCache(t, tc.handle)
			if err := c.deliver(tc.operation, map[string]interface{}{
				"displayName": "Moved", "origin": "lemmy.ml", "originUsername": "Bob_C",
				"originUrl": "https://lemmy.ml/u/Bob_C", "originSoftware": "kbin",
			}); err != nil {
				t.Fatalf("deliver moved profile: %v", err)
			}
			if tc.known {
				c.assertOrigin(t, profileOriginFields{})
				if got := c.read(t).DisplayName; got != "Moved" {
					t.Errorf("displayName = %q, want Moved", got)
				}
			} else if _, err := c.service.GetUserByDID(context.Background(), tc.did); !errors.Is(err, users.ErrUserNotFound) {
				t.Errorf("unknown moved user lookup = %v, want ErrUserNotFound", err)
			}
			if c.uncached.documentReads != 1 || c.uncached.resolves != 0 || c.caching.resolves != 0 {
				t.Errorf("delivery calls: uncached document reads=%d, uncached Resolve=%d, caching Resolve=%d; want 1, 0, 0 (the document settles a non-bridge PDS)",
					c.uncached.documentReads, c.uncached.resolves, c.caching.resolves)
			}
		})
	}
}

func TestProfileOriginFreshResolutionFailureAndSubject(t *testing.T) {
	for _, tc := range []struct {
		name, did, handle string
		known, retry      bool
		resolveErr        error
		resolvedHandle    string
		resolvedDID       string
		wantErr           error
		wantDisplayName   string
	}{
		{"resolver error then replay", "did:plc:originb9error", "alice-originb9error.bridge.test", true, true, errors.New("directory unavailable"), "alice-originb9error.bridge.test", "did:plc:originb9error", jetstream.ErrUnresolvedReference, "Before"},
		{"known unverified handle", "did:plc:originb9invalid", "alice-originb9invalid.bridge.test", true, false, nil, identity.InvalidHandle, "did:plc:originb9invalid", jetstream.ErrUnresolvedReference, "Before"},
		{"unknown unverified handle", "did:plc:originb9unknown", "alice-originb9unknown.bridge.test", false, false, nil, identity.InvalidHandle, "did:plc:originb9unknown", jetstream.ErrUnresolvedReference, ""},
		{"different resolved DID", "did:plc:originb9subject", "alice-originb9subject.bridge.test", true, false, nil, "other-originb9subject.bridge.test", "did:plc:originb9other", nil, "Changed"},
		{"unknown different resolved DID", "did:plc:originb9unknownsubject", "alice-originb9unknownsubject.bridge.test", false, false, nil, "other-originb9unknownsubject.bridge.test", "did:plc:originb9unknownother", nil, ""},
		{"known invalid identifier", "did:plc:originb9malformed", "alice-originb9malformed.bridge.test", true, false, &identity.ErrInvalidIdentifier{Identifier: "did:plc:originb9malformed", Reason: "rejected by the identifier parser"}, "alice-originb9malformed.bridge.test", "did:plc:originb9malformed", jetstream.ErrPermanentEvent, "Before"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newProfileOriginCase(t, tc.did, tc.handle, "https://bridge.test", false)
			if tc.known {
				c.seed(t, tc.handle, "https://bridge.test", "Before", true)
			}
			c.uncached.answer = &identity.Identity{DID: tc.resolvedDID, Handle: tc.resolvedHandle, PDSURL: "https://bridge.test"}
			c.uncached.declaredHandle = tc.handle
			c.uncached.err = tc.resolveErr
			// A document describing another DID is settled before verification.
			wantResolves := 1
			if tc.resolvedDID != tc.did {
				wantResolves = 0
			}
			record := map[string]interface{}{
				"displayName": "Changed", "origin": "lemmy.ml", "originUsername": "Bob_C",
				"originUrl": "https://lemmy.ml/u/Bob_C", "originSoftware": "kbin",
			}
			operation := "update"
			if !tc.known {
				operation = "create"
			}
			if err := c.deliver(operation, record); tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("profile error = %v, want errors.Is(..., %v)", err, tc.wantErr)
			} else if tc.wantErr == nil && err != nil {
				t.Errorf("profile error = %v, want nil", err)
			}
			if tc.known {
				wantOrigin := profileOriginFields{"lemmy.world", "Alice_B", "https://lemmy.world/u/Alice_B", "lemmy"}
				if tc.wantErr == nil {
					wantOrigin = profileOriginFields{}
				}
				c.assertOrigin(t, wantOrigin)
				if got := c.read(t).DisplayName; got != tc.wantDisplayName {
					t.Errorf("displayName = %q, want %q", got, tc.wantDisplayName)
				}
			} else if _, err := c.service.GetUserByDID(context.Background(), tc.did); !errors.Is(err, users.ErrUserNotFound) {
				t.Errorf("unknown unverified user lookup = %v, want ErrUserNotFound", err)
			}
			if c.caching.resolves != 0 || c.uncached.documentReads != 1 || c.uncached.resolves != wantResolves {
				t.Errorf("calls: uncached document reads=%d, uncached Resolve=%d, caching Resolve=%d; want 1, %d, 0",
					c.uncached.documentReads, c.uncached.resolves, c.caching.resolves, wantResolves)
			}
			if tc.retry {
				c.uncached.err = nil
				if err := c.deliver("update", record); err != nil {
					t.Fatalf("replay after resolver recovery: %v", err)
				}
				c.assertOrigin(t, profileOriginFields{"lemmy.ml", "Bob_C", "https://lemmy.ml/u/Bob_C", "kbin"})
				if got := c.read(t).DisplayName; got != "Changed" {
					t.Errorf("displayName after replay = %q, want Changed", got)
				}
				if c.caching.resolves != 0 || c.uncached.resolves != 2 {
					t.Errorf("Resolve calls after replay: uncached=%d, caching=%d; want 2, 0", c.uncached.resolves, c.caching.resolves)
				}
			}
		})
	}
}

// TestProfileOriginUntrustedHostDropsWithoutRedrive pins that an identity
// outside bridge trust never earns a redrive by also failing handle
// verification: the redrive would give it a window to move its PLC entry to the
// bridge PDS and have impersonating origin fields stored.
func TestProfileOriginUntrustedHostDropsWithoutRedrive(t *testing.T) {
	for _, tc := range []struct {
		name, did, handle string
		known             bool
	}{
		{"known user", "did:plc:originb10known", "alice-originb10known.bridge.test", true},
		{"unknown user", "did:plc:originb10unknown", "alice-originb10unknown.bridge.test", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newProfileOriginCase(t, tc.did, tc.handle, "https://pds.example.com", false)
			operation := "create"
			if tc.known {
				c.seed(t, tc.handle, "https://bridge.test", "Before", true)
				operation = "update"
			}
			c.uncached.answer = &identity.Identity{DID: tc.did, Handle: identity.InvalidHandle, PDSURL: "https://pds.example.com"}
			if err := c.deliver(operation, map[string]interface{}{
				"displayName": "Changed", "origin": "lemmy.ml", "originUsername": "Bob_C",
				"originUrl": "https://lemmy.ml/u/Bob_C", "originSoftware": "kbin",
			}); err != nil {
				t.Fatalf("profile error = %v, want nil (an untrusted host is dropped, not redriven)", err)
			}
			if tc.known {
				c.assertOrigin(t, profileOriginFields{})
				if got := c.read(t).DisplayName; got != "Changed" {
					t.Errorf("displayName = %q, want Changed (the rest of the profile still applies)", got)
				}
			} else if _, err := c.service.GetUserByDID(context.Background(), tc.did); !errors.Is(err, users.ErrUserNotFound) {
				t.Errorf("unknown untrusted user lookup = %v, want ErrUserNotFound", err)
			}
			if c.caching.resolves != 0 || c.uncached.documentReads != 1 || c.uncached.resolves != 0 {
				t.Errorf("calls: uncached document reads=%d, uncached Resolve=%d, caching Resolve=%d; want 1, 0, 0 (the document settles admission before any handle lookup)",
					c.uncached.documentReads, c.uncached.resolves, c.caching.resolves)
			}
		})
	}
}

// TestProfileOriginForeignHandleOnTrustedHostDropsWithoutRedrive pins that a
// trusted PDS URL alone does not admit origin fields: the DID document's PDS is
// self-asserted, so the freshly verified handle must also sit under the bridge
// PDS host's registrable domain. A foreign handle is treated like an untrusted
// host: the fields are dropped and nothing is redriven.
func TestProfileOriginForeignHandleOnTrustedHostDropsWithoutRedrive(t *testing.T) {
	for _, tc := range []struct {
		name, did, storedHandle, verifiedHandle string
		known                                   bool
	}{
		{"known user", "did:plc:originb11known", "alice-originb11known.bridge.test", "alice.originb11known.example.com", true},
		{"unknown user", "did:plc:originb11unknown", "alice-originb11unknown.bridge.test", "alice.originb11unknown.example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newProfileOriginCase(t, tc.did, tc.verifiedHandle, "https://bridge.test", false)
			operation := "create"
			if tc.known {
				c.seed(t, tc.storedHandle, "https://bridge.test", "Before", true)
				operation = "update"
			}
			if err := c.deliver(operation, map[string]interface{}{
				"displayName": "Changed", "origin": "lemmy.ml", "originUsername": "Bob_C",
				"originUrl": "https://lemmy.ml/u/Bob_C", "originSoftware": "kbin",
			}); err != nil {
				t.Fatalf("profile error = %v, want nil (a foreign handle is dropped, not redriven)", err)
			}
			if tc.known {
				c.assertOrigin(t, profileOriginFields{})
				if got := c.read(t).DisplayName; got != "Changed" {
					t.Errorf("displayName = %q, want Changed (the rest of the profile still applies)", got)
				}
			} else if _, err := c.service.GetUserByDID(context.Background(), tc.did); !errors.Is(err, users.ErrUserNotFound) {
				t.Errorf("unknown foreign-handle user lookup = %v, want ErrUserNotFound", err)
			}
			if c.caching.resolves != 0 || c.uncached.documentReads != 1 || c.uncached.resolves != 0 {
				t.Errorf("calls: uncached document reads=%d, uncached Resolve=%d, caching Resolve=%d; want 1, 0, 0 (the document settles admission before any handle lookup)",
					c.uncached.documentReads, c.uncached.resolves, c.caching.resolves)
			}
		})
	}
}
