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
)

const (
	createOrderPath         = "/internal/create-order"
	minimumRequestKeyLength = 32
	maxResponseBytes        = 64 << 10
)

var (
	// ErrInvalidCreateOrderRequest 表示建单所需的 Go 自有冻结事实不完整。
	ErrInvalidCreateOrderRequest = errors.New("invalid paycores create order request")
	// ErrCreateOrderRejected 表示 PayCores 已明确拒绝本次建单，而不是网络层未知状态。
	ErrCreateOrderRejected = errors.New("paycores create order rejected")
)

// CreateOrderRequest 是交给 PayCores 的最小收款合同。
// 金额和 credits 都来自 Go 冻结订单；严禁加入钻石余额、VIP 权益或 Node 钱包字段。
type CreateOrderRequest struct {
	UserID          string
	ProductID       string
	AmountCents     int64
	Credits         int64
	Label           string
	ClientRequestID string
	ReturnURL       string
	CancelURL       string
}

// CreateOrderResult 是 PayCores 成功建单后可用于回调关联和浏览器跳转的最小结果。
type CreateOrderResult struct {
	OrderID     string
	CheckoutURL string
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
// body 先序列化一次并由两种签名共用，确保签名内容和实际发送内容完全一致。
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
	timestamp := strconvFormatMillis(client.now())
	nonce := strings.TrimSpace(client.newNonce())
	if timestamp == "" || nonce == "" {
		return CreateOrderResult{}, ErrInvalidCreateOrderRequest
	}

	endpoint := client.baseURL.ResolveReference(&url.URL{Path: createOrderPath})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return CreateOrderResult{}, fmt.Errorf("create paycores HTTP request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Signature", sign(client.key, body))
	request.Header.Set("X-Signature-V2", sign(client.key, []byte(v2Payload(http.MethodPost, createOrderPath, timestamp, nonce, body))))
	request.Header.Set("X-Timestamp", timestamp)
	request.Header.Set("X-Request-Nonce", nonce)

	response, err := client.http.Do(request)
	if err != nil {
		return CreateOrderResult{}, fmt.Errorf("call paycores create order: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return CreateOrderResult{}, fmt.Errorf("read paycores create order response: %w", err)
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

// createOrderPayload 的字段声明顺序故意与 Node 既有 JSON.stringify 约定对齐。
// 旧签名覆盖原始 JSON 文本，不能用 map 或二次序列化破坏字节级一致性。
type createOrderPayload struct {
	UserID          string `json:"userId"`
	ProductID       string `json:"productId"`
	AmountCents     int64  `json:"amountCents"`
	Credits         int64  `json:"credits"`
	Label           string `json:"label"`
	ClientRequestID string `json:"clientRequestId"`
	ReturnURL       string `json:"returnUrl"`
	CancelURL       string `json:"cancelUrl"`
}

func (input CreateOrderRequest) toPayload() (createOrderPayload, error) {
	if strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.ProductID) == "" || input.AmountCents <= 0 || input.Credits <= 0 || strings.TrimSpace(input.Label) == "" || strings.TrimSpace(input.ClientRequestID) == "" || !validHTTPURL(input.ReturnURL) || !validHTTPURL(input.CancelURL) {
		return createOrderPayload{}, ErrInvalidCreateOrderRequest
	}
	return createOrderPayload{UserID: input.UserID, ProductID: input.ProductID, AmountCents: input.AmountCents, Credits: input.Credits, Label: input.Label, ClientRequestID: input.ClientRequestID, ReturnURL: input.ReturnURL, CancelURL: input.CancelURL}, nil
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
