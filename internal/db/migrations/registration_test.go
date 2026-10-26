package migrations_test

import (
	"testing"

	"github.com/pressly/goose/v3"

	"Coves/internal/db/migrations"
)

// The server and cmd/migrate run migrations through goose's global API
// (goose.SetBaseFS(migrations.FS) + goose.UpContext), not goose.NewProvider.
// FS embeds only *.sql, so a Go migration reaches that path solely through
// goose.AddMigrationContext in its init(). This pins that registration.
func TestGoMigrationsRegisteredForGlobalGoosePath(t *testing.T) {
	goose.SetBaseFS(migrations.FS)
	t.Cleanup(func() { goose.SetBaseFS(nil) })

	collected, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		t.Fatalf("collecting migrations from the embedded filesystem: %v", err)
	}

	goMigrationVersions := []int64{62, 63, 64, 65}
	for _, version := range goMigrationVersions {
		migration, err := collected.Current(version)
		if err != nil {
			t.Fatalf("migration %d missing from the global goose path: %v", version, err)
		}
		if !migration.Registered {
			t.Errorf("migration %d: Registered = false, want true", version)
		}
		if migration.UpFnContext == nil {
			t.Errorf("migration %d: UpFnContext is nil", version)
		}
	}
}
