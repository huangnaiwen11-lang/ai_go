package providercallback

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bizgeneration "ai-business-service/internal/biz/generation"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const (
	handlerTestSecret  = "b2b-callback-secret-0123456789abcdef"
	handlerTestTenant  = "tenant-cling"
	handlerTestAccount = "account-main"
	handlerTestBody    = `{"contractVersion":"b2b.callback.v2","tenantId":"tenant-cling","jobId":"job-b2b-1","externalId":"step-b2b-1","capability":"text_to_image","status":"completed","output":{"resultUrl":"https://cdn.polarstar.work/r.png"},"deliveryId":"delivery-b2b-1"}`
)

type recordingDeliveryUsecase struct {
	facts  []bizgeneration.ProviderDeliveryFact
	result bizgeneration.ProviderInboxApplyResult
	err    error
	calls  int
}

func (usecase *recordingDeliveryUsecase) Handle(_ context.Context, fact bizgeneration.ProviderDeliveryFact) (bizgeneration.ProviderInboxApplyResult, error) {
	usecase.calls++
	usecase.facts = append(usecase.facts, fact)
	if usecase.err != nil {
		return "", usecase.err
	}
	if usecase.result == "" {
		return bizgeneration.InboxApplyInserted, nil
	}
	return usecase.result, nil
}

func handlerTestVerifier(t *testing.T) *polarstarb2b.CallbackVerifier {
	t.Helper()
	verifier, err := polarstarb2b.NewCallbackVerifier(handlerTestSecret, handlerTestTenant, handlerTestAccount)
	if err != nil {
		t.Fatalf("NewCallbackVerifier() error = %v", err)
	}
	return verifier
}

func signedCallbackRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, platform.B2BCallbackPath, strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte(handlerTestSecret))
	_, _ = mac.Write([]byte(body))
	request.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	request.Header.Set("X-Job-Id", "job-b2b-1")
	request.Header.Set("X-Delivery-Id", "delivery-b2b-1")
	request.Header.Set("X-Callback-Attempt", "1")
	return request
}

func TestHandler接受已验签投递并落库(t *testing.T) {
	usecase := &recordingDeliveryUsecase{}
	recorder := httptest.NewRecorder()
	NewHandler(handlerTestVerifier(t), usecase).ServeHTTP(recorder, signedCallbackRequest(handlerTestBody))

	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		t.Fatalf("响应 = %d %s", recorder.Code, recorder.Body.String())
	}
	if usecase.calls != 1 {
		t.Fatalf("用例被调用 %d 次，want 1", usecase.calls)
	}
	fact := usecase.facts[0]
	if fact.AccountRef != handlerTestAccount || fact.DeliveryID != "delivery-b2b-1" ||
		fact.StepID != "step-b2b-1" || fact.JobID != "job-b2b-1" || fact.Attempt != 1 {
		t.Fatalf("投递事实 = %#v", fact)
	}
	// 落库的必须是**原始字节**：终态语义由 inbox consumer 从这份字节重新解析，
	// 这里若改写成规范化结构，消费者就再也看不到供应商实际发了什么。
	if string(fact.Payload) != handlerTestBody {
		t.Fatalf("落库载荷 = %s", fact.Payload)
	}
}

func TestHandler重复投递仍返回成功(t *testing.T) {
	usecase := &recordingDeliveryUsecase{result: bizgeneration.InboxApplyNoop}
	recorder := httptest.NewRecorder()
	NewHandler(handlerTestVerifier(t), usecase).ServeHTTP(recorder, signedCallbackRequest(handlerTestBody))
	if recorder.Code != http.StatusOK {
		t.Fatalf("重复投递响应 = %d %s, want 200", recorder.Code, recorder.Body.String())
	}
}

// 同一 deliveryId 携带不同字节属确定性冲突：证据已落库，重投不会改变结论，
// 用 409 让平台提前停止，而不是把同一份冲突再送 7 次。
func TestHandler冲突投递返回确定性拒绝(t *testing.T) {
	usecase := &recordingDeliveryUsecase{result: bizgeneration.InboxApplyQuarantined}
	recorder := httptest.NewRecorder()
	NewHandler(handlerTestVerifier(t), usecase).ServeHTTP(recorder, signedCallbackRequest(handlerTestBody))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("冲突投递响应 = %d %s, want 409", recorder.Code, recorder.Body.String())
	}
}

func TestHandler拒绝未验签投递(t *testing.T) {
	usecase := &recordingDeliveryUsecase{}
	cases := map[string]func(*http.Request){
		"缺少签名":   func(r *http.Request) { r.Header.Del("X-Signature") },
		"签名错误":   func(r *http.Request) { r.Header.Set("X-Signature", strings.Repeat("0", 64)) },
		"缺少任务头":  func(r *http.Request) { r.Header.Del("X-Job-Id") },
		"缺少投递头":  func(r *http.Request) { r.Header.Del("X-Delivery-Id") },
		"缺少投递次数": func(r *http.Request) { r.Header.Del("X-Callback-Attempt") },
		"载荷被篡改":  func(r *http.Request) { r.Body = http.NoBody },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := signedCallbackRequest(handlerTestBody)
			mutate(request)
			recorder := httptest.NewRecorder()
			NewHandler(handlerTestVerifier(t), usecase).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("响应 = %d %s, want 401", recorder.Code, recorder.Body.String())
			}
		})
	}
	// 未通过验签的报文绝不能进入落库路径。
	if usecase.calls != 0 {
		t.Fatalf("未验签投递仍触发落库 %d 次", usecase.calls)
	}
}

func TestHandler只接受精确路径与方法(t *testing.T) {
	usecase := &recordingDeliveryUsecase{}
	handler := NewHandler(handlerTestVerifier(t), usecase)
	cases := map[string]struct {
		method string
		target string
		want   int
	}{
		"非 POST":  {http.MethodGet, platform.B2BCallbackPath, http.StatusMethodNotAllowed},
		"错误路径":    {http.MethodPost, "/api/v1/internal/other-callback", http.StatusNotFound},
		"路径前缀":    {http.MethodPost, platform.B2BCallbackPath + "/extra", http.StatusNotFound},
		"携带查询串":   {http.MethodPost, platform.B2BCallbackPath + "?x=1", http.StatusBadRequest},
		"路径被转义编码": {http.MethodPost, "/api/v1/internal/polarstar-callback%2F", http.StatusNotFound},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.target, strings.NewReader(handlerTestBody))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != testCase.want {
				t.Fatalf("响应 = %d, want %d", recorder.Code, testCase.want)
			}
		})
	}
	if usecase.calls != 0 {
		t.Fatalf("未命中路径仍触发落库 %d 次", usecase.calls)
	}
}

func TestHandler映射落库错误(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"载荷非法": {bizgeneration.ErrInvalidProviderDelivery, http.StatusBadRequest},
		"存储失败": {errors.New("mongo unavailable"), http.StatusInternalServerError},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			usecase := &recordingDeliveryUsecase{err: testCase.err}
			recorder := httptest.NewRecorder()
			NewHandler(handlerTestVerifier(t), usecase).ServeHTTP(recorder, signedCallbackRequest(handlerTestBody))
			if recorder.Code != testCase.want {
				t.Fatalf("响应 = %d %s, want %d", recorder.Code, recorder.Body.String(), testCase.want)
			}
		})
	}
}

func TestHandler拒绝超限载荷(t *testing.T) {
	usecase := &recordingDeliveryUsecase{}
	oversized := strings.Repeat("a", maxCallbackBodyBytes+1)
	recorder := httptest.NewRecorder()
	NewHandler(handlerTestVerifier(t), usecase).ServeHTTP(recorder, signedCallbackRequest(oversized))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限响应 = %d, want 413", recorder.Code)
	}
	if usecase.calls != 0 {
		t.Fatal("超限载荷仍进入落库路径")
	}
}

func TestHandler拒绝任何ContentEncoding(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			usecase := &recordingDeliveryUsecase{}
			request := signedCallbackRequest(handlerTestBody)
			request.Header.Set("Content-Encoding", encoding)
			recorder := httptest.NewRecorder()
			NewHandler(handlerTestVerifier(t), usecase).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("Content-Encoding=%q 响应 = %d %s, want 415", encoding, recorder.Code, recorder.Body.String())
			}
			if usecase.calls != 0 {
				t.Fatalf("Content-Encoding=%q 仍进入落库路径", encoding)
			}
		})
	}
}

func TestHandler依赖缺失时返回内部错误(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(nil, nil).ServeHTTP(recorder, signedCallbackRequest(handlerTestBody))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("响应 = %d, want 500", recorder.Code)
	}
}
