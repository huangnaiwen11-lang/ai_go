package r2audit

import (
	"context"
	"testing"
)

func TestUsecaseAuditCountsOnlyUnreferencedManagedObjects(t *testing.T) {
	inventory := &inventoryFake{page: Page{
		Objects: []Object{
			{Key: "gen/text_to_image/tenant-a/step-1.png", StorageKey: "https://media.example.test/gen/text_to_image/tenant-a/step-1.png"},
			{Key: "gen/text_to_image/tenant-a/step-2.png", StorageKey: "https://media.example.test/gen/text_to_image/tenant-a/step-2.png"},
			{Key: "gen/image_to_video/tenant-a/step-3.mp4", StorageKey: "https://media.example.test/gen/image_to_video/tenant-a/step-3.mp4"},
		},
		NextCursor: "next-page",
	}}
	assets := &assetIndexFake{referenced: map[string]struct{}{
		"https://media.example.test/gen/text_to_image/tenant-a/step-1.png":  {},
		"https://media.example.test/gen/image_to_video/tenant-a/step-3.mp4": {},
	}}

	result, err := NewUsecase(inventory, assets).Audit(context.Background(), Command{Limit: 100})
	if err != nil {
		t.Fatalf("Audit() error = %v", err)
	}
	if result.Scanned != 3 || result.Orphaned != 1 || result.NextCursor != "next-page" || result.Complete {
		t.Fatalf("Audit() result = %#v", result)
	}
	if inventory.prefix != ManagedResultPrefix || inventory.limit != 100 {
		t.Fatalf("list = prefix %q, limit %d", inventory.prefix, inventory.limit)
	}
	if len(assets.storageKeys) != 3 {
		t.Fatalf("asset lookup keys = %#v", assets.storageKeys)
	}
}

func TestUsecaseAuditRejectsObjectOutsideManagedPrefixBeforeAssetLookup(t *testing.T) {
	inventory := &inventoryFake{page: Page{Objects: []Object{{
		Key: "img-cache/unrelated.webp", StorageKey: "https://media.example.test/img-cache/unrelated.webp",
	}}}}
	assets := &assetIndexFake{}

	_, err := NewUsecase(inventory, assets).Audit(context.Background(), Command{Limit: 1})
	if err != ErrInvalidObject {
		t.Fatalf("Audit() error = %v, want ErrInvalidObject", err)
	}
	if len(assets.storageKeys) != 0 {
		t.Fatalf("asset lookup ran for invalid object: %#v", assets.storageKeys)
	}
}

func TestUsecaseAuditRejectsInvalidCommandBeforeInventory(t *testing.T) {
	inventory := &inventoryFake{}

	_, err := NewUsecase(inventory, &assetIndexFake{}).Audit(context.Background(), Command{Limit: 1001})
	if err != ErrInvalidCommand {
		t.Fatalf("Audit() error = %v, want ErrInvalidCommand", err)
	}
	if inventory.calls != 0 {
		t.Fatalf("inventory calls = %d, want 0", inventory.calls)
	}
}

type inventoryFake struct {
	page   Page
	err    error
	calls  int
	prefix string
	cursor string
	limit  int32
}

func (fake *inventoryFake) List(_ context.Context, prefix, cursor string, limit int32) (Page, error) {
	fake.calls++
	fake.prefix, fake.cursor, fake.limit = prefix, cursor, limit
	return fake.page, fake.err
}

type assetIndexFake struct {
	referenced  map[string]struct{}
	storageKeys []string
	err         error
}

func (fake *assetIndexFake) ReferencedStorageKeys(_ context.Context, storageKeys []string) (map[string]struct{}, error) {
	fake.storageKeys = append([]string(nil), storageKeys...)
	return fake.referenced, fake.err
}
