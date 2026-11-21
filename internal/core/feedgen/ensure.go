package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"Coves/internal/atproto/pds"
)

// RecordClient reads and writes records in the instance account's repo.
type RecordClient interface {
	GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error)
	PutRecord(ctx context.Context, collection, rkey string, record any, swapRecord string) (uri, cid string, err error)
}

// EnsureBuiltinRecords makes the instance repo hold an up-to-date generator
// record for every builtin. A missing record is created with createdAt now; a
// record that differs (other than createdAt) is rewritten, keeping its stored
// createdAt, guarded by the CID that was read. Failures are collected and the
// remaining builtins are still processed.
func EnsureBuiltinRecords(ctx context.Context, client RecordClient, registry *Registry, now time.Time) error {
	var errs []error
	for _, b := range registry.Builtins() {
		if err := ensureBuiltinRecord(ctx, client, registry, b, now); err != nil {
			errs = append(errs, fmt.Errorf("ensure feed generator record %q: %w", b.RKey, err))
		}
	}
	return errors.Join(errs...)
}

func ensureBuiltinRecord(ctx context.Context, client RecordClient, registry *Registry, b Builtin, now time.Time) error {
	desired := registry.Record(b, now)

	stored, err := client.GetRecord(ctx, GeneratorCollection, b.RKey)
	if errors.Is(err, pds.ErrNotFound) {
		if _, _, err := client.PutRecord(ctx, GeneratorCollection, b.RKey, desired, ""); err != nil {
			return fmt.Errorf("create record: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read record: %w", err)
	}
	if stored == nil {
		return fmt.Errorf("read record: empty response")
	}

	equal, err := equalExceptCreatedAt(desired, stored.Value)
	if err != nil {
		return err
	}
	if equal {
		return nil
	}

	if storedCreatedAt, ok := stored.Value["createdAt"].(string); ok && storedCreatedAt != "" {
		desired["createdAt"] = storedCreatedAt
	}
	if _, _, err := client.PutRecord(ctx, GeneratorCollection, b.RKey, desired, stored.CID); err != nil {
		return fmt.Errorf("update record: %w", err)
	}
	return nil
}

// equalExceptCreatedAt compares the desired record with a stored, JSON-decoded
// one, ignoring createdAt. The desired record is JSON round-tripped first so
// both sides have the same Go types.
func equalExceptCreatedAt(desired, stored map[string]any) (bool, error) {
	raw, err := json.Marshal(desired)
	if err != nil {
		return false, fmt.Errorf("encode record: %w", err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return false, fmt.Errorf("decode record: %w", err)
	}
	delete(normalized, "createdAt")

	storedWithoutCreatedAt := make(map[string]any, len(stored))
	for key, value := range stored {
		if key != "createdAt" {
			storedWithoutCreatedAt[key] = value
		}
	}
	return reflect.DeepEqual(normalized, storedWithoutCreatedAt), nil
}
