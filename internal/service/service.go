// Package service 仅负责将外部 DTO 转换为业务 DO。
package service

import "github.com/google/wire"

// ProviderSet 是传输适配层的依赖注入入口。
// 当前没有暴露业务接口，后续每个 Cling 资源在此注册独立适配器。
var ProviderSet = wire.NewSet()
