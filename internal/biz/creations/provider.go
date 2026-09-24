// Package creations 管理创作占位和步骤计划。
package creations

import (
	"context"
	"time"

	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"

	"github.com/google/wire"
)

// userReaderAdapter 仅为 Wire 将身份仓储适配为创作所需的用户读取边界。
type userReaderAdapter struct {
	repository identity.UserRepository
}

// NewUserReaderAdapter 创建用户读取边界适配器。
func NewUserReaderAdapter(repository identity.UserRepository) *userReaderAdapter {
	return &userReaderAdapter{repository: repository}
}

// Find 原样转发用户读取请求。
func (adapter *userReaderAdapter) Find(ctx context.Context, userID string) (*identity.User, error) {
	return adapter.repository.Find(ctx, userID)
}

// entitlementEvaluatorAdapter 仅为 Wire 将权益用例适配为创作所需的权益计算边界。
type entitlementEvaluatorAdapter struct {
	usecase *entitlement.Usecase
}

// NewEntitlementEvaluatorAdapter 创建权益计算边界适配器。
func NewEntitlementEvaluatorAdapter(usecase *entitlement.Usecase) *entitlementEvaluatorAdapter {
	return &entitlementEvaluatorAdapter{usecase: usecase}
}

// EvaluateGenerationAt 原样转发固定时刻的生成权益计算。
func (adapter *entitlementEvaluatorAdapter) EvaluateGenerationAt(user entitlement.UserSnapshot, request entitlement.GenerationRequest, now time.Time) (*entitlement.GenerationDecision, error) {
	return adapter.usecase.EvaluateGenerationAt(user, request, now)
}

// ResolveDailyBenefitsAt 原样转发固定时刻的每日权益计算。
func (adapter *entitlementEvaluatorAdapter) ResolveDailyBenefitsAt(user entitlement.UserSnapshot, now time.Time) (*entitlement.DailyBenefits, error) {
	return adapter.usecase.ResolveDailyBenefitsAt(user, now)
}

// reservationUsecaseAdapter 仅为 Wire 将账本用例适配为创作所需的预留边界。
type reservationUsecaseAdapter struct {
	usecase *ledger.Usecase
}

// NewReservationUsecaseAdapter 创建预留边界适配器。
func NewReservationUsecaseAdapter(usecase *ledger.Usecase) *reservationUsecaseAdapter {
	return &reservationUsecaseAdapter{usecase: usecase}
}

// ReserveInTx 原样转发调用方已开启事务中的预留请求。
func (adapter *reservationUsecaseAdapter) ReserveInTx(ctx context.Context, request ledger.ReserveRequest) (*ledger.Reservation, error) {
	return adapter.usecase.ReserveInTx(ctx, request)
}

// FindReservation 原样转发既有创作的预留读取。
func (adapter *reservationUsecaseAdapter) FindReservation(ctx context.Context, creationID string) (*ledger.Reservation, error) {
	return adapter.usecase.FindReservation(ctx, creationID)
}

// CurrentDiamondBalance 仅在预留后读取事务内余额快照，供创建响应投影使用。
func (adapter *reservationUsecaseAdapter) CurrentDiamondBalance(ctx context.Context, userID string) (int64, error) {
	return adapter.usecase.CurrentDiamondBalance(ctx, userID)
}

// ProviderSet 是创作模块的依赖注入入口。
//
// 它不提供 AdmissionResolver：归属解析器必须由组合根按运行配置构造
// （见 integrations/generation.NewAdmissionResolver），这样「selector 选 B2B
// 却没有可用解析器」会在启动时报错，而不是在运行期把新步骤悄悄冻成本地。
var ProviderSet = wire.NewSet(
	NewUsecase,
	NewUserReaderAdapter,
	NewEntitlementEvaluatorAdapter,
	NewReservationUsecaseAdapter,
	wire.Bind(new(userReader), new(*userReaderAdapter)),
	wire.Bind(new(entitlementEvaluator), new(*entitlementEvaluatorAdapter)),
	wire.Bind(new(reservationUsecase), new(*reservationUsecaseAdapter)),
)
