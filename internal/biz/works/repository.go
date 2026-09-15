package works

import "context"

// Repository 是作品历史读取的反转依赖。
// 实现必须先以 userID 过滤，再追加类型、游标和结果资产读取条件。
type Repository interface {
	List(context.Context, ListQuery) (*Page, error)
	FindByID(ctx context.Context, userID, id string) (*Work, error)
}
