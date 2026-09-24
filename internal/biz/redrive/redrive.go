// Package redrive 实现人工受限重驱的领域编排。
//
// 它不在 biz/outbox 里，因为本用例需要 biz/generation 的门禁载入器，而
// biz/generation 已经导入 biz/outbox —— 放进去会形成导入环。
//
// 本包刻意只持有「只读 + 重新打开事件」的能力：没有任何退款、冲正、额度或
// 发布写入口。真正的业务动作（搬运素材、账本冲正）仍由既有 worker 的 CAS
// 逻辑执行，重驱只负责把事件重新交给它们。
package redrive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
)

// RedriveAuditAction 是写进 admin_audit 的固定动作名。
const RedriveAuditAction = "outbox.redrive"

var (
	// ErrRedriveDependenciesUnavailable 表示编排缺少必需边界。
	ErrRedriveDependenciesUnavailable = errors.New("redrive: dependencies are unavailable")
	// ErrRedriveAuditConflict 表示同一条审计已经存在但内容不同 —— 同一幂等键
	// 被用于了不同的请求，必须显式失败而不是覆盖既有证据。
	ErrRedriveAuditConflict = errors.New("redrive: audit record conflicts with the request")
	// ErrUnsupportedRedriveEventType 表示该事件类型不参与人工重驱。
	ErrUnsupportedRedriveEventType = errors.New("redrive: unsupported event type")
)

// GateLoader 只读重跑两个既有门禁载入器。
//
// 它拿不到任何写方法，所以「不绕过 publication gate」是靠重新执行同一门禁
// 实现的：门禁不通过就是 Conflict，不存在「跳过门禁直接放行」的路径。
type GateLoader interface {
	LoadProviderResultMaterialization(context.Context, generation.ProviderResultMaterializeEventPayload) (generation.ProviderResultMaterializationTarget, error)
	LoadProviderTerminalSettlement(context.Context, generation.ProviderTerminalSettlementEventPayload) (generation.ProviderTerminalSettlementTarget, error)
}

// SettlementLedger 只暴露预扣当前处于什么状态，没有任何写入方法。
//
// 「不调用退款接口、不新增用户资金路径」因此是编译期不可能，而不是纪律约定：
// 这个接口在类型上就不提供资金出口。真正的冲正由既有结算 worker 的
// `ReverseInTx` 执行，它本身已对 reversed 幂等、并靠条件迁移防止双冲正。
type SettlementLedger interface {
	FindReservation(context.Context, string) (*ledger.Reservation, error)
}

// EventStore 是重驱所需的发件箱最小边界，比 outbox.Repository 窄，
// 因此本包的替身不必实现整个发件箱契约。
type EventStore interface {
	// FindForRedrive 读取事件做前置校验。实现必须把「不存在」表达为错误，
	// 而不是返回 nil 让调用方自己猜。
	FindForRedrive(context.Context, string) (*outbox.Event, error)
	// RedriveAttention 的语义与 outbox.Repository 的同名方法完全一致。
	RedriveAttention(context.Context, outbox.RedriveCommand) (*outbox.Event, error)
}

// AuditKey 是审计幂等键：操作者 + 调用方事件键 + 操作者观察到的重驱次数。
//
// 次数必须在键里：否则同一操作者的第二次重驱会与第一次撞上同一个键，
// 被误判为旧请求重放。次数由命令携带（乐观并发令牌）而不是在事务里现读，
// 否则同一请求的重放会读到已经自增之后的值、算出不同的键，重放检测失效。
type AuditKey struct {
	ActorID              string
	EventKey             string
	ExpectedRedriveCount int32
}

// Audit 是一条人工重驱的永久审计记录。
//
// 它必须自足到能重建 Result：重放时不再读事件，只回放这条记录，
// 因此窗口基线与重驱后的次数都要落在这里。它**不含 payload**：事件载荷是
// 未受信 provider 数据，审计表没有理由持有它。
type Audit struct {
	Key               AuditKey
	EventID           string
	AggregateID       string
	EventType         string
	Reason            string
	WindowAttemptBase int32
	RedriveCount      int32
	At                time.Time
}

// AuditID 由幂等键确定性地派生，与 internal/data 的写入实现共用同一算法。
// 重复提交同一请求因此命中同一条记录，而不是新增一条。
func AuditID(key AuditKey) string {
	digest := sha256.Sum256([]byte(key.ActorID + "\x00" + key.EventKey + "\x00" + strconv.FormatInt(int64(key.ExpectedRedriveCount), 10)))
	return "redrive-" + hex.EncodeToString(digest[:])
}

// AuditWriter 在调用方事务里读写审计。
type AuditWriter interface {
	// FindAudit 按幂等键读取既有审计；不存在返回 ok=false。
	FindAudit(context.Context, AuditKey) (Audit, bool, error)
	// WriteAudit 必须与事件 CAS 同事务；实现不得自行开启事务。
	WriteAudit(context.Context, Audit) error
}

// Result 描述一次重驱的最终状态。Replayed 为真表示命中了既有审计，
// 本次没有产生第二次重驱。
type Result struct {
	EventID           string
	AggregateID       string
	EventType         string
	RedriveCount      int32
	WindowAttemptBase int32
	WindowStartedAt   time.Time
	RedrivenAt        time.Time
	Replayed          bool
}

// Usecase 编排一次人工受限重驱。
type Usecase struct {
	store  EventStore
	gates  GateLoader
	ledger SettlementLedger
	audits AuditWriter
	tx     shared.TxRunner
	now    func() time.Time
}

// NewUsecase 创建重驱用例。它不做任何 I/O，也不注册任何路由。
func NewUsecase(store EventStore, gates GateLoader, ledgerPort SettlementLedger, audits AuditWriter, tx shared.TxRunner) *Usecase {
	return NewUsecaseWithClock(store, gates, ledgerPort, audits, tx, time.Now)
}

// NewUsecaseWithClock 创建使用受控时钟的重驱用例，供测试固定业务时刻。
func NewUsecaseWithClock(store EventStore, gates GateLoader, ledgerPort SettlementLedger, audits AuditWriter, tx shared.TxRunner, clock func() time.Time) *Usecase {
	if clock == nil {
		clock = time.Now
	}
	return &Usecase{store: store, gates: gates, ledger: ledgerPort, audits: audits, tx: tx, now: clock}
}

// Redrive 在同一事务里完成：审计重放探测 → 事件资格 → 门禁重跑 → 账本资格
// → 事件 CAS → 写审计。任一步失败都会让整个事务回滚，不会留下「事件被重驱了
// 但没有审计」或「审计写了但事件没动」的半完成状态。
//
// 业务时间在进入事务前冻结，与既有的回调用例一致。
func (usecase *Usecase) Redrive(ctx context.Context, command outbox.RedriveCommand) (Result, error) {
	if usecase == nil || usecase.store == nil || usecase.gates == nil || usecase.ledger == nil ||
		usecase.audits == nil || usecase.tx == nil || usecase.now == nil {
		return Result{}, ErrRedriveDependenciesUnavailable
	}
	if err := command.Validate(); err != nil {
		return Result{}, err
	}
	businessAt := usecase.now().UTC()
	if businessAt.IsZero() {
		return Result{}, outbox.ErrInvalidRedriveCommand
	}
	command.At = businessAt

	key := AuditKey{ActorID: command.ActorID, EventKey: command.Key, ExpectedRedriveCount: command.ExpectedRedriveCount}
	var result Result
	err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		// 重放优先：同一请求不得产生第二次重驱，也不得再读一次事件。
		existing, found, err := usecase.audits.FindAudit(txCtx, key)
		if err != nil {
			return err
		}
		if found {
			if existing.EventID != command.EventID || existing.Reason != command.Reason {
				return ErrRedriveAuditConflict
			}
			result = resultFromAudit(existing, true)
			return nil
		}

		event, err := usecase.store.FindForRedrive(txCtx, command.EventID)
		if err != nil {
			return err
		}
		if err := usecase.checkEligibility(txCtx, event, command); err != nil {
			return err
		}
		updated, err := usecase.store.RedriveAttention(txCtx, command)
		if err != nil {
			return err
		}
		audit := Audit{
			Key: key, EventID: updated.ID, AggregateID: updated.AggregateID, EventType: string(updated.EventType),
			Reason: command.Reason, WindowAttemptBase: updated.RedriveAttemptBase, RedriveCount: updated.RedriveCount,
			At: businessAt,
		}
		if err := usecase.audits.WriteAudit(txCtx, audit); err != nil {
			return err
		}
		result = resultFromAudit(audit, false)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// checkEligibility 只做只读校验：事件资格、门禁重跑、账本资格。
func (usecase *Usecase) checkEligibility(ctx context.Context, event *outbox.Event, command outbox.RedriveCommand) error {
	if event == nil {
		return outbox.ErrRedriveConflict
	}
	if event.DeliveryStatus != outbox.DeliveryStatusNeedsAttention {
		return outbox.ErrRedriveNotEligible
	}
	if event.RedriveCount != command.ExpectedRedriveCount {
		// 操作者看到的次数已过期。与存储层同一个判定，这里再判一次是为了在
		// 读不到事件与门禁之间就短路，避免用过期令牌去跑门禁。
		return outbox.ErrRedriveConflict
	}
	if !event.RedriveEligible() {
		return outbox.ErrRedriveNotEligible
	}

	switch event.EventType {
	case outbox.EventType(generation.ProviderResultMaterializeEventType):
		payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
		if err != nil {
			return generation.ErrInvalidProviderResultPublication
		}
		// 重跑 publication gate：它内部要求 creation 仍在 pending_submission，
		// 所以已被退款或已发布的创作不会因为人工重驱而重新打开。
		if _, err := usecase.gates.LoadProviderResultMaterialization(ctx, payload); err != nil {
			return err
		}
		return nil
	case outbox.EventType(generation.ProviderTerminalSettlementEventType):
		payload, err := generation.ParseProviderTerminalSettlementEventPayload(event.Payload)
		if err != nil {
			return generation.ErrInvalidProviderTerminalSettlement
		}
		// 重跑终态 gate：终态版本/摘要已变时这里会冲突。
		target, err := usecase.gates.LoadProviderTerminalSettlement(ctx, payload)
		if err != nil {
			return err
		}
		return usecase.checkSettlementEligibility(ctx, target)
	default:
		return ErrUnsupportedRedriveEventType
	}
}

// checkSettlementEligibility 判断该失败/取消结算是否还值得补做。
//
// 逐态依据 ReverseInTx 的既有状态机：reserved 才需要结算；reversed 会让
// ReverseInTx 直接返回既有结果（重驱是空操作）；confiscated 会让它返回状态冲突
// （该结算本就不该冲正）；预扣不存在则无从补做。
func (usecase *Usecase) checkSettlementEligibility(ctx context.Context, target generation.ProviderTerminalSettlementTarget) error {
	found, err := usecase.ledger.FindReservation(ctx, target.Payload.CreationID)
	if err != nil {
		return err
	}
	if found == nil {
		return outbox.ErrRedriveConflict
	}
	if found.Status != ledger.ReservationStatusReserved {
		return outbox.ErrRedriveNotEligible
	}
	return nil
}

func resultFromAudit(audit Audit, replayed bool) Result {
	return Result{
		EventID: audit.EventID, AggregateID: audit.AggregateID, EventType: audit.EventType,
		RedriveCount: audit.RedriveCount, WindowAttemptBase: audit.WindowAttemptBase,
		WindowStartedAt: audit.At, RedrivenAt: audit.At, Replayed: replayed,
	}
}

// Validate 保证审计记录自足。写库前调用，避免把不可回放的记录落盘。
func (audit Audit) Validate() error {
	if audit.Key.ActorID == "" || audit.Key.EventKey == "" || audit.Key.ExpectedRedriveCount < 0 ||
		audit.EventID == "" || audit.Reason == "" || audit.At.IsZero() ||
		audit.RedriveCount < 1 || audit.RedriveCount > outbox.MaxRedriveCount {
		return fmt.Errorf("%w: incomplete audit record", ErrRedriveAuditConflict)
	}
	return nil
}
