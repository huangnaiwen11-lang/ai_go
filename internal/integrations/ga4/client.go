// Package ga4 提供 GA4 Analytics Data API 的有界出站适配器。
//
// 它持有服务账号凭据、自行签发并缓存 access token，并把上游响应归一化成
// biz/adminanalyticsga4 的 Report。它不读取 Node 运行时环境或登录态，
// 也不依赖任何 Google 官方 SDK —— 凭据签名只用标准库 crypto，
// 报表走受控的 net/http。
package ga4

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bizga4 "ai-business-service/internal/biz/adminanalyticsga4"
)

// 旧实现的取值口径，逐项固定。
const (
	defaultAPIBase   = "https://analyticsdata.googleapis.com/v1beta"
	defaultTokenURL  = "https://oauth2.googleapis.com/token"
	analyticsScope   = "https://www.googleapis.com/auth/analytics.readonly"
	defaultTimeout   = 20 * time.Second
	assertionTTL     = time.Hour
	tokenSafetySlack = 60 * time.Second

	// 响应体积上限。上限的意义是「绝不无限读入」，
	// 触发时 JSON 会因截断而解码失败，被当作上游错误，而不是静默接受半个报表。
	maxTokenBytes       = 64 << 10
	maxReportBytes      = 16 << 20
	maxCredentialsBytes = 64 << 10
	maxErrorBodyBytes   = 2 << 10
)

// Environment 是 GA4 的运行时配置：属性 ID + 凭据来源。
// 凭据来源既可以是一段内联 JSON，也可以是 .json 文件路径。
type Environment struct {
	PropertyID       string
	CredentialSource string
}

// Configured 表示两项配置都已给出。它只报告「存在性」，
// 不保证凭据内容可解析 —— 可解析性由 New 判定。
func (e Environment) Configured() bool {
	return strings.TrimSpace(e.PropertyID) != "" && strings.TrimSpace(e.CredentialSource) != ""
}

// FromEnvironment 按旧实现 getServiceAccountKeyEnv / getGa4PropertyIdEnv 的
// 别名顺序读取配置。
//
// 顺序是载荷性的，不要重排：旧实现把四个凭据别名放在同一个列表里取第一个非空值，
// 所以 GOOGLE_APPLICATION_CREDENTIALS 的优先级高于 GOOGLE_SERVICE_ACCOUNT_JSON。
func FromEnvironment() Environment {
	return Environment{
		PropertyID: firstEnvironment("GA4_PROPERTY_ID", "GOOGLE_ANALYTICS_PROPERTY_ID", "GA_PROPERTY_ID", "GA4_PROPERTY"),
		CredentialSource: firstEnvironment(
			"GOOGLE_SERVICE_ACCOUNT_KEY",
			"GOOGLE_APPLICATION_CREDENTIALS",
			"GOOGLE_SERVICE_ACCOUNT_JSON",
			"GOOGLE_SERVICE_ACCOUNT",
		),
	}
}

func firstEnvironment(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

// Credentials 是 Google 服务账号密钥里适配器需要的字段。
type Credentials struct {
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
}

// Options 是装配参数。APIBase / TokenURL / HTTPClient / Now 留空时取生产默认值，
// 测试用它们把出站流量指向本地伪上游。
type Options struct {
	PropertyID  string
	Credentials Credentials
	APIBase     string
	TokenURL    string
	Timeout     time.Duration
	HTTPClient  *http.Client
	Now         func() time.Time
}

// Client 是 bizga4.Reporter 的有界实现。
type Client struct {
	propertyID  string
	apiBase     *url.URL
	tokenURL    *url.URL
	credentials Credentials
	signer      *rsa.PrivateKey
	httpClient  *http.Client
	now         func() time.Time

	refreshMu   sync.Mutex
	tokenMu     sync.RWMutex
	token       string
	tokenExpiry time.Time
}

var _ bizga4.Reporter = (*Client)(nil)

// New 装配客户端。任何配置缺失或凭据不可解析都返回 bizga4.ErrNotConfigured ——
// 这个错误的语义是「没配好」，与「上游拒绝了凭据」和「上游故障」严格区分。
func New(options Options) (*Client, error) {
	propertyID := strings.TrimSpace(options.PropertyID)
	if propertyID == "" {
		return nil, fmt.Errorf("%w: GA4 property id is missing", bizga4.ErrNotConfigured)
	}
	credentials := Credentials{
		ClientEmail:  strings.TrimSpace(options.Credentials.ClientEmail),
		PrivateKey:   options.Credentials.PrivateKey,
		PrivateKeyID: strings.TrimSpace(options.Credentials.PrivateKeyID),
	}
	if credentials.ClientEmail == "" || credentials.PrivateKey == "" || credentials.PrivateKeyID == "" {
		return nil, fmt.Errorf("%w: service account credential is incomplete", bizga4.ErrNotConfigured)
	}
	signer, err := parsePrivateKey(credentials.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", bizga4.ErrNotConfigured, err.Error())
	}

	apiBase, err := parseEndpoint(options.APIBase, defaultAPIBase)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", bizga4.ErrNotConfigured, err.Error())
	}
	tokenURL, err := parseEndpoint(options.TokenURL, defaultTokenURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", bizga4.ErrNotConfigured, err.Error())
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	baseClient := options.HTTPClient
	if baseClient == nil {
		baseClient = &http.Client{}
	}
	controlledClient := *baseClient
	controlledClient.Timeout = timeout
	// 出站请求不跟随重定向：token 端点与数据端点都不应有跳转，
	// 跟随会让 Bearer 令牌被发往第三方地址。
	controlledClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &Client{
		propertyID:  propertyID,
		apiBase:     apiBase,
		tokenURL:    tokenURL,
		credentials: credentials,
		signer:      signer,
		httpClient:  &controlledClient,
		now:         now,
	}, nil
}

// NewFromEnvironment 读取进程环境并装配客户端，供组合根一行调用。
func NewFromEnvironment() (*Client, error) {
	environment := FromEnvironment()
	if !environment.Configured() {
		return nil, fmt.Errorf("%w: GA4 is not configured", bizga4.ErrNotConfigured)
	}
	credentials, err := LoadCredentials(environment.CredentialSource)
	if err != nil {
		return nil, err
	}
	return New(Options{PropertyID: environment.PropertyID, Credentials: credentials})
}

// LoadCredentials 复刻旧实现 getServiceAccountCredentials 的取值规则：
// 以 .json 结尾、或以 / 或 ./ 开头当作文件路径，其余当作内联 JSON。
func LoadCredentials(source string) (Credentials, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return Credentials{}, fmt.Errorf("%w: service account credential is missing", bizga4.ErrNotConfigured)
	}

	raw := []byte(source)
	if strings.HasSuffix(source, ".json") || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "./") {
		path := source
		if !filepath.IsAbs(path) {
			workingDirectory, err := os.Getwd()
			if err != nil {
				return Credentials{}, fmt.Errorf("%w: %s", bizga4.ErrNotConfigured, err.Error())
			}
			path = filepath.Join(workingDirectory, path)
		}
		file, err := os.Open(path)
		if err != nil {
			return Credentials{}, fmt.Errorf("%w: cannot open service account file: %s", bizga4.ErrNotConfigured, err.Error())
		}
		defer file.Close()
		raw, err = io.ReadAll(io.LimitReader(file, maxCredentialsBytes))
		if err != nil {
			return Credentials{}, fmt.Errorf("%w: cannot read service account file: %s", bizga4.ErrNotConfigured, err.Error())
		}
	}

	var credentials Credentials
	if err := json.Unmarshal(raw, &credentials); err != nil {
		return Credentials{}, fmt.Errorf("%w: service account credential is not valid JSON", bizga4.ErrNotConfigured)
	}
	return credentials, nil
}

// RunReport 执行一次 :runReport。
//
// 与旧实现的一处刻意差异：日期缺省不再发生在这一层。
// 缺省区间是业务口径，已集中在 biz 层；适配器只做校验，
// 避免同一个默认值出现两个事实源。旧实现的两个调用路径都会先经过 biz 的补全，故行为不变。
func (c *Client) RunReport(ctx context.Context, request bizga4.ReportRequest) (bizga4.Report, error) {
	if c == nil {
		return bizga4.Report{}, bizga4.ErrNotConfigured
	}
	if strings.TrimSpace(request.StartDate) == "" || strings.TrimSpace(request.EndDate) == "" {
		return bizga4.Report{}, fmt.Errorf("%w: startDate and endDate are required", bizga4.ErrInvalidQuery)
	}
	limit := request.Limit
	if limit <= 0 {
		limit = bizga4.DefaultReportRowLimit
	}
	body := reportBody{
		DateRanges:      []dateRangeEntry{{StartDate: request.StartDate, EndDate: request.EndDate}},
		Dimensions:      namedEntries(request.Dimensions),
		Metrics:         namedEntries(request.Metrics),
		Limit:           limit,
		Offset:          request.Offset,
		OrderBys:        request.OrderBys,
		DimensionFilter: request.DimensionFilter,
		MetricFilter:    request.MetricFilter,
	}
	return c.execute(ctx, "runReport", body, request.Dimensions, request.Metrics)
}

// RunRealtimeReport 执行一次 :runRealtimeReport。
func (c *Client) RunRealtimeReport(ctx context.Context, request bizga4.ReportRequest) (bizga4.Report, error) {
	if c == nil {
		return bizga4.Report{}, bizga4.ErrNotConfigured
	}
	limit := request.Limit
	if limit <= 0 {
		limit = bizga4.DefaultReportRowLimit
	}
	body := realtimeBody{
		Dimensions:   namedEntries(request.Dimensions),
		Metrics:      namedEntries(request.Metrics),
		MinuteRanges: request.MinuteRanges,
		Limit:        limit,
	}
	return c.execute(ctx, "runRealtimeReport", body, request.Dimensions, request.Metrics)
}

// ---- 请求与响应结构 ----

type namedEntry struct {
	Name string `json:"name"`
}

type dateRangeEntry struct {
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
}

type reportBody struct {
	DateRanges      []dateRangeEntry `json:"dateRanges"`
	Dimensions      []namedEntry     `json:"dimensions"`
	Metrics         []namedEntry     `json:"metrics"`
	Limit           int              `json:"limit"`
	Offset          int              `json:"offset"`
	OrderBys        []bizga4.OrderBy `json:"orderBys,omitempty"`
	DimensionFilter json.RawMessage  `json:"dimensionFilter,omitempty"`
	MetricFilter    json.RawMessage  `json:"metricFilter,omitempty"`
}

type realtimeBody struct {
	Dimensions   []namedEntry         `json:"dimensions"`
	Metrics      []namedEntry         `json:"metrics"`
	MinuteRanges []bizga4.MinuteRange `json:"minuteRanges"`
	Limit        int                  `json:"limit"`
}

type reportResponse struct {
	Rows []struct {
		DimensionValues []struct {
			Value string `json:"value"`
		} `json:"dimensionValues"`
		MetricValues []struct {
			Value string `json:"value"`
		} `json:"metricValues"`
	} `json:"rows"`
	// GA4 把 rowCount 序列化成字符串。这里用 any 接收再归一化成整数：
	// 前端的 GA4Report.rowCount 声明为 number，旧实现因 `||` 短路返回的却是字符串。
	RowCount         any                  `json:"rowCount"`
	DimensionHeaders *[]namedEntry        `json:"dimensionHeaders"`
	MetricHeaders    *[]metricHeaderEntry `json:"metricHeaders"`
}

type metricHeaderEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type tokenResponse struct {
	AccessToken string  `json:"access_token"`
	ExpiresIn   float64 `json:"expires_in"`
}

// ---- 执行 ----

func namedEntries(names []string) []namedEntry {
	entries := make([]namedEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, namedEntry{Name: name})
	}
	return entries
}

func (c *Client) execute(ctx context.Context, endpoint string, body any, dimensions, metrics []string) (bizga4.Report, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return bizga4.Report{}, fmt.Errorf("%w: cannot encode report body", bizga4.ErrInvalidQuery)
	}
	response, err := c.call(ctx, endpoint, payload)
	if err != nil {
		return bizga4.Report{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// 数据端点回 401 只可能是令牌被拒 —— 那属于凭据问题，不是「上游故障」。
		if response.StatusCode == http.StatusUnauthorized {
			return bizga4.Report{}, fmt.Errorf("%w: %s", bizga4.ErrCredentialRejected, errorSnippet(response))
		}
		return bizga4.Report{}, upstreamError(response)
	}

	decoder := json.NewDecoder(io.LimitReader(response.Body, maxReportBytes))
	var decoded reportResponse
	if err := decoder.Decode(&decoded); err != nil {
		return bizga4.Report{}, fmt.Errorf("%w: cannot decode report response: %s", bizga4.ErrUpstream, err.Error())
	}
	return transform(decoded, dimensions, metrics), nil
}

// call 组装一次带 Bearer 令牌的 POST，并在 401 时强制重取一次令牌。
// Google 会在令牌被提前吊销时返回 401，此时缓存里的令牌还没到期。
func (c *Client) call(ctx context.Context, endpoint string, payload []byte) (*http.Response, error) {
	token, err := c.accessToken(ctx, false)
	if err != nil {
		return nil, err
	}
	response, err := c.post(ctx, endpoint, payload, token)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusUnauthorized {
		return response, nil
	}
	// 只在令牌「应当仍是有效」的情况下才值得重试一次，避免把持续的 401 变成两倍出站流量。
	response.Body.Close()
	token, err = c.accessToken(ctx, true)
	if err != nil {
		return nil, err
	}
	return c.post(ctx, endpoint, payload, token)
}

func (c *Client) post(ctx context.Context, endpoint string, payload []byte, token string) (*http.Response, error) {
	target := *c.apiBase
	// 属性 ID 由环境给出，与旧实现一样直接拼接：它只可能是一串属性编号。
	target.Path = strings.TrimRight(target.Path, "/") + "/properties/" + c.propertyID + ":" + endpoint
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build report request", bizga4.ErrUpstream)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", bizga4.ErrUpstream, err.Error())
	}
	return response, nil
}

// accessToken 返回可用的 access token。force 为真时无条件重取。
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	if !force {
		if token, ok := c.cachedToken(); ok {
			return token, nil
		}
	}
	// 串行化刷新，避免并发报表各自签一份断言、打同一端点。
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if !force {
		if token, ok := c.cachedToken(); ok {
			return token, nil
		}
	}
	return c.refreshToken(ctx)
}

func (c *Client) cachedToken() (string, bool) {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	if c.token == "" || !c.now().Before(c.tokenExpiry) {
		return "", false
	}
	return c.token, true
}

func (c *Client) refreshToken(ctx context.Context) (string, error) {
	assertion, err := c.signedAssertion()
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: cannot build token request", bizga4.ErrUpstream)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: %s", bizga4.ErrUpstream, err.Error())
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w: %s", bizga4.ErrCredentialRejected, errorSnippet(response))
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", upstreamError(response)
	}

	decoder := json.NewDecoder(io.LimitReader(response.Body, maxTokenBytes))
	var token tokenResponse
	if err := decoder.Decode(&token); err != nil || strings.TrimSpace(token.AccessToken) == "" {
		return "", fmt.Errorf("%w: token endpoint returned no access token", bizga4.ErrCredentialRejected)
	}

	// 余量固定 60 秒，与旧实现一致；expires_in 缺失时余量会把到期时刻推到过去，
	// 于是下一份报表自然重取，不会用到可能已失效的令牌。
	expiry := c.now().Add(time.Duration(token.ExpiresIn)*time.Second - tokenSafetySlack)
	c.tokenMu.Lock()
	c.token = token.AccessToken
	c.tokenExpiry = expiry
	c.tokenMu.Unlock()
	return token.AccessToken, nil
}

// signedAssertion 签出 RS256 的服务账号断言。
//
// 字段顺序刻意与旧实现一致（header: alg/typ/kid，payload: iss/sub/aud/iat/exp/scope），
// 这样断言在字节层面与旧实现可对照。
func (c *Client) signedAssertion() (string, error) {
	issuedAt := c.now().Unix()
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}{Algorithm: "RS256", Type: "JWT", KeyID: c.credentials.PrivateKeyID})
	if err != nil {
		return "", fmt.Errorf("%w: cannot encode assertion header", bizga4.ErrNotConfigured)
	}
	claims, err := json.Marshal(struct {
		Issuer   string `json:"iss"`
		Subject  string `json:"sub"`
		Audience string `json:"aud"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
		Scope    string `json:"scope"`
	}{
		Issuer:   c.credentials.ClientEmail,
		Subject:  c.credentials.ClientEmail,
		Audience: c.tokenURL.String(),
		IssuedAt: issuedAt,
		Expires:  issuedAt + int64(assertionTTL/time.Second),
		Scope:    analyticsScope,
	})
	if err != nil {
		return "", fmt.Errorf("%w: cannot encode assertion claims", bizga4.ErrNotConfigured)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.signer, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("%w: cannot sign assertion", bizga4.ErrNotConfigured)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// transform 复刻 transformReportResponse：
// 维度直接取字符串，度量按 parseFloat 语义归一化，取不到的字段留空。
func transform(decoded reportResponse, dimensions, metrics []string) bizga4.Report {
	rows := make([]map[string]any, 0, len(decoded.Rows))
	for _, row := range decoded.Rows {
		result := make(map[string]any, len(dimensions)+len(metrics))
		for index, value := range row.DimensionValues {
			if index >= len(dimensions) {
				break
			}
			result[dimensions[index]] = value.Value
		}
		for index, value := range row.MetricValues {
			if index >= len(metrics) {
				break
			}
			result[metrics[index]] = coerceMetric(value.Value)
		}
		rows = append(rows, result)
	}

	metadata := bizga4.ReportMetadata{Dimensions: make([]string, 0, len(dimensions)), Metrics: make([]bizga4.MetricMetadata, 0, len(metrics))}
	if decoded.DimensionHeaders != nil {
		for _, header := range *decoded.DimensionHeaders {
			metadata.Dimensions = append(metadata.Dimensions, header.Name)
		}
	} else {
		metadata.Dimensions = append(metadata.Dimensions, dimensions...)
	}
	if decoded.MetricHeaders != nil {
		for _, header := range *decoded.MetricHeaders {
			metadata.Metrics = append(metadata.Metrics, bizga4.MetricMetadata{Name: header.Name, Type: header.Type})
		}
	} else {
		for _, name := range metrics {
			metadata.Metrics = append(metadata.Metrics, bizga4.MetricMetadata{Name: name})
		}
	}

	return bizga4.Report{
		Rows:     rows,
		RowCount: coerceRowCount(decoded.RowCount, len(rows)),
		Metadata: metadata,
	}
}

// coerceRowCount 把上游的 rowCount 归一化成整数。
//
// 旧实现是 `data.rowCount || rows.length`，而 GA4 返回的是字符串，
// 于是前端拿到的是 "1234" 而不是 1234 —— 与它自己声明的 `rowCount: number` 相矛盾。
// 这里按声明契约输出整数。
func coerceRowCount(value any, fallback int) int {
	switch raw := value.(type) {
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			return parsed
		}
	case float64:
		return int(raw)
	}
	return fallback
}

// coerceMetric 与 biz 层同名的助手保持同一语义：解析失败保留原始字符串。
// NaN / ±Inf 必须回退，否则 json.Marshal 会因非有限浮点数直接报错。
func coerceMetric(value string) any {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return value
	}
	return parsed
}

func upstreamError(response *http.Response) error {
	return &bizga4.UpstreamError{Status: response.StatusCode, Body: errorSnippet(response)}
}

// errorSnippet 读取有上限的错误体，供上游错误分类与运维定位使用。
func errorSnippet(response *http.Response) string {
	if response == nil || response.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// parseEndpoint 校验一个出站端点：必须有 scheme 与 host，且不得携带
// userinfo / query / fragment —— 这些都会让「发往哪个地址」变得不透明。
func parseEndpoint(raw, fallback string) (*url.URL, error) {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		normalized = fallback
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil, errors.New("invalid GA4 endpoint url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("invalid GA4 endpoint scheme")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("invalid GA4 endpoint url")
	}
	parsed.Path = strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.RawPath = ""
	return parsed, nil
}

// parsePrivateKey 接受 PKCS#8 与 PKCS#1 两种 PEM 编码的 RSA 私钥。
// Google 服务账号签发的 key 是 PKCS#8（-----BEGIN PRIVATE KEY-----）。
func parsePrivateKey(raw string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, errors.New("service account private key is not PEM encoded")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return key, nil
		}
		return nil, errors.New("service account private key is not RSA")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("service account private key is not a supported RSA key")
	}
	return key, nil
}
