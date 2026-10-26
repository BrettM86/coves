package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const testPassword = "s3cretpw"

// TestRunErrorsNeverContainDSNPassword pins that no error run returns, and
// therefore nothing main logs, carries the connection password.
func TestRunErrorsNeverContainDSNPassword(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{
			// lib/pq returns the net/url parse error, which quotes the URL.
			name: "malformed URL escape",
			dsn:  "postgresql://user:" + testPassword + "@localhost:1/x%zz?sslmode=disable",
		},
		{
			// lib/pq quotes the unparsed key in a key=value parse error.
			name: "key=value with a bare word",
			dsn:  "host=localhost " + testPassword,
		},
		{
			name: "well-formed URL to an unreachable port",
			dsn:  "postgresql://user:" + testPassword + "@127.0.0.1:1/x?sslmode=disable&connect_timeout=2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := run(ctx, tc.dsn, "up")
			if err == nil {
				t.Fatal("run returned nil error, want a connection failure")
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Fatalf("error leaks the DSN password: %v", err)
			}
		})
	}
}

func TestRedactPasswordRemovesPasswordFromWrappedErrors(t *testing.T) {
	inner := errors.New(`dial "postgresql://user:` + testPassword + `@db/x" failed`)
	got := redactPassword(inner, testPassword)
	if strings.Contains(got.Error(), testPassword) {
		t.Fatalf("redacted error still contains the password: %v", got)
	}
	if !strings.Contains(got.Error(), "failed") {
		t.Fatalf("redaction dropped the rest of the message: %v", got)
	}
	if redactPassword(nil, testPassword) != nil {
		t.Fatal("redactPassword(nil) must stay nil")
	}
	if redactPassword(inner, "") != inner {
		t.Fatal("an empty password must leave the error unchanged")
	}
}
