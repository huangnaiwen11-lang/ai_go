package paycores

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 该测试冻结 Go 到 PayCores 的建单合同：服务端价格与订单号由 Go 决定，
// 同时携带兼容旧服务的 JSON HMAC 和不可被请求体重排绕过的 V2 HMAC。
func TestClient创建订单发送双签名与冻结商品事实(t *testing.T) {
	const key = "paycores-request-signing-key-must-be-at-least-32-bytes"
	const timestamp = "1789027200000"
	const nonce = "11111111-2222-3333-4444-555555555555"

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/internal/create-order" {
			t.Fatalf("请求 = %s %s，期望 POST /internal/create-order", request.Method, request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("解析建单请求: %v", err)
		}
		if body["userId"] != "go-user-1" || body["productId"] != "coins_100" || body["amountCents"] != float64(999) || body["credits"] != float64(100) || body["clientRequestId"] != "local-payment-1" {
			t.Fatalf("建单字段 = %#v，期望只使用 Go 冻结商品事实", body)
		}
		if _, exists := body["diamondBalance"]; exists {
			t.Fatalf("请求不得携带余额：%#v", body)
		}
		raw := `{"userId":"go-user-1","productId":"coins_100","amountCents":999,"credits":100,"label":"100 Diamonds","clientRequestId":"local-payment-1","returnUrl":"http://127.0.0.1:5173/payment/success","cancelUrl":"http://127.0.0.1:5173/payment/cancel"}`
		if got := request.Header.Get("X-Signature"); got != hmacHex(key, raw) {
			t.Fatalf("旧版签名 = %q，期望按原始请求体签名", got)
		}
		v2 := strings.Join([]string{http.MethodPost, "/internal/create-order", timestamp, nonce, sha256Hex(raw)}, "\n")
		if got := request.Header.Get("X-Signature-V2"); got != hmacHex(key, v2) {
			t.Fatalf("V2 签名 = %q，期望请求方法、路径、时间、nonce 和 body 摘要签名", got)
		}
		if request.Header.Get("X-Timestamp") != timestamp || request.Header.Get("X-Request-Nonce") != nonce {
			t.Fatalf("V2 元数据不匹配: timestamp=%q nonce=%q", request.Header.Get("X-Timestamp"), request.Header.Get("X-Request-Nonce"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true,"orderId":"pco_1","checkoutUrl":"https://checkout.example.test/pco_1","amountCents":999,"credits":100}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, key, server.Client(), func() time.Time {
		return time.UnixMilli(1789027200000)
	}, func() string { return nonce })
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	result, err := client.CreateOrder(t.Context(), CreateOrderRequest{
		UserID:          "go-user-1",
		ProductID:       "coins_100",
		AmountCents:     999,
		Credits:         100,
		Label:           "100 Diamonds",
		ClientRequestID: "local-payment-1",
		ReturnURL:       "http://127.0.0.1:5173/payment/success",
		CancelURL:       "http://127.0.0.1:5173/payment/cancel",
	})
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if result.OrderID != "pco_1" || result.CheckoutURL != "https://checkout.example.test/pco_1" {
		t.Fatalf("建单结果 = %#v", result)
	}
}

func hmacHex(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func sha256Hex(message string) string {
	digest := sha256.Sum256([]byte(message))
	return hex.EncodeToString(digest[:])
}
