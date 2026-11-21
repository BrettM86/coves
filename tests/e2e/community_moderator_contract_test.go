//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

const moderatorCollection = "social.coves.community.moderator"

// moderatorRecordKey is the record key the moderator lexicon fixes for a
// subject. It is the same digest as the ban record key: unpadded lowercase
// base32 of SHA-256 over the subject DID.
func moderatorRecordKey(subjectDID string) string {
	return banRecordKey(subjectDID)
}

// TestCommunityModeratorIngestion is the pipeline proof for the community role record.
//
// coves:ingestion-contract social.coves.community.moderator
//
//	appoint → the community writes a moderator record on A to its own repo;
//	          an unauthenticated getModerators lists A with role moderator
func TestCommunityModeratorIngestion(t *testing.T) {
	p := newPipeline(t)

	founder := p.IndexedAccount(t, "mdf")
	subject := p.IndexedAccount(t, "mds")
	community := hostedCommunitySession(t, p, founder)

	community.PutRecord(t, moderatorCollection, moderatorRecordKey(subject.DID), map[string]any{
		"$type":     moderatorCollection,
		"subject":   subject.DID,
		"role":      "moderator",
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	})

	p.Await(t, "the community's moderator record to reach getModerators", func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
		defer cancel()
		var response struct {
			Community  string `json:"community"`
			Moderators []struct {
				Subject string `json:"subject"`
				Role    string `json:"role"`
			} `json:"moderators"`
		}
		if err := p.AppView.Query(ctx, "social.coves.community.getModerators",
			url.Values{"community": {community.DID}}, &response); err != nil {
			return false, err
		}
		if response.Community != community.DID {
			return false, fmt.Errorf("getModerators served community %q, want %q", response.Community, community.DID)
		}
		for _, moderator := range response.Moderators {
			if moderator.Subject == subject.DID {
				if moderator.Role != "moderator" {
					return false, fmt.Errorf("subject listed with role %q, want moderator", moderator.Role)
				}
				return true, nil
			}
		}
		return false, nil
	})
}

// hostedCommunitySession creates a community through the AppView, so the
// AppView stores its PDS credentials and hosts it (roles are served only in
// hosted communities), and returns a session on the community's repo.
//
// The AppView keeps the community's password to itself, so the test sets a
// known one through the PDS admin API and logs in with it. That revokes the
// AppView's refresh token for the community, which this contract does not
// need: nothing here asks the AppView to write to the community's repo.
func hostedCommunitySession(t *testing.T, p *pipeline, founder *testkit.Account) *testkit.Account {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, p.AppView.As(p.AppView.SignIn(t, founder)).Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "md"), "description": "a community this AppView hosts",
	}, &created), "the founder creates a community through the AppView, which stores its PDS credentials")
	require.NotEmpty(t, created.DID)

	require.NoError(t, p.PDS.AdminClient().Procedure(ctx, "com.atproto.admin.updateAccountPassword", map[string]any{
		"did": created.DID, "password": testkit.DefaultPassword,
	}, nil), "set a known password on the hosted community's account")
	return p.PDS.Login(t, created.DID, testkit.DefaultPassword)
}
