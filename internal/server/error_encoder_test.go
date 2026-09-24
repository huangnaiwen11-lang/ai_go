package server

import (
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/shared"
)

func TestGenerationGateErrorsUseStableRootEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "游客绑定门禁",
			err:        shared.ErrAccountBindingRequired,
			wantStatus: stdhttp.StatusForbidden,
			wantCode:   "ACCOUNT_BINDING_REQUIRED",
		},
		{
			name:       "VIP 门禁",
			err:        shared.ErrVIPRequired,
			wantStatus: stdhttp.StatusForbidden,
			wantCode:   "VIP_REQUIRED",
		},
		{
			name:       "余额门禁",
			err:        shared.ErrInsufficientFunds,
			wantStatus: stdhttp.StatusPaymentRequired,
			wantCode:   "INSUFFICIENT_FUNDS",
		},
		{
			name:       "生成依赖不可用",
			err:        creations.ErrAdmissionDependencyUnavailable,
			wantStatus: stdhttp.StatusServiceUnavailable,
			wantCode:   "SERVICE_UNAVAILABLE",
		},
		{
			name:       "生成发布配置错误",
			err:        creations.ErrAdmissionConfigurationUnavailable,
			wantStatus: stdhttp.StatusServiceUnavailable,
			wantCode:   "GENERATION_CONFIGURATION_UNAVAILABLE",
		},
		{
			name:       "生成产品配方缺失",
			err:        creations.ErrB2BProductRecipeUnavailable,
			wantStatus: stdhttp.StatusServiceUnavailable,
			wantCode:   "GENERATION_RECIPE_UNAVAILABLE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/creations", nil)
			request.Header.Set("X-Request-Id", "req-gate-test-1")

			EncodeError(recorder, request, tc.err)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			// Result 会冻结首次写入时的响应头，等价于真实 HTTP 客户端看到的结果。
			if contentType := recorder.Result().Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json", contentType)
			}

			var body errorEnvelope
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Success {
				t.Fatal("success = true, want false")
			}
			if body.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", body.Code, tc.wantCode)
			}
			if body.Details != nil {
				t.Fatalf("details = %#v, want nil", body.Details)
			}
			if body.RequestID != "req-gate-test-1" {
				t.Fatalf("requestId = %q, want %q", body.RequestID, "req-gate-test-1")
			}
		})
	}
}
