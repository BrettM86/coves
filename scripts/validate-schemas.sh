#!/bin/bash
# Validate all lexicon schemas and test data

set -e

echo "🔍 Validating Coves lexicon schemas..."
echo ""

# Run the Go validation tool
go run ./cmd/validate-lexicon/main.go

echo ""
echo "✅ Schema validation complete!"
