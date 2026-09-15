package catalogcontract

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

func TestReplayConditionalUsesBaselineETagForBothUpstreams(t *testing.T) {
	const etag = `"catalog-etag"`
	baselineRequests := make(chan observedRequest, 2)
	baseline := newConditionalReplayServer(t, baselineRequests, conditionalReplayServerConfig{
		initialETags:      []string{etag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   etag,
	})
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 2)
	candidate := newConditionalReplayServer(t, candidateRequests, conditionalReplayServerConfig{
		initialETags:      []string{etag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   etag,
	})
	defer candidate.Close()

	requestCase := RequestCase{
		Name:     "homepage-content-etag",
		Path:     "/api/homepage/content/item",
		RawPath:  "/api/homepage/content%2Fitem",
		RawQuery: "category=all&limit=24",
		Header: http.Header{
			"X-Request-Id": {"catalog-contract-homepage-content"},
		},
	}

	if err := ReplayConditional(context.Background(), baseline.Client(), baseline.URL, candidate.URL, requestCase); err != nil {
		t.Fatalf("ReplayConditional() error = %v, want nil", err)
	}
	if got := requestCase.Header.Get("If-None-Match"); got != "" {
		t.Fatalf("requestCase If-None-Match = %q, want empty", got)
	}

	for _, upstream := range []struct {
		name     string
		requests <-chan observedRequest
	}{
		{name: "baseline", requests: baselineRequests},
		{name: "candidate", requests: candidateRequests},
	} {
		assertConditionalObservedRequest(t, upstream.name+" initial", <-upstream.requests, requestCase, "")
		assertConditionalObservedRequest(t, upstream.name+" conditional", <-upstream.requests, requestCase, etag)
	}
}

func TestReplayConditionalStripsAllCallerConditionalHeaders(t *testing.T) {
	const baselineETag = `"catalog-etag"`
	baselineRequests := make(chan observedRequest, 2)
	baseline := newConditionalReplayServer(t, baselineRequests, conditionalReplayServerConfig{
		initialETags:      []string{baselineETag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   baselineETag,
	})
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 2)
	candidate := newConditionalReplayServer(t, candidateRequests, conditionalReplayServerConfig{
		initialETags:      []string{baselineETag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   baselineETag,
	})
	defer candidate.Close()

	requestCase := RequestCase{
		Name:     "caller-conditional-headers",
		Path:     "/api/homepage/content/item",
		RawPath:  "/api/homepage/content%2Fitem",
		RawQuery: "category=all&limit=24",
		Header: http.Header{
			"If-Match":            {`"caller-match"`},
			"if-none-match":       {`"caller-none-match"`},
			"If-Modified-Since":   {"Mon, 01 Sep 2026 08:00:00 GMT"},
			"If-Unmodified-Since": {"Mon, 01 Sep 2026 09:00:00 GMT"},
			"If-Range":            {`"caller-range"`},
			"X-Request-Id":        {"catalog-contract-conditional-headers"},
		},
	}

	if err := ReplayConditional(context.Background(), baseline.Client(), baseline.URL, candidate.URL, requestCase); err != nil {
		t.Fatalf("ReplayConditional() error = %v, want nil", err)
	}

	for _, upstream := range []struct {
		name     string
		requests <-chan observedRequest
	}{
		{name: "baseline", requests: baselineRequests},
		{name: "candidate", requests: candidateRequests},
	} {
		initialRequest := <-upstream.requests
		conditionalRequest := <-upstream.requests
		assertConditionalObservedRequest(t, upstream.name+" initial", initialRequest, requestCase, "")
		assertConditionalObservedRequest(t, upstream.name+" conditional", conditionalRequest, requestCase, baselineETag)
		assertReplayConditionHeaders(t, upstream.name+" initial", initialRequest.header, "")
		assertReplayConditionHeaders(t, upstream.name+" conditional", conditionalRequest.header, baselineETag)
	}
}

func TestReplayConditionalRejectsInvalidBaselineETagWithoutLeakingValues(t *testing.T) {
	const authorizationValue = "Bearer authorization-secret"
	const cookieValue = "session=cookie-secret"
	const proxyAuthorizationValue = "Basic proxy-secret"
	const privateBody = `{"success":true,"data":"baseline-private-value"}`
	testCases := []struct {
		name          string
		baselineETags []string
	}{
		{name: "missing"},
		{name: "duplicate", baselineETags: []string{`"catalog-a"`, `"catalog-b"`}},
		{name: "empty", baselineETags: []string{""}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := newConditionalReplayServer(t, nil, conditionalReplayServerConfig{
				initialETags: testCase.baselineETags,
				initialBody:  privateBody,
			})
			defer baseline.Close()
			candidate := newConditionalReplayServer(t, nil, conditionalReplayServerConfig{
				initialETags: []string{`"catalog-etag"`},
				initialBody:  privateBody,
			})
			defer candidate.Close()

			err := ReplayConditional(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
				Name: "invalid-baseline-etag",
				Path: "/api/homepage/content",
				Header: http.Header{
					"Authorization":       {authorizationValue},
					"Cookie":              {cookieValue},
					"Proxy-Authorization": {proxyAuthorizationValue},
				},
			})
			if err == nil {
				t.Fatal("ReplayConditional() error = nil, want invalid baseline ETag")
			}
			if !strings.Contains(err.Error(), "invalid-baseline-etag") || !strings.Contains(err.Error(), "baseline") {
				t.Fatalf("ReplayConditional() error = %q, want baseline phase", err)
			}
			for _, value := range []string{authorizationValue, cookieValue, proxyAuthorizationValue, privateBody, "catalog-a", "catalog-b"} {
				if strings.Contains(err.Error(), value) {
					t.Fatalf("ReplayConditional() error exposed private value: %q", err)
				}
			}
		})
	}
}

func TestReplayConditionalRejectsCandidateConditionalSuccess(t *testing.T) {
	const etag = `"catalog-etag"`
	baseline := newConditionalReplayServer(t, nil, conditionalReplayServerConfig{
		initialETags:      []string{etag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   etag,
	})
	defer baseline.Close()
	candidate := newConditionalReplayServer(t, nil, conditionalReplayServerConfig{
		initialETags:      []string{etag},
		conditionalStatus: http.StatusOK,
		conditionalETag:   etag,
	})
	defer candidate.Close()

	assertConditionalReplayError(t, ReplayConditional(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name: "candidate-ignores-condition",
		Path: "/api/homepage/content",
	}), "candidate-ignores-condition", "conditional response must be 304")
}

func Test条件回放拒绝双方均返回200(t *testing.T) {
	const etag = `"baseline-private-etag"`
	baseline := newConditionalReplayServer(t, nil, conditionalReplayServerConfig{
		initialETags:      []string{etag},
		conditionalStatus: http.StatusOK,
		conditionalETag:   etag,
	})
	defer baseline.Close()
	candidate := newConditionalReplayServer(t, nil, conditionalReplayServerConfig{
		initialETags:      []string{etag},
		conditionalStatus: http.StatusOK,
		conditionalETag:   etag,
	})
	defer candidate.Close()

	err := ReplayConditional(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name:          "both-ignore-condition",
		Path:          "/api/homepage/content",
		CacheContract: CacheContract{ETagPolicy: ETagRequired},
	})
	if err == nil {
		t.Fatal("条件回放在双方第二阶段均返回 200 时必须失败")
	}
	if !strings.Contains(err.Error(), "conditional compare") || !strings.Contains(err.Error(), "conditional response must be 304") {
		t.Fatalf("条件回放错误必须标识第二阶段 304 差异：%q", err)
	}
	if strings.Contains(err.Error(), etag) || strings.Contains(err.Error(), `{"success":true,"data":[]}`) {
		t.Fatalf("条件回放错误不得泄露 ETag 或响应正文：%q", err)
	}
}

func Test条件回放拒绝首次ETag不一致且不发送第二阶段请求(t *testing.T) {
	const baselineETag = `"baseline-private-etag"`
	const candidateETag = `"candidate-private-etag"`
	baselineRequests := make(chan observedRequest, 2)
	baseline := newConditionalReplayServer(t, baselineRequests, conditionalReplayServerConfig{
		initialETags:      []string{baselineETag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   baselineETag,
	})
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 2)
	candidate := newConditionalReplayServer(t, candidateRequests, conditionalReplayServerConfig{
		initialETags:      []string{candidateETag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   baselineETag,
	})
	defer candidate.Close()

	err := ReplayConditional(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name:          "initial-etag-mismatch",
		Path:          "/api/homepage/content",
		CacheContract: CacheContract{ETagPolicy: ETagRequired},
	})
	if err == nil {
		t.Fatal("首次 ETag 不一致时条件回放必须失败")
	}
	if !strings.Contains(err.Error(), "initial compare") || !strings.Contains(err.Error(), "initial ETag differs") {
		t.Fatalf("条件回放错误必须标识首次 ETag 差异：%q", err)
	}
	if strings.Contains(err.Error(), baselineETag) || strings.Contains(err.Error(), candidateETag) || strings.Contains(err.Error(), `{"success":true,"data":[]}`) {
		t.Fatalf("条件回放错误不得泄露 ETag 或响应正文：%q", err)
	}

	for _, upstream := range []struct {
		name     string
		requests <-chan observedRequest
	}{
		{name: "baseline", requests: baselineRequests},
		{name: "candidate", requests: candidateRequests},
	} {
		if count := len(upstream.requests); count != 1 {
			t.Fatalf("%s 上游请求数为 %d，首次 ETag 不一致时只能发送一次初始请求", upstream.name, count)
		}
		request := <-upstream.requests
		if values := request.header.Values("If-None-Match"); len(values) != 0 {
			t.Fatalf("%s 初始请求不得携带 If-None-Match：%q", upstream.name, values)
		}
	}
}

func TestReplayConditionalRejectsCandidateConditionalETagMismatchWithoutLeakingIt(t *testing.T) {
	const baselineETag = `"catalog-etag"`
	const candidateETag = `"candidate-private-etag"`
	baseline := newConditionalReplayServer(t, nil, conditionalReplayServerConfig{
		initialETags:      []string{baselineETag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   baselineETag,
	})
	defer baseline.Close()
	candidate := newConditionalReplayServer(t, nil, conditionalReplayServerConfig{
		initialETags:      []string{baselineETag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   candidateETag,
	})
	defer candidate.Close()

	err := ReplayConditional(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name: "candidate-etag-mismatch",
		Path: "/api/homepage/content",
	})
	assertConditionalReplayError(t, err, "candidate-etag-mismatch", "304 ETag differs")
	if strings.Contains(err.Error(), baselineETag) || strings.Contains(err.Error(), candidateETag) {
		t.Fatalf("ReplayConditional() error exposed ETag: %q", err)
	}
}

func TestReplayConditionalRejectsCandidateConditionalBodyWithoutLeakingIt(t *testing.T) {
	const etag = `"catalog-etag"`
	const privateBody = "candidate-private-304-body"
	client := &http.Client{Transport: replayRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		conditional := request.Header.Get("If-None-Match") != ""
		requestID := request.Header.Get("X-Request-Id")
		switch request.URL.Host {
		case "baseline.test":
			if conditional {
				return newConditionalHTTPResponse(http.StatusNotModified, etag, "", requestID), nil
			}
			return newConditionalHTTPResponse(http.StatusOK, etag, `{"success":true,"data":[]}`, requestID), nil
		case "candidate.test":
			if conditional {
				return newConditionalHTTPResponse(http.StatusNotModified, etag, privateBody, requestID), nil
			}
			return newConditionalHTTPResponse(http.StatusOK, etag, `{"success":true,"data":[]}`, requestID), nil
		default:
			t.Fatalf("unexpected replay host %q", request.URL.Host)
			return nil, nil
		}
	})}

	err := ReplayConditional(context.Background(), client, "http://baseline.test", "http://candidate.test", RequestCase{
		Name:   "candidate-304-body",
		Path:   "/api/homepage/content",
		Header: http.Header{"X-Request-Id": {"candidate-304-body-request"}},
	})
	assertConditionalReplayError(t, err, "candidate-304-body", "304 response body differs")
	if strings.Contains(err.Error(), privateBody) {
		t.Fatalf("ReplayConditional() error exposed 304 response body: %q", err)
	}
}

func TestReplayConditionalStripsSensitiveHeadersAndStartsWithoutIfNoneMatch(t *testing.T) {
	const etag = `"catalog-etag"`
	const authorizationValue = "Bearer authorization-secret"
	const cookieValue = "session=cookie-secret"
	const proxyAuthorizationValue = "Basic proxy-secret"
	const callerETag = `"caller-etag"`
	baselineRequests := make(chan observedRequest, 2)
	baseline := newConditionalReplayServer(t, baselineRequests, conditionalReplayServerConfig{
		initialETags:      []string{etag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   etag,
	})
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 2)
	candidate := newConditionalReplayServer(t, candidateRequests, conditionalReplayServerConfig{
		initialETags:      []string{etag},
		conditionalStatus: http.StatusNotModified,
		conditionalETag:   etag,
	})
	defer candidate.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	baselineURL := mustParseURL(t, baseline.URL)
	candidateURL := mustParseURL(t, candidate.URL)
	jar.SetCookies(baselineURL, []*http.Cookie{{Name: "baseline_jar_cookie", Value: "test"}})
	jar.SetCookies(candidateURL, []*http.Cookie{{Name: "candidate_jar_cookie", Value: "test"}})
	client := &http.Client{Jar: jar}
	requestCase := RequestCase{
		Name:     "conditional-anonymous-headers",
		Path:     "/api/homepage/content/item",
		RawPath:  "/api/homepage/content%2Fitem",
		RawQuery: "category=all&limit=24",
		Header: http.Header{
			"Authorization":       {authorizationValue},
			"Cookie":              {cookieValue},
			"Proxy-Authorization": {proxyAuthorizationValue},
			"X-Request-Id":        {"catalog-contract-anonymous"},
			"if-none-match":       {callerETag},
		},
	}

	if err := ReplayConditional(context.Background(), client, baseline.URL, candidate.URL, requestCase); err != nil {
		t.Fatalf("ReplayConditional() error = %v, want nil", err)
	}
	if client.Jar != jar {
		t.Fatal("ReplayConditional() modified the caller HTTP client cookie jar")
	}
	if got := requestCase.Header["if-none-match"]; len(got) != 1 || got[0] != callerETag {
		t.Fatalf("requestCase If-None-Match mutated to %q", got)
	}

	for _, upstream := range []struct {
		name     string
		requests <-chan observedRequest
	}{
		{name: "baseline", requests: baselineRequests},
		{name: "candidate", requests: candidateRequests},
	} {
		initialRequest := <-upstream.requests
		conditionalRequest := <-upstream.requests
		assertConditionalObservedRequest(t, upstream.name+" initial", initialRequest, requestCase, "")
		assertConditionalObservedRequest(t, upstream.name+" conditional", conditionalRequest, requestCase, etag)
		for requestNumber, request := range []observedRequest{initialRequest, conditionalRequest} {
			for _, headerName := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
				if got := request.header.Get(headerName); got != "" {
					t.Fatalf("%s request %d %s = %q, want empty", upstream.name, requestNumber+1, headerName, got)
				}
			}
		}
	}
}
