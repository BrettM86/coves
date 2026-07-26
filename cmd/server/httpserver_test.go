package main

import (
	"Coves/internal/config"
	"Coves/internal/core/imageproxy"
	"net/http"
	"testing"
	"time"
)

// A zero timeout in net/http means "no deadline", so every one of these must
// be carried through from config. This test is the regression guard: dropping
// any of them reintroduces the slowloris exposure silently, because the server
// still works perfectly for well-behaved clients.
func TestNewHTTPServer_AppliesEveryTimeout(t *testing.T) {
	cfg := config.ServerConfig{
		Port:              "9090",
		ReadHeaderTimeout: 11 * time.Second,
		ReadTimeout:       22 * time.Second,
		WriteTimeout:      33 * time.Second,
		IdleTimeout:       44 * time.Second,
		ShutdownTimeout:   55 * time.Second,
	}

	server := newHTTPServer(cfg, http.NotFoundHandler())

	if server.Addr != ":9090" {
		t.Errorf("Addr = %q, want %q", server.Addr, ":9090")
	}

	timeouts := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", server.ReadHeaderTimeout, cfg.ReadHeaderTimeout},
		{"ReadTimeout", server.ReadTimeout, cfg.ReadTimeout},
		{"WriteTimeout", server.WriteTimeout, cfg.WriteTimeout},
		{"IdleTimeout", server.IdleTimeout, cfg.IdleTimeout},
	}
	for _, tc := range timeouts {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
		if tc.got == 0 {
			t.Errorf("%s is zero, which net/http reads as no deadline at all", tc.name)
		}
	}

	if server.Handler == nil {
		t.Error("Handler must be set")
	}
}

// ShutdownTimeout belongs to the shutdown path, not the listener, so it must
// not leak into any of the server's own deadlines.
func TestNewHTTPServer_ShutdownTimeoutIsNotAListenerTimeout(t *testing.T) {
	cfg := config.ServerConfig{
		Port:              "8080",
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       4 * time.Second,
		ShutdownTimeout:   99 * time.Second,
	}

	server := newHTTPServer(cfg, http.NotFoundHandler())

	for name, got := range map[string]time.Duration{
		"ReadHeaderTimeout": server.ReadHeaderTimeout,
		"ReadTimeout":       server.ReadTimeout,
		"WriteTimeout":      server.WriteTimeout,
		"IdleTimeout":       server.IdleTimeout,
	} {
		if got == cfg.ShutdownTimeout {
			t.Errorf("%s picked up ShutdownTimeout (%v)", name, got)
		}
	}
}

// The defaults must actually be usable: the image proxy can spend its full
// fetch timeout on a remote PDS before writing a byte, so a WriteTimeout at or
// below that would truncate legitimate image responses.
//
// The relation is asserted against imageproxy's own default rather than a
// hardcoded 30s, so raising that default fails here instead of silently
// invalidating the invariant.
func TestNewHTTPServer_DefaultWriteTimeoutAccommodatesImageProxy(t *testing.T) {
	// config.Load reads the whole environment, so the test must be hermetic:
	// a stray legacy JETSTREAM_URL or malformed DB_* in the developer's shell
	// would fail this for a reason unrelated to what it asserts.
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}

	server := newHTTPServer(cfg.Server, http.NotFoundHandler())
	imageProxyFetchTimeout := imageproxy.DefaultConfig().FetchTimeout
	if server.WriteTimeout <= imageProxyFetchTimeout {
		t.Errorf("default WriteTimeout = %v, must exceed the image proxy's %v fetch timeout",
			server.WriteTimeout, imageProxyFetchTimeout)
	}
}
