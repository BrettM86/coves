package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// statementTimeoutParam is the PostgreSQL runtime parameter that bounds how
// long any single statement may run. lib/pq forwards unrecognised connection
// parameters to the server in the startup packet, so setting it here applies
// to every connection the pool opens without a per-query round trip.
const statementTimeoutParam = "statement_timeout"

// AppDSN returns the connection string used by the application pool, with
// statement_timeout applied.
//
// Enforcing the bound server-side rather than with a per-query
// context.WithTimeout is deliberate, though not because context cancellation
// does nothing: lib/pq does send a PostgreSQL CancelRequest when a query's
// context is cancelled. That path is simply weaker. It has to dial a second
// connection to deliver the cancellation, it races the query finishing, and it
// does nothing at all if the client process dies outright. statement_timeout
// is enforced by the server itself, needs no extra round trip, and applies to
// every query without each call site remembering a deadline.
//
// An explicit statement_timeout already present in the URL is left alone, so
// an operator can override it per deployment.
func (d DatabaseConfig) AppDSN() (string, error) {
	if err := requireURL(d.URL); err != nil {
		return "", err
	}
	if d.StatementTimeout <= 0 {
		return d.URL, nil
	}
	// PostgreSQL reads a bare integer as milliseconds. Clamp up rather than
	// down: statement_timeout=0 means "no limit", so rounding a sub-millisecond
	// setting to zero would silently disable the bound entirely.
	milliseconds := d.StatementTimeout.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	return withConnParam(d.URL, statementTimeoutParam, strconv.FormatInt(milliseconds, 10))
}

// MigrationDSN returns the connection string used for schema migrations.
//
// Migrations intentionally run without statement_timeout: a CREATE INDEX or a
// backfill over a large table can legitimately exceed the query bound that is
// right for a request handler, and a migration killed halfway is far worse
// than a slow one. Any statement_timeout the operator put directly in
// DATABASE_URL is stripped here too — otherwise the guarantee would hold only
// for the timeout this package adds, which is the case that needed it least.
func (d DatabaseConfig) MigrationDSN() (string, error) {
	if err := requireURL(d.URL); err != nil {
		return "", err
	}
	return withoutConnParam(d.URL, statementTimeoutParam)
}

// requireURL rejects an empty connection string. Without this, sql.Open("")
// succeeds and libpq quietly falls back to PGHOST/PGUSER or the OS username,
// connecting somewhere nobody intended.
func requireURL(dsn string) error {
	if strings.TrimSpace(dsn) == "" {
		return errors.New("database URL is empty")
	}
	return nil
}

// withConnParam adds a connection parameter to a libpq connection string,
// leaving it untouched if the parameter is already present.
//
// Both accepted forms are handled: the URL form ("postgres://...") that this
// project uses everywhere, and the keyword/value form ("host=... user=...")
// that libpq also accepts, so an operator switching styles does not silently
// lose the timeout.
func withConnParam(dsn, key, value string) (string, error) {
	trimmed := strings.TrimSpace(dsn)
	if err := requireURL(trimmed); err != nil {
		return "", err
	}

	if !isURLForm(trimmed) {
		if hasKeyword(trimmed, key) {
			return trimmed, nil
		}
		return trimmed + " " + key + "=" + value, nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parsing database URL: %w", err)
	}
	query := parsed.Query()
	if query.Has(key) {
		return trimmed, nil
	}
	query.Set(key, value)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// withoutConnParam removes a connection parameter from a libpq connection
// string, in either accepted form.
func withoutConnParam(dsn, key string) (string, error) {
	trimmed := strings.TrimSpace(dsn)
	if err := requireURL(trimmed); err != nil {
		return "", err
	}

	if !isURLForm(trimmed) {
		kept := make([]string, 0, 8)
		for _, field := range splitKeywordFields(trimmed) {
			if name, _, found := strings.Cut(field, "="); found && name == key {
				continue
			}
			kept = append(kept, field)
		}
		return strings.Join(kept, " "), nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parsing database URL: %w", err)
	}
	query := parsed.Query()
	if !query.Has(key) {
		return trimmed, nil
	}
	query.Del(key)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// isURLForm reports whether dsn uses the URL syntax rather than libpq's
// keyword/value syntax.
func isURLForm(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// hasKeyword reports whether a keyword/value DSN sets key as a whole keyword.
// Matching on the whole key avoids mistaking a substring for the key — libpq's
// "options=-c statement_timeout=1000" contains the name but does not set it as
// a connection parameter.
func hasKeyword(dsn, key string) bool {
	for _, field := range splitKeywordFields(dsn) {
		if name, _, found := strings.Cut(field, "="); found && name == key {
			return true
		}
	}
	return false
}

// splitKeywordFields splits a libpq keyword/value connection string into its
// fields, keeping single-quoted values intact.
//
// strings.Fields alone is wrong here: libpq allows quoted values containing
// spaces, so "options='-c statement_timeout=1000'" would split into two
// fields, the second of which parses as a bare statement_timeout keyword.
func splitKeywordFields(dsn string) []string {
	var fields []string
	var current strings.Builder
	quoted := false

	for i := 0; i < len(dsn); i++ {
		switch char := dsn[i]; {
		case char == '\'':
			quoted = !quoted
			current.WriteByte(char)
		case char == ' ' && !quoted:
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(char)
		}
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}
