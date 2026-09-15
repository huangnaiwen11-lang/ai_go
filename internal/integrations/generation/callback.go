package generation

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ai-business-service/internal/executionv2"
)

const (
	callbackVersionHeader   = "X-Generation-Callback-Version"
	callbackTimestampHeader = "X-Generation-Callback-Timestamp"
	callbackNonceHeader     = "X-Generation-Callback-Nonce"
	callbackSignatureHeader = "X-Generation-Callback-Signature"
	callbackVersion         = "2"
	callbackContractVersion = "execution.callback.v2"
	callbackSignatureDomain = "generation-callback-v2"
	callbackWindow          = 5 * time.Minute
	mainSiteTenantID        = "cling-ai"
	mainSiteSignupSource    = "internal"
)

var (
	// ErrInvalidCallback 是所有不可信回调输入共用的固定错误，避免回显敏感内容。
	ErrInvalidCallback        = errors.New("invalid generation callback")
	callbackNonceFormat       = regexp.MustCompile(`^[A-Za-z0-9._:-]{16,128}$`)
	callbackFingerprintFormat = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// CallbackResult 是已完成回调中经合同校验后的唯一结果素材。
type CallbackResult struct {
	MediaType string
	URL       string
}

// CallbackEvent 只保留终态编排需要的受控技术事实，不保留原始报文或上游错误文本。
type CallbackEvent struct {
	CallbackVersion string
	JobID           string
	ExternalRef     string
	Capability      executionv2.Capability
	Failed          bool
	Cancelled       bool
	Result          CallbackResult
	NonceHash       string
	PayloadDigest   string
}

// CallbackVerifier 冻结 V2 原始报文签名合同，不依赖 HTTP 路由或持久化实现。
type CallbackVerifier struct {
	key []byte
	now func() time.Time
}

// NewCallbackVerifier 创建回调验签器。调用方只能提供足够长度的 HMAC 密钥和受控时钟。
func NewCallbackVerifier(hmacKey string, now func() time.Time) (*CallbackVerifier, error) {
	key := strings.TrimSpace(hmacKey)
	if len(key) < minimumHMACKey || now == nil {
		return nil, ErrInvalidCallback
	}
	return &CallbackVerifier{key: []byte(key), now: now}, nil
}

// Verify 验证 V2 回调并输出不包含原始报文、签名或错误文本的受控事件。
func (verifier *CallbackVerifier) Verify(method, path string, headers http.Header, rawBody []byte) (CallbackEvent, error) {
	if verifier == nil || len(verifier.key) < minimumHMACKey || verifier.now == nil || !executionv2.IsJSONSizeWithinLimit(rawBody) || method != http.MethodPost || !isAllowedCallbackPath(path) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	timestamp, nonce, signature, err := callbackHeaders(headers)
	if err != nil || !isFreshCallbackTimestamp(timestamp, verifier.now()) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	if !verifyCallbackSignature(verifier.key, timestamp, method, path, nonce, signature, rawBody) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	event, err := parseCallbackEvent(rawBody)
	if err != nil {
		return CallbackEvent{}, ErrInvalidCallback
	}
	event.NonceHash = callbackSHA256(nonce)
	event.PayloadDigest = callbackSHA256Bytes(rawBody)
	return event, nil
}

func callbackHeaders(headers http.Header) (string, string, string, error) {
	if headers == nil || singleHeader(headers, callbackVersionHeader) != callbackVersion {
		return "", "", "", ErrInvalidCallback
	}
	timestamp := singleHeader(headers, callbackTimestampHeader)
	nonce := singleHeader(headers, callbackNonceHeader)
	signature := singleHeader(headers, callbackSignatureHeader)
	if timestamp == "" || !callbackNonceFormat.MatchString(nonce) || signature == "" {
		return "", "", "", ErrInvalidCallback
	}
	if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		return "", "", "", ErrInvalidCallback
	}
	return timestamp, nonce, signature, nil
}

func singleHeader(headers http.Header, key string) string {
	values := headers.Values(key)
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] {
		return ""
	}
	return values[0]
}

func isAllowedCallbackPath(path string) bool {
	return path == generationCallbackPath || path == "/api/internal/generation-callback"
}

func isFreshCallbackTimestamp(raw string, now time.Time) bool {
	milliseconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	delta := now.UTC().Sub(time.UnixMilli(milliseconds).UTC())
	return delta >= -callbackWindow && delta <= callbackWindow
}

func verifyCallbackSignature(key []byte, timestamp, method, path, nonce, supplied string, rawBody []byte) bool {
	provided, err := hex.DecodeString(supplied)
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(callbackSignaturePayload(timestamp, method, path, nonce, rawBody)))
	return hmac.Equal(mac.Sum(nil), provided)
}

func callbackSignaturePayload(timestamp, method, path, nonce string, rawBody []byte) string {
	return strings.Join([]string{
		callbackSignatureDomain,
		timestamp,
		method,
		path,
		nonce,
		callbackSHA256Bytes(rawBody),
	}, "\n")
}

func parseCallbackEvent(rawBody []byte) (CallbackEvent, error) {
	if err := executionv2.ValidateJSON(rawBody); err != nil {
		return CallbackEvent{}, ErrInvalidCallback
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &raw); err != nil || raw == nil || !hasOnlyCallbackKeys(raw) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	contractVersion, ok := requiredJSONString(raw, "contractVersion")
	if !ok || contractVersion != callbackContractVersion || !requiredCallbackIdentifiers(raw) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	eventType, eventTypeOK := callbackRequiredString(raw, "eventType", 32)
	jobID, jobIDOK := callbackRequiredString(raw, "jobId", 256)
	externalRef, externalRefOK := callbackRequiredString(raw, "externalRef", 512)
	status, statusOK := callbackRequiredString(raw, "status", 32)
	capability, executionOK := parseCallbackExecutionRef(raw["executionRef"])
	if !eventTypeOK || !jobIDOK || !externalRefOK || !statusOK || !executionOK {
		return CallbackEvent{}, ErrInvalidCallback
	}
	if !isJSONObject(raw["usage"]) || !isJSONObject(raw["timestamps"]) || !validateCallbackMetadata(raw["metadata"]) || !isJSONArray(raw["outputs"]) || !validateCallbackError(raw["error"]) {
		return CallbackEvent{}, ErrInvalidCallback
	}
	switch {
	case eventType == "execution.completed" && status == "completed":
		result, ok := parseCompletedResult(raw["outputs"], capability)
		if !ok {
			return CallbackEvent{}, ErrInvalidCallback
		}
		return CallbackEvent{CallbackVersion: callbackVersion, JobID: jobID, ExternalRef: externalRef, Capability: capability, Result: result}, nil
	case eventType == "execution.failed" && status == "failed":
		if !hasNoResultOutput(raw["outputs"]) {
			return CallbackEvent{}, ErrInvalidCallback
		}
		return CallbackEvent{CallbackVersion: callbackVersion, JobID: jobID, ExternalRef: externalRef, Capability: capability, Failed: true}, nil
	case eventType == "execution.cancelled" && status == "cancelled":
		if !hasNoResultOutput(raw["outputs"]) {
			return CallbackEvent{}, ErrInvalidCallback
		}
		return CallbackEvent{CallbackVersion: callbackVersion, JobID: jobID, ExternalRef: externalRef, Capability: capability, Cancelled: true}, nil
	default:
		return CallbackEvent{}, ErrInvalidCallback
	}
}

func hasOnlyCallbackKeys(raw map[string]json.RawMessage) bool {
	return callbackHasOnlyKeys(raw, "contractVersion", "eventId", "deliveryId", "fingerprint", "eventType", "jobId", "externalRef", "status", "outputs", "error", "usage", "timestamps", "executionRef", "metadata")
}

func requiredCallbackIdentifiers(raw map[string]json.RawMessage) bool {
	_, deliveryOK := callbackRequiredString(raw, "deliveryId", 256)
	eventID, eventIDOK := callbackRequiredString(raw, "eventId", sha256.Size*2)
	fingerprint, fingerprintOK := callbackRequiredString(raw, "fingerprint", sha256.Size*2)
	return deliveryOK && eventIDOK && fingerprintOK && callbackFingerprintFormat.MatchString(eventID) && callbackFingerprintFormat.MatchString(fingerprint) && eventID == fingerprint
}

func requiredJSONString(raw map[string]json.RawMessage, key string) (string, bool) {
	value, found := raw[key]
	if !found || !callbackIsJSONString(value) {
		return "", false
	}
	var decoded string
	if json.Unmarshal(value, &decoded) != nil {
		return "", false
	}
	return decoded, true
}

func callbackRequiredString(raw map[string]json.RawMessage, key string, maximum int) (string, bool) {
	value, ok := requiredJSONString(raw, key)
	trimmed := strings.TrimSpace(value)
	return value, ok && value == trimmed && value != "" && len(value) <= maximum
}

func parseCallbackExecutionRef(raw json.RawMessage) (executionv2.Capability, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || !callbackHasOnlyKeys(fields, "tenantId", "tenantSignupSource", "capability", "modelSku") {
		return "", false
	}
	tenantID, tenantOK := callbackRequiredString(fields, "tenantId", 128)
	signupSource, signupOK := callbackRequiredString(fields, "tenantSignupSource", 64)
	capabilityRaw, capabilityOK := callbackRequiredString(fields, "capability", 64)
	modelSKU, modelOK := callbackRequiredString(fields, "modelSku", 100)
	capability := executionv2.Capability(capabilityRaw)
	if !tenantOK || !signupOK || tenantID != mainSiteTenantID || signupSource != mainSiteSignupSource || !capabilityOK || !modelOK || !isCallbackCapability(capability) || !executionv2.IsSupportedModelSKU(capability, modelSKU) {
		return "", false
	}
	return capability, true
}

func isCallbackCapability(capability executionv2.Capability) bool {
	return capability == executionv2.CapabilityTextToImage || capability == executionv2.CapabilityImageEdit || capability == executionv2.CapabilityImageToVideo
}

func parseCompletedResult(raw json.RawMessage, capability executionv2.Capability) (CallbackResult, bool) {
	var outputs []json.RawMessage
	if json.Unmarshal(raw, &outputs) != nil || len(outputs) != 1 {
		return CallbackResult{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(outputs[0], &fields) != nil || fields == nil || !callbackHasOnlyKeys(fields, "role", "mediaType", "url", "thumbnailUrl") {
		return CallbackResult{}, false
	}
	role, roleOK := requiredJSONString(fields, "role")
	mediaType, mediaOK := requiredJSONString(fields, "mediaType")
	resultURL, urlOK := normalizeCallbackMediaURL(fields["url"])
	if thumbnail, found := fields["thumbnailUrl"]; found {
		if _, thumbnailOK := normalizeCallbackMediaURL(thumbnail); !thumbnailOK {
			return CallbackResult{}, false
		}
	}
	if !roleOK || !mediaOK || !urlOK || role != "result" || !isCompatibleResultMediaType(capability, mediaType) {
		return CallbackResult{}, false
	}
	return CallbackResult{MediaType: mediaType, URL: resultURL}, true
}

func isCompatibleResultMediaType(capability executionv2.Capability, mediaType string) bool {
	if capability == executionv2.CapabilityImageToVideo {
		return mediaType == "video"
	}
	return mediaType == "image"
}

func isSafeCallbackResultURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func normalizeCallbackMediaURL(raw json.RawMessage) (string, bool) {
	var value string
	if !callbackIsJSONString(raw) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, value == strings.TrimSpace(value) && value != "" && len(value) <= 4096 && isSafeCallbackResultURL(value)
}

func hasNoResultOutput(raw json.RawMessage) bool {
	var outputs []json.RawMessage
	if json.Unmarshal(raw, &outputs) != nil {
		return false
	}
	for _, output := range outputs {
		var fields map[string]json.RawMessage
		if json.Unmarshal(output, &fields) == nil && fields != nil {
			if role, ok := requiredJSONString(fields, "role"); ok && role == "result" {
				return false
			}
		}
	}
	return true
}

func validateCallbackMetadata(raw json.RawMessage) bool {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(raw, &metadata) != nil || metadata == nil || !callbackHasOnlyKeys(metadata, "site", "traceId") {
		return false
	}
	for key, maximum := range map[string]int{"site": 100, "traceId": 200} {
		value, found := metadata[key]
		if !found || strings.TrimSpace(string(value)) == "null" {
			continue
		}
		if _, ok := callbackRequiredString(metadata, key, maximum); !ok {
			return false
		}
	}
	return true
}

func validateCallbackError(raw json.RawMessage) bool {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return true
	}
	var callbackError map[string]json.RawMessage
	if json.Unmarshal(raw, &callbackError) != nil || callbackError == nil || !callbackHasOnlyKeys(callbackError, "code", "message") {
		return false
	}
	for key, maximum := range map[string]int{"code": 128, "message": 2048} {
		value, found := callbackError[key]
		if !found || strings.TrimSpace(string(value)) == "null" {
			continue
		}
		if _, ok := callbackRequiredString(callbackError, key, maximum); !ok {
			return false
		}
	}
	return true
}

func isJSONObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func isJSONArray(raw json.RawMessage) bool {
	var array []json.RawMessage
	return json.Unmarshal(raw, &array) == nil && array != nil
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

func callbackIsJSONString(raw json.RawMessage) bool {
	if strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func callbackSHA256(raw string) string {
	return callbackSHA256Bytes([]byte(raw))
}

func callbackSHA256Bytes(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
