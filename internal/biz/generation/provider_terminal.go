package generation

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// ProviderTerminalStatus is the only set of terminal outcomes that a
// PolarStar provider may publish for a generation step.  A terminal slot is
// deliberately separate from the delivery/inbox status: a delivery can be
// retried while the step terminal is written at most once.
type ProviderTerminalStatus string

const (
	ProviderTerminalCompleted ProviderTerminalStatus = "completed"
	ProviderTerminalFailed    ProviderTerminalStatus = "failed"
	ProviderTerminalCancelled ProviderTerminalStatus = "cancelled"
)

var (
	ErrInvalidProviderTerminal  = errors.New("generation: invalid provider terminal fact")
	ErrProviderTerminalConflict = errors.New("generation: provider terminal conflict")
	ErrProviderTerminalFence    = errors.New("generation: stale provider terminal fence")
	ErrProviderTerminalVersion  = errors.New("generation: provider terminal version conflict")
)

// ProviderTerminalFact is the normalized fact produced by callback, lookup,
// or inbox consumers.  All three paths must call ApplyProviderTerminal rather
// than updating a creation step directly.
//
// ExpectedVersion is the value read immediately before the CAS.  The first
// writer uses 0; after it wins, the slot's TerminalVersion is incremented to
// 1. AttemptFence identifies the provider submission attempt and prevents a
// reclaimed lease from completing a newer attempt. LeaseToken is optional for
// externally delivered callbacks, but when a slot carries one it must match.
type ProviderTerminalFact struct {
	Provider            string
	AccountRef          string
	StepID              string
	ExternalExecutionID string
	Capability          string
	Status              ProviderTerminalStatus
	// ResultRef is the provider's transient result locator. It is retained only
	// until the materializer has copied the object into R2; it is never a user
	// facing asset URL.
	ResultRef       string
	TerminalDigest  string
	PayloadDigest   string
	AttemptFence    int64
	LeaseToken      string
	ExpectedVersion int64
	ObservedAt      time.Time
}

// ProviderTerminalFactFromObservation 由冻结提交意图、归一化结论与外部任务号
// 组装统一终态事实。
//
// 三条终态路径（回调消费、对账查询、提交时的对账分支）必须共用这一处组装：
// 各自拼一遍字段，任何一处漏填或错填都会写出一份「看起来合法但归因错误」的
// 终态，而终态一旦落库就无法靠重放纠正。
//
// payloadDigest 标识这份结论出自哪一串供应商字节：回调路径传原始报文摘要，
// 查询路径传查询响应摘要。它只作取证，不参与判重——判重只用结论 + 语义摘要，
// 否则同一结论经两条路径到达会被判成冲突。
func ProviderTerminalFactFromObservation(
	intent *ProviderSubmissionIntent,
	observation ProviderTerminalObservation,
	externalExecutionID, payloadDigest string,
	at time.Time,
) (ProviderTerminalFact, error) {
	if intent == nil || externalExecutionID == "" || at.IsZero() {
		return ProviderTerminalFact{}, ErrInvalidProviderTerminal
	}
	// 冻结请求的内部一致性由领域再验一次，不依赖适配器替我们验过。
	if err := intent.Request.Validate(); err != nil {
		return ProviderTerminalFact{}, ErrInvalidProviderTerminal
	}
	if err := observation.Validate(); err != nil {
		return ProviderTerminalFact{}, err
	}
	// 能力不一致说明这份结论不属于这一步；fence 缺失说明无法把它归因到具体
	// 一次提交尝试。两者都只能拒绝，不能挑一个信。
	if intent.Request.Capability != observation.Capability || intent.Fence <= 0 {
		return ProviderTerminalFact{}, ErrProviderInboxStepMismatch
	}
	fact := ProviderTerminalFact{
		Provider:            intent.Request.Route.Provider,
		AccountRef:          intent.Request.Route.AccountRef,
		StepID:              intent.Request.StepID,
		ExternalExecutionID: externalExecutionID,
		Capability:          intent.Request.Capability,
		Status:              observation.Status,
		ResultRef:           observation.ResultRef,
		TerminalDigest:      ProviderTerminalSummaryDigest(observation.Status, observation.ResultRef),
		PayloadDigest:       payloadDigest,
		AttemptFence:        int64(intent.Fence),
		// 外部投递与查询都不携带租约令牌；槽位若自带令牌，则由仓储按空值
		// 跳过比较，这与回调路径保持一致。
		LeaseToken:      "",
		ExpectedVersion: 0,
		ObservedAt:      at.UTC(),
	}
	if err := fact.Validate(); err != nil {
		return ProviderTerminalFact{}, ErrInvalidProviderTerminal
	}
	return fact, nil
}

// ProviderTerminalSlot is the persisted CAS projection for one step.  It is
// intentionally a value type so repository adapters can load it, call the
// pure transition, and save the returned slot with a Mongo filter containing
// step_id + terminal_version + attempt_fence (and lease_token when present).
type ProviderTerminalSlot struct {
	Provider            string
	AccountRef          string
	StepID              string
	ExternalExecutionID string
	Capability          string
	AttemptFence        int64
	LeaseToken          string
	TerminalVersion     int64
	Status              ProviderTerminalStatus
	// ResultRef is retained only for semantic replay comparison. It remains an
	// internal provider locator and is never exposed as a user asset URL.
	ResultRef        string
	TerminalDigest   string
	PayloadDigest    string
	ConfirmedAt      time.Time
	Quarantined      bool
	QuarantineReason string
}

// ProviderTerminalStore 是 callback、lookup、inbox consumer 共用的持久化边界。
// 实现必须在外层事务中按 terminal_version/attempt_fence 做条件更新。
type ProviderTerminalStore interface {
	ApplyProviderTerminal(context.Context, ProviderTerminalFact) (ProviderTerminalApplyResult, error)
}

// ProviderTerminalApplyResult describes the result of the single terminal
// transition. Noop means the same fact was replayed and has no side effect.
type ProviderTerminalApplyResult string

const (
	ProviderTerminalApplied     ProviderTerminalApplyResult = "applied"
	ProviderTerminalNoop        ProviderTerminalApplyResult = "noop"
	ProviderTerminalQuarantined ProviderTerminalApplyResult = "quarantined"
)

const providerTerminalMaxIdentity = 200

// ApplyProviderTerminal is the shared terminal transition used by callbacks,
// lookup reconciliation, and inbox consumers. It never aliases or mutates
// either input. A conflicting fact returns a quarantined copy so a repository
// can persist the conflict for operator review; callers must not apply that
// copy as a normal terminal state.
//
// 三类结果必须区分清楚，混在一起会把「竞态」永久写成「数据损坏」：
//
//   - 已终态槽位：只按 结论 + 语义摘要 判重放。raw transport digest、陈旧
//     ExpectedVersion、callback 缺失的 lease 都不参与比较，否则同一结论经
//     lookup 与 callback 两条路到达时会被判成冲突。
//   - 待定槽位 + 陈旧租约/版本：瞬时竞态，返回空 result 且槽位原样返回，
//     调用方重试即可；不能落 quarantine，否则后续有效终态会被自己挡住。
//   - 身份不一致或同身份异结论：完整性冲突，落 quarantine 等待人工处理。
func ApplyProviderTerminal(existing *ProviderTerminalSlot, incoming ProviderTerminalFact) (ProviderTerminalSlot, ProviderTerminalApplyResult, error) {
	if err := incoming.Validate(); err != nil {
		return ProviderTerminalSlot{}, "", err
	}
	if existing == nil {
		if incoming.ExpectedVersion != 0 {
			return ProviderTerminalSlot{}, ProviderTerminalQuarantined, ErrProviderTerminalVersion
		}
		return ProviderTerminalSlot{
			Provider: incoming.Provider, AccountRef: incoming.AccountRef, StepID: incoming.StepID,
			ExternalExecutionID: incoming.ExternalExecutionID, Capability: incoming.Capability,
			AttemptFence: incoming.AttemptFence, LeaseToken: incoming.LeaseToken,
			TerminalVersion: 1, Status: incoming.Status, ResultRef: incoming.ResultRef, TerminalDigest: incoming.TerminalDigest,
			PayloadDigest: incoming.PayloadDigest, ConfirmedAt: incoming.ObservedAt.UTC(),
		}, ProviderTerminalApplied, nil
	}

	current := *existing
	if err := current.Validate(); err != nil {
		return ProviderTerminalSlot{}, "", err
	}
	if current.Quarantined {
		// 隔离只能由显式处置解除，晚到事实不能自动解锁或被标记正常消费。
		return current, ProviderTerminalQuarantined, ErrProviderTerminalConflict
	}
	// 一个槽位只服务一个冻结路由与一个外部任务；身份不一致说明两笔业务
	// 抢同一个槽位，必须留证据而不是继续写。
	if !sameProviderTerminalIdentity(current, incoming) {
		return quarantineProviderTerminal(current, ErrProviderTerminalConflict), ProviderTerminalQuarantined, ErrProviderTerminalConflict
	}
	if current.Status != "" {
		if current.Status == incoming.Status && providerResultRefsEquivalent(current.ResultRef, incoming.ResultRef) {
			return current, ProviderTerminalNoop, nil
		}
		return quarantineProviderTerminal(current, ErrProviderTerminalConflict), ProviderTerminalQuarantined, ErrProviderTerminalConflict
	}
	if current.AttemptFence != incoming.AttemptFence || (current.LeaseToken != "" && current.LeaseToken != incoming.LeaseToken) {
		return current, "", ErrProviderTerminalFence
	}
	if incoming.ExpectedVersion != current.TerminalVersion {
		return current, "", ErrProviderTerminalVersion
	}
	// A pending slot may be created by job binding. The caller must use a
	// repository CAS on TerminalVersion=0 and AttemptFence to make the race
	// between completed and failed single-winner.
	current.TerminalVersion++
	current.Status = incoming.Status
	current.ResultRef = incoming.ResultRef
	current.TerminalDigest = incoming.TerminalDigest
	current.PayloadDigest = incoming.PayloadDigest
	current.ConfirmedAt = incoming.ObservedAt.UTC()
	return current, ProviderTerminalApplied, nil
}

// providerResultRefsEquivalent compares the stable object locator while
// deliberately ignoring only the provider's rotating signed query string.
// Scheme, authority, path and fragment remain part of the identity: a change
// to any of them is a different object and must quarantine rather than replay.
func providerResultRefsEquivalent(left, right string) bool {
	if left == right {
		return true
	}
	if left == "" || right == "" {
		return false
	}
	l, lerr := url.Parse(left)
	r, rerr := url.Parse(right)
	if lerr != nil || rerr != nil || l.Scheme == "" || r.Scheme == "" ||
		!strings.EqualFold(l.Scheme, r.Scheme) || l.User != nil || r.User != nil ||
		l.Host == "" || r.Host == "" || !strings.EqualFold(l.Host, r.Host) ||
		l.EscapedPath() != r.EscapedPath() || l.Fragment != r.Fragment ||
		l.Opaque != "" || r.Opaque != "" {
		return false
	}
	return stableResultQuery(l) == stableResultQuery(r)
}

// stableResultQuery keeps ordinary query parameters in the object identity and
// removes only the bounded set used by signed-download schemes. A provider may
// rotate a signature/expiry token, but it cannot silently point the same job at
// a different variant by changing an arbitrary query parameter.
func stableResultQuery(value *url.URL) string {
	query := value.Query()
	for key := range query {
		if isRotatingResultQueryKey(key) {
			delete(query, key)
		}
	}
	return query.Encode()
}

func isRotatingResultQueryKey(key string) bool {
	switch strings.ToLower(key) {
	case "signature", "sig", "expires", "expires_at", "expiration",
		"x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-expires",
		"x-amz-signedheaders", "x-amz-signature", "x-amz-security-token":
		return true
	default:
		return false
	}
}

func quarantineProviderTerminal(current ProviderTerminalSlot, reason error) ProviderTerminalSlot {
	current.Quarantined = true
	current.QuarantineReason = reason.Error()
	return current
}

// Validate checks normalized, bounded terminal facts before they reach a
// repository. Digests are lowercase SHA-256 values in the mapper and are
// intentionally not recomputed here so lookup and callback can share this
// contract without retaining raw provider payloads.
func (fact ProviderTerminalFact) Validate() error {
	if !providerTerminalIdentity(fact.Provider) || !providerTerminalIdentity(fact.AccountRef) ||
		!providerTerminalIdentity(fact.StepID) || !providerTerminalIdentity(fact.ExternalExecutionID) ||
		!providerTerminalIdentity(fact.Capability) || !providerInboxDigest(fact.TerminalDigest) ||
		!providerInboxDigest(fact.PayloadDigest) || fact.AttemptFence <= 0 || fact.ExpectedVersion < 0 ||
		fact.ObservedAt.IsZero() || !validProviderTerminalStatus(fact.Status) {
		return ErrInvalidProviderTerminal
	}
	if fact.Status == ProviderTerminalCompleted {
		if !validProviderResultRef(fact.ResultRef) || fact.TerminalDigest != ProviderTerminalSummaryDigest(fact.Status, fact.ResultRef) {
			return ErrInvalidProviderTerminal
		}
	} else if fact.ResultRef != "" || fact.TerminalDigest != ProviderTerminalSummaryDigest(fact.Status, "") {
		return ErrInvalidProviderTerminal
	}
	if fact.LeaseToken != "" && !providerTerminalIdentity(fact.LeaseToken) {
		return ErrInvalidProviderTerminal
	}
	return nil
}

func (slot ProviderTerminalSlot) Validate() error {
	if !providerTerminalIdentity(slot.Provider) || !providerTerminalIdentity(slot.AccountRef) ||
		!providerTerminalIdentity(slot.StepID) || !providerTerminalIdentity(slot.ExternalExecutionID) ||
		!providerTerminalIdentity(slot.Capability) || slot.AttemptFence <= 0 || slot.TerminalVersion < 0 ||
		slot.ConfirmedAt.IsZero() && slot.Status != "" {
		return ErrInvalidProviderTerminal
	}
	if slot.LeaseToken != "" && !providerTerminalIdentity(slot.LeaseToken) {
		return ErrInvalidProviderTerminal
	}
	if slot.Status == "" {
		// 待定槽位允许带 quarantine：身份冲突可以在终态写入之前发生，
		// 此时槽位没有结论但已经被标记为需要人工处理。
		if slot.TerminalVersion != 0 || slot.TerminalDigest != "" || slot.PayloadDigest != "" {
			return ErrInvalidProviderTerminal
		}
		return nil
	}
	if !validProviderTerminalStatus(slot.Status) || slot.TerminalVersion <= 0 ||
		!providerInboxDigest(slot.TerminalDigest) || !providerInboxDigest(slot.PayloadDigest) {
		return ErrInvalidProviderTerminal
	}
	return nil
}

func sameProviderTerminalIdentity(slot ProviderTerminalSlot, fact ProviderTerminalFact) bool {
	return slot.Provider == fact.Provider && slot.AccountRef == fact.AccountRef && slot.StepID == fact.StepID &&
		slot.ExternalExecutionID == fact.ExternalExecutionID && slot.Capability == fact.Capability
}

func validProviderTerminalStatus(status ProviderTerminalStatus) bool {
	return status == ProviderTerminalCompleted || status == ProviderTerminalFailed || status == ProviderTerminalCancelled
}

func providerTerminalIdentity(value string) bool {
	if value == "" || len(value) > providerTerminalMaxIdentity || strings.TrimSpace(value) != value {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		if index == 0 || !strings.ContainsRune("._:-", char) {
			return false
		}
	}
	return true
}

// validProviderOutboxLeaseToken matches the token alphabet generated by the
// Mongo Outbox repository (`base64.RawURLEncoding`). A lease is not a step,
// provider or account identity: unlike those identifiers it may legitimately
// begin with '-' or '_'. Reusing providerTerminalIdentity here made a small,
// random subset of claimed events impossible to settle.
func validProviderOutboxLeaseToken(value string) bool {
	if value == "" || len(value) > 512 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validProviderResultRef(value string) bool {
	return value != "" && len(value) <= 8192 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n")
}
