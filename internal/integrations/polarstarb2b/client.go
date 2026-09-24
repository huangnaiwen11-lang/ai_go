// Package polarstarb2b 是 PolarStar 公开 B2B 合同的独立适配器：出站请求
// （Submit/Lookup/Cancel）、入站终态回调（b2b.callback.v2）与映射快照都在这里，
// 且**不含任何执行 execution.v2 私有合同的能力**——两套合同的字段不允许互相透传。
//
// 适配器本身不做授权判断，也不访问数据库：调用方必须先取得已发布的映射快照、
// 冻结归属与凭据句柄，再调用这里的纯函数。
package polarstarb2b

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxResponseBytes int64 = 1 << 20

type ClientOptions struct {
	BaseURL               string
	APIKey                string
	AccountRef            string
	ExpectedTenantID      string
	Timeout               time.Duration
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	MaxConnectionsPerHost int
	MaxResponseBytes      int64
	MaxRetryAfter         time.Duration
	// RootCAs adds an explicit trust store (useful for isolated TLS fixtures).
	// TLS verification and the HTTPS-only requirement cannot be disabled.
	RootCAs *x509.CertPool
}

type Client struct {
	baseURL       string
	apiKey        string
	tenantID      string
	accountRef    string
	http          *http.Client
	maxBytes      int64
	maxRetryAfter time.Duration
}

// LookupKey must be recovered from the same frozen submission/account scope.
// JobID is optional only before the first trustworthy binding exists.
type LookupKey struct {
	AccountRef     string
	IdempotencyKey string
	ExternalID     string
	Capability     string
	JobID          string
}

// Job contains validated public identity, state and a completed result URL.
// ResultURL is not download authorization: the media worker must still enforce
// its host allowlist, DNS/IP checks, redirect rules and content validation.
type Job struct {
	ContractVersion string
	JobID           string
	TenantID        string
	ExternalID      string
	Capability      string
	Status          string
	ResultURL       string
	// ResponseDigest 是本次查询响应原始字节的 SHA-256。
	//
	// 它与回调路径持久化的 PayloadDigest 同义：都是「这份结论出自哪一串供应
	// 商字节」的取证标识。两条路径共用同一个字段语义，下游才不需要按来源
	// 猜测这个摘要代表什么。它不参与终态判重——判重只用结论 + 语义摘要。
	ResponseDigest string
}

// CancelReceipt acknowledges only the cancellation HTTP request. It is never
// proof of a terminal cancellation, non-acceptance, or permission to refund.
type CancelReceipt struct {
	JobID               string
	RequestAcknowledged bool
}

// NewClient validates configuration without network access and owns one bounded
// reusable connection pool. No custom RoundTripper can introduce POST retries.
func NewClient(o ClientOptions) (*Client, error) {
	u, err := url.Parse(o.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, notSent("invalid_configuration", nil)
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return nil, notSent("invalid_configuration", nil)
		}
	}
	if !safeHeader(o.APIKey, 4096) || !validIdentity(o.ExpectedTenantID, 200) || !validIdentity(o.AccountRef, 200) {
		return nil, notSent("invalid_configuration", nil)
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = min(5*time.Second, o.Timeout)
	}
	if o.ResponseHeaderTimeout == 0 {
		o.ResponseHeaderTimeout = min(15*time.Second, o.Timeout)
	}
	if o.MaxConnectionsPerHost == 0 {
		o.MaxConnectionsPerHost = 32
	}
	if o.MaxResponseBytes == 0 {
		o.MaxResponseBytes = maxResponseBytes
	}
	if o.MaxRetryAfter == 0 {
		o.MaxRetryAfter = 5 * time.Minute
	}
	if o.Timeout < 0 || o.Timeout > 2*time.Minute || o.ConnectTimeout < 0 || o.ConnectTimeout > 5*time.Second || o.ResponseHeaderTimeout < 0 || o.ResponseHeaderTimeout > 15*time.Second || o.MaxConnectionsPerHost < 1 || o.MaxConnectionsPerHost > 32 || o.ConnectTimeout > o.Timeout || o.ResponseHeaderTimeout > o.Timeout || o.MaxResponseBytes < 1 || o.MaxResponseBytes > maxResponseBytes || o.MaxRetryAfter < 0 || o.MaxRetryAfter > 5*time.Minute {
		return nil, notSent("invalid_configuration", nil)
	}
	var roots *x509.CertPool
	if o.RootCAs != nil {
		roots = o.RootCAs.Clone()
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: o.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout: o.ConnectTimeout, ResponseHeaderTimeout: o.ResponseHeaderTimeout,
		MaxIdleConns: 32, MaxIdleConnsPerHost: o.MaxConnectionsPerHost, MaxConnsPerHost: o.MaxConnectionsPerHost,
		IdleConnTimeout: 90 * time.Second, ExpectContinueTimeout: time.Second,
		MaxResponseHeaderBytes: 32 << 10, DisableCompression: true,
	}
	return &Client{baseURL: strings.TrimSuffix(o.BaseURL, "/"), apiKey: o.APIKey, tenantID: o.ExpectedTenantID, accountRef: o.AccountRef, maxBytes: o.MaxResponseBytes, maxRetryAfter: o.MaxRetryAfter,
		http: &http.Client{Transport: transport, Timeout: o.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

func (c *Client) Submit(ctx context.Context, request Request) (Job, error) {
	if err := request.Validate(); err != nil {
		return Job{}, notSent("invalid_frozen_request", nil)
	}
	if request.AccountRef() != c.accountRef {
		return Job{}, notSent("account_mismatch", nil)
	}
	expected := LookupKey{AccountRef: request.AccountRef(), IdempotencyKey: request.IdempotencyKey(), ExternalID: request.ExternalID(), Capability: request.Capability()}
	if !validLookupKey(expected) {
		return Job{}, notSent("invalid_identity", nil)
	}
	return c.execute(ctx, http.MethodPost, "/api/v1/jobs", request.Payload(), http.StatusCreated, expected)
}

func (c *Client) Lookup(ctx context.Context, key LookupKey) (Job, error) {
	if !validLookupKey(key) || key.AccountRef != c.accountRef {
		return Job{}, notSent("invalid_identity", nil)
	}
	query := url.Values{"idempotencyKey": []string{key.IdempotencyKey}}
	return c.execute(ctx, http.MethodGet, "/api/v1/jobs/lookup?"+query.Encode(), nil, http.StatusOK, key)
}

func (c *Client) Cancel(ctx context.Context, jobID string) (CancelReceipt, error) {
	if !validJobID(jobID) {
		return CancelReceipt{}, notSent("invalid_identity", nil)
	}
	job, err := c.execute(ctx, http.MethodPost, "/api/v1/jobs/"+jobID+"/cancel", nil, http.StatusOK, LookupKey{JobID: jobID})
	if err != nil {
		return CancelReceipt{}, err
	}
	return CancelReceipt{JobID: job.JobID, RequestAcknowledged: true}, nil
}

func (c *Client) execute(ctx context.Context, method, path string, payload []byte, successStatus int, expected LookupKey) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, notSent("context_done", err)
	}
	// A non-rewindable body and no Idempotency-Key header keep net/http from
	// replaying POST, including after a reused connection fails.
	var body io.Reader
	if len(payload) > 0 {
		body = io.LimitReader(bytes.NewReader(payload), int64(len(payload)))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return Job{}, notSent("invalid_request", nil)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return Job{}, &ClientError{Code: "transport_failure", kind: ErrOutcomeUnknown, contextErr: ctx.Err()}
	}
	defer res.Body.Close()
	failure := func(code string) (Job, error) {
		return Job{}, &ClientError{HTTPStatus: res.StatusCode, Code: code, kind: ErrOutcomeUnknown, RetryAfter: c.retryAfter(res)}
	}
	if res.ContentLength > c.maxBytes {
		return failure("response_too_large")
	}
	if enc := res.Header.Values("Content-Encoding"); len(enc) > 1 || (len(enc) == 1 && enc[0] != "" && enc[0] != "identity") {
		return failure("unsupported_encoding")
	}
	ct := res.Header.Values("Content-Type")
	if len(ct) != 1 {
		return failure("invalid_content_type")
	}
	media, _, err := mime.ParseMediaType(ct[0])
	if err != nil || media != "application/json" {
		return failure("invalid_content_type")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, c.maxBytes+1))
	if err != nil {
		return failure("incomplete_response")
	}
	if int64(len(raw)) > c.maxBytes {
		return failure("response_too_large")
	}
	if err = validateJSON(raw); err != nil {
		return failure("invalid_response")
	}
	if res.StatusCode != successStatus {
		return failure(safeHTTPCode(res.StatusCode, raw))
	}
	job, err := decodeJob(raw, c.tenantID, expected)
	if err != nil {
		return failure("invalid_response")
	}
	// 只在解析成功后写入摘要：一份被拒绝的响应不该留下任何可被下游引用的标识。
	job.ResponseDigest = bodyDigest(raw)
	return job, nil
}

func decodeJob(raw []byte, tenant string, expected LookupKey) (Job, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || string(envelope["success"]) != "true" {
		return Job{}, fmt.Errorf("invalid envelope")
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(envelope["data"], &data) != nil || data == nil {
		return Job{}, fmt.Errorf("invalid data")
	}
	read := func(key string) string { var value string; _ = json.Unmarshal(data[key], &value); return value }
	j := Job{ContractVersion: read("contractVersion"), JobID: read("jobId"), TenantID: read("tenantId"), ExternalID: read("externalId"), Capability: read("capability"), Status: read("status")}
	if j.ContractVersion != "b2b.job.v2" || j.TenantID != tenant || !validJobID(j.JobID) || !validIdentity(j.ExternalID, 200) || !validCapability(j.Capability) || !validStatus(j.Status) {
		return Job{}, fmt.Errorf("invalid job")
	}
	if (expected.ExternalID != "" && j.ExternalID != expected.ExternalID) || (expected.Capability != "" && j.Capability != expected.Capability) || (expected.JobID != "" && j.JobID != expected.JobID) {
		return Job{}, fmt.Errorf("identity mismatch")
	}
	if j.Status == "completed" {
		var output map[string]json.RawMessage
		if json.Unmarshal(data["output"], &output) != nil || output == nil || json.Unmarshal(output["resultUrl"], &j.ResultURL) != nil || !safeResultURL(j.ResultURL) {
			return Job{}, fmt.Errorf("invalid completed output")
		}
	}
	return j, nil
}

func safeResultURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 8192 || !utf8.ValidString(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.Contains(host, "%") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return false
		}
		if v4 := ip.To4(); v4 != nil && v4[0] == 100 && (v4[1]&0xc0) == 64 {
			return false
		}
	}
	return true
}

func validLookupKey(k LookupKey) bool {
	return validIdentity(k.AccountRef, 200) && validIdentity(k.IdempotencyKey, 200) && validIdentity(k.ExternalID, 200) && k.IdempotencyKey == "cling-step:"+k.ExternalID && validCapability(k.Capability) && (k.JobID == "" || validJobID(k.JobID))
}
func validCapability(s string) bool {
	return s == "text_to_image" || s == "image_edit" || s == "image_to_video"
}
func validStatus(s string) bool {
	switch s {
	case "queued", "dispatching", "processing", "completed", "failed", "cancelled":
		return true
	}
	return false
}
func validIdentity(s string, max int) bool {
	if s == "" || len(s) > max || strings.TrimSpace(s) != s || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func validJobID(s string) bool {
	if !validIdentity(s, 200) || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:", r)) {
			return false
		}
	}
	return true
}
func safeHeader(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

// Reject duplicate JSON keys (including case aliases), excessive nesting,
// non-UTF8 and trailing data before extracting security-sensitive identities.
func validateJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("invalid utf8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := scanJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing json")
	}
	return nil
}
func scanJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("json too deep")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid key")
			}
			s = strings.ToLower(s)
			if keys[s] {
				return fmt.Errorf("duplicate key")
			}
			keys[s] = true
			if e = scanJSON(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if err := scanJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

func safeHTTPCode(status int, raw []byte) string {
	if status == 409 {
		return "conflict"
	}
	var envelope map[string]json.RawMessage
	var code string
	if json.Unmarshal(raw, &envelope) == nil && string(envelope["success"]) == "false" {
		_ = json.Unmarshal(envelope["code"], &code)
	}
	switch code {
	case "BAD_REQUEST", "VALIDATION_ERROR", "MODEL_INVALID", "UNAUTHORIZED", "PAYMENT_REQUIRED", "FORBIDDEN", "NOT_FOUND", "CONFLICT", "MODEL_INCOMPATIBLE", "RATE_LIMITED", "UPSTREAM_ERROR", "SERVICE_UNAVAILABLE", "MODEL_BUSY", "UPSTREAM_TIMEOUT":
		return code
	}
	return "unexpected_response"
}

func (c *Client) retryAfter(res *http.Response) time.Duration {
	if res.StatusCode != 429 && res.StatusCode != 503 {
		return 0
	}
	values := res.Header.Values("Retry-After")
	if len(values) != 1 || len(values[0]) > 128 {
		return 0
	}
	s := values[0]
	var delay time.Duration
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(c.maxRetryAfter/time.Second) {
			return c.maxRetryAfter
		}
		delay = time.Duration(n) * time.Second
	} else if date, err := http.ParseTime(s); err == nil {
		delay = time.Until(date)
	} else {
		return 0
	}
	if delay < 0 {
		return 0
	}
	if delay > c.maxRetryAfter {
		return c.maxRetryAfter
	}
	return delay
}
