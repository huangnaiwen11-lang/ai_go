package safefetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultMaxBytes 是基线 §7.1 的默认字节上限（100 MiB）。
	DefaultMaxBytes int64 = 100 << 20
	// DefaultMaxRedirects 是基线 §7.1 的重定向上限。
	DefaultMaxRedirects = 3
	// maxRedirectsHardClamp 是基线的硬上限：调用方给再大也只到 5。
	maxRedirectsHardClamp = 5
	// DefaultTimeout 是单次下载的总时长上限（含全部重定向跳与响应体读取）。
	//
	// 基线没有规定这一项。它是一个**资源兜底**而不是策略值：100 MiB 的响应在
	// 5 分钟内读完只需要约 0.3 MiB/s，任何可用链路都够；而没有上限意味着一个
	// 不发送数据的对端能把 goroutine 永久挂住。需要更长时限的调用方显式调高。
	DefaultTimeout = 5 * time.Minute
	// maxURLLength 与基线「必须有 host、不得含 hash」的前置条件配合，防止
	// 超长 URL 把内存与日志拖垮。
	maxURLLength = 8192
)

var (
	// ErrInvalidTarget 表示目标 URL 不满足基线的形状要求。
	ErrInvalidTarget = errors.New("safefetch: invalid download target")
	// ErrUnsupportedScheme 表示协议不是 http/https。
	ErrUnsupportedScheme = errors.New("safefetch: unsupported scheme")
	// ErrNonPublicAddress 表示目标解析出的地址里有非公网地址。
	//
	// 只要**任一**解析结果非公网就返回它：只看第一个会让 DNS rebinding 有可乘之机。
	ErrNonPublicAddress = errors.New("safefetch: target resolves to a non-public address")
	// ErrTooManyRedirects 表示重定向超过上限。
	ErrTooManyRedirects = errors.New("safefetch: too many redirects")
	// ErrResponseTooLarge 表示响应体超过字节上限。
	ErrResponseTooLarge = errors.New("safefetch: response exceeds the byte limit")
	// ErrUnexpectedStatus 表示响应状态不是 2xx。具体状态码见 *StatusError。
	ErrUnexpectedStatus = errors.New("safefetch: unexpected response status")
	// ErrUnsupportedEncoding 表示响应带 Content-Encoding 而传输层没有解开它。
	//
	// 这条是**必须有**的：原项目（axios）默认请求 gzip 并由 axios 透明解压，
	// Go 的 http.Transport 只对 gzip 做同样的事。任何没被解开的编码（br / zstd /
	// 对端自作主张的 gzip）如果放行，调用方会把压缩字节当成图片或视频存进对象
	// 存储 —— 那是静默的数据损坏，比直接失败危险得多。
	ErrUnsupportedEncoding = errors.New("safefetch: response uses an unsupported content encoding")
	// ErrDependenciesUnavailable 表示下载器未被完整装配。
	ErrDependenciesUnavailable = errors.New("safefetch: dependencies unavailable")
	// ErrHostNotAllowed means a provider result URL (or one of its redirects)
	// escaped the account-scoped result host policy. This is distinct from a
	// public-network check: a public but unrelated host is still not a valid
	// provider result source.
	ErrHostNotAllowed = errors.New("safefetch: target host is not allowlisted")
	// ErrInvalidAllowedHosts means a caller tried to install an ambiguous host
	// policy. An invalid policy must not silently become an unrestricted fetch.
	ErrInvalidAllowedHosts = errors.New("safefetch: invalid allowed host policy")
)

// sensitiveHeaders 在**跨 origin** 重定向时被剥离（基线 §7.1）。
//
// Go 的 http.Client 只会自动剥离 Authorization/Cookie 一类，`x-admin-key` /
// `x-api-key` / `x-signature` 不在它的名单里。显式剥离把这条不变量从「依赖
// 标准库的实现细节」变成「我们自己保证」。
var sensitiveHeaders = []string{
	"authorization", "cookie", "proxy-authorization", "x-admin-key", "x-api-key", "x-signature",
}

// forbiddenHeaders 永远不出现在出站请求上（基线 §7.1）。
var forbiddenHeaders = []string{
	"connection", "content-length", "host", "proxy-connection", "transfer-encoding",
}

// Resolver 抽象 DNS 解析，使「任一解析结果非公网即拒绝」这条规则可被受控验证。
// 生产实现是 *net.Resolver。
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options 是下载器的受控参数。零值即基线默认值。
type Options struct {
	// MaxBytes 是响应体上限；<= 0 取 DefaultMaxBytes。
	MaxBytes int64
	// MaxRedirects 是重定向上限。
	//
	// 编码方式与 Node 侧不同（那边「显式传 0」= 不允许重定向、「不传」= 默认 3，
	// Go 的零值无法区分这两者）。这里的约定是：**0 取默认 3**，**负数表示不允许
	// 任何重定向**。上限一律硬 clamp 到 0..5，与基线一致。
	MaxRedirects int
	// Timeout 是单次下载的总时长；<= 0 取 DefaultTimeout。
	Timeout time.Duration
	// Resolver 默认 net.DefaultResolver。
	Resolver Resolver
	// UserAgent 会作为出站 UA 发送；为空则用 Go 的默认 UA。
	UserAgent string
	// AllowedHosts is an optional exact hostname allowlist. When set, both the
	// initial result URL and every redirect must belong to it. It accepts only
	// DNS hostnames (no wildcard, URL, port or IP literal) and is normalized
	// case-insensitively.
	AllowedHosts []string
}

// Response 是一次成功的受控下载。
// Body 必须由调用方关闭；读取超过上限时会返回 ErrResponseTooLarge。
type Response struct {
	StatusCode int
	Header     http.Header
	FinalURL   string
	Body       io.ReadCloser
}

// StatusError 携带非 2xx 的状态码。它只暴露状态码：响应体属于未受信内容，
// 不进错误信息，避免被写进日志后二次利用。
type StatusError struct{ StatusCode int }

func (err *StatusError) Error() string {
	return fmt.Sprintf("safefetch: unexpected status %d", err.StatusCode)
}

func (err *StatusError) Is(target error) bool { return target == ErrUnexpectedStatus }

// IsPolicyError 报告错误是否由「目标 URL 本身不符合基线 §7.1」产生。
//
// 这类错误与瞬时网络状况无关：同一个 URL 再试一次，结论不会变。调用方据此把
// 「这个结果没救了」与「等会儿再试」分开——分错的代价不对称，把策略拒绝当瞬时
// 故障会让一个坏 URL 无限重试。
//
// 非 2xx 状态码**不在**这里：5xx / 429 值得重试而 404 不值得，判定见
// RetryableStatus。传输层错误（DNS 超时、连接被拒）同样不算策略错误。
func IsPolicyError(err error) bool {
	return errors.Is(err, ErrInvalidTarget) ||
		errors.Is(err, ErrUnsupportedScheme) ||
		errors.Is(err, ErrNonPublicAddress) ||
		errors.Is(err, ErrUnsupportedEncoding) ||
		errors.Is(err, ErrTooManyRedirects) ||
		errors.Is(err, ErrResponseTooLarge) ||
		errors.Is(err, ErrHostNotAllowed) ||
		errors.Is(err, ErrInvalidAllowedHosts)
}

// RetryableStatus 报告一个非 2xx 状态码是否值得重试。
//
// 408 / 429 / 5xx 视为可重试，其余 4xx 视为确定性拒绝。
func RetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// Downloader 是按基线 §7.1 收敛过的出站下载器。
//
// 它不做任何缓存、重试或内容类型判断：那些属于调用方的语义（例如「结果必须是
// 图片或视频」），放进这里会让一个安全组件开始替业务做决定。
type Downloader struct {
	maxBytes     int64
	maxRedirects int
	resolver     Resolver
	userAgent    string
	allowedHosts map[string]struct{}
	client       *http.Client

	// dial 是拨号接缝，仅供同包测试注入受控连接。
	//
	// 它**不绕过地址校验**：解析与公网判定在 dialContext 里完成，dial 只负责
	// 把已经验过的地址连起来。这样测试能验证整条链路，而生产路径没有开关。
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewDownloader 构造下载器。它不发起任何网络请求。
func NewDownloader(options Options) (*Downloader, error) {
	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	maxRedirects := options.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = DefaultMaxRedirects
	}
	if maxRedirects < 0 {
		maxRedirects = 0
	}
	if maxRedirects > maxRedirectsHardClamp {
		maxRedirects = maxRedirectsHardClamp
	}
	allowedHosts, err := normalizeAllowedHosts(options.AllowedHosts)
	if err != nil {
		return nil, err
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	resolver := options.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	downloader := &Downloader{
		maxBytes: maxBytes, maxRedirects: maxRedirects, resolver: resolver, userAgent: options.UserAgent, allowedHosts: allowedHosts,
	}
	transport := &http.Transport{
		// 显式关闭代理：环境里的 HTTP_PROXY 不能把出站流量改道（基线 §7.1 的
		// `proxy: false`）。
		Proxy: nil,
		// 每一跳都重新解析并重新判定公网性；地址在拨号前就已验过。
		DialContext:     downloader.dialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		// 刻意**不**设 DisableCompression，也刻意不手工写 Accept-Encoding。
		// 让 http.Transport 自己请求 gzip 并透明解压，与 axios 的行为一致；
		// 于是字节上限统计的是解压后的字节，也就是调用方真正拿到的字节。
		// 没被解开的编码由 Fetch 显式拒绝，见 ErrUnsupportedEncoding。
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		MaxIdleConns:           4,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        30 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
	}
	downloader.client = &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: downloader.checkRedirect,
	}
	return downloader, nil
}

// CloseIdleConnections 释放空闲连接；下载器本身可继续复用。
func (downloader *Downloader) CloseIdleConnections() {
	if downloader != nil && downloader.client != nil {
		if transport, ok := downloader.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

// Fetch 下载一个 URL 并返回受控响应。
//
// 非 2xx 返回 *StatusError，不返回可读的响应体：调用方拿不到未受信内容，
// 也就不会误把它当成结果。
func (downloader *Downloader) Fetch(ctx context.Context, rawURL string) (*Response, error) {
	if downloader == nil || downloader.client == nil || downloader.resolver == nil {
		return nil, ErrDependenciesUnavailable
	}
	target, err := parseTarget(rawURL)
	if err != nil {
		return nil, err
	}
	if err := downloader.requireAllowedHost(target); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, ErrInvalidTarget
	}
	if downloader.userAgent != "" {
		request.Header.Set("User-Agent", downloader.userAgent)
	}
	sanitizeHeaders(request.Header, false)
	response, err := downloader.client.Do(request)
	if err != nil {
		// CheckRedirect 的哨兵错误会被包进 *url.Error；errors.Is 仍能命中。
		if IsPolicyError(err) {
			return nil, err
		}
		return nil, fmt.Errorf("safefetch: fetch %s: %w", target.Host, err)
	}
	// 状态先判：重定向（无 Location 时 Go 会把它当最终响应交回来）不该被当成
	// 「响应体超限」，两者的处置方向不同。
	if response.StatusCode < 200 || response.StatusCode > 299 {
		_ = response.Body.Close()
		return nil, &StatusError{StatusCode: response.StatusCode}
	}
	// 声明值先判一次：能在读第一个字节之前就拒掉明显超限的响应。
	if response.ContentLength > downloader.maxBytes {
		_ = response.Body.Close()
		return nil, ErrResponseTooLarge
	}
	// 只接受「没有编码」或「传输层已解开」的响应：见 ErrUnsupportedEncoding。
	switch strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding"))) {
	case "", "identity":
	default:
		_ = response.Body.Close()
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedEncoding, response.Header.Get("Content-Encoding"))
	}
	finalURL := target.String()
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	return &Response{
		StatusCode: response.StatusCode, Header: response.Header, FinalURL: finalURL,
		Body: &limitedBody{reader: response.Body, remaining: downloader.maxBytes},
	}, nil
}

// FetchBytes 下载并读完全部字节，读到上限即失败。它总是关闭响应体。
//
// 给「结果必须整体校验」的调用方使用（例如要按 sha256 核对不可变写入）。
func (downloader *Downloader) FetchBytes(ctx context.Context, rawURL string) ([]byte, string, error) {
	response, err := downloader.Fetch(ctx, rawURL)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, "", err
	}
	return content, response.Header.Get("Content-Type"), nil
}

// checkRedirect 逐跳重做完整校验，并在跨 origin 时剥离敏感头。
//
// 上限判定刻意写成 `maxRedirects == 0 || len(via) > maxRedirects`，而不是
// `len(via) >= maxRedirects`。Go 在**发下一跳之前**调这个函数，此时 via 只包含
// 已发出的请求：`len(via) >= n` 实际只允许跟随 n-1 次重定向，比基线少一次；
// 而 n == 0 时 `0 >= 0` 恰好成立，语义又碰巧正确。分开写才两条都对。
func (downloader *Downloader) checkRedirect(request *http.Request, via []*http.Request) error {
	if downloader.maxRedirects == 0 || len(via) > downloader.maxRedirects {
		return ErrTooManyRedirects
	}
	if request == nil || request.URL == nil {
		return ErrInvalidTarget
	}
	if err := validateTargetURL(request.URL); err != nil {
		return err
	}
	if err := downloader.requireAllowedHost(request.URL); err != nil {
		return err
	}
	if len(via) == 0 {
		return nil
	}
	// 与 Node 侧 `sanitizeHeaders(..., { stripSensitive })` 一致：跨 origin 时
	// 敏感头与非转发头一起清掉，同 origin 时只清非转发头。
	sanitizeHeaders(request.Header, !sameOrigin(via[len(via)-1].URL, request.URL))
	return nil
}

// dialContext 解析目标主机，确认**全部**解析结果都是公网地址后才拨号。
//
// 顺序是安全性的一部分：先解析、再判定、最后才连接。反过来的话，一次
// DNS rebinding 就能让判定看到公网答案、连接落到内网地址上。
func (downloader *Downloader) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrInvalidTarget
	}
	addresses, err := downloader.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("safefetch: resolve %s: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, ErrNonPublicAddress
	}
	for _, addr := range addresses {
		if !IsPublicAddress(addr) {
			return nil, fmt.Errorf("%w: %s -> %s", ErrNonPublicAddress, host, addr)
		}
	}
	dial := downloader.dial
	if dial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	var lastErr error
	for _, addr := range addresses {
		connection, dialErr := dial(ctx, network, net.JoinHostPort(addr.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("safefetch: dial %s: %w", host, lastErr)
}

// parseTarget 解析并校验字符串形式的 URL。
func parseTarget(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxURLLength || !utf8.ValidString(raw) ||
		raw != strings.TrimSpace(raw) || strings.Contains(raw, "#") {
		return nil, ErrInvalidTarget
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, ErrInvalidTarget
	}
	if err := validateTargetURL(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// validateTargetURL 是形状校验的唯一实现：首次请求与每一跳重定向都走它。
//
// IP 字面量一律拒绝，IPv4 与 IPv6 一视同仁。基线 §7.1 把这件事写成了两条：
// 「IP 字面量必须通过公网地址校验」与「形如 `^https?://\d+\.\d+\.\d+\.\d+[:/]`
// 的 provider 结果 URL 一律拒绝从后端抓取」。本包在 Cling 里只服务于 provider
// 结果抓取（结果地址由平台给出，正常形态永远是域名），没有任何调用方需要按 IP
// 字面量抓取，因此把两条合并为最严的一条：**不区分是否公网，裸 IP 一律拒绝**。
//
// 顺带覆盖了 IPv6 字面量 —— 基线那条正则只写了 IPv4，但裸 IP 的问题（无法用
// 证书证明「这就是对方说的那个主机」）对 IPv6 完全一样。
func validateTargetURL(parsed *url.URL) error {
	if parsed == nil || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.Hostname() == "" {
		// 含 username/password 的 URL 一律拒绝：凭据不该出现在结果地址里。
		return ErrInvalidTarget
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return ErrUnsupportedScheme
	}
	if isIPLiteral(parsed.Hostname()) {
		return ErrInvalidTarget
	}
	return nil
}

func isIPLiteral(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

func (downloader *Downloader) requireAllowedHost(target *url.URL) error {
	if downloader == nil || len(downloader.allowedHosts) == 0 {
		return nil
	}
	host, ok := canonicalAllowedHost(target.Hostname())
	if !ok {
		return ErrHostNotAllowed
	}
	if _, allowed := downloader.allowedHosts[host]; !allowed {
		return ErrHostNotAllowed
	}
	return nil
}

func normalizeAllowedHosts(input []string) (map[string]struct{}, error) {
	if len(input) == 0 {
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(input))
	for _, raw := range input {
		host, ok := canonicalAllowedHost(raw)
		if !ok {
			return nil, ErrInvalidAllowedHosts
		}
		allowed[host] = struct{}{}
	}
	return allowed, nil
}

func canonicalAllowedHost(raw string) (string, bool) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > 253 || strings.ContainsAny(raw, ":/@?#[\\]") {
		return "", false
	}
	host := strings.ToLower(strings.TrimSuffix(raw, "."))
	if host == "" || isIPLiteral(host) {
		return "", false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, char := range label {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' {
				continue
			}
			return "", false
		}
	}
	return host, true
}

// sanitizeHeaders 就地清掉非转发头，并按需清掉敏感头。
func sanitizeHeaders(header http.Header, stripSensitive bool) {
	for _, name := range forbiddenHeaders {
		header.Del(name)
	}
	if !stripSensitive {
		return
	}
	for _, name := range sensitiveHeaders {
		header.Del(name)
	}
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

// limitedBody 是流式计数的那一重限制：Content-Length 缺失或说谎时，它是
// 唯一能拦住超限响应的地方。
type limitedBody struct {
	reader    io.ReadCloser
	remaining int64
}

func (body *limitedBody) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if int64(len(buffer)) > body.remaining {
		buffer = buffer[:body.remaining]
	}
	if len(buffer) == 0 {
		// 配额刚好用尽：再读一个字节以区分「流正好结束」与「还有更多」。
		var probe [1]byte
		read, err := body.reader.Read(probe[:])
		if read > 0 {
			return 0, ErrResponseTooLarge
		}
		return 0, err
	}
	read, err := body.reader.Read(buffer)
	body.remaining -= int64(read)
	if body.remaining < 0 {
		return 0, ErrResponseTooLarge
	}
	return read, err
}

func (body *limitedBody) Close() error { return body.reader.Close() }

var _ Resolver = (*net.Resolver)(nil)
