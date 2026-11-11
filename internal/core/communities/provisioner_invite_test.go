package communities_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Community provisioning against a PDS that requires invite codes.
//
// The production PDS has invites required, so createAccount without an
// inviteCode fails with "InvalidInviteCode: No invite code provided". With an
// admin password configured, the provisioner mints a single-use invite via
// com.atproto.server.createInviteCode (basic auth admin:<password>, useCount 1)
// and passes the returned code to createAccount. A mint failure aborts
// provisioning before createAccount is attempted.
func TestProvisioner_MintsInviteCodeForInviteRequiredPDS(t *testing.T) {
	t.Parallel()

	const (
		adminPassword = "pds-admin-secret"
		inviteCode    = "coves-invite-1"
		communityDID  = "did:plc:invitecommunity1"
	)

	tests := []struct {
		name                   string
		configuredPassword     string
		wantErr                bool
		wantDID                string
		wantCreateAccountCalls int64
	}{
		{
			name:                   "correct admin password mints an invite and creates the account",
			configuredPassword:     adminPassword,
			wantDID:                communityDID,
			wantCreateAccountCalls: 1,
		},
		{
			name:                   "wrong admin password fails before createAccount",
			configuredPassword:     "wrong-password",
			wantErr:                true,
			wantCreateAccountCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pds := newInviteRequiredFakePDS(t, adminPassword, inviteCode, communityDID)

			opts := append(communities.PrivateHostOptions(true), communities.WithPDSAdminPassword(tt.configuredPassword))
			provisioner := communities.NewPDSAccountProvisioner("coves.social", pds.server.URL, opts...)

			account, err := provisioner.ProvisionCommunityAccount(context.Background(), "invitetest")

			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, account)
			} else {
				require.NoError(t, err)
				require.NotNil(t, account)
				assert.Equal(t, tt.wantDID, account.DID)
				assert.Equal(t, "c-invitetest.coves.social", account.Handle)
			}
			assert.Equal(t, int64(1), pds.createInviteCalls.Load(), "createInviteCode calls")
			assert.Equal(t, tt.wantCreateAccountCalls, pds.createAccountCalls.Load(), "createAccount calls")
		})
	}
}

// inviteRequiredFakePDS is an httptest PDS that requires invite codes. It
// counts createInviteCode calls, createAccount calls, and every request that
// carries an Authorization header.
type inviteRequiredFakePDS struct {
	server             *httptest.Server
	createInviteCalls  atomic.Int64
	createAccountCalls atomic.Int64
	authorizedRequests atomic.Int64
}

func newInviteRequiredFakePDS(t *testing.T, adminPassword, inviteCode, communityDID string) *inviteRequiredFakePDS {
	t.Helper()
	fake := &inviteRequiredFakePDS{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			fake.authorizedRequests.Add(1)
		}
		writeJSON := func(status int, body any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createInviteCode":
			fake.createInviteCalls.Add(1)
			if r.Method != http.MethodPost {
				writeJSON(http.StatusMethodNotAllowed, map[string]string{"error": "MethodNotAllowed"})
				return
			}
			user, pass, ok := r.BasicAuth()
			if !ok || user != "admin" || pass != adminPassword {
				writeJSON(http.StatusUnauthorized, map[string]string{"error": "AuthMissing", "message": "Authentication Required"})
				return
			}
			var body struct {
				UseCount int `json:"useCount"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UseCount != 1 {
				writeJSON(http.StatusBadRequest, map[string]string{"error": "InvalidRequest", "message": "useCount must be 1"})
				return
			}
			writeJSON(http.StatusOK, map[string]string{"code": inviteCode})
		case "/xrpc/com.atproto.server.createAccount":
			fake.createAccountCalls.Add(1)
			var body struct {
				Handle     string `json:"handle"`
				InviteCode string `json:"inviteCode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(http.StatusBadRequest, map[string]string{"error": "InvalidRequest", "message": err.Error()})
				return
			}
			if body.InviteCode == "" {
				writeJSON(http.StatusBadRequest, map[string]string{"error": "InvalidInviteCode", "message": "No invite code provided"})
				return
			}
			if body.InviteCode != inviteCode {
				writeJSON(http.StatusBadRequest, map[string]string{"error": "InvalidInviteCode", "message": "Provided invite code not available"})
				return
			}
			writeJSON(http.StatusOK, map[string]string{
				"did":        communityDID,
				"handle":     body.Handle,
				"accessJwt":  "access-jwt",
				"refreshJwt": "refresh-jwt",
			})
		default:
			writeJSON(http.StatusNotFound, map[string]string{"error": "MethodNotImplemented"})
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// The PDS admin password is sent only to the provisioner's configured PDS. A
// prepared account whose PDSURL names another host (on retry it is read back
// from a persisted row) fails before anything is contacted: the other host gets
// no Authorization-bearing request and no createAccount, and the configured PDS
// mints no invite, since a code minted there could not be redeemed elsewhere.
func TestProvisioner_AdminPasswordOnlySentToConfiguredPDS(t *testing.T) {
	t.Parallel()

	configured := newInviteRequiredFakePDS(t, "pds-admin-secret", "coves-invite-1", "did:plc:invitecommunity1")
	other := newInviteRequiredFakePDS(t, "pds-admin-secret", "coves-invite-1", "did:plc:invitecommunity1")

	opts := append(communities.PrivateHostOptions(true), communities.WithPDSAdminPassword("pds-admin-secret"))
	provisioner := communities.NewPDSAccountProvisioner("coves.social", configured.server.URL, opts...)

	prepared, err := provisioner.PrepareCommunityAccount("invitetest")
	require.NoError(t, err)
	prepared.PDSURL = other.server.URL

	account, err := provisioner.CreatePreparedCommunityAccount(context.Background(), prepared)

	assert.Error(t, err)
	assert.Nil(t, account)
	assert.Equal(t, int64(0), other.authorizedRequests.Load(), "other host requests with an Authorization header")
	assert.Equal(t, int64(0), other.createAccountCalls.Load(), "other host createAccount calls")
	assert.Equal(t, int64(0), configured.createInviteCalls.Load(), "configured PDS createInviteCode calls")
}
