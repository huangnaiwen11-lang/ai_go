package schema

import (
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// IndexSpec 描述一个 MongoDB 集合索引的声明。
type IndexSpec struct {
	Collection         string
	Name               string
	Keys               bson.D
	Unique             bool
	Sparse             bool
	ExpireAfterSeconds *int32
	PartialFilter      bson.D
	Collation          *options.Collation
}

// RuntimeAppIdentifierCollation preserves case-insensitive App identifier
// matching while allowing MongoDB to perform exact indexed equality lookups.
// It must be used both by the lookup query and every matching index.
func RuntimeAppIdentifierCollation() *options.Collation {
	return &options.Collation{Locale: "en", Strength: 2}
}

var runtimeAppIndexNames = map[string]struct{}{
	"ix_apps_runtime_client_id":              {},
	"ix_apps_runtime_package_name":           {},
	"ix_apps_runtime_android_application_id": {},
	"ix_apps_runtime_bundle_id":              {},
	"ix_apps_runtime_ios_bundle_id":          {},
	"ix_apps_runtime_domain":                 {},
}

const (
	// LegacyCreationStepsExternalExecutionUniqueIndex is deliberately absent
	// from indexSpecs. Existing deployments retain it until the standalone,
	// preflight-gated migration retires it; automatic schema initialization
	// must never recreate a global job-ID constraint after that retirement.
	LegacyCreationStepsExternalExecutionUniqueIndex = "ux_creation_steps_external_execution_present"
	// CreationStepsScopedExternalExecutionUniqueIndex scopes provider job IDs
	// to the immutable provider/account route frozen on a creation step.
	CreationStepsScopedExternalExecutionUniqueIndex = "ux_creation_steps_provider_account_external_execution_present"
)

var indexSpecs = []IndexSpec{
	{Collection: CollectionCredentials, Name: "ix_credentials_user_active", Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "active", Value: 1}}},
	{Collection: CollectionIdentities, Name: "ix_identities_user", Keys: bson.D{{Key: "user_id", Value: 1}}},
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
		// R2 孤儿审计按一页对象的规范 storage_key 做精确查找，并且只把
		// 可用的 creation_step 资产视为引用。三列都为等值条件，避免每页
		// 退化为全 assets 集合扫描；该索引不唯一，也不改变发布语义。
		Collection: CollectionAssets,
		Name:       "ix_assets_storage_key_owner_status",
		Keys:       bson.D{{Key: "storage_key", Value: 1}, {Key: "owner_type", Value: 1}, {Key: "status", Value: 1}},
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
	CreationStepsScopedExternalExecutionIndex(),
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
		Collection: CollectionGenerationProviderInbox,
		Name:       "ux_generation_provider_inbox_source_account_delivery",
		Keys:       bson.D{{Key: "source", Value: 1}, {Key: "account_ref", Value: 1}, {Key: "delivery_id", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionGenerationProviderInbox,
		Name:       "ix_generation_provider_inbox_step_status_updated",
		Keys:       bson.D{{Key: "step_id", Value: 1}, {Key: "status", Value: 1}, {Key: "updated_at", Value: -1}},
	},
	{
		// A published recipe is addressed by the immutable visible template version
		// and one technical atom. The unique index prevents an ambiguous runtime
		// “pick one of two recipes” decision.
		Collection: CollectionGenerationProductRecipes,
		Name:       "ux_generation_product_recipes_template_version_atom",
		Keys:       bson.D{{Key: "template_id", Value: 1}, {Key: "template_version", Value: 1}, {Key: "atom", Value: 1}},
		Unique:     true,
	},
	{
		// 读取「最新已发布目录版本」是 admission 与 Worker 的热路径查询；
		// 索引与排序同序，避免每次映射都退化为集合扫描。
		Collection: CollectionGenerationModelMappings,
		Name:       "ix_generation_model_mappings_status_published_at",
		Keys:       bson.D{{Key: "status", Value: 1}, {Key: "published_at", Value: -1}},
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
	{
		// 博客 slug 是站内稳定标识，必须唯一，否则同一路径会出现两篇文章。
		Collection: CollectionBlogPosts,
		Name:       "ux_blog_posts_slug",
		Keys:       bson.D{{Key: "slug", Value: 1}},
		Unique:     true,
	},
	{
		// 后台列表默认按状态筛选并按创建时间倒序。
		Collection: CollectionBlogPosts,
		Name:       "ix_blog_posts_status_created",
		Keys:       bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: -1}},
	},
	{
		// slug 是公开短链 `/api/v1/r/<slug>` 的唯一标识，重复会让跳转产生歧义。
		// 唯一约束同时让并发创建同名 slug 由数据库裁决，而不是靠先查后写的竞态。
		Collection: CollectionUTMLinks,
		Name:       "ux_utmlinks_slug",
		Keys:       bson.D{{Key: "slug", Value: 1}},
		Unique:     true,
	},
	{
		// 后台列表按启用状态过滤并按创建时间倒序。
		Collection: CollectionUTMLinks,
		Name:       "ix_utmlinks_enabled_created",
		Keys:       bson.D{{Key: "enabled", Value: 1}, {Key: "createdAt", Value: -1}},
	},
	{
		// 与 Node `models/App.js` 的 uniq_android_native_identifiers 逐字一致。
		//
		// 这是 App 唯一性的**真正保障**：写入前的四次 findOne 与写入之间没有事务边界，
		// 并发创建同名包标识只能由这个唯一索引裁决（11000 -> 409）。
		// partialFilterExpression 也不能省 —— 没有标识的 App（例如 web）会在
		// nativeIdentifiers 缺失时被当成同一个 null 而互相冲突。
		Collection:    CollectionApps,
		Name:          "uniq_android_native_identifiers",
		Keys:          bson.D{{Key: "nativeIdentifiers", Value: 1}},
		Unique:        true,
		PartialFilter: bson.D{{Key: "nativeIdentifiers.0", Value: bson.D{{Key: "$exists", Value: true}}}},
	},
	// Runtime App scope resolves one active App through a case-insensitive,
	// exact match on platform-specific legacy fields. Keep one index per $or
	// branch: a compound index cannot cover different terminal fields in the
	// same query. The collation must exactly match the resolver's Find option.
	{
		Collection: CollectionApps,
		Name:       "ix_apps_runtime_client_id",
		Keys:       bson.D{{Key: "platform", Value: 1}, {Key: "status", Value: 1}, {Key: "clientId", Value: 1}},
		Collation:  RuntimeAppIdentifierCollation(),
	},
	{
		Collection: CollectionApps,
		Name:       "ix_apps_runtime_package_name",
		Keys:       bson.D{{Key: "platform", Value: 1}, {Key: "status", Value: 1}, {Key: "packageName", Value: 1}},
		Collation:  RuntimeAppIdentifierCollation(),
	},
	{
		Collection: CollectionApps,
		Name:       "ix_apps_runtime_android_application_id",
		Keys:       bson.D{{Key: "platform", Value: 1}, {Key: "status", Value: 1}, {Key: "nativeBuild.android.applicationId", Value: 1}},
		Collation:  RuntimeAppIdentifierCollation(),
	},
	{
		Collection: CollectionApps,
		Name:       "ix_apps_runtime_bundle_id",
		Keys:       bson.D{{Key: "platform", Value: 1}, {Key: "status", Value: 1}, {Key: "bundleId", Value: 1}},
		Collation:  RuntimeAppIdentifierCollation(),
	},
	{
		Collection: CollectionApps,
		Name:       "ix_apps_runtime_ios_bundle_id",
		Keys:       bson.D{{Key: "platform", Value: 1}, {Key: "status", Value: 1}, {Key: "nativeBuild.ios.bundleId", Value: 1}},
		Collation:  RuntimeAppIdentifierCollation(),
	},
	{
		Collection: CollectionApps,
		Name:       "ix_apps_runtime_domain",
		Keys:       bson.D{{Key: "platform", Value: 1}, {Key: "status", Value: 1}, {Key: "domain", Value: 1}},
		Collation:  RuntimeAppIdentifierCollation(),
	},
	{
		Collection: CollectionPlatformConfigs,
		Name:       "ux_platform_configs_platform_client",
		Keys:       bson.D{{Key: "platform", Value: 1}, {Key: "clientId", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionAdminPricingConfigs,
		Name:       "ux_admin_pricing_configs_key_environment",
		Keys:       bson.D{{Key: "key", Value: 1}, {Key: "environment", Value: 1}},
		Unique:     true,
	},
	{
		Collection: CollectionAdminReviewItems,
		Name:       "ix_admin_review_items_media_status_created",
		Keys:       bson.D{{Key: "media_type", Value: 1}, {Key: "review_status", Value: 1}, {Key: "created_at", Value: -1}, {Key: "_id", Value: -1}},
	},
	{
		Collection:    CollectionAdminReviewItems,
		Name:          "ux_admin_review_items_source_legacy",
		Keys:          bson.D{{Key: "source", Value: 1}, {Key: "legacy_source_id", Value: 1}},
		Unique:        true,
		PartialFilter: bson.D{{Key: "legacy_source_id", Value: bson.D{{Key: "$exists", Value: true}}}},
	},
}

// CreationStepsScopedExternalExecutionIndex returns the desired non-global
// uniqueness constraint for provider jobs. The type-only partial filter keeps
// unbound and legacy steps out of the index; migration preflight separately
// rejects empty or malformed strings before the constraint is installed on an
// existing database.
func CreationStepsScopedExternalExecutionIndex() IndexSpec {
	return IndexSpec{
		Collection: CollectionCreationSteps,
		Name:       CreationStepsScopedExternalExecutionUniqueIndex,
		Keys:       bson.D{{Key: "provider", Value: 1}, {Key: "account_ref", Value: 1}, {Key: "external_execution_id", Value: 1}},
		Unique:     true,
		PartialFilter: bson.D{
			{Key: "provider", Value: bson.D{{Key: "$type", Value: "string"}}},
			{Key: "account_ref", Value: bson.D{{Key: "$type", Value: "string"}}},
			{Key: "external_execution_id", Value: bson.D{{Key: "$type", Value: "string"}}},
		},
	}
}

// AllIndexes 返回所有索引规格的独立副本，调用方可安全修改返回值。
func AllIndexes() []IndexSpec {
	indexes := make([]IndexSpec, len(indexSpecs))
	for i, spec := range indexSpecs {
		indexes[i] = spec
		indexes[i].Keys = append(bson.D(nil), spec.Keys...)
		indexes[i].PartialFilter = append(bson.D(nil), spec.PartialFilter...)
		if spec.Collation != nil {
			collation := *spec.Collation
			indexes[i].Collation = &collation
		}
		if spec.ExpireAfterSeconds != nil {
			expireAfterSeconds := *spec.ExpireAfterSeconds
			indexes[i].ExpireAfterSeconds = &expireAfterSeconds
		}
	}

	return indexes
}

// RuntimeAppIndexes returns only the indexes required by the runtime App
// resolver. Gateway startup must use this instead of initializing all schema.
func RuntimeAppIndexes() []IndexSpec {
	all := AllIndexes()
	indexes := make([]IndexSpec, 0, len(runtimeAppIndexNames))
	for _, spec := range all {
		if spec.Collection != CollectionApps {
			continue
		}
		if _, ok := runtimeAppIndexNames[spec.Name]; ok {
			indexes = append(indexes, spec)
		}
	}
	return indexes
}

// OutboxRetentionIndexes 返回按 delivery_status 分档的 TTL 索引声明。
//
// 它刻意不并入 AllIndexes：普通业务进程启动时不能无条件创建 TTL 索引，
// 否则历史终态文档可能在没有预检和低峰清理的情况下被 MongoDB 立即删除。
// 由受控的 outbox-retention 运维命令在预检通过后显式创建。
func OutboxRetentionIndexes() []IndexSpec {
	return []IndexSpec{
		{
			Collection:         CollectionOutboxEvents,
			Name:               "ix_outbox_events_delivered_updated_at_ttl",
			Keys:               bson.D{{Key: "updated_at", Value: 1}},
			ExpireAfterSeconds: int32Pointer(14 * 24 * 60 * 60),
			PartialFilter:      bson.D{{Key: "delivery_status", Value: "delivered"}},
		},
		{
			Collection:         CollectionOutboxEvents,
			Name:               "ix_outbox_events_failed_updated_at_ttl",
			Keys:               bson.D{{Key: "updated_at", Value: 1}},
			ExpireAfterSeconds: int32Pointer(30 * 24 * 60 * 60),
			PartialFilter:      bson.D{{Key: "delivery_status", Value: "failed"}},
		},
		{
			Collection:         CollectionOutboxEvents,
			Name:               "ix_outbox_events_needs_attention_updated_at_ttl",
			Keys:               bson.D{{Key: "updated_at", Value: 1}},
			ExpireAfterSeconds: int32Pointer(90 * 24 * 60 * 60),
			PartialFilter:      bson.D{{Key: "delivery_status", Value: "needs_attention"}},
		},
	}
}

func int32Pointer(value int32) *int32 {
	return &value
}
