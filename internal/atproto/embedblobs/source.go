package embedblobs

import "embed"

// SourceFS holds the blob extractor's Go sources for template fingerprinting.
//
//go:embed *.go
var SourceFS embed.FS
