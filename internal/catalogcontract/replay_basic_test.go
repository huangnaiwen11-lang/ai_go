package catalogcontract

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

func TestReplaySendsIdenticalRequestToBothUpstreams(t *testing.T) {
	baselineRequests := make(chan observedRequest, 1)
	baseline := newReplayServer(t, baselineRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer baseline.Close()

	candidateRequests := make(chan observedRequest, 1)
	candidate := newReplayServer(t, candidateRequests, http.StatusOK, `{"data":[],"success":true}`)
	defer candidate.Close()

	requestCase := RequestCase{
		Name:     "templates-api",
		Path:     "/api/homepage/video-templates",
		RawQuery: "category=all&limit=24",
		Header: http.Header{
			"X-Request-Id":      {"catalog-contract-templates-api"},
			"X-Client-Platform": {"web"},
		},
	}

	if err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, requestCase); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}

	assertObservedRequest(t, <-baselineRequests, requestCase)
	assertObservedRequest(t, <-candidateRequests, requestCase)
}

func TestReplayDoesNotExposeDifferingResponseBody(t *testing.T) {
	baseline := newReplayServer(t, nil, http.StatusOK, `{"success":true,"data":"baseline-private-value"}`)
	defer baseline.Close()
	candidate := newReplayServer(t, nil, http.StatusOK, `{"success":true,"data":"candidate-private-value"}`)
	defer candidate.Close()

	err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name: "body-diff",
		Path: "/api/homepage/video-templates",
	})
	if err == nil {
		t.Fatal("Replay() error = nil, want contract mismatch")
	}
	if !strings.Contains(err.Error(), "body-diff") || !strings.Contains(err.Error(), "compare") || !strings.Contains(err.Error(), "JSON body differs") {
		t.Fatalf("Replay() error = %q, want case name and comparison field", err)
	}
	if strings.Contains(err.Error(), "baseline-private-value") || strings.Contains(err.Error(), "candidate-private-value") {
		t.Fatalf("Replay() error exposed response body: %q", err)
	}
}

func TestReplayPreservesEscapedPathAndRawQuery(t *testing.T) {
	baselineRequests := make(chan observedRequest, 1)
	baseline := newReplayServer(t, baselineRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 1)
	candidate := newReplayServer(t, candidateRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer candidate.Close()

	requestCase := RequestCase{
		Name:     "escaped-path",
		Path:     "/api/homepage/video-templates/featured/item",
		RawPath:  "/api/homepage/video-templates/featured%2Fitem",
		RawQuery: "category=all&filter=kind%2Ffeatured",
	}
	if err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, requestCase); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}

	for upstream, request := range map[string]observedRequest{
		"baseline":  <-baselineRequests,
		"candidate": <-candidateRequests,
	} {
		if request.escapedPath != requestCase.RawPath {
			t.Fatalf("%s escaped path = %q, want %q", upstream, request.escapedPath, requestCase.RawPath)
		}
		if request.rawQuery != requestCase.RawQuery {
			t.Fatalf("%s raw query = %q, want %q", upstream, request.rawQuery, requestCase.RawQuery)
		}
	}
}

func TestReplayPreservesBaseAndCaseEscapedPaths(t *testing.T) {
	baselineRequests := make(chan observedRequest, 1)
	baseline := newReplayServer(t, baselineRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 1)
	candidate := newReplayServer(t, candidateRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer candidate.Close()

	requestCase := RequestCase{
		Name:     "base-and-case-escaped-path",
		Path:     "/child/item",
		RawPath:  "/child%2Fitem",
		RawQuery: "x=1",
	}
	if err := Replay(
		context.Background(),
		baseline.Client(),
		baseline.URL+"/base%2F",
		candidate.URL+"/base%2F",
		requestCase,
	); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}

	for _, upstream := range []struct {
		name    string
		request observedRequest
	}{
		{name: "baseline", request: <-baselineRequests},
		{name: "candidate", request: <-candidateRequests},
	} {
		if upstream.request.escapedPath != "/base%2F/child%2Fitem" {
			t.Fatalf("%s escaped path = %q, want %q", upstream.name, upstream.request.escapedPath, "/base%2F/child%2Fitem")
		}
		if upstream.request.path != "/base//child/item" {
			t.Fatalf("%s path = %q, want %q", upstream.name, upstream.request.path, "/base//child/item")
		}
		if upstream.request.rawQuery != "x=1" {
			t.Fatalf("%s raw query = %q, want %q", upstream.name, upstream.request.rawQuery, "x=1")
		}
	}
}

func TestReplayPreservesRepeatedTrailingSlashesInBasePath(t *testing.T) {
	baselineRequests := make(chan observedRequest, 1)
	baseline := newReplayServer(t, baselineRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 1)
	candidate := newReplayServer(t, candidateRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer candidate.Close()

	requestCase := RequestCase{Name: "repeated-base-slashes", Path: "/child"}
	if err := Replay(
		context.Background(),
		baseline.Client(),
		baseline.URL+"/base//",
		candidate.URL+"/base//",
		requestCase,
	); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}

	for _, upstream := range []struct {
		name    string
		request observedRequest
	}{
		{name: "baseline", request: <-baselineRequests},
		{name: "candidate", request: <-candidateRequests},
	} {
		if upstream.request.escapedPath != "/base///child" {
			t.Fatalf("%s escaped path = %q, want %q", upstream.name, upstream.request.escapedPath, "/base///child")
		}
		if upstream.request.path != "/base///child" {
			t.Fatalf("%s path = %q, want %q", upstream.name, upstream.request.path, "/base///child")
		}
	}
}

func TestReplayStripsSensitiveAnonymousHeaders(t *testing.T) {
	baselineRequests := make(chan observedRequest, 1)
	baseline := newReplayServer(t, baselineRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 1)
	candidate := newReplayServer(t, candidateRequests, http.StatusOK, `{"success":true,"data":["different"]}`)
	defer candidate.Close()

	const authorizationValue = "Bearer authorization-secret"
	const cookieValue = "session=cookie-secret"
	const proxyAuthorizationValue = "Basic proxy-secret"
	requestCase := RequestCase{
		Name: "anonymous-header-filter",
		Path: "/api/homepage/video-templates",
		Header: http.Header{
			"Authorization":       {authorizationValue},
			"Cookie":              {cookieValue},
			"Proxy-Authorization": {proxyAuthorizationValue},
			"X-Client-Platform":   {"web"},
			"X-Request-Id":        {"catalog-contract-anonymous-header-filter"},
		},
	}

	err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, requestCase)
	if err == nil {
		t.Fatal("Replay() error = nil, want contract mismatch")
	}
	for _, sensitiveValue := range []string{authorizationValue, cookieValue, proxyAuthorizationValue} {
		if strings.Contains(err.Error(), sensitiveValue) {
			t.Fatalf("Replay() error exposed sensitive header value: %q", err)
		}
	}

	for _, upstream := range []struct {
		name    string
		request observedRequest
	}{
		{name: "baseline", request: <-baselineRequests},
		{name: "candidate", request: <-candidateRequests},
	} {
		for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
			if got := upstream.request.header.Get(header); got != "" {
				t.Fatalf("%s %s = %q, want empty", upstream.name, header, got)
			}
		}
		if got := upstream.request.header.Get("X-Client-Platform"); got != "web" {
			t.Fatalf("%s X-Client-Platform = %q, want web", upstream.name, got)
		}
		if got := upstream.request.header.Get("X-Request-Id"); got != "catalog-contract-anonymous-header-filter" {
			t.Fatalf("%s X-Request-Id = %q, want stable request ID", upstream.name, got)
		}
	}
}

func TestReplayDisablesCallerCookieJar(t *testing.T) {
	baselineRequests := make(chan observedRequest, 1)
	baseline := newReplayServer(t, baselineRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 1)
	candidate := newReplayServer(t, candidateRequests, http.StatusOK, `{"success":true,"data":[]}`)
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

	if err := Replay(context.Background(), client, baseline.URL, candidate.URL, RequestCase{
		Name:   "cookie-jar-filter",
		Path:   "/api/homepage/video-templates",
		Header: make(http.Header),
	}); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}
	if client.Jar != jar {
		t.Fatal("Replay() modified the caller HTTP client cookie jar")
	}

	for _, upstream := range []struct {
		name    string
		request observedRequest
	}{
		{name: "baseline", request: <-baselineRequests},
		{name: "candidate", request: <-candidateRequests},
	} {
		if got := upstream.request.header.Get("Cookie"); got != "" {
			t.Fatalf("%s Cookie = %q, want empty", upstream.name, got)
		}
	}
}

func TestReplayComparesNonSuccessResponses(t *testing.T) {
	baseline := newReplayServer(t, nil, http.StatusOK, `{"success":false,"error":"not found"}`)
	defer baseline.Close()
	candidate := newReplayServer(t, nil, http.StatusNotFound, `{"success":false,"error":"not found"}`)
	defer candidate.Close()

	err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name: "non-success",
		Path: "/api/homepage/video-templates",
	})
	if err == nil {
		t.Fatal("Replay() error = nil, want HTTP status mismatch")
	}
	if !strings.Contains(err.Error(), "compare") || !strings.Contains(err.Error(), "HTTP status differs") {
		t.Fatalf("Replay() error = %q, want comparison of non-success response", err)
	}
}

func TestReplayComparesBaselineNonSuccessResponse(t *testing.T) {
	baseline := newReplayServer(t, nil, http.StatusServiceUnavailable, `{"success":false,"error":"unavailable"}`)
	defer baseline.Close()
	candidate := newReplayServer(t, nil, http.StatusOK, `{"success":false,"error":"unavailable"}`)
	defer candidate.Close()

	err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name: "baseline-non-success",
		Path: "/api/homepage/video-templates",
	})
	if err == nil {
		t.Fatal("Replay() error = nil, want HTTP status mismatch")
	}
	if !strings.Contains(err.Error(), "compare") || !strings.Contains(err.Error(), "HTTP status differs") {
		t.Fatalf("Replay() error = %q, want comparison of baseline non-success response", err)
	}
}

func TestReplayPreservesCallerIfNoneMatch(t *testing.T) {
	const etag = `"catalog-etag"`
	baselineRequests := make(chan observedRequest, 1)
	baseline := newReplayServer(t, baselineRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer baseline.Close()
	candidateRequests := make(chan observedRequest, 1)
	candidate := newReplayServer(t, candidateRequests, http.StatusOK, `{"success":true,"data":[]}`)
	defer candidate.Close()

	requestCase := RequestCase{
		Name: "ordinary-if-none-match",
		Path: "/api/homepage/content",
		Header: http.Header{
			"If-None-Match": {etag},
			"X-Request-Id":  {"catalog-contract-ordinary-condition"},
		},
	}

	if err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, requestCase); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}
	for _, upstream := range []struct {
		name    string
		request observedRequest
	}{
		{name: "baseline", request: <-baselineRequests},
		{name: "candidate", request: <-candidateRequests},
	} {
		if got := upstream.request.header.Values("If-None-Match"); strings.Join(got, ",") != etag {
			t.Fatalf("%s If-None-Match = %q, want %q", upstream.name, got, etag)
		}
	}
}

func TestReplayResponseClosesBodyAfterSuccessfulRead(t *testing.T) {
	body := &observedReadCloser{reader: strings.NewReader(`{"success":true,"data":[]}`)}
	client := &http.Client{Transport: replayRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       body,
		}, nil
	})}

	if _, err := replayResponse(context.Background(), client, mustParseURL(t, "http://replay.test/api/homepage/content"), nil); err != nil {
		t.Fatalf("replayResponse() error = %v, want nil", err)
	}
	if !body.closed {
		t.Fatal("replayResponse() did not close body after a successful read")
	}
}

func TestReplayResponseClosesBodyAfterReadFailure(t *testing.T) {
	body := &observedReadCloser{readErr: errors.New("read failed")}
	client := &http.Client{Transport: replayRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       body,
		}, nil
	})}

	if _, err := replayResponse(context.Background(), client, mustParseURL(t, "http://replay.test/api/homepage/content"), nil); err == nil {
		t.Fatal("replayResponse() error = nil, want read failure")
	}
	if !body.closed {
		t.Fatal("replayResponse() did not close body after a read failure")
	}
}
