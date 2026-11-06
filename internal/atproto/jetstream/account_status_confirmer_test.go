package jetstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/accountstatus"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// confirmerTargetDID is the repository whose status the confirmer asks for.
const confirmerTargetDID = "did:plc:repostatustargetaaaaaaaa"

// repoStatusPDS is a loopback PDS that answers getRepoStatus for
// confirmerTargetDID with one scripted response, answers 404 to any other
// path or query, and counts the requests that reached it.
type repoStatusPDS struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newRepoStatusPDS(t *testing.T, statusCode int, body string) *repoStatusPDS {
	t.Helper()
	pds := &repoStatusPDS{}
	pds.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		pds.requests.Add(1)
		if request.URL.Path != "/xrpc/com.atproto.sync.getRepoStatus" || request.URL.Query().Get("did") != confirmerTargetDID {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(statusCode)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(pds.server.Close)
	return pds
}

func repoStatusResolver(pdsURL string) *mockIdentityResolverForUser {
	return &mockIdentityResolverForUser{identities: map[string]*identity.Identity{
		confirmerTargetDID: {DID: confirmerTargetDID, Handle: "target.coves.test", PDSURL: pdsURL},
	}}
}

// TestPDSRepoStatusConfirmer pins the getRepoStatus confirmation: the
// request goes to the DID's resolved PDS, a well-formed answer for the
// requested DID is returned as-is, RepoNotFound is ErrRepoNotFound, a 400
// RepoTakendown, RepoSuspended or RepoDeactivated is that inactive answer, and
// every other failure or malformed answer is an error that is not
// ErrRepoNotFound.
func TestPDSRepoStatusConfirmer(t *testing.T) {
	tests := []struct {
		name           string
		statusCode     int
		body           string
		resolveError   error
		noPDSEndpoint  bool
		want           accountstatus.RepoStatus
		wantError      bool
		wantRepoAbsent bool
		// wantFailure is the confirmation failure code a non-RepoNotFound
		// error carries.
		wantFailure accountstatus.ConfirmationFailure
	}{
		{
			name:       "takendown repository",
			statusCode: http.StatusOK,
			body:       `{"did":"did:plc:repostatustargetaaaaaaaa","active":false,"status":"takendown"}`,
			want:       accountstatus.RepoStatus{Active: false, Status: "takendown"},
		},
		{
			name:       "suspended repository",
			statusCode: http.StatusOK,
			body:       `{"did":"did:plc:repostatustargetaaaaaaaa","active":false,"status":"suspended"}`,
			want:       accountstatus.RepoStatus{Active: false, Status: "suspended"},
		},
		{
			name:       "active repository",
			statusCode: http.StatusOK,
			body:       `{"did":"did:plc:repostatustargetaaaaaaaa","active":true,"rev":"3l4abcdefghij"}`,
			want:       accountstatus.RepoStatus{Active: true},
		},
		{
			name:           "repository not found",
			statusCode:     http.StatusBadRequest,
			body:           `{"error":"RepoNotFound","message":"Could not find repo for DID: did:plc:repostatustargetaaaaaaaa"}`,
			wantError:      true,
			wantRepoAbsent: true,
		},
		{
			name:        "internal server error",
			statusCode:  http.StatusInternalServerError,
			body:        `{"error":"InternalServerError","message":"Internal Server Error"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSErrorStatus,
		},
		{
			name:        "service unavailable",
			statusCode:  http.StatusServiceUnavailable,
			body:        `{"error":"ServiceUnavailable"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSErrorStatus,
		},
		{
			name:        "answer for a different DID",
			statusCode:  http.StatusOK,
			body:        `{"did":"did:plc:someoneelseaaaaaaaaaaaaa","active":false,"status":"takendown"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSInvalidResponse,
		},
		{
			name:        "answer with no DID",
			statusCode:  http.StatusOK,
			body:        `{"active":false,"status":"takendown"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSInvalidResponse,
		},
		{
			name:        "answer with active missing",
			statusCode:  http.StatusOK,
			body:        `{"did":"did:plc:repostatustargetaaaaaaaa","status":"takendown"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSInvalidResponse,
		},
		{
			name:        "answer with active null",
			statusCode:  http.StatusOK,
			body:        `{"did":"did:plc:repostatustargetaaaaaaaa","active":null,"status":"takendown"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSInvalidResponse,
		},
		{
			name:        "non-JSON answer",
			statusCode:  http.StatusOK,
			body:        `<html>takendown</html>`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSInvalidResponse,
		},
		{
			name:         "DID resolution failure",
			statusCode:   http.StatusOK,
			body:         `{"did":"did:plc:repostatustargetaaaaaaaa","active":false,"status":"takendown"}`,
			resolveError: &identity.ErrResolutionFailed{Identifier: confirmerTargetDID, Reason: "plc directory unreachable"},
			wantError:    true,
			wantFailure:  accountstatus.FailureIdentityUnresolved,
		},
		{
			name:          "DID document with no PDS endpoint",
			statusCode:    http.StatusOK,
			body:          `{"did":"did:plc:repostatustargetaaaaaaaa","active":false,"status":"takendown"}`,
			noPDSEndpoint: true,
			wantError:     true,
			wantFailure:   accountstatus.FailureIdentityUnresolved,
		},
		{
			name:        "bad request other than RepoNotFound",
			statusCode:  http.StatusBadRequest,
			body:        `{"error":"InvalidRequest","message":"bad did"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSErrorStatus,
		},
		{
			name:       "bad request RepoTakendown is a takendown answer",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"RepoTakendown","message":"Repo has been takendown: did:plc:repostatustargetaaaaaaaa"}`,
			want:       accountstatus.RepoStatus{Active: false, Status: "takendown"},
		},
		{
			name:       "bad request RepoSuspended is a suspended answer",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"RepoSuspended","message":"Repo has been suspended: did:plc:repostatustargetaaaaaaaa"}`,
			want:       accountstatus.RepoStatus{Active: false, Status: "suspended"},
		},
		{
			name:       "bad request RepoDeactivated is a deactivated answer",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"RepoDeactivated","message":"Repo has been deactivated: did:plc:repostatustargetaaaaaaaa"}`,
			want:       accountstatus.RepoStatus{Active: false, Status: "deactivated"},
		},
		{
			name:        "bad request RepoDeleted is an error status",
			statusCode:  http.StatusBadRequest,
			body:        `{"error":"RepoDeleted"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSErrorStatus,
		},
		{
			name:        "internal server error naming RepoTakendown is an error status",
			statusCode:  http.StatusInternalServerError,
			body:        `{"error":"RepoTakendown"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSErrorStatus,
		},
		{
			name:        "forbidden naming RepoSuspended is an error status",
			statusCode:  http.StatusForbidden,
			body:        `{"error":"RepoSuspended"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSErrorStatus,
		},
		{
			name:        "oversize answer",
			statusCode:  http.StatusOK,
			body:        `{"did":"did:plc:repostatustargetaaaaaaaa","active":false,"status":"` + strings.Repeat("x", 70000) + `"}`,
			wantError:   true,
			wantFailure: accountstatus.FailurePDSInvalidResponse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pds := newRepoStatusPDS(t, tt.statusCode, tt.body)
			resolver := repoStatusResolver(pds.server.URL)
			resolver.resolveErr = tt.resolveError
			if tt.noPDSEndpoint {
				resolver.identities[confirmerTargetDID].PDSURL = ""
			}
			confirmer := NewPDSRepoStatusConfirmer(resolver, PrivateRepoStatusConfirmerOptions(true)...)

			got, err := confirmer.RepoStatus(context.Background(), confirmerTargetDID)
			if !tt.wantError {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantRepoAbsent, errors.Is(err, accountstatus.ErrRepoNotFound),
				"errors.Is(err, ErrRepoNotFound) for %v", err)
			if tt.wantRepoAbsent {
				return
			}
			var confirmationError *accountstatus.ConfirmationError
			assert.True(t, errors.As(err, &confirmationError), "expected a *ConfirmationError, got %T: %v", err, err)
			assert.Equal(t, tt.wantFailure, accountstatus.FailureOf(err), "failure code for %v", err)
		})
	}
}

// TestPDSRepoStatusConfirmer_UnreachablePDS pins that a PDS that cannot be
// reached is a pds_unreachable confirmation failure.
func TestPDSRepoStatusConfirmer_UnreachablePDS(t *testing.T) {
	pds := newRepoStatusPDS(t, http.StatusOK,
		`{"did":"did:plc:repostatustargetaaaaaaaa","active":false,"status":"takendown"}`)
	pds.server.Close()
	confirmer := NewPDSRepoStatusConfirmer(repoStatusResolver(pds.server.URL), PrivateRepoStatusConfirmerOptions(true)...)

	_, err := confirmer.RepoStatus(context.Background(), confirmerTargetDID)
	require.Error(t, err)
	assert.False(t, errors.Is(err, accountstatus.ErrRepoNotFound), "unreachable PDS must not be RepoNotFound: %v", err)
	var confirmationError *accountstatus.ConfirmationError
	assert.True(t, errors.As(err, &confirmationError), "expected a *ConfirmationError, got %T: %v", err, err)
	assert.Equal(t, accountstatus.FailurePDSUnreachable, accountstatus.FailureOf(err), "failure code for %v", err)
}

// TestPDSRepoStatusConfirmer_RefusesAPrivateHostByDefault pins the SSRF guard:
// the DID document naming the PDS is attacker-controlled, so without the
// private-host hatch a loopback PDS is refused and never receives the request.
func TestPDSRepoStatusConfirmer_RefusesAPrivateHostByDefault(t *testing.T) {
	pds := newRepoStatusPDS(t, http.StatusOK,
		`{"did":"did:plc:repostatustargetaaaaaaaa","active":false,"status":"takendown"}`)
	confirmer := NewPDSRepoStatusConfirmer(repoStatusResolver(pds.server.URL), PrivateRepoStatusConfirmerOptions(false)...)

	_, err := confirmer.RepoStatus(context.Background(), confirmerTargetDID)
	require.Error(t, err)
	assert.Equal(t, int64(0), pds.requests.Load(), "the guarded confirmer delivered a request to a loopback PDS")
}
