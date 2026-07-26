package main

import (
	"Coves/internal/config"
	"net/http"
)

// newHTTPServer builds the HTTP server with every timeout set.
//
// net/http defaults all four to zero, which means "no deadline". A client that
// opens a connection and then sends its request headers one byte at a time
// holds a goroutine and a file descriptor for as long as it likes; enough of
// them exhaust the process's file-descriptor limit without a single complete
// request being sent. That is the slowloris attack, and ReadHeaderTimeout is
// the specific defence against it.
//
// The remaining three bound the other ways a connection can be held open: a
// slow request body (ReadTimeout), a slow response consumer (WriteTimeout),
// and an idle keep-alive connection that is never reused (IdleTimeout).
func newHTTPServer(cfg config.ServerConfig, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}
