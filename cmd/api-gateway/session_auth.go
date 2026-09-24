package main

import (
	"context"
	"fmt"
	"time"

	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/transport/sessionauth"
)

// newConfiguredSessionAuthenticator 只装配本机 rs0 的 Go 自有会话验证依赖。
// 它不注册公开路由，也不接受或转换 Node 的登录态。
func newConfiguredSessionAuthenticator(dataConfig *conf.Data) (*sessionauth.Authenticator, func(), error) {
	if err := conf.ValidateConfiguredMongo(dataConfig); err != nil {
		return nil, nil, err
	}
	storage, cleanup, err := data.NewData(dataConfig)
	if err != nil {
		return nil, nil, err
	}
	fail := func(cause error) (*sessionauth.Authenticator, func(), error) {
		cleanup()
		return nil, nil, cause
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := data.NewLocalSchemaInitializer(storage).Ensure(ctx); err != nil {
		return fail(fmt.Errorf("initialize local MongoDB schema: %w", err))
	}
	identityUsecase := identity.NewUsecase(
		data.NewUserRepository(storage),
		data.NewIdentityRepository(storage),
		data.NewSessionRepository(storage),
		data.NewTxRunner(storage),
	)
	return sessionauth.NewAuthenticator(identityUsecase), cleanup, nil
}

// newOptionalSessionAuthenticator 只在开关显式开启后读取受控本地配置并连接 MongoDB。
// 默认关闭时必须保持 Node 代理路径完全无感。
func newOptionalSessionAuthenticator(enabled bool, configPath string) (*sessionauth.Authenticator, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil {
		return nil, nil, err
	}
	if bootstrap == nil {
		return nil, nil, nil
	}
	return newConfiguredSessionAuthenticator(bootstrap.GetData())
}
