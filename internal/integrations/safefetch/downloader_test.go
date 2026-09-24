package safefetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testHost = "media.example.com"

// publicIPv4 是一个确定落在所有非公网段之外、且不会真的被拨到的地址。
const publicIPv4 = "93.184.216.34"

// privateIPv4 命中 10.0.0.0/8。
const privateIPv4 = "10.1.2.3"

type stubResolver struct {
	answers map[string][]netip.Addr
	err     error
	calls   int32
}

func (resolver *stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	atomic.AddInt32(&resolver.calls, 1)
	if resolver.err != nil {
		return nil, resolver.err
	}
	addresses, ok := resolver.answers[host]
	if !ok {
		return nil, fmt.Errorf("stub resolver has no answer for %q", host)
	}
	return addresses, nil
}

func publicAnswer(t *testing.T, addresses ...string) []netip.Addr {
	t.Helper()
	resolved := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		parsed, err := netip.ParseAddr(address)
		if err != nil {
			t.Fatalf("ParseAddr(%q) error = %v", address, err)
		}
		resolved = append(resolved, parsed)
	}
	return resolved
}

// newServer 起一个测试服务端，并把它的错误日志丢掉：多个用例会刻意让客户端
// 提前断开，那些 "broken pipe" 噪音会淹没真正的失败信息。
func newServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	t.Cleanup(server.Close)
	return server
}

type harness struct {
	downloader *Downloader
	server     *httptest.Server
	resolver   *stubResolver
	dials      atomic.Int32
	// dialed 记录拨号时实际使用的 address（应为「已校验过的 IP:port」）。
	dialed []string
}

// newHarness 构造一个「域名解析被接管、拨号指向本地测试服务端」的下载器。
//
// 拨号接缝刻意不绕过地址校验：dialContext 先解析、再判定、最后才调 dial，
// 因此这里注入的 dial 只影响「连到哪台机器」，不影响「允不允许连」。
func newHarness(t *testing.T, handler http.Handler, options Options) *harness {
	t.Helper()
	server := newServer(t, handler)
	resolver := &stubResolver{answers: map[string][]netip.Addr{testHost: publicAnswer(t, publicIPv4)}}
	options.Resolver = resolver
	downloader, err := NewDownloader(options)
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	current := &harness{downloader: downloader, server: server, resolver: resolver}
	downloader.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		current.dials.Add(1)
		current.dialed = append(current.dialed, address)
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	return current
}

func (current *harness) url(path string) string {
	return "http://" + testHost + path
}

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(writer, body)
	})
}

// --- URL 形状 ---------------------------------------------------------------

func TestParseTarget接受正常域名(t *testing.T) {
	for _, raw := range []string{
		"https://media.example.com/result.png",
		"http://media.example.com:8080/a/b?c=1&d=2",
		"https://sub.deep.example.com./x",
	} {
		if _, err := parseTarget(raw); err != nil {
			t.Errorf("parseTarget(%q) error = %v, want nil", raw, err)
		}
	}
}

func TestParseTarget拒绝非HTTP协议(t *testing.T) {
	for _, raw := range []string{
		"ftp://media.example.com/a",
		"ws://media.example.com/a",
		"gopher://media.example.com/a",
	} {
		err := validateTargetURLOrNil(t, raw)
		if !errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("parseTarget(%q) error = %v, want ErrUnsupportedScheme", raw, err)
		}
	}
}

// validateTargetURLOrNil 只跑形状校验，不要求 URL 能被 http.NewRequest 接受。
func validateTargetURLOrNil(t *testing.T, raw string) error {
	t.Helper()
	_, err := parseTarget(raw)
	return err
}

func TestParseTarget拒绝凭据与片段(t *testing.T) {
	cases := map[string]string{
		"带用户名":      "https://user@media.example.com/a",
		"带用户名密码":    "https://user:secret@media.example.com/a",
		"带片段":       "https://media.example.com/a#frag",
		"只有井号":      "https://media.example.com/a#",
		"无主机":       "https:///a",
		"opaque 形式": "https:media.example.com/a",
		"空串":        "",
		"首尾空白":      " https://media.example.com/a ",
		"超长":        "https://media.example.com/" + strings.Repeat("a", maxURLLength),
		"非法UTF-8":   "https://media.example.com/\xff\xfe",
	}
	for name, raw := range cases {
		if _, err := parseTarget(raw); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("%s: parseTarget(%q) error = %v, want ErrInvalidTarget", name, raw, err)
		}
	}
}

// 这是本包相对基线的**收紧**：裸 IP 一律拒绝，不分公网私网，IPv4/IPv6 同等。
func TestParseTarget拒绝一切IP字面量(t *testing.T) {
	for _, raw := range []string{
		"http://93.184.216.34/a",           // 公网 IPv4
		"https://8.8.8.8/a",                // 公网 IPv4
		"http://10.1.2.3/a",                // 私网 IPv4
		"https://[2606:4700:4700::1111]/a", // 公网 IPv6
		"https://[::1]/a",                  // 回环 IPv6
		"http://127.0.0.1:8080/a",          // 回环 IPv4 + 端口
		"http://[::ffff:93.184.216.34]/a",  // IPv4-mapped
	} {
		if _, err := parseTarget(raw); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("parseTarget(%q) error = %v, want ErrInvalidTarget", raw, err)
		}
	}
}

// --- 公网地址判定 -----------------------------------------------------------

func TestIsPublicAddress接受公网地址(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "93.184.216.34", "1.1.1.1", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		addr := netip.MustParseAddr(raw)
		if !IsPublicAddress(addr) {
			t.Errorf("IsPublicAddress(%s) = false, want true", raw)
		}
	}
}

func TestIsPublicAddress拒绝IPv4映射地址(t *testing.T) {
	for _, raw := range []string{"::ffff:93.184.216.34", "::ffff:8.8.8.8"} {
		addr := netip.MustParseAddr(raw)
		if !addr.Is4In6() {
			t.Fatalf("测试前提不成立：%s 不是 IPv4-mapped", raw)
		}
		if IsPublicAddress(addr) {
			t.Errorf("IsPublicAddress(%s) = true, want false（IPv4-mapped 一律拒绝）", raw)
		}
	}
}

func TestIsPublicAddress拒绝无效地址(t *testing.T) {
	if IsPublicAddress(netip.Addr{}) {
		t.Error("IsPublicAddress(零值) = true, want false")
	}
}

// 逐项钉住基线 §7.1 的 32 段清单：任何一个前缀被漏掉，这里立刻变红。
func TestIsPublicAddress非公网清单逐项拒绝(t *testing.T) {
	if got, want := len(nonPublicPrefixes), 32; got != want {
		t.Fatalf("nonPublicPrefixes 条数 = %d, want %d（IPv4 15 + IPv6 17）", got, want)
	}
	for _, prefix := range nonPublicPrefixes {
		network := prefix.Masked().Addr()
		if IsPublicAddress(network) {
			t.Errorf("IsPublicAddress(%s) = true, want false（前缀 %s）", network, prefix)
		}
		if next := network.Next(); prefix.Contains(next) && IsPublicAddress(next) {
			t.Errorf("IsPublicAddress(%s) = true, want false（前缀 %s 内）", next, prefix)
		}
	}
}

// --- 下载主路径 -------------------------------------------------------------

func TestFetch正常下载(t *testing.T) {
	current := newHarness(t, okHandler("payload"), Options{})
	response, err := current.downloader.Fetch(context.Background(), current.url("/result.png"))
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(content) != "payload" {
		t.Errorf("body = %q, want payload", content)
	}
	if !strings.HasSuffix(response.FinalURL, "/result.png") {
		t.Errorf("FinalURL = %q, want 以 /result.png 结尾", response.FinalURL)
	}
}

func TestFetch拒绝初始非白名单Host且不拨号(t *testing.T) {
	current := newHarness(t, okHandler("payload"), Options{AllowedHosts: []string{"results.example.com"}})
	if _, err := current.downloader.Fetch(context.Background(), current.url("/result.png")); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("Fetch() error = %v, want ErrHostNotAllowed", err)
	}
	if got := current.dials.Load(); got != 0 {
		t.Fatalf("dial count = %d, want 0", got)
	}
}

func TestFetch拒绝重定向到非白名单Host(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Redirect(writer, &http.Request{}, "http://unlisted.example.com/result.png", http.StatusFound)
	}), Options{AllowedHosts: []string{testHost}})
	if _, err := current.downloader.Fetch(context.Background(), current.url("/redirect")); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("Fetch() error = %v, want ErrHostNotAllowed", err)
	}
	if got := current.dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want only the allowlisted first hop", got)
	}
}

// 拨号用的必须是**已校验过的解析结果**，而不是原始域名。
func TestFetch拨号使用已校验的解析地址(t *testing.T) {
	current := newHarness(t, okHandler("x"), Options{})
	response, err := current.downloader.Fetch(context.Background(), current.url("/a"))
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	_ = response.Body.Close()
	if len(current.dialed) != 1 {
		t.Fatalf("拨号次数 = %d, want 1", len(current.dialed))
	}
	if want := net.JoinHostPort(publicIPv4, "80"); current.dialed[0] != want {
		t.Errorf("拨号地址 = %q, want %q", current.dialed[0], want)
	}
}

func TestFetch非2xx返回StatusError且不返回响应体(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, "secret upstream detail")
	}), Options{})
	response, err := current.downloader.Fetch(context.Background(), current.url("/missing"))
	if response != nil {
		t.Fatalf("Fetch() response = %v, want nil（不得把未受信内容交给调用方）", response)
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("Fetch() error = %v, want *StatusError", err)
	}
	if statusErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", statusErr.StatusCode)
	}
	if !errors.Is(err, ErrUnexpectedStatus) {
		t.Error("errors.Is(err, ErrUnexpectedStatus) = false, want true")
	}
	if strings.Contains(err.Error(), "secret upstream detail") {
		t.Error("错误信息里出现了上游响应体内容")
	}
}

func TestFetch声明值超限直接拒绝(t *testing.T) {
	payload := make([]byte, 1<<20)
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = writer.Write(payload)
	}), Options{MaxBytes: 1024})
	if _, err := current.downloader.Fetch(context.Background(), current.url("/big")); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Fetch() error = %v, want ErrResponseTooLarge", err)
	}
}

func TestFetch流式计数拦截超限响应(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		// 先 flush 强制 chunked：这样 Content-Length 缺失，只有流式计数能拦住。
		writer.(http.Flusher).Flush()
		_, _ = writer.Write(make([]byte, 1000))
	}), Options{MaxBytes: 100})
	response, err := current.downloader.Fetch(context.Background(), current.url("/stream"))
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if _, err := io.ReadAll(response.Body); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("ReadAll() error = %v, want ErrResponseTooLarge", err)
	}
}

// 边界：正好等于上限必须放行，多一个字节必须拒绝。
func TestFetch字节上限边界(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"正好等于上限", 1024, false},
		{"多一个字节", 1025, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.(http.Flusher).Flush()
				_, _ = writer.Write(make([]byte, testCase.size))
			}), Options{MaxBytes: 1024})
			response, err := current.downloader.Fetch(context.Background(), current.url("/edge"))
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			content, err := io.ReadAll(response.Body)
			if testCase.wantErr {
				if !errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("ReadAll() error = %v, want ErrResponseTooLarge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadAll() error = %v, want nil", err)
			}
			if len(content) != testCase.size {
				t.Errorf("读取字节数 = %d, want %d", len(content), testCase.size)
			}
		})
	}
}

// --- 压缩 -------------------------------------------------------------------

// 传输层自己请求 gzip 并透明解压：上限必须按**解压后**的字节计。
func TestFetchgzip透明解压且按解压后字节计上限(t *testing.T) {
	plain := bytes.Repeat([]byte("a"), 4096)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(plain); err != nil {
		t.Fatalf("gzip write error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close error = %v", err)
	}
	if compressed.Len() >= len(plain) {
		t.Fatalf("测试前提不成立：压缩后 %d 字节未小于原始 %d 字节", compressed.Len(), len(plain))
	}

	t.Run("上限按解压后字节判定", func(t *testing.T) {
		current := newHarness(t, http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
			responseWriter.Header().Set("Content-Encoding", "gzip")
			responseWriter.Header().Set("Content-Length", fmt.Sprint(compressed.Len()))
			_, _ = responseWriter.Write(compressed.Bytes())
		}), Options{MaxBytes: 1024})
		response, err := current.downloader.Fetch(context.Background(), current.url("/gz"))
		if err != nil {
			t.Fatalf("Fetch() error = %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		if _, err := io.ReadAll(response.Body); !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("ReadAll() error = %v, want ErrResponseTooLarge（解压后 4096 > 1024）", err)
		}
	})

	t.Run("上限足够时返回解压后内容", func(t *testing.T) {
		current := newHarness(t, http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
			responseWriter.Header().Set("Content-Encoding", "gzip")
			responseWriter.Header().Set("Content-Length", fmt.Sprint(compressed.Len()))
			_, _ = responseWriter.Write(compressed.Bytes())
		}), Options{MaxBytes: 1 << 20})
		content, _, err := current.downloader.FetchBytes(context.Background(), current.url("/gz"))
		if err != nil {
			t.Fatalf("FetchBytes() error = %v", err)
		}
		if !bytes.Equal(content, plain) {
			t.Errorf("解压后字节数 = %d, want %d（必须是明文而不是压缩字节）", len(content), len(plain))
		}
	})
}

// 没被传输层解开的编码必须直接拒绝，而不是把压缩字节当结果交出去。
func TestFetch拒绝未解开的ContentEncoding(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(writer, "\x1b\x02\x80not-really-brotli")
	}), Options{})
	response, err := current.downloader.Fetch(context.Background(), current.url("/br"))
	if response != nil {
		_ = response.Body.Close()
		t.Fatal("Fetch() response != nil, want nil")
	}
	if !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("Fetch() error = %v, want ErrUnsupportedEncoding", err)
	}
	if !IsPolicyError(err) {
		t.Error("IsPolicyError(ErrUnsupportedEncoding) = false, want true")
	}
}

// --- 解析与公网判定 ---------------------------------------------------------

// 任一解析结果非公网即拒绝，且**一个字节都不发**。
func TestFetch任一解析结果非公网即拒绝且不拨号(t *testing.T) {
	current := newHarness(t, okHandler("x"), Options{})
	current.resolver.answers[testHost] = publicAnswer(t, publicIPv4, privateIPv4)
	response, err := current.downloader.Fetch(context.Background(), current.url("/a"))
	if response != nil {
		_ = response.Body.Close()
		t.Fatal("Fetch() response != nil, want nil")
	}
	if !errors.Is(err, ErrNonPublicAddress) {
		t.Fatalf("Fetch() error = %v, want ErrNonPublicAddress", err)
	}
	if got := current.dials.Load(); got != 0 {
		t.Errorf("拨号次数 = %d, want 0（判定必须先于连接）", got)
	}
}

func TestFetch解析结果为空即拒绝(t *testing.T) {
	current := newHarness(t, okHandler("x"), Options{})
	current.resolver.answers[testHost] = nil
	if _, err := current.downloader.Fetch(context.Background(), current.url("/a")); !errors.Is(err, ErrNonPublicAddress) {
		t.Fatalf("Fetch() error = %v, want ErrNonPublicAddress", err)
	}
}

func TestFetch解析失败不是策略错误(t *testing.T) {
	current := newHarness(t, okHandler("x"), Options{})
	current.resolver.err = errors.New("temporary DNS failure")
	_, err := current.downloader.Fetch(context.Background(), current.url("/a"))
	if err == nil {
		t.Fatal("Fetch() error = nil, want 解析错误")
	}
	if IsPolicyError(err) {
		t.Errorf("IsPolicyError(%v) = true, want false（DNS 抖动值得重试）", err)
	}
}

func TestFetch连接失败不是策略错误(t *testing.T) {
	current := newHarness(t, okHandler("x"), Options{})
	current.downloader.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	_, err := current.downloader.Fetch(context.Background(), current.url("/a"))
	if err == nil {
		t.Fatal("Fetch() error = nil, want 连接错误")
	}
	if IsPolicyError(err) {
		t.Errorf("IsPolicyError(%v) = true, want false", err)
	}
}

// --- 重定向 -----------------------------------------------------------------

func TestFetch重定向超过上限(t *testing.T) {
	var hits atomic.Int32
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		http.Redirect(writer, request, "/hop", http.StatusFound)
	}), Options{MaxRedirects: 2})
	_, err := current.downloader.Fetch(context.Background(), current.url("/start"))
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("Fetch() error = %v, want ErrTooManyRedirects", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("请求次数 = %d, want 3（1 次初始 + 2 次重定向）", got)
	}
}

func TestFetch负数重定向上限表示不跟随(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/hop", http.StatusFound)
	}), Options{MaxRedirects: -1})
	if _, err := current.downloader.Fetch(context.Background(), current.url("/start")); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("Fetch() error = %v, want ErrTooManyRedirects", err)
	}
}

func TestFetch重定向到裸IP即拒绝(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "http://93.184.216.34/steal")
		writer.WriteHeader(http.StatusFound)
	}), Options{})
	if _, err := current.downloader.Fetch(context.Background(), current.url("/start")); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("Fetch() error = %v, want ErrInvalidTarget", err)
	}
}

func TestFetch重定向到非公网解析结果即拒绝(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "http://evil.example.net/steal", http.StatusFound)
			return
		}
		_, _ = io.WriteString(writer, "should not be reachable")
	}), Options{})
	current.resolver.answers["evil.example.net"] = publicAnswer(t, privateIPv4)
	if _, err := current.downloader.Fetch(context.Background(), current.url("/start")); !errors.Is(err, ErrNonPublicAddress) {
		t.Fatalf("Fetch() error = %v, want ErrNonPublicAddress", err)
	}
}

// 跨 origin 重定向必须剥掉敏感头，且**每一跳都重新解析并拨号**。
//
// 两个域名映射到两个不同的公网 IP，dial 按 IP 分流到两个本地服务端：这样
// 「初始请求打到 A、重定向后的请求打到 B」才是可证的事实，而不是靠顺序猜。
func TestFetch跨origin重定向剥离敏感头(t *testing.T) {
	const redirectHost = testHost
	const targetHost = "cdn.example.net"
	const targetIPv4 = "93.184.216.35"

	var leaked atomic.Bool
	target := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/final" {
			t.Errorf("目标服务端收到了非预期的路径 %q", request.URL.Path)
		}
		for _, name := range sensitiveHeaders {
			if request.Header.Get(name) != "" {
				leaked.Store(true)
			}
		}
		_, _ = io.WriteString(writer, "ok")
	}))
	redirector := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", "http://"+targetHost+"/final")
		writer.WriteHeader(http.StatusFound)
	}))

	resolver := &stubResolver{answers: map[string][]netip.Addr{
		redirectHost: publicAnswer(t, publicIPv4),
		targetHost:   publicAnswer(t, targetIPv4),
	}}
	downloader, err := NewDownloader(Options{Resolver: resolver})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	downloader.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, splitErr
		}
		var upstream *httptest.Server
		switch host {
		case publicIPv4:
			upstream = redirector
		case targetIPv4:
			upstream = target
		default:
			return nil, fmt.Errorf("未预期的拨号目标 %q", address)
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, upstream.Listener.Addr().String())
	}

	// 显式把敏感头塞进出站请求：Fetch 自己不设置它们，但调用方将来可能传。
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+redirectHost+"/start", nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	request.Header.Set("X-Api-Key", "super-secret")
	request.Header.Set("Authorization", "Bearer super-secret")
	response, err := downloader.client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if leaked.Load() {
		t.Error("跨 origin 重定向后仍带上了敏感头")
	}
}

func TestCheckRedirect同origin保留敏感头但清掉禁止头(t *testing.T) {
	downloader, err := NewDownloader(Options{})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	previous, _ := http.NewRequest(http.MethodGet, "https://media.example.com/a", nil)
	next, _ := http.NewRequest(http.MethodGet, "https://media.example.com/b", nil)
	next.Header.Set("Authorization", "Bearer keep")
	next.Header.Set("Host", "evil")
	next.Header.Set("Transfer-Encoding", "chunked")
	if err := downloader.checkRedirect(next, []*http.Request{previous}); err != nil {
		t.Fatalf("checkRedirect() error = %v", err)
	}
	if next.Header.Get("Authorization") != "Bearer keep" {
		t.Error("同 origin 时 Authorization 被误删")
	}
	if next.Header.Get("Host") != "" || next.Header.Get("Transfer-Encoding") != "" {
		t.Error("禁止头没有被清掉")
	}
}

func TestCheckRedirect跨origin清掉敏感头(t *testing.T) {
	downloader, err := NewDownloader(Options{})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	previous, _ := http.NewRequest(http.MethodGet, "https://media.example.com/a", nil)
	next, _ := http.NewRequest(http.MethodGet, "https://cdn.example.net/b", nil)
	for _, name := range sensitiveHeaders {
		next.Header.Set(name, "secret")
	}
	if err := downloader.checkRedirect(next, []*http.Request{previous}); err != nil {
		t.Fatalf("checkRedirect() error = %v", err)
	}
	for _, name := range sensitiveHeaders {
		if next.Header.Get(name) != "" {
			t.Errorf("跨 origin 后 %s 未被剥离", name)
		}
	}
}

func TestCheckRedirect协议降级被拒(t *testing.T) {
	downloader, err := NewDownloader(Options{})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	previous, _ := http.NewRequest(http.MethodGet, "https://media.example.com/a", nil)
	next, _ := http.NewRequest(http.MethodGet, "ftp://media.example.com/b", nil)
	if err := downloader.checkRedirect(next, []*http.Request{previous}); !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("checkRedirect() error = %v, want ErrUnsupportedScheme", err)
	}
}

// --- 构造与参数 -------------------------------------------------------------

func TestNewDownloader参数收敛(t *testing.T) {
	zero, err := NewDownloader(Options{})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	if zero.maxBytes != DefaultMaxBytes {
		t.Errorf("maxBytes = %d, want %d", zero.maxBytes, DefaultMaxBytes)
	}
	if zero.maxRedirects != DefaultMaxRedirects {
		t.Errorf("maxRedirects = %d, want %d", zero.maxRedirects, DefaultMaxRedirects)
	}
	if zero.client.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", zero.client.Timeout, DefaultTimeout)
	}
	if zero.resolver == nil {
		t.Error("resolver 为 nil，应回退到 net.DefaultResolver")
	}

	clamped, err := NewDownloader(Options{MaxRedirects: 99, MaxBytes: -1})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	if clamped.maxRedirects != maxRedirectsHardClamp {
		t.Errorf("maxRedirects = %d, want 硬 clamp 到 %d", clamped.maxRedirects, maxRedirectsHardClamp)
	}
	if clamped.maxBytes != DefaultMaxBytes {
		t.Errorf("maxBytes = %d, want %d", clamped.maxBytes, DefaultMaxBytes)
	}

	// 代理必须显式关闭：环境变量不能把出站流量改道。
	transport, ok := zero.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport 不是 *http.Transport")
	}
	if transport.Proxy != nil {
		t.Error("Proxy != nil, want nil（基线 §7.1 的 proxy: false）")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Error("TLS 最低版本低于 1.2")
	}
}

func TestNilDownloader安全(t *testing.T) {
	var downloader *Downloader
	if _, err := downloader.Fetch(context.Background(), "https://media.example.com/a"); !errors.Is(err, ErrDependenciesUnavailable) {
		t.Fatalf("Fetch() error = %v, want ErrDependenciesUnavailable", err)
	}
	downloader.CloseIdleConnections()
}

// --- 错误分类 ---------------------------------------------------------------

func TestIsPolicyError分类(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"非法目标":     {ErrInvalidTarget, true},
		"协议不支持":    {ErrUnsupportedScheme, true},
		"非公网地址":    {ErrNonPublicAddress, true},
		"编码不支持":    {ErrUnsupportedEncoding, true},
		"重定向超限":    {ErrTooManyRedirects, true},
		"响应超限":     {ErrResponseTooLarge, true},
		"结果域未白名单":  {ErrHostNotAllowed, true},
		"白名单配置非法":  {ErrInvalidAllowedHosts, true},
		"包装后的非法目标": {fmt.Errorf("fetch: %w", ErrInvalidTarget), true},
		"状态码错误":    {&StatusError{StatusCode: 404}, false},
		"依赖未装配":    {ErrDependenciesUnavailable, false},
		"普通错误":     {errors.New("boom"), false},
	}
	for name, testCase := range cases {
		if got := IsPolicyError(testCase.err); got != testCase.want {
			t.Errorf("%s: IsPolicyError(%v) = %v, want %v", name, testCase.err, got, testCase.want)
		}
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		200: false, 301: false, 400: false, 404: false, 410: false,
		408: true, 429: true, 500: true, 502: true, 503: true, 504: true,
	}
	for status, want := range cases {
		if got := RetryableStatus(status); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

// --- TLS --------------------------------------------------------------------

// 生产路径的 TLS 必须走真实证书校验，且 SNI 用的是**域名**而不是解析出的 IP。
func TestFetchTLS校验域名而非IP(t *testing.T) {
	server := httptest.NewTLSServer(okHandler("tls-payload"))
	t.Cleanup(server.Close)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)

	// httptest 的自签证书签给 example.com；URL 域名必须与它一致才能证明
	// 「SNI/校验用的是域名」。这里用 example.com 作为域名，同时把它映射到一个
	// 公网地址，拨号指向本地 TLS 服务端。
	const host = "example.com"
	resolver := &stubResolver{answers: map[string][]netip.Addr{host: publicAnswer(t, publicIPv4)}}
	downloader, err := NewDownloader(Options{Resolver: resolver})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	transport := downloader.client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	downloader.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	response, err := downloader.Fetch(context.Background(), "https://"+host+"/a")
	if err != nil {
		t.Fatalf("Fetch() error = %v（证书校验应当用域名 example.com 并通过）", err)
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(content) != "tls-payload" {
		t.Errorf("body = %q, want tls-payload", content)
	}
}

func TestFetchTLS证书不匹配时失败(t *testing.T) {
	server := httptest.NewTLSServer(okHandler("x"))
	t.Cleanup(server.Close)
	resolver := &stubResolver{answers: map[string][]netip.Addr{testHost: publicAnswer(t, publicIPv4)}}
	downloader, err := NewDownloader(Options{Resolver: resolver})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	// 刻意不注入 RootCAs：自签证书必须导致失败。
	downloader.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	if _, err := downloader.Fetch(context.Background(), "https://"+testHost+"/a"); err == nil {
		t.Fatal("Fetch() error = nil, want 证书校验失败")
	}
}

// --- FetchBytes / 超时 ------------------------------------------------------

func TestFetchBytes返回内容与类型(t *testing.T) {
	current := newHarness(t, okHandler("bytes-payload"), Options{})
	content, contentType, err := current.downloader.FetchBytes(context.Background(), current.url("/a"))
	if err != nil {
		t.Fatalf("FetchBytes() error = %v", err)
	}
	if string(content) != "bytes-payload" {
		t.Errorf("content = %q, want bytes-payload", content)
	}
	if contentType != "image/png" {
		t.Errorf("contentType = %q, want image/png", contentType)
	}
}

func TestFetchBytes失败时返回错误(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}), Options{})
	if _, _, err := current.downloader.FetchBytes(context.Background(), current.url("/a")); !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("FetchBytes() error = %v, want ErrUnexpectedStatus", err)
	}
}

// 总时限必须兜住「对端不发数据」的情形：响应头迟迟不来时 Fetch 要失败，
// 而不是把 goroutine 永久挂住。
func TestFetch响应头迟迟不来时超时(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(5 * time.Second):
		}
		_, _ = io.WriteString(writer, "too late")
	}), Options{Timeout: 200 * time.Millisecond})
	started := time.Now()
	_, err := current.downloader.Fetch(context.Background(), current.url("/slow"))
	if err == nil {
		t.Fatal("Fetch() error = nil, want 超时错误")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("耗时 %v，超时没有生效", elapsed)
	}
	if IsPolicyError(err) {
		t.Errorf("IsPolicyError(%v) = true, want false（超时值得重试）", err)
	}
}

// 总时限同样覆盖响应体读取：头已到、体卡住时不能在 Fetch 里放行。
func TestFetch响应体卡住时读取出错(t *testing.T) {
	current := newHarness(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.(http.Flusher).Flush()
		select {
		case <-request.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}), Options{Timeout: 300 * time.Millisecond})
	response, err := current.downloader.Fetch(context.Background(), current.url("/stall"))
	if err != nil {
		t.Fatalf("Fetch() error = %v（响应头已发出，应能拿到 Response）", err)
	}
	defer func() { _ = response.Body.Close() }()
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("ReadAll() error = nil, want 超时错误")
	}
}
