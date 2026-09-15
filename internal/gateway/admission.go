package gateway

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const growthPingInspectionLimit = len(growthPingResponse) + 1

func newAdmissionClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 准入响应需保留 Node 实际返回的编码形态；客户端自动解压会改变后续兼容性判断。
	transport.DisableCompression = true
	return &http.Client{
		Transport: transport,
		// Client.Timeout 会在尚未收到完整响应时取消底层请求；异常最终统一映射为
		// 既有的 UPSTREAM_TIMEOUT envelope，避免把 Go transport 错误泄露给前端。
		Timeout: timeout,
		// Node 的重定向是对外契约的一部分，网关只能原样中继，不能在内部悄然跟随。
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// serveAfterNodeAdmission 在引入极窄本地金丝雀期间仍由 Node 负责鉴权、限流和
// 策略判定。只有匹配冻结兼容性证明的响应才会本地处理，其余响应原样转发，以保持
// Node 对外可观测行为不变。
func (g *Gateway) serveAfterNodeAdmission(w http.ResponseWriter, r *http.Request, route exactLocalRoute, requestID string) {
	response, err := g.requestNodeAdmission(r)
	if err != nil {
		writeUpstreamFailure(w, r, err)
		return
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" {
		relayNodeAdmissionResponse(w, response, nil, requestID)
		return
	}

	prefix, err := readGrowthPingInspectionPrefix(response.Body)
	if err != nil {
		writeUpstreamFailure(w, r, err)
		return
	}

	if !bytes.Equal(prefix, []byte(growthPingResponse)) {
		relayNodeAdmissionResponse(w, response, prefix, requestID)
		return
	}

	copyResponseHeaders(w.Header(), response.Header, true)
	w.Header().Set("X-Request-Id", requestID)
	route.handler(w, r)
}

// readGrowthPingInspectionPrefix 只读取足以区分冻结响应、较长响应和不同响应的字节，
// 避免为兼容性判断无界读取上游流。
func readGrowthPingInspectionPrefix(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, int64(growthPingInspectionLimit)))
}

func relayNodeAdmissionResponse(w http.ResponseWriter, response *http.Response, prefix []byte, requestID string) {
	copyResponseHeaders(w.Header(), response.Header, false)
	// 即便由 Node 返回限流、重定向或其他回退响应，入口关联 ID 仍由网关统一持有，
	// 以避免同一请求因金丝雀开关状态不同而出现不可关联的外部标识。
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(response.StatusCode)
	if len(prefix) > 0 {
		_, _ = w.Write(prefix)
	}
	_, _ = io.Copy(w, response.Body)
}

func (g *Gateway) requestNodeAdmission(incoming *http.Request) (*http.Response, error) {
	target := admissionURL(g.defaultUpstream, incoming.URL)
	request, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, target.String(), incoming.Body)
	if err != nil {
		return nil, err
	}
	request.Header = incoming.Header.Clone()
	removeHopByHopHeaders(request.Header)
	// NewSingleHostReverseProxy 会保留入站 Host。准入请求必须使用相同 authority，
	// 这样 Node 才会按正常生产策略评估该请求。
	request.Host = incoming.Host
	request.ContentLength = incoming.ContentLength
	request.TransferEncoding = append([]string(nil), incoming.TransferEncoding...)
	request.GetBody = incoming.GetBody
	appendForwardedClientIP(request.Header, incoming.RemoteAddr)
	if _, present := request.Header["User-Agent"]; !present {
		request.Header.Set("User-Agent", "")
	}
	return g.admissionClient.Do(request)
}

func admissionURL(upstream, incoming *url.URL) *url.URL {
	target := cloneURL(upstream)
	target.Path = singleJoiningSlash(target.Path, incoming.Path)
	target.RawPath = ""
	target.RawQuery = joinRawQuery(target.RawQuery, incoming.RawQuery)
	target.ForceQuery = incoming.ForceQuery
	return target
}

func joinRawQuery(upstreamQuery, incomingQuery string) string {
	if upstreamQuery == "" || incomingQuery == "" {
		return upstreamQuery + incomingQuery
	}
	return upstreamQuery + "&" + incomingQuery
}

func singleJoiningSlash(left, right string) string {
	leftHasSlash := strings.HasSuffix(left, "/")
	rightHasSlash := strings.HasPrefix(right, "/")
	switch {
	case leftHasSlash && rightHasSlash:
		return left + right[1:]
	case !leftHasSlash && !rightHasSlash:
		return left + "/" + right
	default:
		return left + right
	}
}

func appendForwardedClientIP(headers http.Header, remoteAddress string) {
	clientIP, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return
	}
	previous, present := headers["X-Forwarded-For"]
	if present && previous == nil {
		return
	}
	if len(previous) > 0 {
		headers.Set("X-Forwarded-For", strings.Join(previous, ", ")+", "+clientIP)
		return
	}
	headers.Set("X-Forwarded-For", clientIP)
}
