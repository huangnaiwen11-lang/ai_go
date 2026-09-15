// Package shared 提供跨业务模块复用的领域错误。
//
// 错误对象不携带传输层或存储层依赖，业务模块只返回它们；HTTP 状态和响应编码
// 由传输层通过稳定接口读取，避免业务规则反向依赖 server 包。
package shared

import (
	"fmt"
	"net/http"
)

// APIError 是不可变的业务错误。
// 字段均不导出，模块外无法修改已经冻结的 HTTP 语义与业务码。
type APIError struct {
	status  int
	code    string
	message string
}

// newAPIError 统一冻结领域错误的 HTTP 合同，避免每个模块重复拼写状态码与业务码。
func newAPIError(status int, code, message string) *APIError {
	return &APIError{status: status, code: code, message: message}
}

// Error 实现 error 接口，便于业务代码直接返回预定义错误。
func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

// StatusCode 返回 API 契约中固定的 HTTP 状态码。
func (e *APIError) StatusCode() int {
	return e.status
}

// Code 返回供客户端区分引导动作的稳定业务码。
func (e *APIError) Code() string {
	return e.code
}

// Message 返回默认的、面向用户的错误说明。
func (e *APIError) Message() string {
	return e.message
}

// Details 当前门禁错误不暴露内部细节，保持现网错误 envelope 的 null 语义。
func (e *APIError) Details() any {
	return nil
}

var (
	// ErrAccountBindingRequired 表示游客需要在原用户上完成绑定，不能创建新用户。
	ErrAccountBindingRequired = newAPIError(403, "ACCOUNT_BINDING_REQUIRED", "请先完成账号绑定")
	// ErrVIPRequired 表示当前模板能力仅向 VIP 用户开放。
	ErrVIPRequired = newAPIError(403, "VIP_REQUIRED", "该功能需要 VIP 权益")
	// ErrInsufficientFunds 表示没有可用日免额度，且自有钻石余额不足。
	ErrInsufficientFunds = newAPIError(402, "INSUFFICIENT_FUNDS", "钻石余额不足")
	// ErrUnauthenticated 表示缺失、格式错误或已失效的 Go 自有会话。
	ErrUnauthenticated = newAPIError(http.StatusUnauthorized, "UNAUTHORIZED", "Authentication required")
	// ErrServiceUnavailable 表示本地会话依赖不可用，不能误判为用户会话无效。
	ErrServiceUnavailable = newAPIError(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	// ErrInvalidRequest 表示账号入口 JSON 或字段不满足已冻结的请求合同。
	ErrInvalidRequest = newAPIError(http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
	// ErrInvalidCredentials 同时表示未知邮箱和错误密码，避免泄露账号存在性。
	ErrInvalidCredentials = newAPIError(http.StatusUnauthorized, "INVALID_CREDENTIALS", "Invalid credentials")
	// ErrEmailAlreadyRegistered 表示正常或封禁账号已占用邮箱。
	ErrEmailAlreadyRegistered = newAPIError(http.StatusConflict, "EMAIL_ALREADY_REGISTERED", "Email already registered")
	// ErrWebGuestLoginDisabled 表示 Web 平台不允许创建设备游客。
	ErrWebGuestLoginDisabled = newAPIError(http.StatusForbidden, "WEB_GUEST_LOGIN_DISABLED", "Web guest login is disabled")
)
