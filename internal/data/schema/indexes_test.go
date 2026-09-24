package schema

import (
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestAllCollectionsReturnsIndependentOrderedList(t *testing.T) {
	want := []string{
		"accounts",
		"users",
		"credentials",
		"identities",
		"sessions",
		"templates",
		"assets",
		"creations",
		"creation_steps",
		"generation_step_recipes",
		"reservations",
		"daily_quotas",
		"ledger_entries",
		"subscriptions",
		"payment_products",
		"payment_orders",
		"payment_receipts",
		"payment_callback_nonces",
		"callback_receipts",
		"generation_provider_inbox",
		"generation_product_recipes",
		"generation_model_mappings",
		"outbox_events",
		"feedbacks",
		"notifications",
		"notification_preferences",
		"admin_audit",
		"blog_posts",
		"utmlinks",
		"admin_menu_visibility",
		"apps",
		"platform_configs",
		"admin_pricing_configs",
		"admin_review_items",
		"admin_review_projection_state",
	}

	got := AllCollections()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllCollections() = %#v, want %#v", got, want)
	}

	got[0] = "changed"
	if next := AllCollections(); !reflect.DeepEqual(next, want) {
		t.Fatalf("AllCollections() after mutation = %#v, want %#v", next, want)
	}
}

func TestAllIndexesReturnsExactIndependentSpecs(t *testing.T) {
	want := expectedIndexes()
	got := AllIndexes()

	assertIndexSpecsEqual(t, want, got)

	got[0].Collection = "changed"
	got[0].Name = "changed"
	got[0].Keys[0].Key = "changed"
	got[0].Unique = false
	got[0].Sparse = true
	if got[len(got)-1].ExpireAfterSeconds != nil {
		*got[len(got)-1].ExpireAfterSeconds = 1
	}
	assertIndexSpecsEqual(t, want, AllIndexes())
}

func expectedIndexes() []IndexSpec {
	return []IndexSpec{
		{Collection: "credentials", Name: "ix_credentials_user_active", Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "active", Value: 1}}},
		{Collection: "identities", Name: "ix_identities_user", Keys: bson.D{{Key: "user_id", Value: 1}}},
		{
			Collection: "identities",
			Name:       "ux_identities_provider_subject",
			Keys:       bson.D{{Key: "provider", Value: 1}, {Key: "subject", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "sessions",
			Name:       "ix_sessions_user_revoked",
			Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "revoked_at", Value: 1}},
		},
		{
			Collection:    "credentials",
			Name:          "ux_credentials_active_email",
			Keys:          bson.D{{Key: "email_normalized", Value: 1}},
			Unique:        true,
			PartialFilter: bson.D{{Key: "active", Value: true}},
		},
		{
			Collection: "users",
			Name:       "ux_users_guest_platform_device",
			Keys:       bson.D{{Key: "guest_platform", Value: 1}, {Key: "guest_device_id", Value: 1}},
			Unique:     true,
			Sparse:     true,
		},
		{
			Collection: "templates",
			Name:       "ux_templates_template_version",
			Keys:       bson.D{{Key: "template_id", Value: 1}, {Key: "version", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "templates",
			Name:       "ix_templates_enabled_surface_sort",
			Keys:       bson.D{{Key: "enabled", Value: 1}, {Key: "content_surface", Value: 1}, {Key: "sort_order", Value: 1}},
		},
		{
			Collection: "assets",
			Name:       "ix_assets_owner_created",
			Keys:       bson.D{{Key: "owner_type", Value: 1}, {Key: "owner_id", Value: 1}, {Key: "created_at", Value: -1}},
		},
		{
			Collection: "assets",
			Name:       "ix_assets_storage_key_owner_status",
			Keys:       bson.D{{Key: "storage_key", Value: 1}, {Key: "owner_type", Value: 1}, {Key: "status", Value: 1}},
		},
		{
			Collection: "creations",
			Name:       "ux_creations_idempotency_key",
			Keys:       bson.D{{Key: "idempotency_key", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "creations",
			Name:       "ix_creations_user_created",
			Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}},
		},
		{
			Collection: "creations",
			Name:       "ix_creations_user_output_created_id",
			Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "product_output", Value: 1}, {Key: "created_at", Value: -1}, {Key: "_id", Value: -1}},
		},
		{
			Collection: "creation_steps",
			Name:       "ux_creation_steps_creation_sequence",
			Keys:       bson.D{{Key: "creation_id", Value: 1}, {Key: "sequence", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "generation_step_recipes",
			Name:       "ux_generation_step_recipes_step_id",
			Keys:       bson.D{{Key: "step_id", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "creation_steps",
			Name:       "ix_creation_steps_external_execution",
			Keys:       bson.D{{Key: "external_execution_id", Value: 1}},
		},
		{
			Collection: "creation_steps",
			Name:       "ux_creation_steps_provider_account_external_execution_present",
			Keys:       bson.D{{Key: "provider", Value: 1}, {Key: "account_ref", Value: 1}, {Key: "external_execution_id", Value: 1}},
			Unique:     true,
			PartialFilter: bson.D{
				{Key: "provider", Value: bson.D{{Key: "$type", Value: "string"}}},
				{Key: "account_ref", Value: bson.D{{Key: "$type", Value: "string"}}},
				{Key: "external_execution_id", Value: bson.D{{Key: "$type", Value: "string"}}},
			},
		},
		{
			Collection: "reservations",
			Name:       "ux_reservations_creation_id",
			Keys:       bson.D{{Key: "creation_id", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "daily_quotas",
			Name:       "ux_daily_quotas_user_kind_date",
			Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "quota_kind", Value: 1}, {Key: "local_date", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "ledger_entries",
			Name:       "ux_ledger_entries_idempotency_key",
			Keys:       bson.D{{Key: "idempotency_key", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "ledger_entries",
			Name:       "ix_ledger_entries_account_created",
			Keys:       bson.D{{Key: "account_id", Value: 1}, {Key: "created_at", Value: -1}},
		},
		{
			Collection: "payment_products",
			Name:       "ux_payment_products_product_version",
			Keys:       bson.D{{Key: "product_id", Value: 1}, {Key: "version", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "payment_orders",
			Name:       "ix_payment_orders_user_created",
			Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}},
		},
		{
			Collection: "payment_orders",
			Name:       "ux_payment_orders_provider_provider_order",
			Keys:       bson.D{{Key: "provider", Value: 1}, {Key: "provider_order_id", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "payment_receipts",
			Name:       "ux_payment_receipts_provider_external_transaction",
			Keys:       bson.D{{Key: "provider", Value: 1}, {Key: "external_transaction_id", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "payment_callback_nonces",
			Name:       "ux_payment_callback_nonces_nonce_hash",
			Keys:       bson.D{{Key: "nonce_hash", Value: 1}},
			Unique:     true,
		},
		{
			Collection:         "payment_callback_nonces",
			Name:               "ix_payment_callback_nonces_expires_at_ttl",
			Keys:               bson.D{{Key: "expires_at", Value: 1}},
			ExpireAfterSeconds: testInt32Pointer(0),
		},
		{
			Collection: "callback_receipts",
			Name:       "ux_callback_receipts_source_nonce_hash",
			Keys:       bson.D{{Key: "source", Value: 1}, {Key: "nonce_hash", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "callback_receipts",
			Name:       "ix_callback_receipts_step_terminal",
			Keys:       bson.D{{Key: "source", Value: 1}, {Key: "step_id", Value: 1}, {Key: "job_id", Value: 1}, {Key: "capability", Value: 1}, {Key: "terminal", Value: 1}},
		},
		{
			Collection: "generation_provider_inbox",
			Name:       "ux_generation_provider_inbox_source_account_delivery",
			Keys:       bson.D{{Key: "source", Value: 1}, {Key: "account_ref", Value: 1}, {Key: "delivery_id", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "generation_provider_inbox",
			Name:       "ix_generation_provider_inbox_step_status_updated",
			Keys:       bson.D{{Key: "step_id", Value: 1}, {Key: "status", Value: 1}, {Key: "updated_at", Value: -1}},
		},
		{
			Collection: "generation_product_recipes",
			Name:       "ux_generation_product_recipes_template_version_atom",
			Keys:       bson.D{{Key: "template_id", Value: 1}, {Key: "template_version", Value: 1}, {Key: "atom", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "generation_model_mappings",
			Name:       "ix_generation_model_mappings_status_published_at",
			Keys:       bson.D{{Key: "status", Value: 1}, {Key: "published_at", Value: -1}},
		},
		{
			Collection: "outbox_events",
			Name:       "ix_outbox_events_status_next_attempt",
			Keys:       bson.D{{Key: "delivery_status", Value: 1}, {Key: "next_attempt_at", Value: 1}},
		},
		{
			Collection: "outbox_events",
			Name:       "ix_outbox_events_claim_type_status_next_attempt_lease_until",
			Keys:       bson.D{{Key: "event_type", Value: 1}, {Key: "delivery_status", Value: 1}, {Key: "next_attempt_at", Value: 1}, {Key: "lease_until", Value: 1}},
		},
		{
			Collection: "notifications",
			Name:       "ix_notifications_user_read_created",
			Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "read", Value: 1}, {Key: "created_at", Value: -1}},
		},
		{
			Collection: "blog_posts",
			Name:       "ux_blog_posts_slug",
			Keys:       bson.D{{Key: "slug", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "blog_posts",
			Name:       "ix_blog_posts_status_created",
			Keys:       bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: -1}},
		},
		{
			Collection: "utmlinks",
			Name:       "ux_utmlinks_slug",
			Keys:       bson.D{{Key: "slug", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "utmlinks",
			Name:       "ix_utmlinks_enabled_created",
			Keys:       bson.D{{Key: "enabled", Value: 1}, {Key: "createdAt", Value: -1}},
		},
		{
			Collection:    "apps",
			Name:          "uniq_android_native_identifiers",
			Keys:          bson.D{{Key: "nativeIdentifiers", Value: 1}},
			Unique:        true,
			PartialFilter: bson.D{{Key: "nativeIdentifiers.0", Value: bson.D{{Key: "$exists", Value: true}}}},
		},
		{
			Collection: "platform_configs",
			Name:       "ux_platform_configs_platform_client",
			Keys:       bson.D{{Key: "platform", Value: 1}, {Key: "clientId", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "admin_pricing_configs",
			Name:       "ux_admin_pricing_configs_key_environment",
			Keys:       bson.D{{Key: "key", Value: 1}, {Key: "environment", Value: 1}},
			Unique:     true,
		},
		{
			Collection: "admin_review_items",
			Name:       "ix_admin_review_items_media_status_created",
			Keys:       bson.D{{Key: "media_type", Value: 1}, {Key: "review_status", Value: 1}, {Key: "created_at", Value: -1}, {Key: "_id", Value: -1}},
		},
		{
			Collection:    "admin_review_items",
			Name:          "ux_admin_review_items_source_legacy",
			Keys:          bson.D{{Key: "source", Value: 1}, {Key: "legacy_source_id", Value: 1}},
			Unique:        true,
			PartialFilter: bson.D{{Key: "legacy_source_id", Value: bson.D{{Key: "$exists", Value: true}}}},
		},
	}
}

func testInt32Pointer(value int32) *int32 {
	return &value
}

func assertIndexSpecsEqual(t *testing.T, want, got []IndexSpec) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("len(AllIndexes()) = %d, want %d", len(got), len(want))
	}

	for i := range want {
		if got[i].Collection != want[i].Collection {
			t.Errorf("index %d collection = %q, want %q", i, got[i].Collection, want[i].Collection)
		}
		if got[i].Name != want[i].Name {
			t.Errorf("index %d name = %q, want %q", i, got[i].Name, want[i].Name)
		}
		if !reflect.DeepEqual(got[i].Keys, want[i].Keys) {
			t.Errorf("index %d keys = %#v, want %#v", i, got[i].Keys, want[i].Keys)
		}
		if got[i].Unique != want[i].Unique {
			t.Errorf("index %d unique = %t, want %t", i, got[i].Unique, want[i].Unique)
		}
		if got[i].Sparse != want[i].Sparse {
			t.Errorf("index %d sparse = %t, want %t", i, got[i].Sparse, want[i].Sparse)
		}
		if !reflect.DeepEqual(got[i].ExpireAfterSeconds, want[i].ExpireAfterSeconds) {
			t.Errorf("index %d expireAfterSeconds = %#v, want %#v", i, got[i].ExpireAfterSeconds, want[i].ExpireAfterSeconds)
		}
		if !reflect.DeepEqual(got[i].PartialFilter, want[i].PartialFilter) {
			t.Errorf("index %d partialFilter = %#v, want %#v", i, got[i].PartialFilter, want[i].PartialFilter)
		}
	}
}
