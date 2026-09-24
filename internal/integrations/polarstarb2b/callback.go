package polarstarb2b

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

const (
	// CallbackContractVersion 是外部租户终态回调（webhook）的固定合同版本。
	// 它与本仓库另一套 execution.callback.v2 是**两份不同合同**：后者面向自有
	// 生成中台，签名域包含 method/path/timestamp/nonce；本合同的签名只覆盖
	// 原始 body，且投递去重依赖 X-Delivery-Id。两套不能互相校验。
	CallbackContractVersion = "b2b.callback.v2"

	callbackSignatureHeader = "X-Signature"
	callbackJobHeader       = "X-Job-Id"
	callbackDeliveryHeader  = "X-Delivery-Id"
	callbackAttemptHeader   = "X-Callback-Attempt"
	callbackEncodingHeader  = "Content-Encoding"

	// callbackMaxBodyBytes 与平台侧单次投递体量上限一致；超过即拒收，
	// 不截断后继续验签——被截断的 body 一定验签失败，报错原因会更难定位。
	callbackMaxBodyBytes = 1 << 20
	// callbackMaxAttempt 是投递次数上界。平台侧有界投递最多 8 次，这里留出
	// 余量但不接受任意大值，避免把一个攻击者可控的整数写进持久化记录。
	callbackMaxAttempt = 1 << 16
	// minimumCallbackSecret 与配置校验（conf.validateB2B）的下限一致。
	minimumCallbackSecret = 32
)

var ErrInvalidCallback = errors.New("polarstar b2b: invalid callback")

// CallbackStatus 是 webhook 允许推送的终态。
//
// 平台只对 completed / failed / cancelled 做有界 webhook 投递；queued /
// dispatching / processing 只经 SSE 或轮询暴露。因此中间态出现在 webhook
// 里就是合同偏离，必须拒收而不是当成「还没完成」悄悄存下来——那会让一次
// 状态回退覆盖掉已经落地的终态。
type CallbackStatus string

const (
	CallbackStatusCompleted CallbackStatus = "completed"
	CallbackStatusFailed    CallbackStatus = "failed"
	CallbackStatusCancelled CallbackStatus = "cancelled"
)

// CallbackEvent 是验签并校验后的受控投递事实。
//
// 它不含凭据、不含上游错误文本、也不含原始报文；PayloadDigest 是原始 body 的
// SHA-256，持久化侧用它做按字节去重与冲突判定。AccountRef 来自本地配置而不是
// 报文：入站投递无权声明自己属于哪个账号。
type CallbackEvent struct {
	AccountRef    string
	DeliveryID    string
	StepID        string
	JobID         string
	Capability    string
	Status        CallbackStatus
	ResultURL     string
	Attempt       int
	PayloadDigest string
}

// CallbackVerifier 冻结 b2b.callback.v2 的原始报文签名合同。
//
// 它不依赖 HTTP 路由或持久化实现，也不持有除租户 HMAC secrets 之外的任何凭据。
// 同一进程可以并存多份验签器（不同租户），因此这里不提供包级单例。
type CallbackVerifier struct {
	// secrets is ordered active first, previous second. During a controlled
	// rotation both signatures are accepted, but no other fallback is allowed.
	secrets    [][]byte
	tenantID   string
	accountRef string
}

// NewCallbackVerifier 创建 B2B 回调验签器。
//
// tenantID 与 accountRef 都必须非空：前者用于核对报文自报的租户，后者是冻结
// 归属的一部分——入站 delivery 只能落进它自己的账号分区，否则一次跨账号投递
// 会写进错误的 inbox 键空间，而那种错误在去重语义上无法被发现。
func NewCallbackVerifier(secret, tenantID, accountRef string) (*CallbackVerifier, error) {
	return NewCallbackVerifierWithSecrets(secret, "", tenantID, accountRef)
}

// NewCallbackVerifierWithSecrets creates a verifier for a controlled secret
// rotation. active is the new signing secret; previous is optional and should
// be retired promptly after the sender has switched. Both secrets are checked
// against the same raw body, with no error distinction that could leak which
// one matched.
func NewCallbackVerifierWithSecrets(active, previous, tenantID, accountRef string) (*CallbackVerifier, error) {
	// 首尾空白必须在这里就被拒绝，而不是 trim 后接受：HMAC 用的是原始字节，
	// 接受一个带空白的 secret 只会让每次投递都验签失败，而失败原因看起来
	// 和「密钥不对」完全一样。
	if !validCallbackSecret(active) || (previous != "" && (!validCallbackSecret(previous) || previous == active)) ||
		!validIdentity(tenantID, 200) || !validID(accountRef, 200) {
		return nil, ErrInvalidCallback
	}
	secrets := [][]byte{[]byte(active)}
	if previous != "" {
		secrets = append(secrets, []byte(previous))
	}
	return &CallbackVerifier{secrets: secrets, tenantID: tenantID, accountRef: accountRef}, nil
}

// Verify 验证一条 webhook 投递并输出受控事件。
//
// 顺序是合同的一部分：先验签、后解析 JSON。反过来的话，未验签的报文会先进入
// JSON 解析器，攻击者可以用畸形结构探测解析器行为，而我们的拒绝原因也会从
// 「签名不符」变成「结构不符」，把可观测性泄漏给未认证方。
func (verifier *CallbackVerifier) Verify(headers http.Header, rawBody []byte) (CallbackEvent, error) {
	if verifier == nil || len(verifier.secrets) == 0 || headers == nil ||
		len(rawBody) == 0 || len(rawBody) > callbackMaxBodyBytes {
		return CallbackEvent{}, ErrInvalidCallback
	}
	if hasContentEncoding(headers) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	signature := singleCallbackHeader(headers, callbackSignatureHeader)
	jobID := singleCallbackHeader(headers, callbackJobHeader)
	deliveryID := singleCallbackHeader(headers, callbackDeliveryHeader)
	attemptRaw := singleCallbackHeader(headers, callbackAttemptHeader)
	attempt, err := strconv.Atoi(attemptRaw)
	if err != nil || attempt < 1 || attempt > callbackMaxAttempt ||
		!validJobID(jobID) || !validIdentity(deliveryID, 200) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	if !verifier.verifySignature(signature, rawBody) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	event, err := parseCallbackEvent(verifier.tenantID, verifier.accountRef, jobID, deliveryID, attempt, rawBody)
	if err != nil {
		return CallbackEvent{}, ErrInvalidCallback
	}
	return event, nil
}

func (verifier *CallbackVerifier) verifySignature(signature string, rawBody []byte) bool {
	matched := false
	for _, secret := range verifier.secrets {
		// Evaluate every configured key so the active/previous slot is not
		// observable through an early-return timing difference.
		if verifyCallbackSignature(secret, signature, rawBody) {
			matched = true
		}
	}
	return matched
}

func validCallbackSecret(secret string) bool {
	return secret != "" && secret == strings.TrimSpace(secret) && len(secret) >= minimumCallbackSecret
}

func hasContentEncoding(headers http.Header) bool {
	for key := range headers {
		if strings.EqualFold(key, callbackEncodingHeader) {
			return true
		}
	}
	return false
}

// ParseVerifiedCallbackBody 从**已验签并持久化**的原始投递字节重新解析终态语义。
//
// 它不做验签，因此调用方必须提供持久化时记录的原始字节摘要；摘要不符直接拒绝。
// 这道校验不是形式主义：它把「读错了记录的字节」与「字节被改写」变成一次确定
// 的拒绝，而不是把一份来路不明的报文当成合法投递解析。
//
// 为什么不在 ACK 时把解析结果一并存下：ACK 路径必须极短（平台只给 8 秒），而
// 解析规则一旦在写入时定型，后续就无法在不改历史数据的前提下修正。重放路径
// 读的是同一份字节，因此重放与首次解析永远同源。
func ParseVerifiedCallbackBody(tenantID, accountRef, jobID, deliveryID string, attempt int, payloadDigest string, rawBody []byte) (CallbackEvent, error) {
	// 身份约束必须在这里独立再验一次：parseCallbackEvent 只用 tenantID 做报文
	// 核对、只用 accountRef 填充结果，它本身不校验这两个入参。少了这一步，
	// 一个空账号会被解析成「属于空账号的投递」，而那个值随后会进 inbox 键空间。
	if !validIdentity(tenantID, 200) || !validID(accountRef, 200) || !validJobID(jobID) || !validIdentity(deliveryID, 200) ||
		attempt < 1 || attempt > callbackMaxAttempt {
		return CallbackEvent{}, ErrInvalidCallback
	}
	sum := sha256.Sum256(rawBody)
	if payloadDigest == "" || len(payloadDigest) != sha256.Size*2 || payloadDigest != strings.ToLower(payloadDigest) || hex.EncodeToString(sum[:]) != payloadDigest {
		return CallbackEvent{}, ErrInvalidCallback
	}
	return parseCallbackEvent(tenantID, accountRef, jobID, deliveryID, attempt, rawBody)
}

func parseCallbackEvent(tenantID, accountRef, jobID, deliveryID string, attempt int, rawBody []byte) (CallbackEvent, error) {
	if err := validateJSON(rawBody); err != nil {
		return CallbackEvent{}, ErrInvalidCallback
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(rawBody, &raw) != nil || raw == nil || !callbackHasOnlyKeys(raw,
		"contractVersion", "tenantId", "jobId", "externalId", "capability", "status",
		"output", "error", "completedAt", "failedAt", "deliveryId") {
		return CallbackEvent{}, ErrInvalidCallback
	}
	contract, ok := callbackRequiredString(raw, "contractVersion", 64)
	if !ok || contract != CallbackContractVersion {
		return CallbackEvent{}, ErrInvalidCallback
	}
	bodyTenant, ok := callbackRequiredString(raw, "tenantId", 200)
	if !ok || bodyTenant != tenantID {
		return CallbackEvent{}, ErrInvalidCallback
	}
	// 头部与报文都声明任务身份；两者不一致说明其中一个是伪造或串号的，
	// 不能挑一个信，只能拒收。
	bodyJobID, ok := callbackRequiredString(raw, "jobId", 200)
	if !ok || bodyJobID != jobID {
		return CallbackEvent{}, ErrInvalidCallback
	}
	externalID, ok := callbackRequiredString(raw, "externalId", 189)
	if !ok {
		return CallbackEvent{}, ErrInvalidCallback
	}
	capability, ok := callbackRequiredString(raw, "capability", 64)
	if !ok || !supportedCapability(capability) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	statusRaw, ok := callbackRequiredString(raw, "status", 32)
	if !ok {
		return CallbackEvent{}, ErrInvalidCallback
	}
	status := CallbackStatus(statusRaw)
	if !validCallbackStatus(status) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	// deliveryId 在头部是权威去重键；报文体里带上它时必须一致。
	if _, found := raw["deliveryId"]; found {
		bodyDelivery, ok := callbackRequiredString(raw, "deliveryId", 200)
		if !ok || bodyDelivery != deliveryID {
			return CallbackEvent{}, ErrInvalidCallback
		}
	}
	event := CallbackEvent{
		AccountRef: accountRef, DeliveryID: deliveryID, StepID: externalID, JobID: jobID,
		Capability: capability, Status: status, Attempt: attempt,
	}
	switch status {
	case CallbackStatusCompleted:
		resultURL, ok := callbackResultURL(raw["output"])
		if !ok {
			return CallbackEvent{}, ErrInvalidCallback
		}
		event.ResultURL = resultURL
	default:
		// 非完成终态不得携带结果。允许它出现会让「失败但带结果」这类
		// 自相矛盾的投递进入持久化，下游只能靠猜来选一个。
		if _, found := raw["output"]; found {
			return CallbackEvent{}, ErrInvalidCallback
		}
	}
	event.PayloadDigest = bodyDigest(rawBody)
	return event, nil
}

// bodyDigest 返回原始字节的小写十六进制 SHA-256。
//
// 回调路径与查询路径共用它：两者产出的摘要必须是同一个函数算出来的，否则
// 「这份结论出自哪一串供应商字节」在两处会有不同的写法。
func bodyDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// callbackResultURL 只接受完成态 output 里的 resultUrl。
// 这是**语法与地址类别**校验，不构成下载授权：素材 worker 仍须独立执行
// host 白名单、DNS/IP 检查、重定向规则与内容校验。
func callbackResultURL(raw json.RawMessage) (string, bool) {
	var output map[string]json.RawMessage
	if json.Unmarshal(raw, &output) != nil || output == nil {
		return "", false
	}
	value, ok := callbackRequiredString(output, "resultUrl", 8192)
	if !ok || !safeResultURL(value) {
		return "", false
	}
	return value, true
}

func validCallbackStatus(status CallbackStatus) bool {
	return status == CallbackStatusCompleted || status == CallbackStatusFailed || status == CallbackStatusCancelled
}

// verifyCallbackSignature 校验 hex(HMAC-SHA256(secret, rawBody))。
// 只接受 64 位小写十六进制：大写或带前缀的写法来自非本合同的发送方，
// 与其猜它的意图，不如让它按合同改。
func verifyCallbackSignature(secret []byte, supplied string, rawBody []byte) bool {
	if len(supplied) != sha256.Size*2 || supplied != strings.ToLower(supplied) {
		return false
	}
	provided, err := hex.DecodeString(supplied)
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(rawBody)
	return hmac.Equal(mac.Sum(nil), provided)
}

// singleCallbackHeader 只接受恰好一个、且无首尾空白的取值。
// 多值意味着代理或发送方在做合并，此时无论取哪个都是猜测。
func singleCallbackHeader(headers http.Header, key string) string {
	values := headers.Values(key)
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] {
		return ""
	}
	return values[0]
}

func callbackRequiredString(raw map[string]json.RawMessage, key string, maximum int) (string, bool) {
	value, found := raw[key]
	if !found {
		return "", false
	}
	var decoded string
	if json.Unmarshal(value, &decoded) != nil {
		return "", false
	}
	return decoded, decoded != "" && decoded == strings.TrimSpace(decoded) && len(decoded) <= maximum
}

func callbackHasOnlyKeys(raw map[string]json.RawMessage, allowed ...string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range raw {
		if _, found := allowedSet[key]; !found {
			return false
		}
	}
	return true
}
