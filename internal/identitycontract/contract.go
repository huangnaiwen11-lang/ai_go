package identitycontract

import "errors"

var (
	// ErrMigrationNotAllowed 表示静态合同被错误标记为可迁移，或有人试图在未放行时
	// 登记 Go 分流。调用方只可据此停止后续操作，不能将其转为对外 HTTP 错误。
	ErrMigrationNotAllowed = errors.New("identity contract: migration is not allowed")

	// ErrInvalidStaticContract 表示静态合同偏离已冻结的路由或会话基线。错误不携带
	// 具体路径、令牌或用户资料，避免审计工具把敏感上下文写入输出。
	ErrInvalidStaticContract = errors.New("identity contract: static contract is invalid")
)

const (
	// ClassificationPending 说明材料仅来自静态源码，不能证明运行时语义。
	ClassificationPending = "待核实"
	// MigrationNotGranted 是本地静态审计的唯一许可状态。
	MigrationNotGranted = "未授予"
	// SourceScopeStaticAudit 防止其他来源的材料被当作本地静态合同使用。
	SourceScopeStaticAudit = "仅静态源码审计"
)

// Route 描述一条仍需保留的身份接口及其 Node 兼容别名。它只保存 method/path，
// 不保存请求体、响应正文、JWT、OAuth code 或账户标识。
type Route struct {
	Method  string
	Path    string
	Aliases []string
}

// LogoutBaseline 记录前端当前的登出语义：仅清理本地 token，不向服务端请求 logout。
// 迁移不得把该客户端行为悄然改成服务端会话撤销。
type LogoutBaseline struct {
	ClearsLocalToken bool
	CallsServer      bool
}

// Manifest 是 Identity 静态合同的最小可验证表示。GoRouteEnabled 仅用于显式捕获
// 错误配置；合法的静态合同必须保持 false。
type Manifest struct {
	Classification      string
	MigrationPermission string
	SourceScope         string
	GoRouteEnabled      bool
	Routes              []Route
	Logout              LogoutBaseline
}

type routeKey struct {
	method string
	path   string
}

var requiredRoutes = map[routeKey]string{
	{method: "POST", path: "/api/auth/register"}:               "/api/v1/auth/register",
	{method: "POST", path: "/api/auth/login"}:                  "/api/v1/auth/login",
	{method: "GET", path: "/api/auth/me"}:                      "/api/v1/auth/me",
	{method: "POST", path: "/api/auth/guest"}:                  "/api/v1/auth/guest",
	{method: "PUT", path: "/api/auth/me/push-token"}:           "/api/v1/auth/me/push-token",
	{method: "DELETE", path: "/api/auth/me/push-token"}:        "/api/v1/auth/me/push-token",
	{method: "POST", path: "/api/auth/me/signup-bonus-repair"}: "/api/v1/auth/me/signup-bonus-repair",
	{method: "POST", path: "/api/auth/email/send-magic-link"}:  "/api/v1/auth/email/send-magic-link",
	{method: "GET", path: "/api/auth/email/verify"}:            "/api/v1/auth/email/verify",
	{method: "GET", path: "/api/auth/google"}:                  "/api/v1/auth/google",
	{method: "GET", path: "/api/auth/apple"}:                   "/api/v1/auth/apple",
	{method: "POST", path: "/api/auth/apple/callback"}:         "/api/v1/auth/apple/callback",
	{method: "GET", path: "/api/auth/facebook"}:                "/api/v1/auth/facebook",
	{method: "GET", path: "/api/auth/facebook/upgrade"}:        "/api/v1/auth/facebook/upgrade",
	{method: "GET", path: "/api/auth/facebook/callback"}:       "/api/v1/auth/facebook/callback",
	{method: "GET", path: "/api/auth/twitter"}:                 "/api/v1/auth/twitter",
	{method: "GET", path: "/api/auth/twitter/callback"}:        "/api/v1/auth/twitter/callback",
	{method: "GET", path: "/api/auth/google/upgrade"}:          "/api/v1/auth/google/upgrade",
	{method: "GET", path: "/api/auth/google/callback"}:         "/api/v1/auth/google/callback",
	{method: "POST", path: "/api/auth/native/apple"}:           "/api/v1/auth/native/apple",
	{method: "POST", path: "/api/auth/native/google"}:          "/api/v1/auth/native/google",
}

// ValidateManifest 校验当前静态合同没有超出已审核范围。通过只表示本地资料与静态
// 源码基线一致；它不代表登录、OAuth、JWT、账号状态或任何流量切换已经获得许可。
func ValidateManifest(manifest Manifest) error {
	if manifest.MigrationPermission != MigrationNotGranted || manifest.GoRouteEnabled {
		return ErrMigrationNotAllowed
	}
	if manifest.Classification != ClassificationPending || manifest.SourceScope != SourceScopeStaticAudit {
		return ErrInvalidStaticContract
	}
	if !manifest.Logout.ClearsLocalToken || manifest.Logout.CallsServer {
		return ErrInvalidStaticContract
	}
	if len(manifest.Routes) != len(requiredRoutes) {
		return ErrInvalidStaticContract
	}

	seen := make(map[routeKey]struct{}, len(manifest.Routes))
	for _, route := range manifest.Routes {
		key := routeKey{method: route.Method, path: route.Path}
		expectedAlias, known := requiredRoutes[key]
		if !known || len(route.Aliases) != 1 || route.Aliases[0] != expectedAlias {
			return ErrInvalidStaticContract
		}
		if _, duplicated := seen[key]; duplicated {
			return ErrInvalidStaticContract
		}
		seen[key] = struct{}{}
	}

	return nil
}
