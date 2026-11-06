package testkit

import (
	"context"
	"encoding/base64"
)

// PDS administration: the operator actions a PDS host takes against a repo it
// hosts, through the PDS' own admin API (basic auth "admin" with the PDS admin
// password).
//
// The hermetic stack's PDS containers are configured with PDS_ADMIN_PASSWORD
// "admin" in docker-compose.ci.yml; the runner may override it through the
// same variable.

// updateSubjectStatusMethod is the PDS admin procedure that applies and
// reverses a repo takedown.
const updateSubjectStatusMethod = "com.atproto.admin.updateSubjectStatus"

// AdminClient returns a client authenticated against this PDS' admin API.
func (p *PDS) AdminClient() *XRPCClient {
	credential := base64.StdEncoding.EncodeToString(
		[]byte("admin:" + envOr("PDS_ADMIN_PASSWORD", "admin")))
	return p.Anon.WithHeader("Authorization", "Basic "+credential)
}

// TakeDownRepo applies a takedown to did's repo on this PDS, as the PDS
// operator would. reference is recorded as the takedown's ref. The PDS emits
// an #account event with status "takendown" for the repo.
func (p *PDS) TakeDownRepo(t TestingT, did, reference string) {
	t.Helper()
	p.setRepoTakedown(t, did, map[string]any{"applied": true, "ref": reference})
}

// RestoreRepo reverses a takedown on did's repo on this PDS. The PDS emits an
// #account event with the repo active again.
func (p *PDS) RestoreRepo(t TestingT, did string) {
	t.Helper()
	p.setRepoTakedown(t, did, map[string]any{"applied": false})
}

func (p *PDS) setRepoTakedown(t TestingT, did string, takedown map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), defaultXRPCTimeout)
	defer cancel()
	err := p.AdminClient().Procedure(ctx, updateSubjectStatusMethod, map[string]any{
		"subject": map[string]any{
			"$type": "com.atproto.admin.defs#repoRef",
			"did":   did,
		},
		"takedown": takedown,
	}, nil)
	if err != nil {
		t.Fatalf("%s on %s for %s (takedown %v): %v", updateSubjectStatusMethod, p.URL(), did, takedown, err)
	}
}
