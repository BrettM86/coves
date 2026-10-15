//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"Coves/tests/testkit"
)

// TestUserIdentityRenameThroughPipeline exercises the shipped user consumer,
// local PLC and local handle verification after an indexed PDS account renames.
func TestUserIdentityRenameThroughPipeline(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "idrename")
	oldHandle := account.Handle
	newHandle := p.PDS.Endpoint.Handle(testkit.UniqueIDWithPrefix(t, "idrenamed"))

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()
	before, err := p.Profile(ctx, oldHandle)
	if err != nil || before.DID != account.DID {
		t.Fatalf("getProfile by original handle before rename = %+v, %v; want indexed DID %s", before, err, account.DID)
	}
	if err := account.XRPC().Procedure(ctx, "com.atproto.identity.updateHandle",
		map[string]string{"handle": newHandle}, nil); err != nil {
		t.Fatalf("rename indexed account on the PDS: %v", err)
	}

	p.Await(t, fmt.Sprintf("getProfile by DID to return the renamed handle %s", newHandle), func() (bool, error) {
		view, err := p.Profile(context.Background(), account.DID)
		if err != nil {
			return false, err
		}
		return view.DID == account.DID && view.Handle == newHandle, nil
	})
	p.Await(t, fmt.Sprintf("getProfile by abandoned handle %s to return 404", oldHandle), func() (bool, error) {
		_, err := p.Profile(context.Background(), oldHandle)
		if err == nil {
			return false, nil
		}
		if testkit.IsStatus(err, http.StatusNotFound) {
			return true, nil
		}
		return false, err
	})
}
