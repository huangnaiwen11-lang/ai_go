package contentreview

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bizreview "ai-business-service/internal/biz/contentreview"
)

// 审核出站请求只能携带提示词审核事实，不能将用户、资金或生成投递信息扩散给审核服务。
func TestClientReviewSendsOnlyPromptFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/chat/completions" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range [][]byte{
			[]byte("diamond"), []byte("balance"), []byte("vip"), []byte("userId"), []byte("callback"),
		} {
			if bytes.Contains(bytes.ToLower(body), bytes.ToLower(forbidden)) {
				t.Fatalf("审核请求包含禁止字段 %q: %s", forbidden, body)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"{\"allowed\":true}"}}]}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "local-test-review-key", "grok-3-fast", time.Second, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	request, err := bizreview.NewRequest("creation-1", "step-1", "safe portrait", "")
	if err != nil {
		t.Fatal(err)
	}

	decision, err := client.Review(context.Background(), request)
	if err != nil || decision.Outcome != bizreview.OutcomeAllowed {
		t.Fatalf("Review() = %#v, %v", decision, err)
	}
}

// 供应商异常不是内容拒绝，调用方需要据此冲正而不能没收用户权益。
func TestClientReviewClassifiesMalformedResponseAsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "local-test-review-key", "grok-3-fast", time.Second, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	request, err := bizreview.NewRequest("creation-1", "step-1", "safe portrait", "")
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Review(context.Background(), request)
	if err == nil || !errors.Is(err, bizreview.ErrUnavailable) {
		t.Fatalf("Review() error = %v, want ErrUnavailable", err)
	}
}
