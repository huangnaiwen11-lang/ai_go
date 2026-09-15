package data

import (
	"context"

	"ai-business-service/internal/data/migrate"
)

// LocalSchemaInitializer 表示本地 MongoDB schema 的启动前初始化能力。
type LocalSchemaInitializer interface {
	Ensure(context.Context) error
}

// NewLocalSchemaInitializer 在 data 层构造 schema 初始化器，避免向 biz 或 service 泄露数据库与驱动细节。
func NewLocalSchemaInitializer(data *Data) LocalSchemaInitializer {
	return migrate.NewInitializer(data.database)
}
