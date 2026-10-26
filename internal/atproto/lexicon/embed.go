// Package lexicon compiles the repository's lexicon schemas into the binary,
// so a deployed server can validate records without a schema directory on disk.
package lexicon

import "embed"

// Schemas holds every lexicon schema under this directory. Non-JSON files
// (READMEs, tests) are embedded too and skipped by the catalog loader.
//
//go:embed com social
var Schemas embed.FS
