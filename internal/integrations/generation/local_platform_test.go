package generation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalPlatformHandlerAcceptsSignedExecutionAndReturnsLookup(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	key := "local-generation-request-key-for-contract-test-32"
	platformHandler := NewLocalPlatformHandler(LocalPlatformOptions{
		RequestHMACKey: key,
		Now:            func() time.Time { return now },
		Callback:       false,
	})
	clientTransport := localRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		platformHandler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})

	client, err := NewClientWithCallbackOrigin("http://local.platform.test", key, "http://127.0.0.1:18000", &http.Client{Transport: clientTransport}, func() time.Time { return now }, func() string { return "local-platform-nonce" })
	if err != nil {
		t.Fatal(err)
	}
	execution, err := BuildExecution(Step{ID: "step-local-platform-1", Capability: CapabilityTextToImage, ModelSKU: "ps-image-v1", Input: Input{Prompt: "一张测试图片"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Submit(context.Background(), execution)
	if err != nil || result.JobID == "" || result.Status != "accepted" {
		t.Fatalf("Submit() = %#v, %v", result, err)
	}
	lookup, err := client.Lookup(context.Background(), execution.ExternalRef)
	if err != nil || lookup.JobID != result.JobID || lookup.Status != "accepted" {
		t.Fatalf("Lookup() = %#v, %v", lookup, err)
	}
}

func TestLocalPlatformHandlerRejectsUnsignedRequest(t *testing.T) {
	handler := NewLocalPlatformHandler(LocalPlatformOptions{RequestHMACKey: "local-generation-request-key-for-contract-test-32"})
	request, _ := http.NewRequest(http.MethodPost, "http://local.platform.test/api/v2/executions", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestLocalPlatformHandlerRejectsIncompleteLookupQuery(t *testing.T) {
	handler := NewLocalPlatformHandler(LocalPlatformOptions{RequestHMACKey: "local-generation-request-key-for-contract-test-32"})
	request, _ := http.NewRequest(http.MethodGet, "http://local.platform.test/api/v2/executions/lookup?externalRef=step-1", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestLocalPlatformHandlerSendsSignedCompletionCallback(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	key := "local-generation-request-key-for-contract-test-32"
	callbackSeen := make(chan *http.Request, 1)
	callbackClient := &http.Client{Transport: localRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		callbackSeen <- request
		recorder := httptest.NewRecorder()
		recorder.WriteHeader(http.StatusOK)
		return recorder.Result(), nil
	})}
	platformHandler := NewLocalPlatformHandler(LocalPlatformOptions{RequestHMACKey: key, CallbackHMACKey: key, Callback: true, Now: func() time.Time { return now }, HTTPClient: callbackClient})
	clientTransport := localRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		platformHandler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})
	client, err := NewClientWithCallbackOrigin("http://local.platform.test", key, "http://127.0.0.1:18000", &http.Client{Transport: clientTransport}, func() time.Time { return now }, func() string { return "local-platform-nonce" })
	if err != nil {
		t.Fatal(err)
	}
	execution, err := BuildExecution(Step{ID: "step-local-platform-callback", Capability: CapabilityTextToImage, ModelSKU: "ps-image-v1", Input: Input{Prompt: "回调测试"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Submit(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-callbackSeen:
		body, _ := io.ReadAll(request.Body)
		verifier, err := NewCallbackVerifier(key, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(request.Method, request.URL.Path, request.Header, body); err != nil {
			t.Fatalf("回调合同校验失败: %v, body=%s", err, strings.TrimSpace(string(body)))
		}
	case <-time.After(time.Second):
		t.Fatal("未收到本地完成回调")
	}
}

type localRoundTripFunc func(*http.Request) (*http.Response, error)

func (function localRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return function(request) }
