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
	CollectionOutboxEvents            = "outbox_events"
	CollectionFeedbacks               = "feedbacks"
	CollectionNotifications           = "notifications"
	CollectionNotificationPreferences = "notification_preferences"
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
	CollectionOutboxEvents,
	CollectionFeedbacks,
	CollectionNotifications,
	CollectionNotificationPreferences,
}

// AllCollections 按稳定顺序返回所有业务集合名称。
func AllCollections() []string {
	return append([]string(nil), collectionNames...)
}
