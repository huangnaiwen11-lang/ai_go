package generation

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const callbackSigningKey = "callback-signing-key-at-least-thirty-two-characters"

var callbackFixedNow = func() time.Time { return time.UnixMilli(1_788_768_550_000).UTC() }

func TestVerifyCallback接受现网V2已签名完成事件(t *testing.T) {
	raw := nodeTerminalCallback("completed")
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}

	event, err := verifier.Verify(http.MethodPost, generationCallbackPath, signedCallbackHeaders(raw), raw)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if event.ExternalRef != "step-1" || event.JobID != "job-1" || event.Capability != CapabilityTextToImage || event.Result.MediaType != "image" {
		t.Fatal("受控完成事件字段不符合预期")
	}
	if event.NonceHash == "" || event.PayloadDigest == "" {
		t.Fatal("回执摘要未生成")
	}
}

func TestVerifyCallback拒绝无效请求且失败事件不泄露错误内容(t *testing.T) {
	completed := nodeTerminalCallback("completed")
	failed := nodeTerminalCallback("failed")
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}

	cases := []struct {
		name    string
		method  string
		path    string
		headers http.Header
		body    []byte
	}{
		{name: "过期", method: http.MethodPost, path: generationCallbackPath, headers: signedCallbackHeadersAt(completed, callbackFixedNow().Add(-6*time.Minute)), body: completed},
		{name: "签名错误", method: http.MethodPost, path: generationCallbackPath, headers: func() http.Header {
			h := signedCallbackHeaders(completed)
			h.Set(callbackSignatureHeader, strings.Repeat("0", 64))
			return h
		}(), body: completed},
		{name: "错误方法", method: http.MethodGet, path: generationCallbackPath, headers: signedCallbackHeaders(completed), body: completed},
		{name: "错误路径", method: http.MethodPost, path: generationCallbackPath + "?x=1", headers: signedCallbackHeaders(completed), body: completed},
		{name: "错误nonce", method: http.MethodPost, path: generationCallbackPath, headers: func() http.Header {
			h := signedCallbackHeaders(completed)
			h.Set(callbackNonceHeader, "short")
			return h
		}(), body: completed},
		{name: "重复键", method: http.MethodPost, path: generationCallbackPath, headers: signedCallbackHeaders([]byte(`{"contractVersion":"execution.callback.v2","contractVersion":"execution.callback.v2"}`)), body: []byte(`{"contractVersion":"execution.callback.v2","contractVersion":"execution.callback.v2"}`)},
		{name: "未知字段", method: http.MethodPost, path: generationCallbackPath, headers: signedCallbackHeaders([]byte(`{"unexpected":true}`)), body: []byte(`{"unexpected":true}`)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := verifier.Verify(testCase.method, testCase.path, testCase.headers, testCase.body); err == nil {
				t.Fatal("Verify() error = nil, want rejection")
			}
		})
	}

	event, err := verifier.Verify(http.MethodPost, generationCallbackPath, signedCallbackHeaders(failed), failed)
	if err != nil {
		t.Fatalf("Verify(failed) error = %v", err)
	}
	if !event.Failed || event.Result.URL != "" {
		t.Fatal("失败事件不应包含结果素材")
	}
}

func TestVerifyCallback兼容现网终态扩展字段且不暴露非业务事实(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		wantFailed bool
		wantCancel bool
	}{
		{name: "完成", status: "completed"},
		{name: "失败", status: "failed", wantFailed: true},
		{name: "取消", status: "cancelled", wantCancel: true},
	}
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw := nodeTerminalCallback(testCase.status)
			event, err := verifier.Verify(http.MethodPost, generationCallbackPath, signedCallbackHeaders(raw), raw)
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if event.Failed != testCase.wantFailed || event.Cancelled != testCase.wantCancel {
				t.Fatalf("终态标记不符合预期，failed=%t cancelled=%t", event.Failed, event.Cancelled)
			}
			if testCase.status == "completed" && event.Result.URL != "https://assets.example.test/result.png?signature=opaque#preview" {
				t.Fatal("完成事件未保留受控结果地址")
			}
			for _, forbidden := range []string{"tenant", "metadata", "thumbnail", "error"} {
				for index := range reflect.TypeOf(event).NumField() {
					if strings.Contains(strings.ToLower(reflect.TypeOf(event).Field(index).Name), forbidden) {
						t.Fatalf("CallbackEvent 包含不应暴露的字段类别 %q", forbidden)
					}
				}
			}
		})
	}
}

func TestVerifyCallback拒绝未知嵌套字段(t *testing.T) {
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	raw := []byte(strings.Replace(string(nodeTerminalCallback("completed")), `"traceId":"trace-1"`, `"wallet":"forbidden"`, 1))
	if _, err := verifier.Verify(http.MethodPost, generationCallbackPath, signedCallbackHeaders(raw), raw); err == nil {
		t.Fatal("Verify() error = nil, want unknown metadata rejection")
	}
}

func TestVerifyCallback拒绝不符合现网终态合同的标识结果和缩略图(t *testing.T) {
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	resultOutput := `[{"role":"result","mediaType":"image","url":"https://assets.example.test/result.png"}]`
	failedWithResult := strings.Replace(string(nodeTerminalCallback("failed")), `"outputs":[]`, `"outputs":`+resultOutput, 1)
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "非小写摘要指纹", raw: []byte(strings.Replace(string(nodeTerminalCallback("completed")), `"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"fingerprint":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`, 1))},
		{name: "过长任务标识", raw: []byte(strings.Replace(string(nodeTerminalCallback("completed")), `"jobId":"job-1"`, `"jobId":"`+strings.Repeat("a", 257)+`"`, 1))},
		{name: "失败携带结果", raw: []byte(failedWithResult)},
		{name: "取消携带结果", raw: []byte(strings.Replace(string(nodeTerminalCallback("cancelled")), `"outputs":[]`, `"outputs":`+resultOutput, 1))},
		{name: "不安全缩略图", raw: []byte(strings.Replace(string(nodeTerminalCallback("completed")), `https://assets.example.test/thumbnail.png?signature=opaque#preview`, `http://assets.example.test/thumbnail.png`, 1))},
		{name: "能力与模型不匹配", raw: []byte(strings.Replace(string(nodeTerminalCallback("completed")), `"modelSku":"ps-image-v1"`, `"modelSku":"ps-auto"`, 1))},
		{name: "非法错误对象", raw: []byte(strings.Replace(string(nodeTerminalCallback("failed")), `"error":{"code":"UPSTREAM_FAILED","message":"upstream-error-secret"}`, `"error":{"message":"upstream-error-secret","detail":"forbidden"}`, 1))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := verifier.Verify(http.MethodPost, generationCallbackPath, signedCallbackHeaders(testCase.raw), testCase.raw); err == nil {
				t.Fatal("Verify() error = nil, want rejection")
			}
		})
	}
}

func TestVerifyCallback拒绝缺失非法或不一致的事件标识(t *testing.T) {
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	valid := string(nodeTerminalCallback("completed"))
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "缺失", raw: []byte(strings.Replace(valid, `"eventId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",`, "", 1))},
		{name: "非小写SHA256", raw: []byte(strings.Replace(valid, `"eventId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"eventId":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`, 1))},
		{name: "与指纹不一致", raw: []byte(strings.Replace(valid, `"eventId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"eventId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`, 1))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := verifier.Verify(http.MethodPost, generationCallbackPath, signedCallbackHeaders(testCase.raw), testCase.raw); err == nil {
				t.Fatal("Verify() error = nil, want eventId rejection")
			}
		})
	}
}

func TestVerifyCallback拒绝已签名标识和媒体地址前后空白(t *testing.T) {
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "任务标识", raw: []byte(strings.Replace(string(nodeTerminalCallback("completed")), `"jobId":"job-1"`, `"jobId":" job-1 "`, 1))},
		{name: "外部引用", raw: []byte(strings.Replace(string(nodeTerminalCallback("completed")), `"externalRef":"step-1"`, `"externalRef":" step-1 "`, 1))},
		{name: "租户标识", raw: []byte(strings.Replace(string(nodeTerminalCallback("completed")), `"tenantId":"cling-ai"`, `"tenantId":" cling-ai "`, 1))},
		{name: "结果地址", raw: []byte(strings.Replace(string(nodeTerminalCallback("completed")), `"url":"https://assets.example.test/result.png?signature=opaque#preview"`, `"url":" https://assets.example.test/result.png?signature=opaque#preview "`, 1))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := verifier.Verify(http.MethodPost, generationCallbackPath, signedCallbackHeaders(testCase.raw), testCase.raw); err == nil {
				t.Fatal("Verify() error = nil, want whitespace rejection")
			}
		})
	}
}

func TestVerifyCallback拒绝超过共享JSON上限的原始报文(t *testing.T) {
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	raw := make([]byte, 1<<20+1)
	if _, err := verifier.Verify(http.MethodPost, generationCallbackPath, signedCallbackHeaders(raw), raw); err == nil {
		t.Fatal("Verify() error = nil, want oversized body rejection")
	}
}

func nodeTerminalCallback(status string) []byte {
	output := `[]`
	errorField := ``
	if status == "completed" {
		output = `[{"role":"result","mediaType":"image","url":"https://assets.example.test/result.png?signature=opaque#preview","thumbnailUrl":"https://assets.example.test/thumbnail.png?signature=opaque#preview"}]`
	}
	if status == "failed" {
		errorField = `,"error":{"code":"UPSTREAM_FAILED","message":"upstream-error-secret"}`
	}
	return []byte(`{"contractVersion":"execution.callback.v2","eventId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","deliveryId":"delivery-1","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","eventType":"execution.` + status + `","jobId":"job-1","externalRef":"step-1","status":"` + status + `","outputs":` + output + errorField + `,"usage":{"processingMs":1,"outputCount":1},"timestamps":{},"executionRef":{"tenantId":"cling-ai","tenantSignupSource":"internal","capability":"text_to_image","modelSku":"ps-image-v1"},"metadata":{"site":"main","traceId":"trace-1"}}`)
}

func signedCallbackHeaders(raw []byte) http.Header {
	return signedCallbackHeadersAt(raw, callbackFixedNow())
}

func signedCallbackHeadersAt(raw []byte, at time.Time) http.Header {
	timestamp := strconv.FormatInt(at.UTC().UnixMilli(), 10)
	nonce := "callback-nonce-0001"
	payload := callbackSignaturePayload(timestamp, http.MethodPost, generationCallbackPath, nonce, raw)
	mac := hmac.New(sha256.New, []byte(callbackSigningKey))
	_, _ = mac.Write([]byte(payload))
	headers := make(http.Header)
	headers.Set(callbackVersionHeader, "2")
	headers.Set(callbackTimestampHeader, timestamp)
	headers.Set(callbackNonceHeader, nonce)
	headers.Set(callbackSignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	return headers
}
