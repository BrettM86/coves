//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"
)

// TestCommunityRepository_AttachHostedCredentials covers the step that makes a
// community hosted after the firehose consumer won the race and indexed its row
// without credentials: credentials attach only to a credential-less row this
// host owns, and never overwrite a finished community or another host's row.
func TestCommunityRepository_AttachHostedCredentials(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()
	pdsURL := testkit.Endpoints().PDS.BaseURL

	const thisHost = "did:web:coves.local"

	newRow := func(t *testing.T, id, hostedBy string) *communities.Community {
		t.Helper()
		did := "did:plc:attach" + id
		return &communities.Community{
			DID:          did,
			Handle:       fmt.Sprintf("!%s@coves.local", id),
			Name:         id,
			OwnerDID:     did,
			CreatedByDID: "did:plc:creator" + id,
			HostedByDID:  hostedBy,
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
	}
	withCredentials := func(c *communities.Community, prefix string) *communities.Community {
		attached := *c
		attached.HostedByDID = thisHost
		attached.PDSEmail = prefix + "@communities.coves.local"
		attached.PDSPassword = prefix + "-password"
		attached.PDSAccessToken = prefix + "-access-token"
		attached.PDSRefreshToken = prefix + "-refresh-token"
		attached.PDSURL = pdsURL
		return &attached
	}

	t.Run("attaches to a credential-less row of this host", func(t *testing.T) {
		row := newRow(t, testkit.UniqueIDWithPrefix(t, "attachok"), thisHost)
		if _, err := repo.Create(ctx, row); err != nil {
			t.Fatalf("create credential-less row: %v", err)
		}

		if err := repo.AttachHostedCredentials(ctx, withCredentials(row, "attached")); err != nil {
			t.Fatalf("AttachHostedCredentials: %v", err)
		}

		got, err := repo.GetByDID(ctx, row.DID)
		if err != nil {
			t.Fatalf("GetByDID: %v", err)
		}
		want := [5]string{"attached@communities.coves.local", "attached-password", "attached-access-token", "attached-refresh-token", pdsURL}
		if have := [5]string{got.PDSEmail, got.PDSPassword, got.PDSAccessToken, got.PDSRefreshToken, got.PDSURL}; have != want {
			t.Errorf("stored credentials after attach = %q, want %q", have, want)
		}

		dids, err := NewHostedCommunityQuery(db).HostedCommunityDIDs(ctx)
		if err != nil {
			t.Fatalf("HostedCommunityDIDs: %v", err)
		}
		if !slices.Contains(dids, row.DID) {
			t.Errorf("community %s is not hosted after AttachHostedCredentials", row.DID)
		}
	})

	tests := []struct {
		name string
		// rowHostedBy is the stored row's hosted_by_did; empty means no row exists.
		rowHostedBy       string
		rowHasCredentials bool
		wantErr           error
		// wantStored is the PDS refresh token the row must still hold afterwards.
		wantStored string
	}{
		{
			name:              "row already has credentials",
			rowHostedBy:       thisHost,
			rowHasCredentials: true,
			wantErr:           communities.ErrCommunityCredentialsConflict,
			wantStored:        "original-refresh-token",
		},
		{
			name:        "row hosted by another instance",
			rowHostedBy: "did:web:elsewhere.example",
			wantErr:     communities.ErrCommunityCredentialsConflict,
			wantStored:  "",
		},
		{
			name:    "no row",
			wantErr: communities.ErrCommunityNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := newRow(t, testkit.UniqueIDWithPrefix(t, "attachno"), tc.rowHostedBy)
			if tc.rowHostedBy != "" {
				stored := row
				if tc.rowHasCredentials {
					stored = withCredentials(row, "original")
				}
				if _, err := repo.Create(ctx, stored); err != nil {
					t.Fatalf("create row: %v", err)
				}
			}

			err := repo.AttachHostedCredentials(ctx, withCredentials(row, "intruder"))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("AttachHostedCredentials error = %v, want %v", err, tc.wantErr)
			}

			if tc.rowHostedBy == "" {
				return
			}
			got, err := repo.GetByDID(ctx, row.DID)
			if err != nil {
				t.Fatalf("GetByDID: %v", err)
			}
			if got.PDSRefreshToken != tc.wantStored {
				t.Errorf("stored refresh token = %q, want %q", got.PDSRefreshToken, tc.wantStored)
			}
		})
	}
}

// TestCommunityRepository_ProvisioningAccounts covers the record that holds a
// community PDS account's generated credentials from before createAccount until
// the community row stores them. It must round-trip by handle, refuse a second
// account for one handle, delete cleanly, and never store the password in
// cleartext.
func TestCommunityRepository_ProvisioningAccounts(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	id := testkit.UniqueIDWithPrefix(t, "prov")
	handle := "c-" + id + ".communities.coves.local"
	account := &communities.ProvisioningAccount{
		Handle:       handle,
		Name:         id,
		CreatedByDID: "did:plc:creator" + id,
		HostedByDID:  "did:web:coves.local",
		PDSURL:       testkit.Endpoints().PDS.BaseURL,
		PDSEmail:     "c-" + id + "@communities.coves.local",
		PDSPassword:  "generated-password-" + id,
	}

	if err := repo.SaveProvisioningAccount(ctx, account); err != nil {
		t.Fatalf("SaveProvisioningAccount: %v", err)
	}

	got, err := repo.GetProvisioningAccountByHandle(ctx, handle)
	switch {
	case err != nil:
		t.Errorf("GetProvisioningAccountByHandle: %v", err)
	case got == nil:
		t.Errorf("GetProvisioningAccountByHandle returned no account and no error")
	default:
		have := [7]string{got.Handle, got.Name, got.CreatedByDID, got.HostedByDID, got.PDSURL, got.PDSEmail, got.PDSPassword}
		want := [7]string{handle, id, "did:plc:creator" + id, "did:web:coves.local", testkit.Endpoints().PDS.BaseURL, "c-" + id + "@communities.coves.local", "generated-password-" + id}
		if have != want {
			t.Errorf("provisioning account = %q, want %q", have, want)
		}
		if got.CreatedAt.IsZero() {
			t.Errorf("provisioning account CreatedAt is zero")
		}
	}

	second := *account
	second.CreatedByDID = "did:plc:other" + id
	second.PDSPassword = "another-password-" + id
	if err := repo.SaveProvisioningAccount(ctx, &second); !errors.Is(err, communities.ErrProvisioningAccountExists) {
		t.Errorf("second SaveProvisioningAccount for one handle error = %v, want %v", err, communities.ErrProvisioningAccountExists)
	}

	var sealed []byte
	if err := db.QueryRowContext(ctx,
		`SELECT pds_password_encrypted FROM community_provisioning_accounts WHERE handle = $1`, handle,
	).Scan(&sealed); err != nil {
		t.Errorf("read raw pds_password_encrypted: %v", err)
	} else if len(sealed) == 0 || string(sealed) == "generated-password-"+id {
		t.Errorf("pds_password_encrypted = %q, want non-empty ciphertext that is not the cleartext password", sealed)
	}

	if err := repo.DeleteProvisioningAccount(ctx, handle); err != nil {
		t.Errorf("DeleteProvisioningAccount: %v", err)
	}
	if _, err := repo.GetProvisioningAccountByHandle(ctx, handle); !errors.Is(err, communities.ErrProvisioningAccountNotFound) {
		t.Errorf("GetProvisioningAccountByHandle after delete error = %v, want %v", err, communities.ErrProvisioningAccountNotFound)
	}
}
