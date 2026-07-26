package config

import "testing"

// loadedEnvVars is every environment variable Load reads. Keeping the list
// beside the loaders rather than in a test file lets other packages that call
// Load in a test reuse it, so only one list has to stay in sync with the code.
var loadedEnvVars = []string{
	"IS_DEV_ENV",
	"DATABASE_URL",
	"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
	"DB_CONN_MAX_IDLE_TIME", "DB_STATEMENT_TIMEOUT",
	"PORT", "APPVIEW_PORT",
	"HTTP_READ_HEADER_TIMEOUT", "HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT",
	"HTTP_IDLE_TIMEOUT", "HTTP_SHUTDOWN_TIMEOUT",
	"PLC_DIRECTORY_URL", "IDENTITY_PLC_URL", "IDENTITY_CACHE_TTL",
	"OAUTH_SEAL_SECRET", "APPVIEW_PUBLIC_URL",
	"OAUTH_CLIENT_PRIVATE_KEY", "OAUTH_CLIENT_KEY_ID",
	"INSTANCE_DID", "INSTANCE_DOMAIN",
	"COMMUNITY_CREATORS", "TRUSTED_BRIDGE_PDS_HOSTS", "SKIP_DID_WEB_VERIFICATION",
	"PDS_URL", "PDS_INSTANCE_HANDLE", "PDS_INSTANCE_PASSWORD", "PDS_ADMIN_PASSWORD",
	"JETSTREAM_FEEDS",
	"CURSOR_SECRET",
	"TURNSTILE_SITE_KEY", "TURNSTILE_SECRET_KEY",
}

// ClearEnvForTest blanks every environment variable Load reads, restoring them
// when the test finishes.
//
// Any test that calls Load needs this. Without it the result depends on the
// developer's shell: an exported JETSTREAM_URL left over from before the
// legacy variables were retired makes Load fail, and the test reports a
// failure that has nothing to do with what it was checking.
//
// Note this *sets* the variables empty rather than unsetting them, which is
// equivalent only because this package reads exclusively through lookup and
// never distinguishes unset from empty. Because it uses t.Setenv, a test that
// calls it cannot also call t.Parallel.
func ClearEnvForTest(t *testing.T) {
	t.Helper()
	for _, name := range loadedEnvVars {
		t.Setenv(name, "")
	}
	for _, name := range legacyJetstreamVars {
		t.Setenv(name, "")
	}
}
