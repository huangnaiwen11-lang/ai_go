// Package schema 定义数据层使用的 MongoDB 集合和索引规格。
package schema

const (
	// CollectionAccounts 存放以 user_id 作为 _id 的钻石余额聚合。
	CollectionAccounts                = "accounts"
	CollectionUsers                   = "users"
	CollectionCredentials             = "credentials"
	CollectionIdentities              = "identities"
	CollectionSessions                = "sessions"
	CollectionTemplates               = "templates"
	CollectionAssets                  = "assets"
	CollectionCreations               = "creations"
	CollectionCreationSteps           = "creation_steps"
	CollectionGenerationStepRecipes   = "generation_step_recipes"
	CollectionReservations            = "reservations"
	CollectionDailyQuotas             = "daily_quotas"
	CollectionLedgerEntries           = "ledger_entries"
	CollectionSubscriptions           = "subscriptions"
	CollectionPaymentProducts         = "payment_products"
	CollectionPaymentOrders           = "payment_orders"
	CollectionPaymentReceipts         = "payment_receipts"
	CollectionPaymentCallbackNonces   = "payment_callback_nonces"
	CollectionCallbackReceipts        = "callback_receipts"
	CollectionGenerationProviderInbox = "generation_provider_inbox"
	// CollectionGenerationProductRecipes stores immutable old-template-version to
	// public B2B product recipes. It is distinct from the public model catalog.
	CollectionGenerationProductRecipes = "generation_product_recipes"
	// CollectionGenerationModelMappings 存放已发布、不可变的 B2B 模型目录版本。
	// `_id` 就是目录版本号；步骤在创建时冻结版本号，因此已发布版本不得原地修改。
	CollectionGenerationModelMappings = "generation_model_mappings"
	CollectionOutboxEvents            = "outbox_events"
	CollectionFeedbacks               = "feedbacks"
	CollectionNotifications           = "notifications"
	CollectionNotificationPreferences = "notification_preferences"
	CollectionAdminAudit              = "admin_audit"
	CollectionBlogPosts               = "blog_posts"
	// CollectionUTMLinks 存放后台维护的 UTM 导量链接。
	// 此前只在 data 层以硬编码常量引用，未登记进本清单；写入路径落地时补登记。
	CollectionUTMLinks = "utmlinks"
	// CollectionAdminMenuVisibility 存放管理后台的全局配置覆盖项。
	// 单文档（_id 固定为 menu-visibility），只存「与前端默认清单的差异」——
	// 节点清单的真相源在 ai-admin 的 routes/routeManifest.tsx。
	CollectionAdminMenuVisibility = "admin_menu_visibility"
	// CollectionApps 存放管理后台的 App 注册表。
	// 此前只在 data 层以硬编码常量引用（只读投影），未登记进本清单；
	// 写入路径落地时补登记 —— 因为唯一性靠 `uniq_android_native_identifiers`
	// 这个唯一部分索引兜底，而索引只在 Ensure 里对已登记的集合创建。
	CollectionApps = "apps"
	// CollectionPlatformConfigs stores the per-client Admin Apps configuration.
	// It is deliberately separate from the Apps registry because one App can
	// have a platform runtime configuration without being a package build.
	CollectionPlatformConfigs = "platform_configs"
	// CollectionAdminPricingConfigs stores Go-owned pricing/SystemConfig
	// overrides. Static defaults remain in the business contract.
	CollectionAdminPricingConfigs = "admin_pricing_configs"
	// CollectionAdminReviewItems stores the Go-owned moderation projection.
	CollectionAdminReviewItems = "admin_review_items"
	// CollectionAdminReviewProjectionState is an explicit readiness fence. An
	// empty collection is not treated as a valid empty moderation queue.
	CollectionAdminReviewProjectionState = "admin_review_projection_state"
)

var collectionNames = []string{
	CollectionAccounts,
	CollectionUsers,
	CollectionCredentials,
	CollectionIdentities,
	CollectionSessions,
	CollectionTemplates,
	CollectionAssets,
	CollectionCreations,
	CollectionCreationSteps,
	CollectionGenerationStepRecipes,
	CollectionReservations,
	CollectionDailyQuotas,
	CollectionLedgerEntries,
	CollectionSubscriptions,
	CollectionPaymentProducts,
	CollectionPaymentOrders,
	CollectionPaymentReceipts,
	CollectionPaymentCallbackNonces,
	CollectionCallbackReceipts,
	CollectionGenerationProviderInbox,
	CollectionGenerationProductRecipes,
	CollectionGenerationModelMappings,
	CollectionOutboxEvents,
	CollectionFeedbacks,
	CollectionNotifications,
	CollectionNotificationPreferences,
	CollectionAdminAudit,
	CollectionBlogPosts,
	CollectionUTMLinks,
	CollectionAdminMenuVisibility,
	CollectionApps,
	CollectionPlatformConfigs,
	CollectionAdminPricingConfigs,
	CollectionAdminReviewItems,
	CollectionAdminReviewProjectionState,
}

// AllCollections 按稳定顺序返回所有业务集合名称。
func AllCollections() []string {
	return append([]string(nil), collectionNames...)
}
