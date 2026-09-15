package admincontract

import "errors"

var (
	// ErrMigrationNotAllowed 表示静态审计资料被错误用于启用 Vue 或 Go Admin 迁移。
	// 它只属于本地门禁，绝不能作为浏览器接口的响应。
	ErrMigrationNotAllowed = errors.New("admin contract: migration is not allowed")

	// ErrInvalidStaticContract 表示入口、权限或审计语义偏离当前静态基线。错误信息不
	// 包含管理员、scope、页面或审计载荷，避免扩大敏感后台信息的可见范围。
	ErrInvalidStaticContract = errors.New("admin contract: static contract is invalid")
)

const (
	// ClassificationPending 表示资料仍只来自静态源码审计。
	ClassificationPending = "待核实"
	// MigrationNotGranted 是当前静态合同唯一允许的迁移许可状态。
	MigrationNotGranted = "未授予"
	// SourceScopeStaticAudit 防止静态资料与未来运行证据混淆。
	SourceScopeStaticAudit = "仅静态源码审计"
)

// AccessBaseline 记录当前 canAccessAdminPath 的三条权限分支。它仅描述已有语义，
// 不替代未来服务端 RBAC 设计或真实访问验证。
type AccessBaseline struct {
	SuperAdminAllPaths               bool
	LegacyAdminWithoutScopesAllPaths bool
	ScopedAdminRequiresPathScope     bool
}

// Manifest 是管理后台静态合同的最小可验证表示。VueShellEnabled 和 GoRouteEnabled
// 是专门用来捕获误放行的开关；在缺少运行证据时它们必须始终保持 false。
type Manifest struct {
	Classification                        string
	MigrationPermission                   string
	SourceScope                           string
	VueShellEnabled                       bool
	GoRouteEnabled                        bool
	BrowserEntry                          string
	APIEntry                              string
	Access                                AccessBaseline
	AuditWriteFailureDoesNotBlockBusiness bool
}

// ValidateManifest 校验当前本地静态合同没有超出已审核范围。通过只说明入口、权限
// 分支和审计语义被记录一致；它不授予 Vue、Go Admin API、页面分流或高风险写入许可。
func ValidateManifest(manifest Manifest) error {
	if manifest.MigrationPermission != MigrationNotGranted || manifest.VueShellEnabled || manifest.GoRouteEnabled {
		return ErrMigrationNotAllowed
	}
	if manifest.Classification != ClassificationPending || manifest.SourceScope != SourceScopeStaticAudit {
		return ErrInvalidStaticContract
	}
	if manifest.BrowserEntry != "/admin/" || manifest.APIEntry != "/api" {
		return ErrInvalidStaticContract
	}
	if !manifest.Access.SuperAdminAllPaths ||
		!manifest.Access.LegacyAdminWithoutScopesAllPaths ||
		!manifest.Access.ScopedAdminRequiresPathScope ||
		!manifest.AuditWriteFailureDoesNotBlockBusiness {
		return ErrInvalidStaticContract
	}

	return nil
}
