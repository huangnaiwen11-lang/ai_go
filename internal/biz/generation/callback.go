package generation

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/executionv2"
)

var (
	// ErrInvalidCallbackEvent 表示受控回调事件或终态命令不完整。
	ErrInvalidCallbackEvent = errors.New("generation: invalid callback event")
	// ErrCallbackDependenciesUnavailable 表示回调编排尚未获得必要的领域依赖。
	ErrCallbackDependenciesUnavailable = errors.New("generation: callback dependencies unavailable")
	// ErrCallbackAlreadyConfirmed 表示相同回执已经确认同一既有终态。
	ErrCallbackAlreadyConfirmed = errors.New("generation: callback already confirmed")
)

// CallbackTerminal 表示生成中台可确认的失败类终态。
type CallbackTerminal string

const (
	// CallbackTerminalFailed 表示中台已确认技术失败。
	CallbackTerminalFailed CallbackTerminal = "failed"
	// CallbackTerminalCancelled 表示中台已确认技术取消。
	CallbackTerminalCancelled CallbackTerminal = "cancelled"
)

// VerifiedCallbackEvent 是验签器输出的外部回调受控事实。
// 它不包含 CreationID，调用方不得从回调请求、URL 或 metadata 推导创作关联。
type VerifiedCallbackEvent struct {
	StepID          string
	ExternalRef     string
	JobID           string
	Capability      executionv2.Capability
	MediaType       string
	ResultURL       string
	Terminal        CallbackTerminal
	NonceHash       string
	PayloadDigest   string
	CallbackVersion string
}

// LinkedCallback 是可信步骤关联器补全创作归属后的内部终态事实。
// 它只能通过 NewLinkedCallback 创建，关联器必须先用持久化步骤事实核验 StepID 与 CreationID。
type LinkedCallback struct {
	creationID string
	event      VerifiedCallbackEvent
}

// NewLinkedCallback 仅供可信步骤关联器在已核验步骤归属后构造内部终态事实。
// Handler 不得绕过关联器调用它；任务 4 的 Mongo 实现必须在同一事务内完成 StepID 到 CreationID 的核验。
func NewLinkedCallback(creationID string, event VerifiedCallbackEvent) (LinkedCallback, error) {
	if !validCallbackIdentifier(creationID) || !validVerifiedCallbackBase(event) {
		return LinkedCallback{}, ErrInvalidCallbackEvent
	}
	return LinkedCallback{creationID: creationID, event: event}, nil
}

// CallbackLinker 定义从已验证回调到已关联内部终态事实的窄边界。
// 实现不得信任外部字段中的创作归属，只能使用已保存的步骤关联。
type CallbackLinker interface {
	Link(context.Context, VerifiedCallbackEvent) (LinkedCallback, error)
}

// CompletedCallback 是写入完成回执和终态 CAS 所需的已关联固定事实。
type CompletedCallback struct {
	CreationID      string
	StepID          string
	JobID           string
	ExternalRef     string
	Capability      executionv2.Capability
	MediaType       string
	ResultURL       string
	NonceHash       string
	PayloadDigest   string
	CallbackVersion string
	At              time.Time
}

// FailedCallback 是写入失败或取消回执和终态 CAS 所需的已关联固定事实。
// 它不保存中台返回的任何错误文本。
type FailedCallback struct {
	CreationID      string
	StepID          string
	JobID           string
	ExternalRef     string
	Capability      executionv2.Capability
	Terminal        CallbackTerminal
	NonceHash       string
	PayloadDigest   string
	CallbackVersion string
	At              time.Time
}

// CallbackStore 定义已关联回调终态在调用方事务中的最小写入边界。
// 同 nonce 的任一受控终态事实不同必须返回 ErrInvalidCallbackEvent；不同 nonce 的同一既有终态
// 只返回 ErrCallbackAlreadyConfirmed，且不得重复写入素材、发件箱或账本。At 只保存首次确认时间，
// 不参与回执重放或终态 CAS 的等价判断。
type CallbackStore interface {
	Complete(context.Context, CompletedCallback) error
	Fail(context.Context, FailedCallback) error
}

type callbackReverser interface {
	ReverseInTx(context.Context, string, ledger.ReversalReason, time.Time) (*ledger.Reservation, error)
}

// CallbackUsecase 编排可信关联后的回调终态和技术失败冲正。
type CallbackUsecase struct {
	store    CallbackStore
	linker   CallbackLinker
	ledger   callbackReverser
	tx       shared.TxRunner
	clockUTC func() time.Time
}

// NewCallbackUsecase 创建回调终态编排用例。
func NewCallbackUsecase(store CallbackStore, linker CallbackLinker, ledgerUsecase *ledger.Usecase, tx shared.TxRunner) *CallbackUsecase {
	return NewCallbackUsecaseWithClock(store, linker, ledgerUsecase, tx, time.Now)
}

// NewCallbackUsecaseWithClock 创建使用受控时钟的回调用例，供本地测试固定业务时刻。
func NewCallbackUsecaseWithClock(store CallbackStore, linker CallbackLinker, reverser callbackReverser, tx shared.TxRunner, clock func() time.Time) *CallbackUsecase {
	if clock == nil {
		clock = time.Now
	}
	return &CallbackUsecase{store: store, linker: linker, ledger: reverser, tx: tx, clockUTC: clock}
}

// Handle 接收验签后的外部回调事实。
// 业务时间在进入可重试事务前冻结；关联、终态 CAS 与必要冲正共用同一事务上下文。
func (usecase *CallbackUsecase) Handle(ctx context.Context, event VerifiedCallbackEvent) error {
	if usecase == nil || usecase.store == nil || usecase.linker == nil || usecase.tx == nil || usecase.clockUTC == nil {
		return ErrCallbackDependenciesUnavailable
	}
	businessAt := usecase.clockUTC().UTC()
	if businessAt.IsZero() {
		return ErrCallbackDependenciesUnavailable
	}
	if event.Terminal != "" && usecase.ledger == nil {
		return ErrCallbackDependenciesUnavailable
	}

	return usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		linked, err := usecase.linker.Link(txCtx, event)
		if err != nil {
			return err
		}
		if event.Terminal == "" {
			command, err := newCompletedCallback(linked, businessAt)
			if err != nil {
				return err
			}
			if err := usecase.store.Complete(txCtx, command); err != nil {
				if errors.Is(err, ErrCallbackAlreadyConfirmed) {
					return nil
				}
				return err
			}
			return nil
		}

		command, err := newFailedCallback(linked, businessAt)
		if err != nil {
			return err
		}
		if err := usecase.store.Fail(txCtx, command); err != nil {
			if errors.Is(err, ErrCallbackAlreadyConfirmed) {
				return nil
			}
			return err
		}
		_, err = usecase.ledger.ReverseInTx(txCtx, command.CreationID, ledger.ReversalReasonGenerationFailed, businessAt)
		return err
	})
}

func newCompletedCallback(linked LinkedCallback, businessAt time.Time) (CompletedCallback, error) {
	event := linked.event
	if !validLinkedCallbackBase(linked, businessAt) || event.Terminal != "" || !compatibleCallbackMedia(event.Capability, event.MediaType) || !safeCallbackResultURL(event.ResultURL) {
		return CompletedCallback{}, ErrInvalidCallbackEvent
	}
	return CompletedCallback{
		CreationID: linked.creationID, StepID: event.StepID, JobID: event.JobID, ExternalRef: event.ExternalRef,
		Capability: event.Capability, MediaType: event.MediaType, ResultURL: event.ResultURL, NonceHash: event.NonceHash,
		PayloadDigest: event.PayloadDigest, CallbackVersion: event.CallbackVersion, At: businessAt,
	}, nil
}

func newFailedCallback(linked LinkedCallback, businessAt time.Time) (FailedCallback, error) {
	event := linked.event
	if !validLinkedCallbackBase(linked, businessAt) || (event.Terminal != CallbackTerminalFailed && event.Terminal != CallbackTerminalCancelled) || event.MediaType != "" || event.ResultURL != "" {
		return FailedCallback{}, ErrInvalidCallbackEvent
	}
	return FailedCallback{
		CreationID: linked.creationID, StepID: event.StepID, JobID: event.JobID, ExternalRef: event.ExternalRef,
		Capability: event.Capability, Terminal: event.Terminal, NonceHash: event.NonceHash,
		PayloadDigest: event.PayloadDigest, CallbackVersion: event.CallbackVersion, At: businessAt,
	}, nil
}

func validLinkedCallbackBase(linked LinkedCallback, businessAt time.Time) bool {
	return validCallbackIdentifier(linked.creationID) && validVerifiedCallbackBase(linked.event) && !businessAt.IsZero()
}

func validVerifiedCallbackBase(event VerifiedCallbackEvent) bool {
	return validCallbackIdentifier(event.StepID) && validCallbackIdentifier(event.JobID) &&
		event.ExternalRef == event.StepID && validCallbackIdentifier(event.NonceHash) &&
		validCallbackIdentifier(event.PayloadDigest) && event.CallbackVersion == "2" && isCallbackCapability(event.Capability)
}

func validCallbackIdentifier(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && len(value) <= maxSubmissionIdentifierLength
}

func isCallbackCapability(capability executionv2.Capability) bool {
	return capability == executionv2.CapabilityTextToImage || capability == executionv2.CapabilityImageEdit || capability == executionv2.CapabilityImageToVideo
}

func compatibleCallbackMedia(capability executionv2.Capability, mediaType string) bool {
	if capability == executionv2.CapabilityImageToVideo {
		return mediaType == "video"
	}
	return mediaType == "image"
}

func safeCallbackResultURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && raw == strings.TrimSpace(raw) && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}
