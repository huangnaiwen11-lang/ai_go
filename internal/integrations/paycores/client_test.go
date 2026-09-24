package paycores

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
		if body["userId"] != "go-user-1" || body["productId"] != "coins_100" || body["amountCents"] != float64(999) || body["credits"] != float64(100) || body["clientRequestId"] != "local-payment-1" || body["provider"] != "shinningpay" || body["account"] != "us_googlepay" || body["clientDevicePlatform"] != "web" {
			t.Fatalf("建单字段 = %#v，期望只使用 Go 冻结商品事实", body)
		}
		if _, exists := body["diamondBalance"]; exists {
			t.Fatalf("请求不得携带余额：%#v", body)
		}
		raw := `{"userId":"go-user-1","productId":"coins_100","amountCents":999,"credits":100,"label":"100 Diamonds","clientRequestId":"local-payment-1","provider":"shinningpay","account":"us_googlepay","clientDevicePlatform":"web","returnUrl":"http://127.0.0.1:5173/payment/success","cancelUrl":"http://127.0.0.1:5173/payment/cancel"}`
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
		UserID:               "go-user-1",
		ProductID:            "coins_100",
		AmountCents:          999,
		Credits:              100,
		Label:                "100 Diamonds",
		ClientRequestID:      "local-payment-1",
		Provider:             "shinningpay",
		Account:              "us_googlepay",
		ClientDevicePlatform: "web",
		ReturnURL:            "http://127.0.0.1:5173/payment/success",
		CancelURL:            "http://127.0.0.1:5173/payment/cancel",
	})
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if result.OrderID != "pco_1" || result.CheckoutURL != "https://checkout.example.test/pco_1" {
		t.Fatalf("建单结果 = %#v", result)
	}
}

func TestClient建单失败与超时不伪造成功订单(t *testing.T) {
	const key = "paycores-request-signing-key-must-be-at-least-32-bytes"
	t.Run("provider failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = writer.Write([]byte(`{"success":false}`))
		}))
		defer server.Close()
		client, err := NewClient(server.URL, key, server.Client(), time.Now, func() string { return "failure-nonce" })
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.CreateOrder(context.Background(), CreateOrderRequest{UserID: "u", ProductID: "p", AmountCents: 1, Credits: 1, Label: "P", ClientRequestID: "req", ReturnURL: "http://127.0.0.1/ok", CancelURL: "http://127.0.0.1/cancel"})
		if !errors.Is(err, ErrCreateOrderRejected) {
			t.Fatalf("failure error = %v", err)
		}
	})
	t.Run("request timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			time.Sleep(50 * time.Millisecond)
		}))
		defer server.Close()
		client, err := NewClient(server.URL, key, server.Client(), time.Now, func() string { return "timeout-nonce" })
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_, err = client.CreateOrder(ctx, CreateOrderRequest{UserID: "u", ProductID: "p", AmountCents: 1, Credits: 1, Label: "P", ClientRequestID: "req", ReturnURL: "http://127.0.0.1/ok", CancelURL: "http://127.0.0.1/cancel"})
		if err == nil || !strings.Contains(err.Error(), "call paycores create order") {
			t.Fatalf("timeout error = %v", err)
		}
	})
}

func TestClient传输丢包后复用ClientRequestID恢复既有订单(t *testing.T) {
	const key = "paycores-request-signing-key-must-be-at-least-32-bytes"
	var calls int
	var bodies []string
	var timestamps []string
	var nonces []string
	httpClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("读取第 %d 次建单请求: %v", calls, err)
		}
		bodies = append(bodies, string(raw))
		timestamps = append(timestamps, request.Header.Get("X-Timestamp"))
		nonces = append(nonces, request.Header.Get("X-Request-Nonce"))
		if calls == 1 {
			return nil, errors.New("response packet lost")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"success":true,"orderId":"pco_recovered","checkoutUrl":"https://checkout.example.test/pco_recovered"}`)),
			Request:    request,
		}, nil
	})}
	var nowCalls int64
	var nonceCalls int
	client, err := NewClient("https://paycores.example.test", key, httpClient, func() time.Time {
		nowCalls++
		return time.UnixMilli(1789027200000 + nowCalls)
	}, func() string {
		nonceCalls++
		return []string{"retry-nonce-0001", "retry-nonce-0002"}[nonceCalls-1]
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CreateOrder(t.Context(), CreateOrderRequest{
		UserID: "user-1", ProductID: "coins_100", AmountCents: 999, Credits: 100,
		Label: "100 Diamonds", ClientRequestID: "stable-client-request-id",
		ReturnURL: "https://cling.example.test/payment/success", CancelURL: "https://cling.example.test/payment/cancel",
	})
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if result.OrderID != "pco_recovered" || calls != 2 {
		t.Fatalf("恢复结果 = %#v, calls = %d", result, calls)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] || !strings.Contains(bodies[0], `"clientRequestId":"stable-client-request-id"`) {
		t.Fatalf("重试必须复用完全相同的幂等请求体: %#v", bodies)
	}
	if timestamps[0] == timestamps[1] || nonces[0] == nonces[1] {
		t.Fatalf("重试必须使用新的签名时钟与 nonce: timestamps=%#v nonces=%#v", timestamps, nonces)
	}
}

func TestClient建单响应体丢包后复用ClientRequestID恢复既有订单(t *testing.T) {
	const key = "paycores-request-signing-key-must-be-at-least-32-bytes"
	var calls int
	httpClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: errorReadCloser{err: errors.New("response body packet lost")}, Request: request}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"success":true,"orderId":"pco_recovered_body","checkoutUrl":"https://checkout.example.test/pco_recovered_body"}`)),
			Request:    request,
		}, nil
	})}
	var nonceCalls int
	client, err := NewClient("https://paycores.example.test", key, httpClient, time.Now, func() string {
		nonceCalls++
		return []string{"body-loss-nonce-01", "body-loss-nonce-02"}[nonceCalls-1]
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CreateOrder(t.Context(), CreateOrderRequest{
		UserID: "user-1", ProductID: "coins_100", AmountCents: 999, Credits: 100,
		Label: "100 Diamonds", ClientRequestID: "stable-body-loss-request-id",
		ReturnURL: "https://cling.example.test/payment/success", CancelURL: "https://cling.example.test/payment/cancel",
	})
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if calls != 2 || result.OrderID != "pco_recovered_body" {
		t.Fatalf("响应体丢包恢复结果 = %#v, calls = %d", result, calls)
	}
}

func TestClient查询支付方式发送Web设备与商品上下文(t *testing.T) {
	const key = "paycores-request-signing-key-must-be-at-least-32-bytes"
	const timestamp = "1789027200000"
	const nonce = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/internal/payment-methods" {
			t.Fatalf("请求 = %s %s，期望 POST /internal/payment-methods", request.Method, request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["productId"] != "coins_100" || body["country"] != "US" || body["clientDevicePlatform"] != "web" {
			t.Fatalf("支付方式上下文 = %#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true,"methods":[{"provider":"shinningpay","account":"us_googlepay","label":"Google Pay","methodType":"googlepay","icon":"googlepay"}]}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, key, server.Client(), func() time.Time { return time.UnixMilli(1789027200000) }, func() string { return nonce })
	if err != nil {
		t.Fatal(err)
	}
	methods, err := client.ListPaymentMethods(t.Context(), PaymentMethodsRequest{ProductID: "coins_100", Country: "US", ClientDevicePlatform: "web"})
	if err != nil || len(methods) != 1 || methods[0].Provider != "shinningpay" || methods[0].Account != "us_googlepay" {
		t.Fatalf("methods = %#v, err = %v", methods, err)
	}
}

func TestClient按订单号与用户读取PayCores真实状态(t *testing.T) {
	const key = "paycores-request-signing-key-must-be-at-least-32-bytes"
	const timestamp = "1789027200000"
	const nonce = "status-nonce-0001"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/internal/order-status" {
			t.Fatalf("请求 = %s %s，期望 POST /internal/order-status", request.Method, request.URL.Path)
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"orderId":"pco_paid_waiting","userId":"user-1"}` {
			t.Fatalf("状态查询 body = %s", raw)
		}
		v2 := strings.Join([]string{http.MethodPost, "/internal/order-status", timestamp, nonce, sha256Hex(string(raw))}, "\n")
		if request.Header.Get("X-Signature-V2") != hmacHex(key, v2) {
			t.Fatalf("状态查询 V2 签名未绑定 method/path/timestamp/nonce/body")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true,"orderId":"pco_paid_waiting","status":"paid","paidAt":"2026-09-24T08:00:00.000Z","provider":"pixpay","amountUsd":4.99,"productId":"coins_500","notifiedMainBackend":false,"paymentReceived":true,"backendReady":false}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, key, server.Client(), func() time.Time { return time.UnixMilli(1789027200000) }, func() string { return nonce })
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.GetOrderStatus(t.Context(), "pco_paid_waiting", "user-1")
	if err != nil {
		t.Fatalf("GetOrderStatus() error = %v", err)
	}
	if status.OrderID != "pco_paid_waiting" || status.Status != "paid" || status.Provider != "pixpay" || status.ProductID != "coins_500" || !status.PaymentReceived || status.BackendReady || status.NotifiedMainBackend {
		t.Fatalf("状态结果 = %#v", status)
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

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (transport roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type errorReadCloser struct{ err error }

func (body errorReadCloser) Read([]byte) (int, error) { return 0, body.err }
func (errorReadCloser) Close() error                  { return nil }
