package generation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ai-business-service/internal/executionv2"
)

const (
	serviceID       = "main-backend"
	serviceAudience = "generation-service"
	signatureDomain = "main-backend-generation-request-v2"
	executionPath   = "/api/v2/executions"
	lookupPath      = "/api/v2/executions/lookup"
	// generationCallbackPath 是主站为生成中台预留的固定回调目标。
	// 本阶段只提交该地址，不注册或启动回调路由。
	generationCallbackPath = "/api/v1/internal/generation-callback"
	minimumHMACKey         = 32
)

var (
	ErrRejected       = errors.New("generation request rejected")
	ErrNotFound       = fmt.Errorf("%w: generation execution not found", ErrRejected)
	ErrOutcomeUnknown = errors.New("generation request outcome unknown")
	ErrInvalidRequest = errors.New("invalid generation request target")
	// ErrLocalSubmission 表示请求在本地配置或签名准备阶段已明确无法发送。
	ErrLocalSubmission = errors.New("generation request cannot be submitted locally")
)

type Client struct {
	baseURL     *url.URL
	callbackURL string
	// apiKey 用于生成中台的租户准入；它与双向 HMAC 密钥职责不同，绝不互相复用。
	apiKey     string
	key        []byte
	httpClient *http.Client
	now        func() time.Time
	nonce      func() string
}

type SubmissionResult struct {
	JobID  string `json:"jobId"`
	Status string `json:"status"`
}

type LookupResult struct {
	JobID  string `json:"jobId"`
	Status string `json:"status"`
}

func NewClient(baseURL, hmacKey string, httpClient *http.Client, now func() time.Time, nonce func() string) (*Client, error) {
	return newClient(baseURL, hmacKey, deliveryCallback, "", httpClient, now, nonce)
}

// NewClientWithCallbackOrigin 创建使用主站自有回调 Origin 的受控出站客户端。
// 回调地址仅由已验证 Origin 与固定路径组成，调用方不能传入任意路径或中台默认地址。
func NewClientWithCallbackOrigin(baseURL, hmacKey, callbackOrigin string, httpClient *http.Client, now func() time.Time, nonce func() string) (*Client, error) {
	return newClientWithCallbackOrigin(baseURL, hmacKey, callbackOrigin, "", httpClient, now, nonce)
}

// NewClientWithCallbackOriginAndAPIKey 创建具备生成中台租户准入信息的出站客户端。
// API Key 只在服务端内存和出站请求头中使用，不会进入 execution.v2 业务载荷或日志。
func NewClientWithCallbackOriginAndAPIKey(baseURL, hmacKey, callbackOrigin, apiKey string, httpClient *http.Client, now func() time.Time, nonce func() string) (*Client, error) {
	return newClientWithCallbackOrigin(baseURL, hmacKey, callbackOrigin, apiKey, httpClient, now, nonce)
}

func newClientWithCallbackOrigin(baseURL, hmacKey, callbackOrigin, apiKey string, httpClient *http.Client, now func() time.Time, nonce func() string) (*Client, error) {
	callbackURL, err := buildCallbackURL(callbackOrigin)
	if err != nil {
		return nil, err
	}
	return newClient(baseURL, hmacKey, callbackURL, apiKey, httpClient, now, nonce)
}

func newClient(baseURL, hmacKey, callbackURL, apiKey string, httpClient *http.Client, now func() time.Time, nonce func() string) (*Client, error) {
	parsed, err := parseGenerationOrigin(baseURL)
	if err != nil {
		return nil, err
	}
	normalizedKey := strings.TrimSpace(hmacKey)
	if len(normalizedKey) < minimumHMACKey {
		return nil, errors.New("generation request hmac key must be at least 32 characters")
	}
	if httpClient == nil || now == nil || nonce == nil {
		return nil, errors.New("generation client dependencies are required")
	}
	if callbackURL == "" {
		return nil, errors.New("generation callback url is required")
	}
	controlledHTTPClient := *httpClient
	controlledHTTPClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL:     parsed,
		callbackURL: callbackURL,
		apiKey:      strings.TrimSpace(apiKey),
		key:         []byte(normalizedKey),
		httpClient:  &controlledHTTPClient,
		now:         now,
		nonce:       nonce,
	}, nil
}

func (client *Client) Submit(ctx context.Context, execution Execution) (SubmissionResult, error) {
	if err := validateExecution(execution); err != nil {
		return SubmissionResult{}, err
	}
	// 冻结快照只能携带固定占位值；实际出站时由受控客户端替换成自己的回调地址。
	execution.Delivery.Callback = client.callbackURL
	rawBody, err := json.Marshal(execution)
	if err != nil {
		return SubmissionResult{}, errors.New("invalid execution json")
	}
	if err := validateRawExecutionForCallback(rawBody, client.callbackURL); err != nil {
		return SubmissionResult{}, err
	}
	response, err := client.do(ctx, http.MethodPost, executionPath, rawBody)
	if err != nil {
		return SubmissionResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return SubmissionResult{}, classifySubmitResponse(response)
	}
	result, err := decodeEnvelope(response.Body)
	if err != nil {
		return SubmissionResult{}, err
	}
	return result, nil
}

func (client *Client) Lookup(ctx context.Context, externalRef string) (LookupResult, error) {
	if !isSafeAtom(externalRef) {
		return LookupResult{}, errors.New("invalid external ref")
	}
	query := url.Values{}
	query.Set("externalRef", externalRef)
	query.Set("idempotencyKey", idempotencyPrefix+externalRef)
	response, err := client.do(ctx, http.MethodGet, lookupPath+"?"+query.Encode(), nil)
	if err != nil {
		return LookupResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return LookupResult{}, classifyLookupResponse(response)
	}
	result, err := decodeEnvelope(response.Body)
	if err != nil {
		return LookupResult{}, err
	}
	return LookupResult(result), nil
}

func (client *Client) do(ctx context.Context, method, target string, rawBody []byte) (*http.Response, error) {
	request, err := client.newRequest(ctx, method, target, rawBody)
	if err != nil {
		return nil, err
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: generation request failed", ErrOutcomeUnknown)
	}
	return response, nil
}

// newRequest 构造一份可审计的出站请求，不执行网络 I/O。
// 拆分该步骤可让 API Key 与 V2 签名合同在不监听本机端口的环境中独立回归。
func (client *Client) newRequest(ctx context.Context, method, target string, rawBody []byte) (*http.Request, error) {
	parsedTarget, err := validateRequestTarget(method, target)
	if err != nil {
		return nil, err
	}
	timestamp := strconv.FormatInt(client.now().UTC().UnixMilli(), 10)
	nonce := strings.TrimSpace(client.nonce())
	if nonce == "" {
		return nil, fmt.Errorf("%w: empty request nonce", ErrLocalSubmission)
	}
	requestURL := *client.baseURL
	requestURL.Path = parsedTarget.Path
	requestURL.RawQuery = parsedTarget.RawQuery
	requestURL.ForceQuery = false
	requestURL.Fragment = ""
	requestURL.RawFragment = ""
	signedTarget := requestURL.EscapedPath()
	if requestURL.RawQuery != "" {
		signedTarget += "?" + requestURL.RawQuery
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bytes.NewReader(rawBody))
	if err != nil {
		return nil, fmt.Errorf("%w: build generation request", ErrOutcomeUnknown)
	}
	contentSHA256 := sha256Hex(rawBody)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Service-Id", serviceID)
	request.Header.Set("X-Timestamp", timestamp)
	request.Header.Set("X-Request-Nonce", nonce)
	request.Header.Set("X-Content-SHA256", contentSHA256)
	request.Header.Set("X-Signature-V2", SignV2(string(client.key), method, signedTarget, timestamp, nonce, rawBody))
	if client.apiKey != "" {
		request.Header.Set("X-API-Key", client.apiKey)
	}
	return request, nil
}

func parseGenerationOrigin(raw string) (*url.URL, error) {
	normalized := strings.TrimSpace(raw)
	parsed, err := url.Parse(normalized)
	if err != nil || strings.Contains(normalized, "#") || parsed.Host == "" || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != "" {
		return nil, errors.New("invalid generation base url")
	}
	parsed.Path = ""
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed, nil
}

func buildCallbackURL(rawOrigin string) (string, error) {
	normalized := strings.TrimSpace(rawOrigin)
	parsed, err := url.Parse(normalized)
	if err != nil || strings.Contains(normalized, "#") || parsed.Host == "" || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != "" {
		return "", errors.New("invalid generation callback origin")
	}
	if isDefaultPlatformCallbackHost(parsed.Hostname()) {
		return "", errors.New("generation callback origin must not use default platform domain")
	}
	parsed.Path = generationCallbackPath
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String(), nil
}

func isDefaultPlatformCallbackHost(host string) bool {
	normalized := strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
	return normalized == "cling-ai.com" || strings.HasSuffix(normalized, ".cling-ai.com")
}

func validateRequestTarget(method, target string) (*url.URL, error) {
	parsed, err := url.ParseRequestURI(target)
	if err != nil || strings.Contains(target, "#") || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, ErrInvalidRequest
	}
	if method == http.MethodPost && parsed.Path == executionPath && parsed.EscapedPath() == executionPath && parsed.RawQuery == "" {
		return parsed, nil
	}
	if method != http.MethodGet || parsed.Path != lookupPath || parsed.EscapedPath() != lookupPath {
		return nil, ErrInvalidRequest
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query) != 2 || len(query["externalRef"]) != 1 || len(query["idempotencyKey"]) != 1 {
		return nil, ErrInvalidRequest
	}
	externalRef := query.Get("externalRef")
	if !isSafeAtom(externalRef) || query.Get("idempotencyKey") != idempotencyPrefix+externalRef {
		return nil, ErrInvalidRequest
	}
	return parsed, nil
}

func classifySubmitResponse(response *http.Response) error {
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusForbidden:
		return ErrRejected
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("%w: generation service returned http %d", ErrOutcomeUnknown, response.StatusCode)
	}
}

// classifyLookupResponse 保守处理查询失败：HTTP 失败只能证明本次查询未完成，
// 不能证明此前提交未受理，因此一律进入对账重试而不是触发冲正。
func classifyLookupResponse(response *http.Response) error {
	return fmt.Errorf("%w: generation lookup returned http %d", ErrOutcomeUnknown, response.StatusCode)
}

func decodeEnvelope(body io.Reader) (SubmissionResult, error) {
	var envelope struct {
		Success bool             `json:"success"`
		Data    SubmissionResult `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(body, 4096))
	if err := decoder.Decode(&envelope); err != nil {
		return SubmissionResult{}, fmt.Errorf("%w: invalid generation response", ErrOutcomeUnknown)
	}
	if !envelope.Success {
		return SubmissionResult{}, ErrOutcomeUnknown
	}
	if strings.TrimSpace(envelope.Data.JobID) == "" {
		return SubmissionResult{}, fmt.Errorf("%w: missing generation job id", ErrOutcomeUnknown)
	}
	if err := classifyExecutionStatus(envelope.Data.Status); err != nil {
		return SubmissionResult{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return SubmissionResult{}, fmt.Errorf("%w: invalid generation response", ErrOutcomeUnknown)
	}
	var extra [1]byte
	if count, err := body.Read(extra[:]); count != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return SubmissionResult{}, fmt.Errorf("%w: invalid generation response", ErrOutcomeUnknown)
	}
	return envelope.Data, nil
}

// classifyExecutionStatus 将中台业务状态与 HTTP 成功分开判断。
// 只要中台返回稳定 jobId，排队、派发、处理中和完成都说明同一执行已被受理；
// 绝不能因此走第二次提交。明确拒绝和未知状态仍分别保持原有失败语义。
func classifyExecutionStatus(status string) error {
	switch status {
	case "accepted", "queued", "dispatching", "processing", "completed":
		return nil
	case "rejected":
		return ErrRejected
	default:
		return ErrOutcomeUnknown
	}
}

func SignaturePayloadV2(method, target, timestamp, nonce string, rawBody []byte) string {
	return strings.Join([]string{
		signatureDomain,
		serviceID,
		serviceAudience,
		method,
		target,
		timestamp,
		nonce,
		sha256Hex(rawBody),
	}, "\n")
}

func SignV2(key, method, target, timestamp, nonce string, rawBody []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(SignaturePayloadV2(method, target, timestamp, nonce, rawBody)))
	return hex.EncodeToString(mac.Sum(nil))
}

func validateExecution(execution Execution) error {
	if execution.ContractVersion != contractVersion || execution.IdempotencyKey != idempotencyPrefix+execution.ExternalRef || execution.PriorityClass != priorityClass || execution.Delivery.Callback != deliveryCallback || execution.Delivery.ResultURLPolicy != resultURLPolicy || execution.Metadata.Site != metadataSite {
		return ErrInvalidExecution
	}
	if !isSafeAtom(execution.ExternalRef) {
		return ErrInvalidExecution
	}
	rawInput, err := marshalStepInput(execution.Input)
	if err != nil {
		return ErrInvalidParameters
	}
	if _, err := executionv2.Compile(execution.Capability, execution.ModelSKU, rawInput); err != nil {
		if errors.Is(err, executionv2.ErrInvalidParameters) {
			return ErrInvalidParameters
		}
		return ErrInvalidExecution
	}
	return nil
}

func sha256Hex(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
