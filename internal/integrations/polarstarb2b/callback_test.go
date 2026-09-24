package polarstarb2b

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const (
	callbackTestSecret  = "b2b-callback-secret-0123456789abcdef"
	callbackTestTenant  = "tenant-cling"
	callbackTestAccount = "account-main"
	callbackTestJobID   = "job-b2b-1"
	callbackTestStepID  = "step-b2b-1"
	callbackTestDeliver = "delivery-b2b-1"
)

func callbackTestVerifier(t *testing.T) *CallbackVerifier {
	t.Helper()
	verifier, err := NewCallbackVerifier(callbackTestSecret, callbackTestTenant, callbackTestAccount)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	return verifier
}

func callbackTestHeaders(secret, body string, overrides map[string]string) http.Header {
	headers := http.Header{}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	headers.Set(callbackSignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	headers.Set(callbackJobHeader, callbackTestJobID)
	headers.Set(callbackDeliveryHeader, callbackTestDeliver)
	headers.Set(callbackAttemptHeader, "1")
	for key, value := range overrides {
		if value == "" {
			headers.Del(key)
			continue
		}
		headers.Set(key, value)
	}
	return headers
}

func callbackTestBody(status string, extra map[string]any) string {
	body := map[string]any{
		"contractVersion": CallbackContractVersion,
		"tenantId":        callbackTestTenant,
		"jobId":           callbackTestJobID,
		"externalId":      callbackTestStepID,
		"capability":      "text_to_image",
		"status":          status,
		"deliveryId":      callbackTestDeliver,
	}
	for key, value := range extra {
		body[key] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(payload)
}

func TestCallbackVerify完成态投递产出受控事实(t *testing.T) {
	verifier := callbackTestVerifier(t)
	body := callbackTestBody("completed", map[string]any{
		"output":      map[string]any{"resultUrl": "https://cdn.polarstar.work/result/step-b2b-1.png"},
		"completedAt": "2026-09-18T10:00:00.000Z",
	})
	event, err := verifier.Verify(callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackAttemptHeader: "3"}), []byte(body))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if event.AccountRef != callbackTestAccount || event.DeliveryID != callbackTestDeliver ||
		event.StepID != callbackTestStepID || event.JobID != callbackTestJobID ||
		event.Capability != "text_to_image" || event.Status != CallbackStatusCompleted ||
		event.ResultURL != "https://cdn.polarstar.work/result/step-b2b-1.png" || event.Attempt != 3 {
		t.Fatalf("Verify() = %#v", event)
	}
	// 摘要必须是原始字节的 SHA-256：持久化侧靠它做按字节去重与冲突判定。
	digest := sha256.Sum256([]byte(body))
	if event.PayloadDigest != hex.EncodeToString(digest[:]) {
		t.Fatalf("PayloadDigest = %q", event.PayloadDigest)
	}
}

func TestCallbackVerify失败与取消终态不带结果(t *testing.T) {
	verifier := callbackTestVerifier(t)
	for _, status := range []CallbackStatus{CallbackStatusFailed, CallbackStatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			body := callbackTestBody(string(status), map[string]any{"error": map[string]any{"code": "MODEL_BUSY"}})
			event, err := verifier.Verify(callbackTestHeaders(callbackTestSecret, body, nil), []byte(body))
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if event.Status != status || event.ResultURL != "" {
				t.Fatalf("Verify() = %#v", event)
			}
		})
	}
}

// 未通过验签的报文必须一律拒绝，且拒绝原因不区分「签名错」与「结构错」，
// 避免把解析器行为暴露给未认证方。
func TestCallbackVerify拒绝未验签报文(t *testing.T) {
	verifier := callbackTestVerifier(t)
	body := callbackTestBody("completed", map[string]any{"output": map[string]any{"resultUrl": "https://cdn.polarstar.work/r.png"}})
	cases := map[string]http.Header{
		"签名错误":     callbackTestHeaders("another-secret-0123456789abcdefgh", body, nil),
		"签名非十六进制":  callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackSignatureHeader: strings.Repeat("z", 64)}),
		"签名长度不符":   callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackSignatureHeader: "abcd"}),
		"缺少签名":     callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackSignatureHeader: ""}),
		"缺少任务头":    callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackJobHeader: ""}),
		"缺少投递头":    callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackDeliveryHeader: ""}),
		"投递次数非数字":  callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackAttemptHeader: "first"}),
		"投递次数为零":   callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackAttemptHeader: "0"}),
		"投递次数为负":   callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackAttemptHeader: "-1"}),
		"任务头含非法字符": callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackJobHeader: "job b2b 1"}),
		"头部取值带空白":  callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackDeliveryHeader: " delivery-b2b-1"}),
		"报文被篡改":    callbackTestHeaders(callbackTestSecret, callbackTestBody("failed", nil), nil),
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(headers, []byte(body)); !errors.Is(err, ErrInvalidCallback) {
				t.Fatalf("Verify() error = %v, want ErrInvalidCallback", err)
			}
		})
	}
	// 只接受小写十六进制：大写写法来自非本合同的发送方，不做兼容猜测。
	// 十六进制摘要可能恰好全为数字，此时大小写无差别，跳过该断言。
	if upper := strings.ToUpper(signatureOf(callbackTestSecret, body)); upper != signatureOf(callbackTestSecret, body) {
		headers := callbackTestHeaders(callbackTestSecret, body, map[string]string{callbackSignatureHeader: upper})
		if _, err := verifier.Verify(headers, []byte(body)); !errors.Is(err, ErrInvalidCallback) {
			t.Fatalf("大写签名 Verify() error = %v, want ErrInvalidCallback", err)
		}
	}
	// 多值头部意味着中间层在做合并：取哪一个都是猜测。
	multi := callbackTestHeaders(callbackTestSecret, body, nil)
	multi.Add(callbackJobHeader, "job-b2b-2")
	if _, err := verifier.Verify(multi, []byte(body)); !errors.Is(err, ErrInvalidCallback) {
		t.Fatalf("多值头部 Verify() error = %v, want ErrInvalidCallback", err)
	}
}

// 签名正确但报文偏离合同：这些是「已认证的发送方说了不该说的话」，
// 必须与未认证输入一样拒收。
func TestCallbackVerify拒绝偏离合同的报文(t *testing.T) {
	verifier := callbackTestVerifier(t)
	validOutput := map[string]any{"output": map[string]any{"resultUrl": "https://cdn.polarstar.work/r.png"}}
	cases := map[string]string{
		"合同版本不符":       strings.Replace(callbackTestBody("completed", validOutput), CallbackContractVersion, "b2b.callback.v1", 1),
		"租户不符":         callbackTestBody("completed", mergeCallbackExtra(validOutput, map[string]any{"tenantId": "tenant-other"})),
		"报文任务号与头部不符":   callbackTestBody("completed", mergeCallbackExtra(validOutput, map[string]any{"jobId": "job-b2b-2"})),
		"投递号与头部不符":     callbackTestBody("completed", mergeCallbackExtra(validOutput, map[string]any{"deliveryId": "delivery-other"})),
		"中间态不应走回调":     callbackTestBody("processing", nil),
		"排队态不应走回调":     callbackTestBody("queued", nil),
		"未知终态":         callbackTestBody("expired", nil),
		"能力不在白名单":      callbackTestBody("completed", mergeCallbackExtra(validOutput, map[string]any{"capability": "text_to_video"})),
		"完成态缺结果":       callbackTestBody("completed", nil),
		"完成态结果为空":      callbackTestBody("completed", map[string]any{"output": map[string]any{"resultUrl": ""}}),
		"完成态结果非 HTTPS": callbackTestBody("completed", map[string]any{"output": map[string]any{"resultUrl": "http://cdn.polarstar.work/r.png"}}),
		"完成态结果为私网":     callbackTestBody("completed", map[string]any{"output": map[string]any{"resultUrl": "https://10.0.0.7/r.png"}}),
		"失败态携带结果":      callbackTestBody("failed", validOutput),
		"缺少外部标识":       strings.Replace(callbackTestBody("completed", validOutput), `"externalId":"`+callbackTestStepID+`",`, "", 1),
		"未知字段":         callbackTestBody("completed", mergeCallbackExtra(validOutput, map[string]any{"privateKey": "x"})),
		"非 JSON":       "not-json",
		"顶层数组":         `[{"contractVersion":"b2b.callback.v2"}]`,
		"重复键":          `{"contractVersion":"b2b.callback.v2","contractVersion":"b2b.callback.v2"}`,
		"尾随数据":         callbackTestBody("completed", validOutput) + "{}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(callbackTestHeaders(callbackTestSecret, body, nil), []byte(body)); !errors.Is(err, ErrInvalidCallback) {
				t.Fatalf("Verify() error = %v, want ErrInvalidCallback", err)
			}
		})
	}
}

func TestCallbackVerify拒绝超限与空报文(t *testing.T) {
	verifier := callbackTestVerifier(t)
	body := callbackTestBody("failed", nil)
	if _, err := verifier.Verify(callbackTestHeaders(callbackTestSecret, body, nil), nil); !errors.Is(err, ErrInvalidCallback) {
		t.Fatalf("空报文 Verify() error = %v, want ErrInvalidCallback", err)
	}
	oversized := strings.Repeat("a", callbackMaxBodyBytes+1)
	if _, err := verifier.Verify(callbackTestHeaders(callbackTestSecret, oversized, nil), []byte(oversized)); !errors.Is(err, ErrInvalidCallback) {
		t.Fatalf("超限报文 Verify() error = %v, want ErrInvalidCallback", err)
	}
}

func TestCallbackVerify拒绝任何ContentEncoding(t *testing.T) {
	verifier := callbackTestVerifier(t)
	body := callbackTestBody("failed", nil)
	for _, encoding := range []string{"identity", "gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			headers := callbackTestHeaders(callbackTestSecret, body, nil)
			headers.Set("Content-Encoding", encoding)
			if _, err := verifier.Verify(headers, []byte(body)); !errors.Is(err, ErrInvalidCallback) {
				t.Fatalf("Content-Encoding=%q Verify() error = %v, want ErrInvalidCallback", encoding, err)
			}
		})
	}
}

func TestNewCallbackVerifierWithSecrets支持Active和Previous轮换(t *testing.T) {
	const previous = "b2b-callback-previous-0123456789abcdef"
	verifier, err := NewCallbackVerifierWithSecrets(callbackTestSecret, previous, callbackTestTenant, callbackTestAccount)
	if err != nil {
		t.Fatalf("NewCallbackVerifierWithSecrets() error = %v", err)
	}
	body := callbackTestBody("failed", nil)
	for name, secret := range map[string]string{"active": callbackTestSecret, "previous": previous} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(callbackTestHeaders(secret, body, nil), []byte(body)); err != nil {
				t.Fatalf("Verify() with %s secret error = %v", name, err)
			}
		})
	}
	if _, err := verifier.Verify(callbackTestHeaders("b2b-callback-retired-0123456789abcdef", body, nil), []byte(body)); !errors.Is(err, ErrInvalidCallback) {
		t.Fatalf("Verify() with unrelated secret error = %v, want ErrInvalidCallback", err)
	}
}

func TestNewCallbackVerifierWithSecrets拒绝不安全Previous(t *testing.T) {
	cases := map[string]string{
		"过短":        "short-secret",
		"首尾空白":      " " + callbackTestSecret,
		"与active相同": callbackTestSecret,
	}
	for name, previous := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCallbackVerifierWithSecrets(callbackTestSecret, previous, callbackTestTenant, callbackTestAccount); !errors.Is(err, ErrInvalidCallback) {
				t.Fatalf("NewCallbackVerifierWithSecrets() error = %v, want ErrInvalidCallback", err)
			}
		})
	}
}

func TestNewCallbackVerifier拒绝不完整配置(t *testing.T) {
	cases := map[string]struct{ secret, tenant, account string }{
		"密钥过短": {"short-secret", callbackTestTenant, callbackTestAccount},
		// 长度足够但带首尾空白：HMAC 用原始字节，接受它等于接受一个
		// 永远验签不过的配置。
		"密钥带尾随空白": {callbackTestSecret + " ", callbackTestTenant, callbackTestAccount},
		"密钥带前导空白": {" " + callbackTestSecret, callbackTestTenant, callbackTestAccount},
		"缺少租户":    {callbackTestSecret, "", callbackTestAccount},
		"缺少账号引用":  {callbackTestSecret, callbackTestTenant, ""},
		"账号引用含空格": {callbackTestSecret, callbackTestTenant, "account main"},
		"租户含控制字符": {callbackTestSecret, "tenant\ncling", callbackTestAccount},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCallbackVerifier(testCase.secret, testCase.tenant, testCase.account); !errors.Is(err, ErrInvalidCallback) {
				t.Fatalf("NewCallbackVerifier() error = %v, want ErrInvalidCallback", err)
			}
		})
	}
}

// 零值验签器不得成为「跳过校验」的后门。
func TestCallbackVerify零值验签器拒绝一切(t *testing.T) {
	body := callbackTestBody("failed", nil)
	var verifier *CallbackVerifier
	if _, err := verifier.Verify(callbackTestHeaders(callbackTestSecret, body, nil), []byte(body)); !errors.Is(err, ErrInvalidCallback) {
		t.Fatalf("Verify() error = %v, want ErrInvalidCallback", err)
	}
}

func signatureOf(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func mergeCallbackExtra(base, extra map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}
