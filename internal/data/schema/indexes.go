package schema

import "go.mongodb.org/mongo-driver/v2/bson"

// IndexSpec 描述一个 MongoDB 集合索引的声明。
type IndexSpec struct {
	Collection         string
	Name               string
	Keys               bson.D
	Unique             bool
	Sparse             bool
	ExpireAfterSeconds *int32
	PartialFilter      bson.D
}

var indexSpecs = []IndexSpec{
	{
		Collection: CollectionIdentities,
		Name:       "ux_identities_provider_subject",
		Keys:       bson.D{{Key: "provider", Value: 1}, {Key: "subject", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionSessions,
		Name:       "ix_sessions_user_revoked",
		Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "revoked_at", Value: 1}},
	},
	{
		Collection:    CollectionCredentials,
		Name:          "ux_credentials_active_email",
		Keys:          bson.D{{Key: "email_normalized", Value: 1}},
		Unique:        true,
		PartialFilter: bson.D{{Key: "active", Value: true}},
	},
	{
		Collection: CollectionUsers,
		Name:       "ux_users_guest_platform_device",
		Keys:       bson.D{{Key: "guest_platform", Value: 1}, {Key: "guest_device_id", Value: 1}},
		Unique:     true,
		Sparse:     true,
	},
	{
		Collection: CollectionTemplates,
		Name:       "ux_templates_template_version",
		Keys:       bson.D{{Key: "template_id", Value: 1}, {Key: "version", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionTemplates,
		Name:       "ix_templates_enabled_surface_sort",
		Keys:       bson.D{{Key: "enabled", Value: 1}, {Key: "content_surface", Value: 1}, {Key: "sort_order", Value: 1}},
	},
	{
		Collection: CollectionAssets,
		Name:       "ix_assets_owner_created",
		Keys:       bson.D{{Key: "owner_type", Value: 1}, {Key: "owner_id", Value: 1}, {Key: "created_at", Value: -1}},
	},
	{
		Collection: CollectionCreations,
		Name:       "ux_creations_idempotency_key",
		Keys:       bson.D{{Key: "idempotency_key", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionCreations,
		Name:       "ix_creations_user_created",
		Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}},
	},
	{
		// 作品历史按本人、输出类型、创建时间和稳定 ID 分页；复合索引与查询及排序保持同序，
		// 避免列表翻页退化为集合扫描，也保证同一时间戳的游标位置可持续命中。
		Collection: CollectionCreations,
		Name:       "ix_creations_user_output_created_id",
		Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "product_output", Value: 1}, {Key: "created_at", Value: -1}, {Key: "_id", Value: -1}},
	},
	{
		Collection: CollectionCreationSteps,
		Name:       "ux_creation_steps_creation_sequence",
		Keys:       bson.D{{Key: "creation_id", Value: 1}, {Key: "sequence", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionGenerationStepRecipes,
		Name:       "ux_generation_step_recipes_step_id",
		Keys:       bson.D{{Key: "step_id", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionCreationSteps,
		Name:       "ix_creation_steps_external_execution",
		Keys:       bson.D{{Key: "external_execution_id", Value: 1}},
	},
	{
		Collection: CollectionCreationSteps,
		Name:       "ux_creation_steps_external_execution_present",
		Keys:       bson.D{{Key: "external_execution_id", Value: 1}},
		Unique:     true,
		Sparse:     true,
	},
	{
		Collection: CollectionReservations,
		Name:       "ux_reservations_creation_id",
		Keys:       bson.D{{Key: "creation_id", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionDailyQuotas,
		Name:       "ux_daily_quotas_user_kind_date",
		Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "quota_kind", Value: 1}, {Key: "local_date", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionLedgerEntries,
		Name:       "ux_ledger_entries_idempotency_key",
		Keys:       bson.D{{Key: "idempotency_key", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionLedgerEntries,
		Name:       "ix_ledger_entries_account_created",
		Keys:       bson.D{{Key: "account_id", Value: 1}, {Key: "created_at", Value: -1}},
	},
	{
		Collection: CollectionPaymentProducts,
		Name:       "ux_payment_products_product_version",
		Keys:       bson.D{{Key: "product_id", Value: 1}, {Key: "version", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionPaymentOrders,
		Name:       "ix_payment_orders_user_created",
		Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}},
	},
	{
		Collection: CollectionPaymentOrders,
		Name:       "ux_payment_orders_provider_provider_order",
		Keys:       bson.D{{Key: "provider", Value: 1}, {Key: "provider_order_id", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionPaymentReceipts,
		Name:       "ux_payment_receipts_provider_external_transaction",
		Keys:       bson.D{{Key: "provider", Value: 1}, {Key: "external_transaction_id", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionPaymentCallbackNonces,
		Name:       "ux_payment_callback_nonces_nonce_hash",
		Keys:       bson.D{{Key: "nonce_hash", Value: 1}},
		Unique:     true,
	},
	{
		Collection:         CollectionPaymentCallbackNonces,
		Name:               "ix_payment_callback_nonces_expires_at_ttl",
		Keys:               bson.D{{Key: "expires_at", Value: 1}},
		ExpireAfterSeconds: int32Pointer(0),
	},
	{
		Collection: CollectionCallbackReceipts,
		Name:       "ux_callback_receipts_source_nonce_hash",
		Keys:       bson.D{{Key: "source", Value: 1}, {Key: "nonce_hash", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionCallbackReceipts,
		Name:       "ix_callback_receipts_step_terminal",
		Keys:       bson.D{{Key: "source", Value: 1}, {Key: "step_id", Value: 1}, {Key: "job_id", Value: 1}, {Key: "capability", Value: 1}, {Key: "terminal", Value: 1}},
	},
	{
		Collection: CollectionOutboxEvents,
		Name:       "ix_outbox_events_status_next_attempt",
		Keys:       bson.D{{Key: "delivery_status", Value: 1}, {Key: "next_attempt_at", Value: 1}},
	},
	{
		Collection: CollectionOutboxEvents,
		Name:       "ix_outbox_events_claim_type_status_next_attempt_lease_until",
		Keys:       bson.D{{Key: "event_type", Value: 1}, {Key: "delivery_status", Value: 1}, {Key: "next_attempt_at", Value: 1}, {Key: "lease_until", Value: 1}},
	},
	{
		// 通知列表按用户隔离并按创建时间倒序；未读数复用 user_id/read 前缀。
		Collection: CollectionNotifications,
		Name:       "ix_notifications_user_read_created",
		Keys:       bson.D{{Key: "user_id", Value: 1}, {Key: "read", Value: 1}, {Key: "created_at", Value: -1}},
	},
}

// AllIndexes 返回所有索引规格的独立副本，调用方可安全修改返回值。
func AllIndexes() []IndexSpec {
	indexes := make([]IndexSpec, len(indexSpecs))
	for i, spec := range indexSpecs {
		indexes[i] = spec
		indexes[i].Keys = append(bson.D(nil), spec.Keys...)
		indexes[i].PartialFilter = append(bson.D(nil), spec.PartialFilter...)
		if spec.ExpireAfterSeconds != nil {
			expireAfterSeconds := *spec.ExpireAfterSeconds
			indexes[i].ExpireAfterSeconds = &expireAfterSeconds
		}
	}

	return indexes
}

func int32Pointer(value int32) *int32 {
	return &value
}
