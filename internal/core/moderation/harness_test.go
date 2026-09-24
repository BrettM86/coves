//go:build integration

package moderation_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
