//go:build e2e

package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"
)

// TestE2E_ResolveHandle is the API contract (§3.4b) for
// com.atproto.identity.resolveHandle, the AppView's only route under the
// com.atproto.* namespace.
//
// # WHY IT IS AN API CONTRACT AND NOT A PIPELINE PROOF
//
// Nothing here waits on the firehose, and nothing here may be read as evidence
// that ingestion works. Signup indexes synchronously (§3.4), and this endpoint
// reads what signup wrote. What it proves is the client-facing shape of the
// route, which is worth its own contract for a reason no unit test reaches:
// this is the endpoint the web client calls BEFORE login, unauthenticated, to
// decide whether the handle a person typed exists at all. The handler's
// classification of failures is pinned at T0; what only the stack can show is
// that an unresolvable handle comes back as the 400 the client keys on rather
// than as a router 404, a proxy error, or a 502 from resolution machinery that
// has no idea the handle was local.
//
// # WHY THE MISS USES A HANDLE ON THE STACK'S OWN PDS HOST
//
// A handle under some other domain would leave the stack to answer, and the CI
// stack blocks egress, so the failure would be a network error rather than the
// endpoint's own answer. A never-created label on the PDS's handle domain is
// resolvable in principle and absent in fact, which is precisely the case a
// person mistyping their handle at the login form produces.
func TestE2E_ResolveHandle(t *testing.T) {
	p := newPipeline(t)

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	account := p.IndexedAccount(t, "resolve")

	t.Run("an indexed handle resolves to its DID", func(t *testing.T) {
		var got struct {
			DID string `json:"did"`
		}
		if err := p.AppView.Query(ctx, "com.atproto.identity.resolveHandle",
			url.Values{"handle": {account.Handle}}, &got); err != nil {
			t.Fatalf("resolving %s: %v", account.Handle, err)
		}

		if got.DID != account.DID {
			t.Fatalf("resolveHandle(%s) = %q, want the DID signup issued, %q",
				account.Handle, got.DID, account.DID)
		}
	})

	t.Run("a handle that was never created is a 400, not a 404 or a 502", func(t *testing.T) {
		absent := p.PDS.Endpoint.Handle(testkit.UniqueIDWithPrefix(t, "ghost"))

		var got struct {
			DID string `json:"did"`
		}
		err := p.AppView.Query(ctx, "com.atproto.identity.resolveHandle",
			url.Values{"handle": {absent}}, &got)
		if err == nil {
			t.Fatalf("resolveHandle(%s) succeeded with DID %q; that account was never created",
				absent, got.DID)
		}

		var status *testkit.StatusError
		if !errors.As(err, &status) {
			t.Fatalf("resolveHandle(%s) failed with %T (%v), want an XRPC error response",
				absent, err, err)
		}

		// All three fields are asserted because the client acts on all three.
		// atcute's XrpcHandleResolver, which the web client's login preflight
		// uses, turns a 400 from this route into DidNotFoundError and every
		// other status into a resolution failure — so the STATUS is what
		// decides whether a person is told "no such handle" or "try again
		// later", and a 404 here would be indistinguishable from an AppView
		// that never registered the route.
		if status.StatusCode != http.StatusBadRequest {
			t.Errorf("resolveHandle(%s) answered HTTP %d, want 400: %v",
				absent, status.StatusCode, status)
		}
		if status.XRPCError != "InvalidRequest" {
			t.Errorf("resolveHandle(%s) answered error %q, want InvalidRequest: %v",
				absent, status.XRPCError, status)
		}
		// Fixed wording, and deliberately the same for every unresolvable
		// handle: this route takes no auth, so a message that distinguished
		// "no such account" from "not a handle we would look up" would let an
		// anonymous caller probe the difference.
		if status.XRPCMessage != "Unable to resolve handle" {
			t.Errorf("resolveHandle(%s) answered message %q, want %q: %v",
				absent, status.XRPCMessage, "Unable to resolve handle", status)
		}
	})
}
