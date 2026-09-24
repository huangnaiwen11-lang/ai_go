// Package paycores 固化 Go 自有订单与 PayCores 的最小 HTTP 合同。
package paycores

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ai-business-service/internal/biz/payments"
)

const (
	createOrderPath         = "/internal/create-order"
	orderStatusPath         = "/internal/order-status"
	paymentMethodsPath      = "/internal/payment-methods"
	minimumRequestKeyLength = 32
	maxResponseBytes        = 64 << 10
)

var (
	// ErrInvalidCreateOrderRequest 表示建单所需的 Go 自有冻结事实不完整。
	ErrInvalidCreateOrderRequest = errors.New("invalid paycores create order request")
	// ErrCreateOrderRejected 表示 PayCores 已明确拒绝本次建单，而不是网络层未知状态。
	ErrCreateOrderRejected = errors.New("paycores create order rejected")
	// ErrOrderStatusRejected 表示 PayCores 未返回与用户、订单绑定的有效状态。
	ErrOrderStatusRejected = errors.New("paycores order status rejected")
)

// CreateOrderRequest 是交给 PayCores 的最小收款合同。
// 金额和 credits 都来自 Go 冻结订单；严禁加入钻石余额、VIP 权益或 Node 钱包字段。
type CreateOrderRequest struct {
	UserID               string
	ProductID            string
	AmountCents          int64
	Credits              int64
	Label                string
	ClientRequestID      string
	Provider             string
	Account              string
	ClientDevicePlatform string
	ReturnURL            string
	CancelURL            string
}

// PaymentMethodsRequest carries only routing context. It never contains a
// client-supplied price or entitlement value.
type PaymentMethodsRequest = payments.PaymentMethodsRequest
type PaymentMethod = payments.PaymentMethod

// CreateOrderResult 是 PayCores 成功建单后可用于回调关联和浏览器跳转的最小结果。
type CreateOrderResult struct {
	OrderID     string
	CheckoutURL string
}

// OrderStatusResult 对齐 PayCores /internal/order-status 的只读投影。
// paymentReceived 与 backendReady 必须分开保留，不能把渠道已收款误当成本地已入账。
type OrderStatusResult struct {
	OrderID             string     `json:"orderId"`
	Status              string     `json:"status"`
	PaidAt              *time.Time `json:"paidAt"`
	Provider            string     `json:"provider"`
	AmountUSD           float64    `json:"amountUsd"`
	ProductID           string     `json:"productId"`
	NotifiedMainBackend bool       `json:"notifiedMainBackend"`
	PaymentReceived     bool       `json:"paymentReceived"`
	BackendReady        bool       `json:"backendReady"`
}

// Client 只负责 PayCores 建单签名和请求，不持有本地订单、账本或回调结算逻辑。
type Client struct {
	baseURL  *url.URL
	key      []byte
	http     *http.Client
	now      func() time.Time
	newNonce func() string
}

// NewClient 创建 PayCores 建单客户端。请求密钥必须独立于回调密钥。
func NewClient(baseURL, key string, client *http.Client, now func() time.Time, newNonce func() string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, ErrInvalidCreateOrderRequest
	}
	trimmedKey := strings.TrimSpace(key)
	if len(trimmedKey) < minimumRequestKeyLength || now == nil || newNonce == nil {
		return nil, ErrInvalidCreateOrderRequest
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{baseURL: parsed, key: []byte(trimmedKey), http: client, now: now, newNonce: newNonce}, nil
}

// CreateOrder 使用 Node/PayCores 既有的双 HMAC 协议创建订单。
// body 先序列化一次并由两种签名共用；传输或响应体读取失败时最多同键重发一次，
// 每次重发重新生成 V2 时间戳与 nonce，明确的 HTTP/业务拒绝不重试。
func (client *Client) CreateOrder(ctx context.Context, input CreateOrderRequest) (CreateOrderResult, error) {
	if client == nil || client.baseURL == nil || len(client.key) < minimumRequestKeyLength || client.http == nil || client.now == nil || client.newNonce == nil {
		return CreateOrderResult{}, ErrInvalidCreateOrderRequest
	}
	payload, err := input.toPayload()
	if err != nil {
		return CreateOrderResult{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return CreateOrderResult{}, fmt.Errorf("marshal paycores create order: %w", err)
	}
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: createOrderPath})
	for attempt := 0; attempt < 2; attempt++ {
		timestamp := strconvFormatMillis(client.now())
		nonce := strings.TrimSpace(client.newNonce())
		if timestamp == "" || nonce == "" {
			return CreateOrderResult{}, ErrInvalidCreateOrderRequest
		}
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if requestErr != nil {
			return CreateOrderResult{}, fmt.Errorf("create paycores HTTP request: %w", requestErr)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Signature", sign(client.key, body))
		request.Header.Set("X-Signature-V2", sign(client.key, []byte(v2Payload(http.MethodPost, createOrderPath, timestamp, nonce, body))))
		request.Header.Set("X-Timestamp", timestamp)
		request.Header.Set("X-Request-Nonce", nonce)

		response, callErr := client.http.Do(request)
		if callErr != nil {
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return CreateOrderResult{}, fmt.Errorf("call paycores create order: %w", callErr)
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return CreateOrderResult{}, fmt.Errorf("read paycores create order response: %w", readErr)
		}
		if len(raw) > maxResponseBytes {
			return CreateOrderResult{}, ErrCreateOrderRejected
		}
		var decoded struct {
			Success     bool   `json:"success"`
			OrderID     string `json:"orderId"`
			CheckoutURL string `json:"checkoutUrl"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || !decoded.Success || strings.TrimSpace(decoded.OrderID) == "" || strings.TrimSpace(decoded.CheckoutURL) == "" {
			return CreateOrderResult{}, ErrCreateOrderRejected
		}
		return CreateOrderResult{OrderID: decoded.OrderID, CheckoutURL: decoded.CheckoutURL}, nil
	}
	return CreateOrderResult{}, ErrCreateOrderRejected
}

// GetOrderStatus 使用旧 PayCores 的 V2 HMAC 只读合同按 orderId + userId 查询渠道状态。
// 返回值只用于恢复/核对；本地入账仍只能由验签回调和本地账本状态决定。
func (client *Client) GetOrderStatus(ctx context.Context, orderID, userID string) (OrderStatusResult, error) {
	if client == nil || client.baseURL == nil || len(client.key) < minimumRequestKeyLength || client.http == nil || client.now == nil || client.newNonce == nil || strings.TrimSpace(orderID) == "" || strings.TrimSpace(userID) == "" {
		return OrderStatusResult{}, ErrOrderStatusRejected
	}
	body, err := json.Marshal(struct {
		OrderID string `json:"orderId"`
		UserID  string `json:"userId"`
	}{OrderID: strings.TrimSpace(orderID), UserID: strings.TrimSpace(userID)})
	if err != nil {
		return OrderStatusResult{}, fmt.Errorf("marshal paycores order status: %w", err)
	}
	timestamp := strconvFormatMillis(client.now())
	nonce := strings.TrimSpace(client.newNonce())
	if timestamp == "" || nonce == "" {
		return OrderStatusResult{}, ErrOrderStatusRejected
	}
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: orderStatusPath})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return OrderStatusResult{}, fmt.Errorf("create paycores order status request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Signature-V2", sign(client.key, []byte(v2Payload(http.MethodPost, orderStatusPath, timestamp, nonce, body))))
	request.Header.Set("X-Timestamp", timestamp)
	request.Header.Set("X-Request-Nonce", nonce)
	response, err := client.http.Do(request)
	if err != nil {
		return OrderStatusResult{}, fmt.Errorf("call paycores order status: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return OrderStatusResult{}, fmt.Errorf("read paycores order status response: %w", err)
	}
	var decoded struct {
		Success bool `json:"success"`
		OrderStatusResult
	}
	if len(raw) > maxResponseBytes || json.Unmarshal(raw, &decoded) != nil || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || !decoded.Success || decoded.OrderID != strings.TrimSpace(orderID) || strings.TrimSpace(decoded.Status) == "" {
		return OrderStatusResult{}, ErrOrderStatusRejected
	}
	return decoded.OrderStatusResult, nil
}

// createOrderPayload 的字段声明顺序故意与 Node 既有 JSON.stringify 约定对齐。
// 旧签名覆盖原始 JSON 文本，不能用 map 或二次序列化破坏字节级一致性。
type createOrderPayload struct {
	UserID               string `json:"userId"`
	ProductID            string `json:"productId"`
	AmountCents          int64  `json:"amountCents"`
	Credits              int64  `json:"credits"`
	Label                string `json:"label"`
	ClientRequestID      string `json:"clientRequestId"`
	Provider             string `json:"provider,omitempty"`
	Account              string `json:"account,omitempty"`
	ClientDevicePlatform string `json:"clientDevicePlatform,omitempty"`
	ReturnURL            string `json:"returnUrl"`
	CancelURL            string `json:"cancelUrl"`
}

func (input CreateOrderRequest) toPayload() (createOrderPayload, error) {
	if strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.ProductID) == "" || input.AmountCents <= 0 || input.Credits <= 0 || strings.TrimSpace(input.Label) == "" || strings.TrimSpace(input.ClientRequestID) == "" || !validHTTPURL(input.ReturnURL) || !validHTTPURL(input.CancelURL) {
		return createOrderPayload{}, ErrInvalidCreateOrderRequest
	}
	return createOrderPayload{UserID: input.UserID, ProductID: input.ProductID, AmountCents: input.AmountCents, Credits: input.Credits, Label: input.Label, ClientRequestID: input.ClientRequestID, Provider: strings.TrimSpace(input.Provider), Account: strings.TrimSpace(input.Account), ClientDevicePlatform: strings.TrimSpace(input.ClientDevicePlatform), ReturnURL: input.ReturnURL, CancelURL: input.CancelURL}, nil
}

// ListPaymentMethods asks PayCores for the methods currently available to a
// Web checkout. The response is treated as untrusted presentation metadata;
// the selected provider/account is still validated by PayCores at checkout.
func (client *Client) ListPaymentMethods(ctx context.Context, input PaymentMethodsRequest) ([]PaymentMethod, error) {
	if client == nil || client.baseURL == nil || len(client.key) < minimumRequestKeyLength || client.http == nil || client.now == nil || client.newNonce == nil || strings.TrimSpace(input.ClientDevicePlatform) == "" {
		return nil, ErrInvalidCreateOrderRequest
	}
	payload := struct {
		ProductID            string `json:"productId,omitempty"`
		Country              string `json:"country,omitempty"`
		ClientDevicePlatform string `json:"clientDevicePlatform"`
		RoutingContext       struct {
			UserID string `json:"userId,omitempty"`
		} `json:"routingContext,omitempty"`
	}{ProductID: strings.TrimSpace(input.ProductID), Country: strings.ToUpper(strings.TrimSpace(input.Country)), ClientDevicePlatform: strings.TrimSpace(input.ClientDevicePlatform)}
	payload.RoutingContext.UserID = strings.TrimSpace(input.UserID)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal paycores payment methods: %w", err)
	}
	timestamp := strconvFormatMillis(client.now())
	nonce := strings.TrimSpace(client.newNonce())
	if timestamp == "" || nonce == "" {
		return nil, ErrInvalidCreateOrderRequest
	}
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: paymentMethodsPath})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create paycores payment methods request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Signature", sign(client.key, body))
	request.Header.Set("X-Signature-V2", sign(client.key, []byte(v2Payload(http.MethodPost, paymentMethodsPath, timestamp, nonce, body))))
	request.Header.Set("X-Timestamp", timestamp)
	request.Header.Set("X-Request-Nonce", nonce)
	response, err := client.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call paycores payment methods: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return nil, ErrCreateOrderRejected
	}
	var decoded struct {
		Success bool            `json:"success"`
		Methods []PaymentMethod `json:"methods"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || !decoded.Success {
		return nil, ErrCreateOrderRejected
	}
	return decoded.Methods, nil
}

func validHTTPURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func sign(key, value []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil))
}

func v2Payload(method, path, timestamp, nonce string, body []byte) string {
	digest := sha256.Sum256(body)
	return strings.Join([]string{strings.ToUpper(method), path, timestamp, nonce, hex.EncodeToString(digest[:])}, "\n")
}

func strconvFormatMillis(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d", at.UTC().UnixMilli())
}
