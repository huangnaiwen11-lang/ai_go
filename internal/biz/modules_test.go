package biz

import "testing"

func TestModuleRegistryListsAllClingBusinessBoundaries(t *testing.T) {
	registry := NewModuleRegistry()
	want := []string{"identity", "catalog", "entitlement", "ledger", "generation", "payments", "creations", "works", "notification"}

	if len(registry.Names) != len(want) {
		t.Fatalf("module count = %d, want %d", len(registry.Names), len(want))
	}
	for index, name := range want {
		if registry.Names[index] != name {
			t.Fatalf("module[%d] = %q, want %q", index, registry.Names[index], name)
		}
	}
}
