package creations

import "context"

// Repository 定义创作占位及其内部步骤的持久化反转依赖。
// 事务由调用方通过 context 传入，仓储不得自行开启事务。
type Repository interface {
	FindByIdempotencyKey(context.Context, string) (*Creation, error)
	ListSteps(context.Context, string) ([]CreationStep, error)
	Create(context.Context, *Creation, []CreationStep) error
}

// DeferredRecipeWriter 定义延迟图生视频配方的最小写入边界。
// 调用方通过 context 传入既有事务，写入方不得自行开启事务。
type DeferredRecipeWriter interface {
	Create(context.Context, *DeferredRecipe) error
}
