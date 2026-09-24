package schema

import (
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOutboxRetentionIndexesAreScopedPartialTTL(t *testing.T) {
	got := OutboxRetentionIndexes()
	want := []struct {
		name    string
		seconds int32
		status  string
	}{
		{name: "ix_outbox_events_delivered_updated_at_ttl", seconds: 14 * 24 * 60 * 60, status: "delivered"},
		{name: "ix_outbox_events_failed_updated_at_ttl", seconds: 30 * 24 * 60 * 60, status: "failed"},
		{name: "ix_outbox_events_needs_attention_updated_at_ttl", seconds: 90 * 24 * 60 * 60, status: "needs_attention"},
	}
	if len(got) != len(want) {
		t.Fatalf("OutboxRetentionIndexes() len = %d, want %d", len(got), len(want))
	}
	for i, expected := range want {
		spec := got[i]
		if spec.Collection != CollectionOutboxEvents || spec.Name != expected.name {
			t.Fatalf("retention index %d identity = %#v, want collection=%q name=%q", i, spec, CollectionOutboxEvents, expected.name)
		}
		if !reflect.DeepEqual(spec.Keys, bson.D{{Key: "updated_at", Value: 1}}) {
			t.Errorf("retention index %q keys = %#v, want updated_at ascending", spec.Name, spec.Keys)
		}
		if spec.ExpireAfterSeconds == nil || *spec.ExpireAfterSeconds != expected.seconds {
			t.Errorf("retention index %q expireAfterSeconds = %#v, want %d", spec.Name, spec.ExpireAfterSeconds, expected.seconds)
		}
		if spec.Unique || spec.Sparse {
			t.Errorf("retention index %q must not be unique/sparse: %#v", spec.Name, spec)
		}
		if !reflect.DeepEqual(spec.PartialFilter, bson.D{{Key: "delivery_status", Value: expected.status}}) {
			t.Errorf("retention index %q partialFilter = %#v, want status=%q", spec.Name, spec.PartialFilter, expected.status)
		}
	}
}

func TestAllIndexesDoesNotEnableOutboxRetentionAutomatically(t *testing.T) {
	for _, spec := range AllIndexes() {
		for _, retention := range OutboxRetentionIndexes() {
			if spec.Name == retention.Name {
				t.Fatalf("retention index %q must not be created by normal schema startup", spec.Name)
			}
		}
	}
}
