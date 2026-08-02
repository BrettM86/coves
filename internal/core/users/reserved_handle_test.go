package users

import (
	"errors"
	"testing"
)

const testInstanceDomain = "coves.social"

// Community actors on this instance are provisioned as c-{name}.{domain}. A
// user who registers c-gardening.coves.social therefore either blocks
// provisioning of the "gardening" community (PDS handle uniqueness) or holds
// the handle the AppView treats as that community's identity. Registration on
// this instance has to hold that namespace back.
func TestValidateLocalHandleNamespace_ReservesTheCommunityPrefixOnThisInstance(t *testing.T) {
	t.Parallel()

	svc := &userService{instanceDomain: testInstanceDomain}

	reserved := []string{
		"c-gardening.coves.social",
		"c-.coves.social",
		"C-Gardening.Coves.Social", // handles are case-insensitive
	}

	for _, handle := range reserved {
		t.Run(handle, func(t *testing.T) {
			t.Parallel()

			err := svc.validateLocalHandleNamespace(handle)

			var invalid *InvalidHandleError
			if !errors.As(err, &invalid) {
				t.Fatalf("validateLocalHandleNamespace(%q) = %v, want InvalidHandleError", handle, err)
			}
		})
	}
}

// The reservation is scoped to OUR domain. A remote actor legitimately named
// c-foo.example.com lives in someone else's namespace; rejecting it would make
// that user un-indexable here for no security benefit.
func TestValidateLocalHandleNamespace_LeavesOtherNamespacesAlone(t *testing.T) {
	t.Parallel()

	svc := &userService{instanceDomain: testInstanceDomain}

	allowed := []struct {
		handle string
		why    string
	}{
		{"c-foo.example.com", "another instance's namespace is not ours to police"},
		{"gardening.coves.social", "an ordinary local handle"},
		{"csharp.coves.social", "starts with \"c\" but not the \"c-\" prefix"},
		{"self-host.coves.social", "a hyphen elsewhere in the label is fine"},
		{"c-gardening.notcoves.social", "the suffix must match at a label boundary, not mid-label"},
	}

	for _, tc := range allowed {
		t.Run(tc.handle, func(t *testing.T) {
			t.Parallel()

			if err := svc.validateLocalHandleNamespace(tc.handle); err != nil {
				t.Errorf("validateLocalHandleNamespace(%q) = %v, want nil (%s)", tc.handle, err, tc.why)
			}
		})
	}
}

// Without a configured instance domain there is no namespace to defend, and
// guessing one would reject handles on instances that never had the convention.
func TestValidateLocalHandleNamespace_DisabledWithoutAnInstanceDomain(t *testing.T) {
	t.Parallel()

	svc := &userService{}

	if err := svc.validateLocalHandleNamespace("c-gardening.coves.social"); err != nil {
		t.Errorf("with no instance domain configured the check must be inert, got %v", err)
	}
}

// The check has to run on the registration path specifically — that is where an
// actor is created on this instance.
func TestValidateRegisterRequest_RejectsReservedCommunityHandles(t *testing.T) {
	t.Parallel()

	svc := &userService{instanceDomain: testInstanceDomain}

	err := svc.validateRegisterRequest(RegisterAccountRequest{
		Handle:   "c-gardening.coves.social",
		Email:    "someone@example.com",
		Password: "a-sufficiently-long-password",
	})

	var invalid *InvalidHandleError
	if !errors.As(err, &invalid) {
		t.Fatalf("registration with a reserved handle = %v, want InvalidHandleError", err)
	}
}
