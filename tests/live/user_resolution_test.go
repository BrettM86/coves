//go:build live

package live

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"context"
	"testing"
)

// TestResolveHandleToDID_RealHandle resolves a handle that exists only on the
// production PLC directory. It was a subtest of
// tests/integration/user_test.go's TestUserCreationAndRetrieval, where it was
// the one thing in that test reaching the public internet.
//
// READ-ONLY: ResolveHandleToDID performs HTTP GET lookups only. It never
// registers or mutates anything on the production directory.
func TestResolveHandleToDID_RealHandle(t *testing.T) {
	db := testkit.DB(t)

	ctx := context.Background()
	userRepo := postgres.NewUserRepository(db)

	// Pinned to the production directory: identity.DefaultConfig() follows
	// PLC_DIRECTORY_URL to whatever local PLC is configured, which 404s on
	// handles that only exist upstream.
	productionPLCConfig := identity.DefaultConfig()
	productionPLCConfig.PLCURL = "https://plc.directory"
	productionResolver := identity.NewResolver(db, productionPLCConfig)
	productionUserService := users.NewUserService(userRepo, productionResolver, testkit.Endpoints().PDS.BaseURL, nil, "")

	did, err := productionUserService.ResolveHandleToDID(ctx, "bretton.dev")
	if err != nil {
		t.Fatalf("Failed to resolve handle bretton.dev: %v", err)
	}

	if did == "" {
		t.Error("Expected non-empty DID")
	}

	t.Logf("✅ Resolved bretton.dev → %s", did)
}
