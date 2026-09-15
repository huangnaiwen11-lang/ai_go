package server

import (
	"encoding/json"
	"errors"
	stdhttp "net/http"

	kratosErrors "github.com/go-kratos/kratos/v3/errors"
	kratosHTTP "github.com/go-kratos/kratos/v3/transport/http"
	"github.com/google/uuid"
)

// httpContractError 是业务层与传输层之间的最小错误契约。
// server 只依赖该接口，不依赖任何具体业务模块或数据库实现。
type httpContractError interface {
	error
	StatusCode() int
	Code() string
	Message() string
	Details() any
}

// successEnvelope 与现网成功响应保持同一结构。
type successEnvelope struct {
	Success bool `json:"success"`
	Data    any  `json:"data"`
}

// errorEnvelope 与现网失败响应保持同一结构。
// 业务码位于根层，不能嵌套到 error 字段中。
type errorEnvelope struct {
	Success   bool   `json:"success"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Details   any    `json:"details"`
	RequestID string `json:"requestId"`
}

// EncodeResponse 将所有普通业务响应包装为稳定的成功 envelope。
func EncodeResponse(writer stdhttp.ResponseWriter, _ *stdhttp.Request, value any) error {
	return encodeJSON(writer, successEnvelope{Success: true, Data: value})
}

// EncodeError 把领域错误或 Kratos 错误转换为现网失败 envelope。
// 业务错误只负责语义，JSON 形状、请求标识和 HTTP 状态统一由这里保证。
func EncodeError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error) {
	status, code, message, details := classifyError(err)
	requestID := normalizeRequestID(request.Header.Get(requestIDHeader))
	if requestID == "" {
		requestID = uuid.NewString()
	}

	writer.Header().Set(requestIDHeader, requestID)
	// HTTP 头必须在 WriteHeader 之前写入；否则真实客户端看不到 JSON 类型。
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(errorEnvelope{
		Success:   false,
		Code:      code,
		Message:   message,
		Details:   details,
		RequestID: requestID,
	})
}

func classifyError(err error) (status int, code, message string, details any) {
	var contractErr httpContractError
	if errors.As(err, &contractErr) {
		return contractErr.StatusCode(), contractErr.Code(), contractErr.Message(), contractErr.Details()
	}

	kratosErr := kratosErrors.FromError(err)
	return int(kratosErr.GetCode()), kratosErr.GetReason(), kratosErr.GetMessage(), nil
}

func encodeJSON(writer stdhttp.ResponseWriter, value any) error {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(writer).Encode(value)
}

// Compile-time assertions keep the adapter signatures aligned with Kratos.
var (
	_ kratosHTTP.EncodeResponseFunc = EncodeResponse
	_ kratosHTTP.EncodeErrorFunc    = EncodeError
)
