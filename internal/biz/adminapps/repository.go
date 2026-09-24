// Package adminapps 定义管理后台 Apps 投影的读写契约。
//
// 读侧是「已迁移的只读投影」；写侧（Create/Update/Delete）照 Node
// `modules/app/app.service.js` 的语义平移，但有一处**有意收窄**：
// 不做服务端 nativeConfig 默认模板生成（那会拖入 pricing / nativeShell 两个配置域），
// 因此 nativeConfig 按「透传 + 就地校验」处理。收窄点逐条写在 nativeconfig.go。
package adminapps

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("admin apps: invalid input")
	ErrNotFound = errors.New("admin apps: not found")
	// ErrExternalUnavailable is intentional: no local request may claim an
	// upload, live verification, CI trigger, or registrar operation completed
	// while its external integration is not configured.
	ErrExternalUnavailable = errors.New("admin apps: external integration unavailable")
)

// Document 是归一化后的 App 字段集合，键名与 Node 文档一致（camelCase）。
type Document map[string]any

// Actor 是执行写操作的管理员身份。
type Actor struct {
	ID   string
	Role string
}

type Query struct {
	Page, Limit      int
	Platform, Status string
	Search           string
}

type App struct {
	ID             string
	Name           string
	Description    string
	Platform       string
	Status         string
	ClientID       string
	BundleID       string
	PackageName    string
	Domain         string
	APIURL         string
	ResolvedAPIURL string
	Fields         map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Page struct {
	Apps  []App
	Total int64
}

type Overview struct {
	Total, Active, Suspended int64
	ByPlatform               map[string]int64
}

type Repository interface {
	List(context.Context, Query) (Page, error)
	Get(context.Context, string) (App, error)
	Overview(context.Context) (Overview, error)
	// Create 收**原始请求体**而不是校验后的文档：仓储是持久化边界，
	// 归一化与校验在这里再走一次，调用方漏调也不能让脏数据落库。
	Create(context.Context, Actor, map[string]any) (App, error)
	Update(context.Context, Actor, string, map[string]any) (App, error)
	// Delete 是软删除（把 status 置为 deprecated），与 Node 一致 —— 不是物理删除。
	Delete(context.Context, Actor, string) (App, error)
	// PackageConfig exports the build inputs already stored on an App. It never
	// contacts a build service or returns secret references.
	PackageConfig(context.Context, string) (Document, error)
	UpdatePackageConfig(context.Context, Actor, string, map[string]any) (App, Document, error)
	NativeConfigTemplate(context.Context, string) (Document, error)
	SetReviewMode(context.Context, Actor, string, map[string]any) (Document, error)
	IntegrationCheck(context.Context, string) (Document, error)
	CreateBuildRequest(context.Context, Actor, string, map[string]any) (Document, error)
	APIDomainCandidates(context.Context, string, map[string]any) (Document, error)
	RegisterAPIDomain(context.Context, Actor, string, map[string]any) (Document, error)
	LegalPages(context.Context, string) (Document, error)
	UpdateLegalPublishing(context.Context, Actor, string, map[string]any) (Document, error)
	EnableManagedLegalPublishing(context.Context, Actor, string) (Document, error)
	LegalAction(context.Context, Actor, string, string, string, map[string]any) (Document, error)
	ListPlatformConfigs(context.Context, bool) ([]Document, error)
	GetPlatformConfig(context.Context, string, string) (Document, error)
	UpdatePlatformConfig(context.Context, Actor, string, string, map[string]any) (Document, error)
	DeletePlatformConfig(context.Context, Actor, string, string) error
	PatchPlatformConfig(context.Context, Actor, string, string, string, map[string]any) (Document, error)
	ClonePlatformConfig(context.Context, Actor, string, map[string]any) (Document, error)
	PlatformPreview(context.Context, string, string, string) (Document, error)
}

func NormalizeQuery(query Query) (Query, error) {
	if query.Page == 0 {
		query.Page = 1
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Page < 1 || query.Page > 10000 || query.Limit < 1 || query.Limit > 100 || (query.Page-1)*query.Limit > 100000 {
		return Query{}, ErrInvalid
	}
	if query.Platform != "" && query.Platform != "ios" && query.Platform != "android" && query.Platform != "web" {
		return Query{}, ErrInvalid
	}
	query.Search = strings.TrimSpace(query.Search)
	return query, nil
}
