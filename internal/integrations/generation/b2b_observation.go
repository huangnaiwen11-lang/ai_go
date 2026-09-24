package generation

import (
	"errors"
	"strings"

	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

// ErrB2BObservationParserConfig 表示归一化解析器拿不到可用的租户身份。
var ErrB2BObservationParserConfig = errors.New("generation b2b observation parser configuration unavailable")

// b2bObservationMaxIdentity 与回调验签器的身份长度上限保持一致。
const b2bObservationMaxIdentity = 200

// B2BObservationParser 把供应商的两种终态来源归一化成同一个观察值：
//   - webhook 投递的**已持久化原始字节**（b2b.callback.v2）；
//   - 状态查询的响应（b2b.job.v2）。
//
// 两种来源必须收敛到同一套归一化规则，否则同一结论经两条路径到达会被下游
// 当成两件事。它是协议与领域之间唯一的解析点：领域只接受归一化结论，只有
// 协议层知道这两份合同的字段布局。
//
// 租户身份必须来自本地配置而不是报文——入站字节无权声明自己属于哪个租户。
type B2BObservationParser struct {
	tenantID   string
	accountRef string
}

// NewB2BObservationParser 创建归一化解析器。
func NewB2BObservationParser(tenantID, accountRef string) (*B2BObservationParser, error) {
	if !validB2BObservationIdentity(tenantID) || !validB2BObservationIdentity(accountRef) {
		return nil, ErrB2BObservationParserConfig
	}
	return &B2BObservationParser{tenantID: tenantID, accountRef: accountRef}, nil
}

// Parse 从**已验签并持久化**的投递事实重新解析终态语义。
//
// 顺序是合同的一部分：先按摘要确认字节就是当初验签过的那一份，再核对报文与
// 持久化事实指向同一步骤，最后才归一化。任何一步失败都必须拒绝，不能挑一个信。
func (parser *B2BObservationParser) Parse(record *bizgeneration.ProviderInboxRecord) (bizgeneration.ProviderTerminalObservation, error) {
	if parser == nil || record == nil {
		return bizgeneration.ProviderTerminalObservation{}, ErrB2BObservationParserConfig
	}
	event, err := polarstarb2b.ParseVerifiedCallbackBody(
		parser.tenantID, parser.accountRef, record.JobID, record.DeliveryID, record.Attempts, record.PayloadDigest, record.Payload,
	)
	if err != nil {
		return bizgeneration.ProviderTerminalObservation{}, err
	}
	// 持久化事实与重放出的报文必须指向同一步骤：否则一次串号会把终态写到
	// 另一笔任务上，而这种错误在账本上不可逆。
	if event.StepID != record.StepID || event.AccountRef != record.AccountRef {
		return bizgeneration.ProviderTerminalObservation{}, bizgeneration.ErrProviderInboxStepMismatch
	}
	observation := bizgeneration.ProviderTerminalObservation{
		Capability: event.Capability,
		Status:     bizgeneration.ProviderTerminalStatus(event.Status),
	}
	// 只有完成态携带结果地址；领域校验会再挡一次，这里提前不填是为了让
	// 「失败但带结果」在解析层就无法构造出来。
	if event.Status == polarstarb2b.CallbackStatusCompleted {
		observation.ResultRef = event.ResultURL
	}
	if err := observation.Validate(); err != nil {
		return bizgeneration.ProviderTerminalObservation{}, err
	}
	return observation, nil
}

// B2BLookupOutcome 是一次状态查询的归一化结果。
type B2BLookupOutcome struct {
	// Terminal 为 false 表示供应商仍在处理；此时 Observation 无意义。
	Terminal bool
	// Observation 只在 Terminal 为真时有效。
	Observation bizgeneration.ProviderTerminalObservation
	// JobID 是供应商给出的外部任务号。终态归属与绑定都以它为准。
	JobID string
	// ResponseDigest 是查询响应原始字节的摘要，作为终态事实的取证标识，
	// 与回调路径持久化的 PayloadDigest 同义。
	ResponseDigest string
}

// ParseLookup 把一次状态查询结果归一化为终态观察值。
//
// 非终态不是错误：它只表示「还没有结论」，调用方应当按退避再次查询。把中间态
// 当成失败会让一次正常的等待变成告警，也会让重排退避失去意义。
func (parser *B2BObservationParser) ParseLookup(intent *bizgeneration.ProviderSubmissionIntent, job polarstarb2b.Job) (B2BLookupOutcome, error) {
	if parser == nil || intent == nil {
		return B2BLookupOutcome{}, ErrB2BObservationParserConfig
	}
	// 查询响应自报的租户、外部标识与能力必须与冻结意图一致。客户端已经在
	// 解码时核过一遍，这里再核一次是因为归一化是另一条独立入口：把完整性
	// 寄托在「上一个调用方已经验过」等于每多一个调用方就多一次漏检机会。
	if job.TenantID != parser.tenantID || job.ExternalID != intent.Request.StepID || job.Capability != intent.Request.Capability {
		return B2BLookupOutcome{}, bizgeneration.ErrProviderInboxStepMismatch
	}
	outcome := B2BLookupOutcome{JobID: job.JobID, ResponseDigest: job.ResponseDigest}
	if !terminalLookupStatus(job.Status) {
		return outcome, nil
	}
	outcome.Terminal = true
	outcome.Observation = bizgeneration.ProviderTerminalObservation{
		Capability: job.Capability,
		Status:     bizgeneration.ProviderTerminalStatus(job.Status),
	}
	if job.Status == string(polarstarb2b.CallbackStatusCompleted) {
		outcome.Observation.ResultRef = job.ResultURL
	}
	if err := outcome.Observation.Validate(); err != nil {
		return B2BLookupOutcome{}, err
	}
	return outcome, nil
}

// terminalLookupStatus 是查询响应里允许被当成终态的三个取值。
//
// queued/dispatching/processing 是中间态：把它们当成终态会把「还在跑」写成
// 「已结束」，而那个写入在账本上不可逆。
func terminalLookupStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// validB2BObservationIdentity 与回调验签器的身份约束同形：非空、无首尾空白、
// 长度有界。两处一旦放宽，重放解析就会接受一个验签器永远不会接受的租户身份。
func validB2BObservationIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= b2bObservationMaxIdentity
}
