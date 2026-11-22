package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/config"
	"Coves/internal/core/feedgen"
)

const syncTestPassword = "literal-instance-password"

// syncLogHandler captures every slog record emitted while the sync runs.
type syncLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *syncLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *syncLogHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}
func (h *syncLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *syncLogHandler) WithGroup(string) slog.Handler      { return h }
func (h *syncLogHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

// syncRecordClient serves GetRecord from getErr and records PutRecord rkeys.
type syncRecordClient struct {
	getErr  error
	putKeys []string
}

func (c *syncRecordClient) GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	return nil, c.getErr
}

func (c *syncRecordClient) PutRecord(ctx context.Context, collection, rkey string, record any, swapRecord string) (string, string, error) {
	c.putKeys = append(c.putKeys, rkey)
	return "at://did:plc:instance/" + collection + "/" + rkey, "bafyreiputcid", nil
}

// Not parallel: each row replaces slog.Default while the sync runs.
func TestSyncBuiltinFeedRecords(t *testing.T) {
	withCredentials := config.PDSConfig{URL: "http://pds.test", ServerHandle: "instance.pds.test", ServerRepoPassword: syncTestPassword}

	for _, tc := range []struct {
		name           string
		pdsCfg         config.PDSConfig
		client         *syncRecordClient
		loginErr       error
		wantLoginCall  bool
		wantErrorLog   bool
		wantPutRecords []string
	}{
		{
			name:          "missing credentials logs an error without logging in",
			pdsCfg:        config.PDSConfig{URL: "http://pds.test", ServerHandle: "instance.pds.test"},
			client:        &syncRecordClient{getErr: pds.ErrNotFound},
			wantLoginCall: false,
			wantErrorLog:  true,
		},
		{
			name:          "login failure logs an error",
			pdsCfg:        withCredentials,
			client:        &syncRecordClient{getErr: pds.ErrNotFound},
			loginErr:      errors.New("createSession: 401 AuthenticationRequired"),
			wantLoginCall: true,
			wantErrorLog:  true,
		},
		{
			name:          "GetRecord failure logs an error",
			pdsCfg:        withCredentials,
			client:        &syncRecordClient{getErr: errors.New("getRecord: 502 bad gateway")},
			wantLoginCall: true,
			wantErrorLog:  true,
		},
		{
			name:           "missing record is written for rkey hot",
			pdsCfg:         withCredentials,
			client:         &syncRecordClient{getErr: fmt.Errorf("getRecord: %w", pds.ErrNotFound)},
			wantLoginCall:  true,
			wantErrorLog:   false,
			wantPutRecords: []string{"hot"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := slog.Default()
			handler := &syncLogHandler{}
			slog.SetDefault(slog.New(handler))
			t.Cleanup(func() { slog.SetDefault(previous) })

			loginCalled := false
			var loginHost, loginHandle string
			login := func(ctx context.Context, host, handle, password string) (feedgen.RecordClient, error) {
				loginCalled = true
				loginHost, loginHandle = host, handle
				if tc.loginErr != nil {
					return nil, tc.loginErr
				}
				return tc.client, nil
			}
			registry := feedgen.NewRegistry("did:web:instance.test", feedgen.HotBuiltin(nil))
			now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

			syncBuiltinFeedRecords(t.Context(), tc.pdsCfg, login, registry, now)

			if loginCalled != tc.wantLoginCall {
				t.Fatalf("login called = %v, want %v", loginCalled, tc.wantLoginCall)
			}
			if tc.wantLoginCall && (loginHost != "http://pds.test" || loginHandle != "instance.pds.test") {
				t.Fatalf("login host/handle = %q/%q, want %q/%q", loginHost, loginHandle, "http://pds.test", "instance.pds.test")
			}

			records := handler.snapshot()
			sawError := false
			for _, record := range records {
				if record.Level == slog.LevelError {
					sawError = true
				}
				text := record.Message
				record.Attrs(func(attr slog.Attr) bool {
					text += " " + attr.Value.String()
					return true
				})
				if strings.Contains(text, syncTestPassword) {
					t.Fatalf("a log record contains the instance password (message %q)", record.Message)
				}
			}
			if sawError != tc.wantErrorLog {
				t.Fatalf("ERROR-level record logged = %v, want %v (%d records)", sawError, tc.wantErrorLog, len(records))
			}

			if strings.Join(tc.client.putKeys, ",") != strings.Join(tc.wantPutRecords, ",") {
				t.Fatalf("PutRecord rkeys = %v, want %v", tc.client.putKeys, tc.wantPutRecords)
			}
		})
	}
}
