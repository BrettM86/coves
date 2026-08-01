//go:build integration

package communities_test

import (
	"Coves/internal/core/communities"
	"Coves/tests/testkit"
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the provisioner does when the PDS is not there.
//
// Provisioning is the one step of community creation that cannot be undone by
// returning an error: it registers a DID with the PLC directory. Everything
// after it in CreateCommunity — the profile record, the database row — is
// downstream of a network call to a server that may be misconfigured,
// unreachable, or simply slow, and the service's contract with its caller is
// that all three come back as an error rather than as a panic, a hang, or a
// half-created community.
//
// These are negative-path tests almost end to end, so they include one positive
// case: FetchPDSDID against the real test PDS. Six assertions that a call fails
// are worth much less without one showing it can succeed — otherwise a
// provisioner broken for every input would pass the whole file.
//
// No database is touched here; the provisioner does not have one. The package's
// Postgres floor is paid for by its neighbours.

// unreachableAddress returns a URL for a port that nothing is listening on.
//
// Taken by binding port zero and immediately releasing it, rather than by
// picking a number that looks unused: a hardcoded high port is occupied often
// enough on a developer's machine to make "connection refused" occasionally
// become "unexpected response from something else entirely".
func unreachableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "could not reserve a local port")
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return "http://" + address
}

func TestProvisioner_FailsClosedWhenThePDSCannotBeReached(t *testing.T) {
	t.Parallel()

	// The subtests are parallel because each one waits out the same retry ladder
	// against a dead endpoint. They share nothing — no database, no fixtures, a
	// fresh provisioner each — so running them serially only adds their backoffs
	// together.
	ctx := context.Background()

	t.Run("rejects a PDS URL that is not usable", func(t *testing.T) {
		t.Parallel()

		// Configuration errors, not network ones: each of these is something an
		// operator can put in PDS_URL, and each must fail at provisioning rather
		// than being coerced into some default host.
		for _, badURL := range []string{
			"not-a-url",
			"ftp://invalid-protocol.com",
			"http://",
			"://missing-scheme",
			"",
		} {
			provisioner := communities.NewPDSAccountProvisioner(instanceDomain, badURL)
			_, err := provisioner.ProvisionCommunityAccount(ctx, "testcommunity")
			assert.Errorf(t, err, "provisioning against PDS URL %q must fail", badURL)
		}
	})

	t.Run("reports an unreachable PDS", func(t *testing.T) {
		t.Parallel()

		provisioner := communities.NewPDSAccountProvisioner(instanceDomain, unreachableAddress(t))
		_, err := provisioner.ProvisionCommunityAccount(ctx, "testcommunity")

		require.Error(t, err)
		assert.ErrorContains(t, err, "PDS account creation failed",
			"the error has to say which step failed: an operator reading a log needs to know the "+
				"community has no account at all, rather than an account with no record")
		assert.ErrorContains(t, err, "testcommunity",
			"and which community it was for")
	})

	t.Run("honours a cancelled context", func(t *testing.T) {
		t.Parallel()

		// Already past its deadline before the call begins. The point is not the
		// duration — it is that the deadline is observed at all: CreateCommunity
		// is called from an HTTP handler whose context is cancelled when the
		// client disconnects, and a provisioner that ignored it would keep
		// registering DIDs for requests nobody is waiting on.
		expired, cancel := context.WithTimeout(ctx, time.Nanosecond)
		defer cancel()

		provisioner := communities.NewPDSAccountProvisioner(instanceDomain, testkit.Endpoints().PDS.BaseURL)
		_, err := provisioner.ProvisionCommunityAccount(expired, "testcommunity")
		require.Error(t, err, "a request with an expired deadline must not reach a live PDS")
	})
}

func TestFetchPDSDID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// The positive case, and the reason the negative ones below mean anything.
	// FetchPDSDID exists so the instance never hardcodes its PDS' identity —
	// did:web:localhost in development, did:web:pds.example.com in production —
	// so the assertion is simply that a real server answers with something.
	t.Run("reads the DID from a live PDS", func(t *testing.T) {
		t.Parallel()

		did, err := communities.FetchPDSDID(ctx, testkit.Endpoints().PDS.BaseURL)
		require.NoError(t, err)
		assert.NotEmpty(t, did)
		assert.Contains(t, did, "did:", "com.atproto.server.describeServer must answer with a DID, got %q", did)
	})

	t.Run("rejects a URL that is not usable", func(t *testing.T) {
		t.Parallel()

		for _, badURL := range []string{"not-a-url", "http://", ""} {
			_, err := communities.FetchPDSDID(ctx, badURL)
			assert.Errorf(t, err, "FetchPDSDID must fail for %q rather than return an empty DID", badURL)
		}
	})

	t.Run("reports an unreachable server", func(t *testing.T) {
		t.Parallel()

		address := unreachableAddress(t)
		_, err := communities.FetchPDSDID(ctx, address)
		require.Error(t, err)
		assert.ErrorContains(t, err, "failed to describe server")
		assert.ErrorContains(t, err, address, "the error must name the server that did not answer")
	})

	t.Run("honours a cancelled context", func(t *testing.T) {
		t.Parallel()

		expired, cancel := context.WithTimeout(ctx, time.Nanosecond)
		defer cancel()

		_, err := communities.FetchPDSDID(expired, testkit.Endpoints().PDS.BaseURL)
		require.Error(t, err)
	})
}
