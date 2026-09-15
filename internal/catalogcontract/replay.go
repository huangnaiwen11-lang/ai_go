package catalogcontract

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Replay 向两个上游发送 requestCase，再比较捕获的响应。它刻意保持只读：只发起 GET，
// 且不会在返回错误中泄露响应正文或请求头值。
func Replay(ctx context.Context, client *http.Client, baselineBaseURL, candidateBaseURL string, requestCase RequestCase) error {
	baseline, candidate, err := replayPair(ctx, client, baselineBaseURL, candidateBaseURL, requestCase)
	if err != nil {
		return err
	}
	if err := compareWithCacheContract(baseline, candidate, requestCase.CacheContract); err != nil {
		return replayError(requestCase.Name, "compare", err.Error())
	}

	return nil
}

// ReplayConditional 使用 baseline ETag 回放同一匿名请求，使两个上游都基于同一份
// 公开条件缓存表示接受评估。
func ReplayConditional(ctx context.Context, client *http.Client, baselineBaseURL, candidateBaseURL string, requestCase RequestCase) error {
	initialCase := unconditionalReplayCase(requestCase)
	baseline, candidate, err := replayPair(ctx, client, baselineBaseURL, candidateBaseURL, initialCase)
	if err != nil {
		return err
	}
	if err := compareWithCacheContract(baseline, candidate, requestCase.CacheContract); err != nil {
		return replayError(requestCase.Name, "initial compare", err.Error())
	}

	baselineETag, ok := singleHeaderValue(baseline.Header, "ETag")
	if !ok || baselineETag == "" {
		return replayError(requestCase.Name, "baseline", "ETag is missing or invalid")
	}
	candidateETag, ok := singleHeaderValue(candidate.Header, "ETag")
	if !ok || candidateETag == "" {
		return replayError(requestCase.Name, "candidate", "ETag is missing or invalid")
	}
	if baselineETag != candidateETag {
		return replayError(requestCase.Name, "initial compare", "initial ETag differs")
	}

	conditionalCase := initialCase
	conditionalCase.Header = initialCase.Header.Clone()
	conditionalCase.Header.Set("If-None-Match", baselineETag)

	baseline, candidate, err = replayPair(ctx, client, baselineBaseURL, candidateBaseURL, conditionalCase)
	if err != nil {
		return err
	}
	if baseline.StatusCode != http.StatusNotModified || candidate.StatusCode != http.StatusNotModified {
		return replayError(requestCase.Name, "conditional compare", "conditional response must be 304")
	}
	if err := compareWithCacheContract(baseline, candidate, requestCase.CacheContract); err != nil {
		return replayError(requestCase.Name, "conditional compare", err.Error())
	}

	return nil
}

// unconditionalReplayCase 从首次回放中移除调用方提供的所有 HTTP 条件，	只允许
// baseline 响应驱动第二阶段的 If-None-Match 请求。
func unconditionalReplayCase(requestCase RequestCase) RequestCase {
	replayCase := requestCase
	replayCase.Header = requestCase.Header.Clone()
	if replayCase.Header == nil {
		replayCase.Header = make(http.Header)
	}
	for name := range replayCase.Header {
		if isHTTPConditionalRequestHeader(name) {
			delete(replayCase.Header, name)
		}
	}

	return replayCase
}

func isHTTPConditionalRequestHeader(name string) bool {
	return strings.EqualFold(name, "If-Match") ||
		strings.EqualFold(name, "If-None-Match") ||
		strings.EqualFold(name, "If-Modified-Since") ||
		strings.EqualFold(name, "If-Unmodified-Since") ||
		strings.EqualFold(name, "If-Range")
}

func replayPair(ctx context.Context, client *http.Client, baselineBaseURL, candidateBaseURL string, requestCase RequestCase) (Response, Response, error) {
	if client == nil {
		return Response{}, Response{}, replayError(requestCase.Name, "baseline", "HTTP client is missing")
	}
	replayClient := *client
	replayClient.Jar = nil
	replayClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	baselineURL, err := buildReplayURL(baselineBaseURL, requestCase)
	if err != nil {
		return Response{}, Response{}, replayError(requestCase.Name, "baseline", "request URL is invalid")
	}
	baseline, err := replayResponse(ctx, &replayClient, baselineURL, requestCase.Header)
	if err != nil {
		return Response{}, Response{}, replayError(requestCase.Name, "baseline", "request failed")
	}

	candidateURL, err := buildReplayURL(candidateBaseURL, requestCase)
	if err != nil {
		return Response{}, Response{}, replayError(requestCase.Name, "candidate", "request URL is invalid")
	}
	candidate, err := replayResponse(ctx, &replayClient, candidateURL, requestCase.Header)
	if err != nil {
		return Response{}, Response{}, replayError(requestCase.Name, "candidate", "request failed")
	}

	return baseline, candidate, nil
}

func replayError(caseName, phase, detail string) error {
	return fmt.Errorf("catalog contract replay case %q %s: %s", caseName, phase, detail)
}
