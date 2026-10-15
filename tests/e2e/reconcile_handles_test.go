//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestReconcileHandlesRepairsStaleBridgedUser runs the built reconcile-handles
// binary against a database whose users row for a local PDS account carries a
// stale handle, with that PDS trusted as a bridge. The binary resolves the DID
// through the local PLC and writes the account's real handle.
func TestReconcileHandlesRepairsStaleBridgedUser(t *testing.T) {
	endpoints := testkit.Endpoints()
	account := testkit.NewPDS(t).CreateAccount(t, testkit.WithHandlePrefix("reconcile"))
	expectedHandle := account.Handle

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := testkit.DB(t)
	var database string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database), "read clone name")
	_, err := db.ExecContext(ctx, `INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`,
		account.DID, "stalehandle.reconcile.test", endpoints.PDS.BaseURL)
	require.NoError(t, err, "seed stale users row")

	goMod, err := exec.CommandContext(ctx, "go", "env", "GOMOD").Output()
	require.NoError(t, err, "go env GOMOD")
	moduleRoot := filepath.Dir(strings.TrimSpace(string(goMod)))
	binary := filepath.Join(t.TempDir(), "reconcile-handles")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/reconcile-handles")
	build.Dir = moduleRoot
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, "build reconcile-handles:\n%s", buildOutput)

	command := exec.CommandContext(ctx, binary, "-trusted-hosts", endpoints.PDS.BaseURL)
	command.Env = append(os.Environ(),
		"DATABASE_URL="+endpoints.Postgres.URL(database),
		"PLC_DIRECTORY_URL="+endpoints.PLC.BaseURL,
	)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "reconcile-handles exit status; output:\n%s", output)

	var storedHandle string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT handle FROM users WHERE did = $1`, account.DID).Scan(&storedHandle),
		"read users row")
	require.Equal(t, expectedHandle, storedHandle, "users row handle after reconcile-handles")
}
