package paycores

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

var callbackFixedNow = func() time.Time {
	return time.UnixMilli(1_788_768_550_000).UTC()
}

// 该向量由 Node 的 JSON.stringify({ orderId, userId, providerTxnId }) 与 crypto HMAC 生成。
const (
	nodeCanonicalPaymentBody = "{\"orderId\":\"order-1\",\"userId\":\"user-1\",\"providerTxnId\":\"txn-1\"}"
	nodePaymentSignature     = "0324fcb64fec5e52b6718eb194963903148fb80dc28f2cd054b838c279265e8e"
	nodePaymentNonce         = "12345678-1234-4123-8123-123456789abc"
)

func TestVerifyPaymentConfirmed接受NodeV2固定签名向量并投影受控确认(t *testing.T) {
	verifier, err := NewCallbackVerifier("  "+callbackSigningKey+"  ", callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}

	// 此处故意保留空白；Node 摘要针对解析后的 JSON.stringify(body)，不是原始字节。
	body := []byte("{\n  \"orderId\": \"order-1\",\n  \"userId\": \"user-1\",\n  \"providerTxnId\": \"txn-1\"\n}")
	headers := nodePaymentHeaders()
	confirmation, err := verifier.Verify(http.MethodPost, "/api/v1/internal/payment-confirmed", headers, body)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if confirmation.UserID != "user-1" || confirmation.OrderID != "order-1" || confirmation.ProviderTxnID != "txn-1" {
		t.Fatalf("确认投影 = %#v，包含的受控事实不符合预期", confirmation)
	}
	wantFields := []string{"UserID", "OrderID", "ProviderTxnID"}
	typeOfConfirmation := reflect.TypeOf(confirmation)
	if typeOfConfirmation.NumField() != len(wantFields) {
		t.Fatalf("VerifiedPaymentConfirmation 字段数 = %d，期望 %d", typeOfConfirmation.NumField(), len(wantFields))
	}
	for index, want := range wantFields {
		if actual := typeOfConfirmation.Field(index).Name; actual != want {
			t.Fatalf("VerifiedPaymentConfirmation 字段[%d] = %q，期望 %q", index, actual, want)
		}
	}
}

func TestNewCallbackVerifier拒绝短密钥和空时钟(t *testing.T) {
	if _, err := NewCallbackVerifier(strings.Repeat("k", 31), callbackFixedNow); err == nil {
		t.Fatal("短密钥应被拒绝")
	}
	if _, err := NewCallbackVerifier(callbackSigningKey, nil); err == nil {
		t.Fatal("空时钟应被拒绝")
	}
}

func TestVerify接受NodeV2大写Nonce的固定签名向量(t *testing.T) {
	const uppercaseNonce = "ABCDEF12-ABCD-4123-ABCD-123456789ABC"
	confirmation, err := newPaymentVerifier(t).Verify(
		http.MethodPost,
		"/api/v1/internal/payment-confirmed",
		nodePaymentHeadersWithNonce(uppercaseNonce, "00d6a356d99b9a31fe4840069faa6fe91d0c86fd2a4ccdd81b07fc29938c41bc"),
		[]byte(nodeCanonicalPaymentBody),
	)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if confirmation.UserID != "user-1" {
		t.Fatalf("确认投影 = %#v，期望保留大写 nonce 的原始签名语义", confirmation)
	}
}

func TestVerifiedNonce仅接受与验签相同的单值Header(t *testing.T) {
	verifier := newPaymentVerifier(t)
	headers := nodePaymentHeaders()
	if _, err := verifier.Verify(
		http.MethodPost,
		"/api/v1/internal/payment-confirmed",
		headers,
		[]byte(nodeCanonicalPaymentBody),
	); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	nonce, err := verifier.VerifiedNonce(headers)
	if err != nil || nonce != nodePaymentNonce {
		t.Fatalf("VerifiedNonce() = %q, %v，期望 %q, nil", nonce, err, nodePaymentNonce)
	}
}

func TestVerifiedNonce拒绝不同大小写Map键重复的NonceHeader(t *testing.T) {
	headers := nodePaymentHeaders()
	headers["x-request-nonce"] = []string{nodePaymentNonce}

	if _, err := newPaymentVerifier(t).VerifiedNonce(headers); err != ErrInvalidCallback {
		t.Fatalf("VerifiedNonce() error = %v，期望 %v", err, ErrInvalidCallback)
	}
}

func TestVerify拒绝已签名身份标识符的首尾Unicode空白(t *testing.T) {
	body := []byte("{\"orderId\":\"\u2003order-1\u2003\",\"userId\":\"user-1\",\"providerTxnId\":\"txn-1\"}")
	_, err := newPaymentVerifier(t).Verify(
		http.MethodPost,
		"/api/v1/internal/payment-confirmed",
		nodePaymentHeadersWithSignature("16758681a5f5a4eea922143aaba2eb3bced3ffd27b5c7d9ab302a7c4b4022560"),
		body,
	)
	if err == nil {
		t.Fatal("Verify() error = nil，期望拒绝会改变本地标识符的首尾空白")
	}
}

func TestVerify拒绝不同大小写Map键重复的签名Header(t *testing.T) {
	headers := nodePaymentHeaders()
	headers["x-signature-v2"] = []string{nodePaymentSignature}
	if _, err := newPaymentVerifier(t).Verify(
		http.MethodPost,
		"/api/v1/internal/payment-confirmed",
		headers,
		[]byte(nodeCanonicalPaymentBody),
	); err == nil {
		t.Fatal("Verify() error = nil，期望拒绝不同大小写 map key 的重复签名 Header")
	}
}

func TestVerify在一次验签中冻结业务时刻(t *testing.T) {
	times := []time.Time{callbackFixedNow(), callbackFixedNow().Add(time.Second)}
	index := 0
	verifier, err := NewCallbackVerifier(callbackSigningKey, func() time.Time {
		current := times[index]
		index++
		return current
	})
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}

	_, err = verifier.Verify(http.MethodPost, "/api/v1/internal/payment-confirmed", nodePaymentHeaders(), []byte(nodeCanonicalPaymentBody))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if index != 1 {
		t.Fatalf("时钟调用次数 = %d，期望验签时只取时一次", index)
	}
}

func TestParsePaymentBody拒绝包含孤立UTF16代理项的身份标识符(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "订单", body: `{"orderId":"\ud800","userId":"user-1","providerTxnId":"txn-1"}`},
		{name: "用户", body: `{"orderId":"order-1","userId":"\udc00","providerTxnId":"txn-1"}`},
		{name: "外部交易", body: `{"orderId":"order-1","userId":"user-1","providerTxnId":"\ud800"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, _, err := parsePaymentBody([]byte(testCase.body)); err == nil {
				t.Fatal("parsePaymentBody() error = nil，期望拒绝不可表示的 JS-only 标识符")
			}
		})
	}
}

func TestVerify保留合法Unicode的NodeJSON字符串语义(t *testing.T) {
	body := []byte("{\"orderId\":\"order-\u2028<>&\",\"userId\":\"用户-1\",\"providerTxnId\":\"txn-😀\"}")
	confirmation, err := newPaymentVerifier(t).Verify(
		http.MethodPost,
		"/api/v1/internal/payment-confirmed",
		nodePaymentHeadersWithSignature("7ca8ef1cce7f88023a1d7b41b6426fb73674c4b651d540e5c99d8f35d9a4f56d"),
		body,
	)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if confirmation.UserID != "用户-1" || confirmation.OrderID != "order-\u2028<>&" || confirmation.ProviderTxnID != "txn-😀" {
		t.Fatalf("合法 Unicode 投影 = %#v，期望保留 Node JSON.stringify 语义", confirmation)
	}
}

func TestVerify拒绝路径和报文篡改(t *testing.T) {
	verifier := newPaymentVerifier(t)
	cases := []struct {
		name    string
		path    string
		headers http.Header
		body    []byte
	}{
		{name: "路径篡改", path: "/api/internal/payment-confirmed", headers: nodePaymentHeaders(), body: []byte(nodeCanonicalPaymentBody)},
		{name: "报文篡改", path: "/api/v1/internal/payment-confirmed", headers: nodePaymentHeaders(), body: []byte(`{"orderId":"order-1","userId":"user-1","providerTxnId":"txn-2"}`)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := verifier.Verify(http.MethodPost, testCase.path, testCase.headers, testCase.body); err == nil {
				t.Fatal("Verify() error = nil，期望拒绝")
			}
		})
	}
}

func TestVerify拒绝过期无效或重复Header及非法Nonce(t *testing.T) {
	verifier := newPaymentVerifier(t)
	cases := []struct {
		name    string
		headers http.Header
	}{
		{name: "过期", headers: func() http.Header {
			headers := nodePaymentHeaders()
			headers.Set("X-Timestamp", strconv.FormatInt(callbackFixedNow().Add(-61*time.Second).UnixMilli(), 10))
			return headers
		}()},
		{name: "缺少签名", headers: func() http.Header {
			headers := nodePaymentHeaders()
			headers.Del("X-Signature-V2")
			return headers
		}()},
		{name: "重复签名", headers: func() http.Header {
			headers := nodePaymentHeaders()
			headers.Add("X-Signature-V2", nodePaymentSignature)
			return headers
		}()},
		{name: "首尾空白", headers: func() http.Header {
			headers := nodePaymentHeaders()
			headers.Set("X-Request-Nonce", " "+nodePaymentNonce)
			return headers
		}()},
		{name: "非法nonce", headers: func() http.Header {
			headers := nodePaymentHeaders()
			headers.Set("X-Request-Nonce", "nonce with spaces")
			return headers
		}()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := verifier.Verify(http.MethodPost, "/api/v1/internal/payment-confirmed", testCase.headers, []byte(nodeCanonicalPaymentBody)); err == nil {
				t.Fatal("Verify() error = nil，期望拒绝")
			}
		})
	}
}

func TestVerify拒绝缺失或额外业务字段(t *testing.T) {
	verifier := newPaymentVerifier(t)
	cases := []struct {
		name string
		body string
	}{
		{name: "缺少订单", body: `{"userId":"user-1","providerTxnId":"txn-1"}`},
		{name: "缺少用户", body: `{"orderId":"order-1","providerTxnId":"txn-1"}`},
		{name: "缺少交易", body: `{"orderId":"order-1","userId":"user-1"}`},
		{name: "额外钻石", body: `{"orderId":"order-1","userId":"user-1","providerTxnId":"txn-1","diamonds":999}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(testCase.body)
			if _, err := verifier.Verify(http.MethodPost, "/api/internal/payment-confirmed", signedPaymentHeaders(body, "/api/internal/payment-confirmed"), body); err == nil {
				t.Fatal("Verify() error = nil，期望拒绝")
			}
		})
	}
}

func newPaymentVerifier(t *testing.T) *CallbackVerifier {
	t.Helper()
	verifier, err := NewCallbackVerifier(callbackSigningKey, callbackFixedNow)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	return verifier
}

func nodePaymentHeaders() http.Header {
	return nodePaymentHeadersWithSignature(nodePaymentSignature)
}

func nodePaymentHeadersWithSignature(signature string) http.Header {
	return nodePaymentHeadersWithNonce(nodePaymentNonce, signature)
}

func nodePaymentHeadersWithNonce(nonce, signature string) http.Header {
	headers := make(http.Header)
	headers.Set("X-Timestamp", strconv.FormatInt(callbackFixedNow().UnixMilli(), 10))
	headers.Set("X-Request-Nonce", nonce)
	headers.Set("X-Signature-V2", signature)
	return headers
}

func signedPaymentHeaders(body []byte, path string) http.Header {
	timestamp := strconv.FormatInt(callbackFixedNow().UnixMilli(), 10)
	nonce := nodePaymentNonce
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{http.MethodPost, path, timestamp, nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(callbackSigningKey))
	_, _ = mac.Write([]byte(canonical))
	headers := make(http.Header)
	headers.Set("X-Timestamp", timestamp)
	headers.Set("X-Request-Nonce", nonce)
	headers.Set("X-Signature-V2", hex.EncodeToString(mac.Sum(nil)))
	return headers
}
