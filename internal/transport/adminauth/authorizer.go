// Package adminauth 统一 /api/admin/ 下所有管理投影的授权入口与拒绝语义。
//
// 为什么要单独抽出来：网关对 /api/admin/ 是 fail-closed，命中前缀的请求全部由 Go 处理。
// 如果每个投影各自决定「认证失败」的响应码，就会出现同一前缀下
// 匿名访问有的返回 401、有的返回 403 的分裂 —— 前端只在 401 时清理令牌并跳转登录，
// 403 会被当成「已登录但没权限」留在页面上。
package adminauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

// ErrForbidden 表示身份已通过验证，但不是 admin/super_admin。
var ErrForbidden = errors.New("admin role required")

// ActorAuthorizer 在统一管理员门禁通过后返回经服务端会话验证的用户标识。
// 返回的 ID 只能用于服务端审计，绝不能由请求头、查询参数或请求体提供。
type ActorAuthorizer func(*http.Request) (string, error)

// Authorizer 复用主投影的同一套会话与角色判定。
// 先验证 Go 会话，再要求 admin 或 super_admin。任何一个投影都不能放宽该门禁。
func Authorizer(authenticator *sessionauth.Authenticator) func(*http.Request) error {
	withActor := AuthorizerWithActor(authenticator)
	return func(request *http.Request) error {
		_, err := withActor(request)
		return err
	}
}

// AuthorizerWithActor 在管理员门禁通过后返回真实会话 UserID，供审核类写操作记审计。
// 与 Authorizer 共用同一认证与角色判定，避免不同管理投影漂移。
func AuthorizerWithActor(authenticator *sessionauth.Authenticator) ActorAuthorizer {
	return func(request *http.Request) (string, error) {
		if authenticator == nil {
			return "", shared.ErrServiceUnavailable
		}
		identity, err := authenticator.Authenticate(request)
		if err != nil {
			return "", err
		}
		if identity == nil || strings.TrimSpace(identity.UserID) == "" {
			return "", shared.ErrUnauthenticated
		}
		if identity.Role != "admin" && identity.Role != "super_admin" {
			return "", ErrForbidden
		}
		return identity.UserID, nil
	}
}

// WriteDenial 把授权错误映射成稳定的 HTTP 语义并写出响应。
//
//	未认证 / 会话失效 -> 401 UNAUTHENTICATED（前端据此清理令牌并跳登录）
//	已认证但非管理员 -> 403 FORBIDDEN
//	依赖不可用       -> 503 SERVICE_UNAVAILABLE
func WriteDenial(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, shared.ErrUnauthenticated):
		failure(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
	case errors.Is(err, shared.ErrServiceUnavailable):
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	default:
		failure(w, http.StatusForbidden, "FORBIDDEN", "Admin access required")
	}
}

func failure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}
