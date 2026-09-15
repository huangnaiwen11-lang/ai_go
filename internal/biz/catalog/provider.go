// Package catalog 管理内容面、模板目录与模板编译。
package catalog

import "github.com/google/wire"

// ProviderSet 是模板目录模块的依赖注入入口。
// Web、iOS 与 Android 的模板一致性由 Usecase 保证，但不注册任何传输路由。
var ProviderSet = wire.NewSet(NewUsecase)
