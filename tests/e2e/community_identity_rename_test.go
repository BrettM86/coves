//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"Coves/tests/testkit"
)

// TestCommunityIdentityRenameThroughPipeline exercises the shipped community
// consumer, local PLC and local handle verification after an indexed
// community's PDS account renames.
func TestCommunityIdentityRenameThroughPipeline(t *testing.T) {
	p := newPipeline(t)
	creator := p.IndexedAccount(t, "cidcreator")
	community := indexedCommunity(t, p, "rc", creator.DID)
	oldHandle := community.Handle
	newHandle := communityHandleFor(testkit.UniqueIDWithPrefix(t, "rn"))
	if label := strings.SplitN(newHandle, ".", 2)[0]; len(label) > testkit.MaxIDLength {
		t.Fatalf("renamed community handle label %q is %d characters, over the PDS' %d-character cap: shorten the prefix",
			label, len(label), testkit.MaxIDLength)
	}

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()
	before, err := p.Community(ctx, oldHandle)
	if err != nil || before.DID != community.DID {
		t.Fatalf("community.get by original handle before rename = %+v, %v; want indexed DID %s", before, err, community.DID)
	}
	if err := community.XRPC().Procedure(ctx, "com.atproto.identity.updateHandle",
		map[string]string{"handle": newHandle}, nil); err != nil {
		t.Fatalf("rename indexed community account on the PDS: %v", err)
	}

	p.Await(t, fmt.Sprintf("community.get by renamed handle %s to return the community", newHandle), func() (bool, error) {
		view, err := p.Community(context.Background(), newHandle)
		if err != nil {
			return testkit.PendingIfNotFound(err)
		}
		return view.DID == community.DID, nil
	})
	p.Await(t, fmt.Sprintf("community.get by abandoned handle %s to return 404", oldHandle), func() (bool, error) {
		_, err := p.Community(context.Background(), oldHandle)
		if err == nil {
			return false, nil
		}
		if testkit.IsStatus(err, http.StatusNotFound) {
			return true, nil
		}
		return false, err
	})
}
