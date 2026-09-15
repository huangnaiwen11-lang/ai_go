package catalogcontract

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// RequestCase 描述要向两个 Catalog 上游回放的一次匿名 GET 请求。Path 必须为绝对路径；
// RawPath 可选，但提供时必须是 Path 的转义表示。RawQuery 不经解析或重新编码地复制。
type RequestCase struct {
	Name          string
	Path          string
	RawPath       string
	RawQuery      string
	Header        http.Header
	CacheContract CacheContract
}

// ETagPolicy 定义普通成功响应的 ETag 是否可选、禁止或必须提供。
type ETagPolicy uint8

const (
	ETagOptional ETagPolicy = iota
	ETagForbidden
	ETagRequired
)

// CacheContract 约束单个匿名回放用例公开可见的缓存头，不比较独立缓存的命中状态值。
type CacheContract struct {
	RequireCacheStatus bool
	ETagPolicy         ETagPolicy
}

func buildReplayURL(rawBaseURL string, requestCase RequestCase) (*url.URL, error) {
	baseURL, err := parseUpstreamURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	if err := validateRequestCase(requestCase); err != nil {
		return nil, err
	}

	rawPath := appendURLPath(baseURL.EscapedPath(), requestCase.escapedPath())
	path, err := url.PathUnescape(rawPath)
	if err != nil || path == "" {
		return nil, fmt.Errorf("invalid request path")
	}

	target := *baseURL
	target.Path = path
	target.RawPath = rawPath
	decodedRawPath, err := url.PathUnescape(target.RawPath)
	if err != nil || decodedRawPath != target.Path {
		return nil, fmt.Errorf("invalid request path")
	}
	target.RawQuery = requestCase.RawQuery
	target.ForceQuery = false
	target.Fragment = ""

	return &target, nil
}

func parseUpstreamURL(raw string) (*url.URL, error) {
	upstream, err := url.Parse(raw)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("invalid upstream URL")
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("invalid upstream URL")
	}
	if upstream.User != nil {
		return nil, fmt.Errorf("invalid upstream URL")
	}

	return upstream, nil
}

func validateRequestCase(requestCase RequestCase) error {
	if requestCase.Name == "" || !strings.HasPrefix(requestCase.Path, "/") {
		return fmt.Errorf("invalid request case")
	}
	if requestCase.RawPath == "" {
		return nil
	}
	decodedPath, err := url.PathUnescape(requestCase.RawPath)
	if err != nil || decodedPath != requestCase.Path || !strings.HasPrefix(requestCase.RawPath, "/") {
		return fmt.Errorf("invalid request case")
	}

	return nil
}

func (requestCase RequestCase) escapedPath() string {
	if requestCase.RawPath != "" {
		return requestCase.RawPath
	}
	return (&url.URL{Path: requestCase.Path}).EscapedPath()
}

func appendURLPath(basePath, requestPath string) string {
	if basePath == "" {
		return requestPath
	}
	return basePath + requestPath
}

func replayResponse(ctx context.Context, client *http.Client, target *url.URL, header http.Header) (Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Response{}, err
	}
	request.Header = anonymousHeaders(header)

	response, err := client.Do(request)
	if err != nil {
		return Response{}, err
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return Response{}, fmt.Errorf("response read failed")
	}

	return Response{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
		Body:       body,
	}, nil
}

// anonymousHeaders 移除能关联用户或代理身份的头；契约回放只允许匿名公开请求，
// 不能因调用方上下文而改变响应或泄露会话。
func anonymousHeaders(header http.Header) http.Header {
	filtered := header.Clone()
	for name := range filtered {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Proxy-Authorization") {
			delete(filtered, name)
		}
	}
	return filtered
}
