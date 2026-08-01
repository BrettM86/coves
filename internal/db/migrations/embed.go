// Package migrations embeds the AppView's goose migration files into the
// binary.
//
// Embedding removes the server's dependency on its working directory for
// migrations. Loading them from the relative path "internal/db/migrations"
// meant the binary only booted when started from the repository root, and it
// forced the container image to reproduce that directory layout around the
// binary — a coupling that breaks silently, at startup, in production.
//
// Static web assets are still served from a relative path (see
// internal/api/routes/web.go), so the working directory still matters for
// those; this removes one such dependency, not all of them.
//
// Test code reaches these migrations through this embedded filesystem too —
// tests/testkit's template provisioning hands it to goose.NewProvider — so the
// working directory no longer matters on that path either. The relative-path
// readers this note used to warn about lived in tests/integration, which phase
// 4 deleted.
package migrations

import "embed"

// FS holds every migration in this directory, in filename order. Pass it to
// goose.SetBaseFS and then run migrations against ".".
//
//go:embed *.sql
var FS embed.FS
