// Package paycores 固化与 PayCores 的窄回调验签边界。
package paycores

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	minimumCallbackKeyLength = 32
	callbackTimestampWindow  = time.Minute
)

var (
	// ErrInvalidCallback 统一表示不可信回调，避免向调用方回显认证细节。
	ErrInvalidCallback = errors.New("invalid paycores callback")
	callbackNonce      = regexp.MustCompile(`(?i)^[a-f0-9-]{16,128}$`)
)

// VerifiedPaymentConfirmation 只保留本地一次性钻石关联所需的受控标识符。
type VerifiedPaymentConfirmation struct {
	UserID        string
	OrderID       string
	ProviderTxnID string
}

// CallbackVerifier 冻结 Node V2 HMAC 合同；它不依赖路由、存储或钱包实现。
type CallbackVerifier struct {
	key []byte
	now func() time.Time
}

// NewCallbackVerifier 使用受控密钥和时钟创建无状态验签器。
func NewCallbackVerifier(key string, now func() time.Time) (*CallbackVerifier, error) {
	trimmedKey := strings.TrimSpace(key)
	if len(trimmedKey) < minimumCallbackKeyLength || now == nil {
		return nil, ErrInvalidCallback
	}
	return &CallbackVerifier{key: []byte(trimmedKey), now: now}, nil
}

// Verify 验证 Node V2 信封，并投影为不含金额、签名或原始报文的受控确认事实。
func (verifier *CallbackVerifier) Verify(method, path string, headers http.Header, rawBody []byte) (VerifiedPaymentConfirmation, error) {
	if verifier == nil || len(verifier.key) < minimumCallbackKeyLength || verifier.now == nil || method != http.MethodPost || !allowedCallbackPath(path) {
		return VerifiedPaymentConfirmation{}, ErrInvalidCallback
	}
	businessAt := verifier.now().UTC()
	if businessAt.IsZero() {
		return VerifiedPaymentConfirmation{}, ErrInvalidCallback
	}
	timestamp, nonce, signature, ok := callbackHeaders(headers)
	if !ok || !freshTimestamp(timestamp, businessAt) {
		return VerifiedPaymentConfirmation{}, ErrInvalidCallback
	}
	payload, canonicalBody, err := parsePaymentBody(rawBody)
	if err != nil {
		return VerifiedPaymentConfirmation{}, ErrInvalidCallback
	}
	if !validSignature(verifier.key, method, path, timestamp, nonce, canonicalBody, signature) {
		return VerifiedPaymentConfirmation{}, ErrInvalidCallback
	}
	return VerifiedPaymentConfirmation{
		UserID:        payload.userID,
		OrderID:       payload.orderID,
		ProviderTxnID: payload.providerTxnID,
	}, nil
}

// VerifiedNonce 仅供已成功完成 Verify 的 transport 立即构造摘要。
// 它不代表调用方已完成 HMAC 验签，原 nonce 不得进入 payments。
func (verifier *CallbackVerifier) VerifiedNonce(headers http.Header) (string, error) {
	_, nonce, _, ok := callbackHeaders(headers)
	if verifier == nil || !ok {
		return "", ErrInvalidCallback
	}
	return nonce, nil
}

func allowedCallbackPath(path string) bool {
	return path == "/api/internal/payment-confirmed" || path == "/api/v1/internal/payment-confirmed"
}

func callbackHeaders(headers http.Header) (timestamp, nonce, signature string, ok bool) {
	if headers == nil {
		return "", "", "", false
	}
	timestamp = exactHeader(headers, "X-Timestamp")
	nonce = exactHeader(headers, "X-Request-Nonce")
	signature = exactHeader(headers, "X-Signature-V2")
	if timestamp == "" || signature == "" || !callbackNonce.MatchString(nonce) {
		return "", "", "", false
	}
	return timestamp, nonce, signature, true
}

func exactHeader(headers http.Header, key string) string {
	var values []string
	for headerKey, headerValues := range headers {
		if strings.EqualFold(headerKey, key) {
			values = append(values, headerValues...)
		}
	}
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] {
		return ""
	}
	return values[0]
}

func freshTimestamp(raw string, now time.Time) bool {
	milliseconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	delta := now.UTC().Sub(time.UnixMilli(milliseconds).UTC())
	return delta >= -callbackTimestampWindow && delta <= callbackTimestampWindow
}

func validSignature(key []byte, method, path, timestamp, nonce, canonicalBody, supplied string) bool {
	provided, err := hex.DecodeString(supplied)
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	digest := sha256.Sum256([]byte(canonicalBody))
	canonical := strings.Join([]string{method, path, timestamp, nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(canonical))
	return hmac.Equal(mac.Sum(nil), provided)
}

type paymentBody struct {
	orderID       string
	userID        string
	providerTxnID string
}

func parsePaymentBody(rawBody []byte) (paymentBody, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	opening, err := decoder.Token()
	if err != nil {
		return paymentBody{}, "", err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return paymentBody{}, "", ErrInvalidCallback
	}

	fields := make(map[string]string, 3)
	canonicalFields := make([]string, 0, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return paymentBody{}, "", err
		}
		name, ok := token.(string)
		if !ok || !allowedPaymentField(name) {
			return paymentBody{}, "", ErrInvalidCallback
		}
		if _, duplicate := fields[name]; duplicate {
			return paymentBody{}, "", ErrInvalidCallback
		}
		var rawValue json.RawMessage
		if err := decoder.Decode(&rawValue); err != nil {
			return paymentBody{}, "", err
		}
		if hasUnpairedUTF16Surrogate(rawValue) {
			return paymentBody{}, "", ErrInvalidCallback
		}
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil || value == "" || strings.TrimSpace(value) != value {
			return paymentBody{}, "", ErrInvalidCallback
		}
		canonicalValue, ok := nodeJSONString(rawValue)
		if !ok {
			return paymentBody{}, "", ErrInvalidCallback
		}
		fields[name] = value
		canonicalFields = append(canonicalFields, `"`+name+`":`+canonicalValue)
	}
	closing, err := decoder.Token()
	if err != nil {
		return paymentBody{}, "", err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return paymentBody{}, "", ErrInvalidCallback
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return paymentBody{}, "", ErrInvalidCallback
	}
	if len(fields) != 3 || fields["orderId"] == "" || fields["userId"] == "" || fields["providerTxnId"] == "" {
		return paymentBody{}, "", ErrInvalidCallback
	}
	return paymentBody{orderID: fields["orderId"], userID: fields["userId"], providerTxnID: fields["providerTxnId"]}, "{" + strings.Join(canonicalFields, ",") + "}", nil
}

func allowedPaymentField(name string) bool {
	return name == "orderId" || name == "userId" || name == "providerTxnId"
}

// hasUnpairedUTF16Surrogate 拒绝 Go/Mongo 无法无歧义承载的 JS-only 标识符。
// 代理对会解码为合法 Unicode 标量值；孤立代理项则不能进入本地结算或唯一索引。
func hasUnpairedUTF16Surrogate(raw json.RawMessage) bool {
	for index := 0; index < len(raw); {
		if raw[index] != '\\' || index+1 >= len(raw) {
			index++
			continue
		}
		if raw[index+1] != 'u' {
			index += 2
			continue
		}
		if index+5 >= len(raw) {
			return true
		}
		codeUnit, ok := jsonUTF16CodeUnit(raw[index+2 : index+6])
		if !ok {
			return true
		}
		if codeUnit >= 0xd800 && codeUnit <= 0xdbff {
			if index+11 >= len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
				return true
			}
			lowSurrogate, ok := jsonUTF16CodeUnit(raw[index+8 : index+12])
			if !ok || lowSurrogate < 0xdc00 || lowSurrogate > 0xdfff {
				return true
			}
			index += 12
			continue
		}
		if codeUnit >= 0xdc00 && codeUnit <= 0xdfff {
			return true
		}
		index += 6
	}
	return false
}

func jsonUTF16CodeUnit(raw []byte) (rune, bool) {
	var codeUnit rune
	for _, value := range raw {
		codeUnit <<= 4
		switch {
		case value >= '0' && value <= '9':
			codeUnit += rune(value - '0')
		case value >= 'a' && value <= 'f':
			codeUnit += rune(value-'a') + 10
		case value >= 'A' && value <= 'F':
			codeUnit += rune(value-'A') + 10
		default:
			return 0, false
		}
	}
	return codeUnit, true
}

// nodeJSONString 使用 Node JSON.stringify 的标量字符串转义规则，避免把原始报文当作摘要。
func nodeJSONString(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	var builder strings.Builder
	builder.Grow(len(value) + 2)
	builder.WriteByte('"')
	for _, runeValue := range value {
		switch runeValue {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if runeValue < 0x20 {
				builder.WriteString(`\u00`)
				builder.WriteByte("0123456789abcdef"[runeValue>>4])
				builder.WriteByte("0123456789abcdef"[runeValue&0x0f])
				continue
			}
			builder.WriteRune(runeValue)
		}
	}
	builder.WriteByte('"')
	return builder.String(), true
}
