// Package r2audit defines the read-only orphan inventory boundary for owned
// B2B result objects. It deliberately has no delete capability: an R2 object
// and its MongoDB asset record do not share a transaction, so V1 only measures
// possible orphans for an operator to investigate.
package r2audit

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

const (
	// ManagedResultPrefix is the only R2 namespace materialized B2B results
	// can occupy. Caches and migrated legacy objects are deliberately excluded.
	ManagedResultPrefix = "gen/"
	maxPageSize         = 1000
)

var (
	// ErrDependenciesUnavailable means the audit cannot safely compare both
	// sides of the R2/Mongo boundary.
	ErrDependenciesUnavailable = errors.New("r2 orphan audit dependencies are unavailable")
	// ErrInvalidCommand rejects unbounded or malformed page requests before an
	// external R2 or MongoDB read is attempted.
	ErrInvalidCommand = errors.New("r2 orphan audit command is invalid")
	// ErrInvalidObject rejects a list result outside the namespace or shape
	// owned by this workflow. Treating it as an orphan would report unrelated
	// data and invite an unsafe future cleanup.
	ErrInvalidObject = errors.New("r2 orphan audit object is invalid")
)

// Object is a public R2 result object paired with its exact canonical asset
// storage key. StorageKey is internal audit input; it is never returned in a
// report or emitted by this package.
type Object struct {
	Key        string
	StorageKey string
}

// Page is one bounded page of inventory. NextCursor is opaque and may be
// supplied to the following audit invocation without being inspected.
type Page struct {
	Objects    []Object
	NextCursor string
}

// Inventory is intentionally read-only. Keeping Delete out of this interface
// makes a first-version audit unable to mutate R2 by construction.
type Inventory interface {
	List(context.Context, string, string, int32) (Page, error)
}

// AssetIndex answers which exact canonical storage keys have a Mongo asset
// record. It must only read; publication continues to be owned by the result
// materializer transaction.
type AssetIndex interface {
	ReferencedStorageKeys(context.Context, []string) (map[string]struct{}, error)
}

// Command controls exactly one bounded inventory page. Cursor is opaque so it
// may be an S3 continuation token; only surrounding whitespace is rejected.
type Command struct {
	Cursor string
	Limit  int32
}

func (command Command) Validate() error {
	if command.Limit < 1 || command.Limit > maxPageSize || strings.TrimSpace(command.Cursor) != command.Cursor {
		return ErrInvalidCommand
	}
	return nil
}

// Result intentionally contains counts and cursor state only. Orphan keys are
// tenant-scoped operational identifiers and must not be written to regular
// logs or surfaced by an unauthenticated HTTP endpoint.
type Result struct {
	Scanned    int
	Orphaned   int
	NextCursor string
	Complete   bool
}

// Usecase compares one managed R2 page with Mongo assets. It neither deletes
// objects nor changes a creation, step, outbox event, or ledger row.
type Usecase struct {
	inventory Inventory
	assets    AssetIndex
}

func NewUsecase(inventory Inventory, assets AssetIndex) *Usecase {
	return &Usecase{inventory: inventory, assets: assets}
}

func (usecase *Usecase) Audit(ctx context.Context, command Command) (Result, error) {
	if usecase == nil || usecase.inventory == nil || usecase.assets == nil {
		return Result{}, ErrDependenciesUnavailable
	}
	if err := command.Validate(); err != nil {
		return Result{}, err
	}
	page, err := usecase.inventory.List(ctx, ManagedResultPrefix, command.Cursor, command.Limit)
	if err != nil {
		return Result{}, err
	}
	storageKeys := make([]string, 0, len(page.Objects))
	seen := make(map[string]struct{}, len(page.Objects))
	for _, object := range page.Objects {
		if !validObject(object) {
			return Result{}, ErrInvalidObject
		}
		if _, exists := seen[object.StorageKey]; exists {
			return Result{}, ErrInvalidObject
		}
		seen[object.StorageKey] = struct{}{}
		storageKeys = append(storageKeys, object.StorageKey)
	}
	if len(storageKeys) == 0 {
		return Result{NextCursor: page.NextCursor, Complete: page.NextCursor == ""}, nil
	}
	referenced, err := usecase.assets.ReferencedStorageKeys(ctx, storageKeys)
	if err != nil {
		return Result{}, err
	}
	orphaned := 0
	for _, storageKey := range storageKeys {
		if _, exists := referenced[storageKey]; !exists {
			orphaned++
		}
	}
	return Result{
		Scanned: len(storageKeys), Orphaned: orphaned, NextCursor: page.NextCursor, Complete: page.NextCursor == "",
	}, nil
}

func validObject(object Object) bool {
	if !strings.HasPrefix(object.Key, ManagedResultPrefix) || len(object.Key) <= len(ManagedResultPrefix) ||
		len(object.Key) > 1024 || strings.TrimSpace(object.Key) != object.Key || strings.Contains(object.Key, "..") {
		return false
	}
	parsed, err := url.Parse(object.StorageKey)
	return err == nil && object.StorageKey == strings.TrimSpace(object.StorageKey) && parsed.Scheme == "https" &&
		parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path != ""
}
