package outbox

import (
	"errors"
	"strings"
	"time"
)

var (
	// ErrInvalidRedriveCommand 表示重驱请求本身不合法：缺少操作者、原因或幂等键，
	// 或次数令牌为负。这类请求必须被拒绝，而不是当成匿名重驱放行。
	ErrInvalidRedriveCommand = errors.New("outbox: invalid redrive command")
	// ErrRedriveNotEligible 表示事件不处于可重驱状态（不是 needs_attention），
	// 或已达 V1 的重驱次数上限。
	ErrRedriveNotEligible = errors.New("outbox: event is not eligible for redrive")
	// ErrRedriveConflict 表示重驱的前提与当前冻结状态不一致：次数令牌已过期，
	// 或创建侧的门禁已经关闭。必须显式失败，不得静默重排。
	ErrRedriveConflict = errors.New("outbox: redrive conflicts with frozen state")
)

const maxRedriveTextLength = 200

// RedriveCommand 是一次受限重驱请求。
//
// 它只描述「把哪个事件重新打开」，不携带任何 provider、账号、路由或幂等键：
// 那些事实全部冻结在事件 payload 里，重驱不读取、也不改写它们，因此
// 「不换供应商 / 不换账号 / 不重建 Job」是结构性成立的，而不是靠调用方自觉。
type RedriveCommand struct {
	EventID string
	// ExpectedRedriveCount 是操作者观察到的当前重驱次数。它同时是乐观并发令牌
	// 与审计幂等键的一部分，必须由调用方携带而不是在事务里现读：
	// 现读会让同一请求的重放读到已经自增之后的值、算出不同的幂等键，
	// 重放检测随之失效，同一操作者提交两次就真的重驱了两次。
	ExpectedRedriveCount int32
	// ActorID 与 Reason 必填：重驱是人工介入，必须能回答「谁、为什么」。
	ActorID string
	Reason  string
	// Key 是调用方的幂等键。同一 (ActorID, Key, ExpectedRedriveCount) 重复提交
	// 只产生一次重驱；第二次重驱用的是新次数，因此键天然不同。
	Key string
	At  time.Time
}

// Validate 只做本地形态校验，不接触存储，也不判断资格。
//
// 次数令牌的上界取「可观察到的最大值」：`redrive_count` 的取值范围是
// 0..MaxRedriveCount，因此观察到 == MaxRedriveCount 是合法请求，只是事件已达上限，
// 由资格判断返回 ErrRedriveNotEligible；写成更大的值不可能被真实观察到，属无效请求。
func (command RedriveCommand) Validate() error {
	if !validRedriveText(command.EventID) || command.ExpectedRedriveCount < 0 ||
		command.ExpectedRedriveCount > MaxRedriveCount ||
		!validRedriveText(command.ActorID) || !validRedriveText(command.Reason) || !validRedriveText(command.Key) ||
		command.At.IsZero() {
		return ErrInvalidRedriveCommand
	}
	return nil
}

func validRedriveText(value string) bool {
	return value != "" && len(value) <= maxRedriveTextLength && strings.TrimSpace(value) == value
}
