package data

import "ai-business-service/internal/data/migrate"

// NewOutboxRetentionMigrator 返回只用于受控运维命令的 outbox 保留期迁移器。
// 普通业务组合根不调用它，因此启动时不会隐式创建或删除 TTL 数据。
func NewOutboxRetentionMigrator(data *Data) *migrate.OutboxRetentionMigrator {
	if data == nil {
		return migrate.NewOutboxRetentionMigrator(nil)
	}
	return migrate.NewOutboxRetentionMigrator(data.database)
}
