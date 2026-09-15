package generation

// 本文件提供仅用于本机联调的“生成中台”合同模拟器。
// 它实现真实 HTTP 请求、签名校验、幂等查询和可选回调，但不会访问任何外部网络。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LocalPlatformOptions struct {
	RequestHMACKey  string
	CallbackHMACKey string
	Now             func() time.Time
	Callback        bool
	HTTPClient      *http.Client
}

const localCallbackSubmissionGracePeriod = 50 * time.Millisecond

type localPlatformHandler struct {
	key         string
	callbackKey string
	now         func() time.Time
	callback    bool
	// callbackDelay 仅用于本机模拟器：为 Worker 写入 submitted 状态预留时间，
	// 防止“中台回调先于本地提交落库”的验收竞态。
	callbackDelay time.Duration
	http          *http.Client
	mu            sync.Mutex
	jobs          map[string]SubmissionResult
}

// NewLocalPlatformHandler 创建隔离的本地模拟中台。生产装配不会引用此函数。
func NewLocalPlatformHandler(options LocalPlatformOptions) http.Handler {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	callbackKey := strings.TrimSpace(options.CallbackHMACKey)
	if callbackKey == "" {
		callbackKey = strings.TrimSpace(options.RequestHMACKey)
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &localPlatformHandler{
		key:           strings.TrimSpace(options.RequestHMACKey),
		callbackKey:   callbackKey,
		now:           now,
		callback:      options.Callback,
		callbackDelay: localCallbackSubmissionGracePeriod,
		http:          httpClient,
		jobs:          make(map[string]SubmissionResult),
	}
}

func (handler *localPlatformHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == lookupPath {
		handler.lookup(writer, request)
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != executionPath {
		writeLocalJSON(writer, http.StatusNotFound, map[string]any{"success": false, "code": "NOT_FOUND"})
		return
	}
	var raw []byte
	var err error
	if request.Body != nil {
		raw, err = io.ReadAll(io.LimitReader(request.Body, 1<<20))
	}
	if err != nil || !handler.validRequest(request, raw) {
		writeLocalJSON(writer, http.StatusUnauthorized, map[string]any{"success": false, "code": "INVALID_SIGNATURE"})
		return
	}
	var execution Execution
	if json.Unmarshal(raw, &execution) != nil || validateRawExecutionForCallback(raw, execution.Delivery.Callback) != nil {
		writeLocalJSON(writer, http.StatusBadRequest, map[string]any{"success": false, "code": "INVALID_EXECUTION"})
		return
	}
	job := SubmissionResult{JobID: "local-job-" + shortHash(execution.ExternalRef), Status: "accepted"}
	handler.mu.Lock()
	if previous, found := handler.jobs[execution.ExternalRef]; found {
		job = previous
	} else {
		handler.jobs[execution.ExternalRef] = job
	}
	handler.mu.Unlock()
	if handler.callback {
		go handler.sendCallbackAfterSubmissionGrace(execution, job)
	}
	writeLocalJSON(writer, http.StatusCreated, map[string]any{"success": true, "data": job})
}

// sendCallbackAfterSubmissionGrace 只服务本机联调。
// 真实中台在异步任务完成后才回调；本地模拟器立即完成，因此必须模拟其最小异步间隔。
func (handler *localPlatformHandler) sendCallbackAfterSubmissionGrace(execution Execution, job SubmissionResult) {
	time.Sleep(handler.callbackDelay)
	handler.sendCallback(execution, job)
}

func (handler *localPlatformHandler) lookup(writer http.ResponseWriter, request *http.Request) {
	if strings.TrimSpace(request.URL.Query().Get("externalRef")) == "" || strings.TrimSpace(request.URL.Query().Get("idempotencyKey")) == "" {
		writeLocalJSON(writer, http.StatusBadRequest, map[string]any{"success": false, "code": "INVALID_LOOKUP"})
		return
	}
	if !handler.validRequest(request, nil) {
		writeLocalJSON(writer, http.StatusUnauthorized, map[string]any{"success": false, "code": "INVALID_SIGNATURE"})
		return
	}
	externalRef := request.URL.Query().Get("externalRef")
	handler.mu.Lock()
	job, found := handler.jobs[externalRef]
	handler.mu.Unlock()
	if !found {
		writeLocalJSON(writer, http.StatusNotFound, map[string]any{"success": false, "code": "NOT_FOUND"})
		return
	}
	writeLocalJSON(writer, http.StatusOK, map[string]any{"success": true, "data": job})
}

func (handler *localPlatformHandler) validRequest(request *http.Request, body []byte) bool {
	if handler.key == "" || request.Header.Get("X-Service-Id") != serviceID || request.Header.Get("X-Timestamp") == "" || request.Header.Get("X-Request-Nonce") == "" || request.Header.Get("X-Signature-V2") == "" {
		return false
	}
	timestamp := request.Header.Get("X-Timestamp")
	millis, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if delta := handler.now().UTC().Sub(time.UnixMilli(millis).UTC()); delta < -5*time.Minute || delta > 5*time.Minute {
		return false
	}
	target := request.URL.EscapedPath()
	if request.URL.RawQuery != "" {
		target += "?" + request.URL.RawQuery
	}
	return SignV2(handler.key, request.Method, target, timestamp, request.Header.Get("X-Request-Nonce"), body) == request.Header.Get("X-Signature-V2")
}

func (handler *localPlatformHandler) sendCallback(execution Execution, job SubmissionResult) {
	callback, err := url.Parse(execution.Delivery.Callback)
	if err != nil || callback.Host == "" || (callback.Hostname() != "127.0.0.1" && callback.Hostname() != "localhost") {
		return
	}
	payload := map[string]any{"contractVersion": callbackContractVersion, "eventId": strings.Repeat("a", 64), "deliveryId": "local-delivery-" + shortHash(execution.ExternalRef), "fingerprint": strings.Repeat("a", 64), "eventType": "execution.completed", "jobId": job.JobID, "externalRef": execution.ExternalRef, "status": "completed", "outputs": []map[string]string{{"role": "result", "mediaType": outputMediaType(execution.Capability), "url": "https://local-generation.invalid/results/" + job.JobID}}, "error": nil, "usage": map[string]any{}, "timestamps": map[string]any{}, "executionRef": map[string]string{"tenantId": mainSiteTenantID, "tenantSignupSource": mainSiteSignupSource, "capability": string(execution.Capability), "modelSku": execution.ModelSKU}, "metadata": map[string]string{"site": metadataSite}}
	raw, _ := json.Marshal(payload)
	timestamp := handler.now().UTC().UnixMilli()
	nonce := "local-callback-nonce-" + shortHash(execution.ExternalRef)
	req, err := http.NewRequest(http.MethodPost, callback.String(), strings.NewReader(string(raw)))
	if err != nil {
		return
	}
	req.Header.Set(callbackVersionHeader, callbackVersion)
	req.Header.Set(callbackTimestampHeader, formatMillis(timestamp))
	req.Header.Set(callbackNonceHeader, nonce)
	req.Header.Set(callbackSignatureHeader, signCallback(handler.callbackKey, formatMillis(timestamp), req.Method, callback.Path, nonce, raw))
	response, err := handler.http.Do(req)
	if err == nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func outputMediaType(capability Capability) string {
	if capability == CapabilityImageToVideo {
		return "video"
	}
	return "image"
}

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:16]
}

func writeLocalJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func formatMillis(value int64) string { return strconv.FormatInt(value, 10) }

func signCallback(key, timestamp, method, path, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(callbackSignaturePayload(timestamp, method, path, nonce, body)))
	return hex.EncodeToString(mac.Sum(nil))
}
