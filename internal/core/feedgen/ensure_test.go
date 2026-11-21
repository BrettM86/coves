package feedgen_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/feedgen"
	"Coves/internal/validation"
)

// ensureTestBuiltin is a builtin whose every record field is known to the test,
// so the stored record below can be authored independently of Registry.Record.
var ensureTestBuiltin = feedgen.Builtin{
	RKey:                "test-feed",
	DisplayName:         "Test Feed",
	Description:         "Posts for the ensure test",
	Kind:                feedgen.KindPosts,
	Scopes:              []string{"home", "community"},
	AcceptsInteractions: true,
	Generator:           &scriptedGenerator{},
}

const storedCreatedAt = "2026-01-02T03:04:05.000Z"

const storedEqualRecord = `{
	"$type": "social.coves.feed.generator",
	"did": "did:web:instance.test",
	"displayName": "Test Feed",
	"description": "Posts for the ensure test",
	"kind": "posts",
	"scopes": ["home", "community"],
	"acceptsInteractions": true,
	"createdAt": "2026-01-02T03:04:05.000Z"
}`

const storedStaleDescriptionRecord = `{
	"$type": "social.coves.feed.generator",
	"did": "did:web:instance.test",
	"displayName": "Test Feed",
	"description": "An older description",
	"kind": "posts",
	"scopes": ["home", "community"],
	"acceptsInteractions": true,
	"createdAt": "2026-01-02T03:04:05.000Z"
}`

const storedCID = "bafyreistoredrecordcid"

type getResult struct {
	resp *pds.RecordResponse
	err  error
}

type putCall struct {
	collection string
	rkey       string
	record     map[string]any
	swap       string
}

type fakeRecordClient struct {
	gets    map[string]getResult
	putErrs map[string]error
	puts    []putCall
}

func (c *fakeRecordClient) GetRecord(_ context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	result, ok := c.gets[rkey]
	if !ok {
		return nil, fmt.Errorf("fakeRecordClient: unscripted GetRecord %s/%s", collection, rkey)
	}
	return result.resp, result.err
}

func (c *fakeRecordClient) PutRecord(_ context.Context, collection, rkey string, record any, swapRecord string) (string, string, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return "", "", fmt.Errorf("fakeRecordClient: marshal record: %w", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", "", fmt.Errorf("fakeRecordClient: decode record: %w", err)
	}
	c.puts = append(c.puts, putCall{collection: collection, rkey: rkey, record: decoded, swap: swapRecord})
	if err := c.putErrs[rkey]; err != nil {
		return "", "", err
	}
	return "at://did:plc:instanceaccount/" + collection + "/" + rkey, "bafyreinewcid", nil
}

func storedRecord(t *testing.T, literal string) *pds.RecordResponse {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(literal), &value); err != nil {
		t.Fatalf("decode stored record literal: %v", err)
	}
	return &pds.RecordResponse{
		URI:   "at://did:plc:instanceaccount/social.coves.feed.generator/test-feed",
		CID:   storedCID,
		Value: value,
	}
}

// B2: EnsureBuiltinRecords creates missing records, leaves equal ones alone,
// rewrites drifted ones under a swap guard keeping the stored createdAt, and
// collects GetRecord/PutRecord failures while still ensuring the rest.
func TestEnsureBuiltinRecords(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	validator, err := validation.NewEmbeddedLexiconValidator(true)
	if err != nil {
		t.Fatalf("NewEmbeddedLexiconValidator: %v", err)
	}

	type expectedPut struct {
		rkey        string
		swap        string
		description string
		// createdAt is either the literal stored string or "" to mean "equals now".
		createdAt string
	}

	tests := []struct {
		name     string
		builtins []feedgen.Builtin
		gets     func(t *testing.T) map[string]getResult
		putErrs  map[string]error
		wantPuts []expectedPut
		wantErr  bool
	}{
		{
			name:     "missing record is created without a swap guard",
			builtins: []feedgen.Builtin{ensureTestBuiltin},
			gets: func(t *testing.T) map[string]getResult {
				return map[string]getResult{"test-feed": {err: fmt.Errorf("get record: %w", pds.ErrNotFound)}}
			},
			wantPuts: []expectedPut{{rkey: "test-feed", swap: "", description: "Posts for the ensure test", createdAt: ""}},
		},
		{
			name:     "record equal except createdAt is not rewritten",
			builtins: []feedgen.Builtin{ensureTestBuiltin},
			gets: func(t *testing.T) map[string]getResult {
				return map[string]getResult{"test-feed": {resp: storedRecord(t, storedEqualRecord)}}
			},
			wantPuts: nil,
		},
		{
			name:     "drifted record is rewritten with the read CID and the stored createdAt",
			builtins: []feedgen.Builtin{ensureTestBuiltin},
			gets: func(t *testing.T) map[string]getResult {
				return map[string]getResult{"test-feed": {resp: storedRecord(t, storedStaleDescriptionRecord)}}
			},
			wantPuts: []expectedPut{{rkey: "test-feed", swap: storedCID, description: "Posts for the ensure test", createdAt: storedCreatedAt}},
		},
		{
			name:     "GetRecord failure skips that builtin, returns an error, and still ensures the next",
			builtins: []feedgen.Builtin{feedgen.HotBuiltin(&scriptedGenerator{}), ensureTestBuiltin},
			gets: func(t *testing.T) map[string]getResult {
				return map[string]getResult{
					"hot":       {err: errors.New("pds unavailable")},
					"test-feed": {err: pds.ErrNotFound},
				}
			},
			wantPuts: []expectedPut{{rkey: "test-feed", swap: "", description: "Posts for the ensure test", createdAt: ""}},
			wantErr:  true,
		},
		{
			name:     "PutRecord failure returns an error and still ensures the next",
			builtins: []feedgen.Builtin{feedgen.HotBuiltin(&scriptedGenerator{}), ensureTestBuiltin},
			gets: func(t *testing.T) map[string]getResult {
				return map[string]getResult{
					"hot":       {err: pds.ErrNotFound},
					"test-feed": {err: pds.ErrNotFound},
				}
			},
			putErrs: map[string]error{"hot": errors.New("put rejected")},
			wantPuts: []expectedPut{
				{rkey: "hot", swap: "", createdAt: ""},
				{rkey: "test-feed", swap: "", description: "Posts for the ensure test", createdAt: ""},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeRecordClient{gets: tc.gets(t), putErrs: tc.putErrs}
			registry := feedgen.NewRegistry(testInstanceDID, tc.builtins...)

			err := feedgen.EnsureBuiltinRecords(context.Background(), client, registry, now)
			if tc.wantErr && err == nil {
				t.Errorf("EnsureBuiltinRecords returned nil error, want non-nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("EnsureBuiltinRecords returned %v, want nil", err)
			}

			putsByRKey := map[string][]putCall{}
			for _, put := range client.puts {
				putsByRKey[put.rkey] = append(putsByRKey[put.rkey], put)
			}
			if len(client.puts) != len(tc.wantPuts) {
				t.Errorf("PutRecord called %d times (%v), want %d", len(client.puts), rkeysOf(client.puts), len(tc.wantPuts))
			}
			for _, want := range tc.wantPuts {
				got := putsByRKey[want.rkey]
				if len(got) != 1 {
					t.Errorf("PutRecord for rkey %q called %d times, want 1", want.rkey, len(got))
					continue
				}
				put := got[0]
				if put.collection != "social.coves.feed.generator" {
					t.Errorf("PutRecord(%q) collection = %q, want %q", want.rkey, put.collection, "social.coves.feed.generator")
				}
				if put.swap != want.swap {
					t.Errorf("PutRecord(%q) swapRecord = %q, want %q", want.rkey, put.swap, want.swap)
				}
				if err := validator.ValidateRecord(put.record, "social.coves.feed.generator"); err != nil {
					t.Errorf("PutRecord(%q) record does not validate: %v", want.rkey, err)
				}
				if want.description != "" && put.record["description"] != want.description {
					t.Errorf("PutRecord(%q) description = %v, want %q", want.rkey, put.record["description"], want.description)
				}
				gotCreatedAt, _ := put.record["createdAt"].(string)
				if want.createdAt != "" {
					if gotCreatedAt != want.createdAt {
						t.Errorf("PutRecord(%q) createdAt = %q, want stored %q", want.rkey, gotCreatedAt, want.createdAt)
					}
				} else {
					parsed, err := time.Parse(time.RFC3339, gotCreatedAt)
					if err != nil || !parsed.Equal(now) {
						t.Errorf("PutRecord(%q) createdAt = %q, want now %s", want.rkey, gotCreatedAt, now.Format(time.RFC3339))
					}
				}
			}
		})
	}
}

func rkeysOf(puts []putCall) []string {
	rkeys := make([]string, 0, len(puts))
	for _, put := range puts {
		rkeys = append(rkeys, put.rkey)
	}
	return rkeys
}
