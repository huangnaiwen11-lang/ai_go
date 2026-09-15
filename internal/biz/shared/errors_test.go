package shared

import (
	"net/http"
	"testing"
)

func TestGenerationGateErrorsKeepIndependentHTTPSemantics(t *testing.T) {
	cases := []struct {
		name        string
		err         *APIError
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:       "游客必须绑定账户",
			err:        ErrAccountBindingRequired,
			wantStatus: 403,
			wantCode:   "ACCOUNT_BINDING_REQUIRED",
		},
		{
			name:       "受限能力要求 VIP",
			err:        ErrVIPRequired,
			wantStatus: 403,
			wantCode:   "VIP_REQUIRED",
		},
		{
			name:       "余额和日免均不足",
			err:        ErrInsufficientFunds,
			wantStatus: 402,
			wantCode:   "INSUFFICIENT_FUNDS",
		},
		{
			name:        "无效会话固定为未认证",
			err:         ErrUnauthenticated,
			wantStatus:  http.StatusUnauthorized,
			wantCode:    "UNAUTHORIZED",
			wantMessage: "Authentication required",
		},
		{
			name:        "会话存储失败固定为服务不可用",
			err:         ErrServiceUnavailable,
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    "SERVICE_UNAVAILABLE",
			wantMessage: "Service unavailable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.StatusCode(); got != tc.wantStatus {
				t.Fatalf("StatusCode() = %d, want %d", got, tc.wantStatus)
			}
			if got := tc.err.Code(); got != tc.wantCode {
				t.Fatalf("Code() = %q, want %q", got, tc.wantCode)
			}
			if tc.wantMessage != "" && tc.err.Message() != tc.wantMessage {
				t.Fatalf("Message() = %q, want %q", tc.err.Message(), tc.wantMessage)
			}
			if tc.err.Details() != nil {
				t.Fatalf("Details() = %#v, want nil", tc.err.Details())
			}
		})
	}
}

// TestAuthEntryErrorsKeepClientActionsDistinct 确保前端不会把“游客需绑定”“需 VIP”
// “余额不足”与账号入口错误混成同一种不可生成提示。
func TestAuthEntryErrorsKeepClientActionsDistinct(t *testing.T) {
	cases := []struct {
		err        *APIError
		wantStatus int
		wantCode   string
	}{
		{err: ErrInvalidRequest, wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST"},
		{err: ErrInvalidCredentials, wantStatus: http.StatusUnauthorized, wantCode: "INVALID_CREDENTIALS"},
		{err: ErrEmailAlreadyRegistered, wantStatus: http.StatusConflict, wantCode: "EMAIL_ALREADY_REGISTERED"},
		{err: ErrWebGuestLoginDisabled, wantStatus: http.StatusForbidden, wantCode: "WEB_GUEST_LOGIN_DISABLED"},
	}
	for _, tc := range cases {
		if got := tc.err.StatusCode(); got != tc.wantStatus {
			t.Fatalf("StatusCode() = %d, want %d", got, tc.wantStatus)
		}
		if got := tc.err.Code(); got != tc.wantCode {
			t.Fatalf("Code() = %q, want %q", got, tc.wantCode)
		}
	}
}
