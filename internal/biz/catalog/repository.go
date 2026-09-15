package catalog

import (
	"context"
	"errors"
)

var (
	// ErrInvalidClientPlatform 表示调用方平台不在三端白名单中。
	ErrInvalidClientPlatform = errors.New("catalog: invalid client platform")
)

// TemplateRepository 是模板读取的反转依赖边界。
// 它只提供已启用模板，不向领域层暴露 MongoDB 查询或 BSON 类型。
type TemplateRepository interface {
	ListEnabled(context.Context) ([]Template, error)
}
