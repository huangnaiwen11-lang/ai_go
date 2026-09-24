package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/integrations/polarstarb2b"
	"ai-business-service/internal/transport/providercallback"
)

// newProviderCallbackHandler 只把 B2B 回调密钥与已装配用例交给 HTTP 适配器；
// 它不注册路由，也不启动任何后台循环。
func newProviderCallbackHandler(integrations *conf.Integrations, usecase *bizgeneration.ProviderCallbackUsecase) (http.Handler, error) {
	if integrations == nil || usecase == nil {
		return nil, errors.New("provider callback dependencies are unavailable")
	}
	b := integrations.GetGeneration().GetPolarstarB2B()
	if b == nil {
		return nil, errors.New("polarstar b2b is not configured")
	}
	// 只有 webhook 交付模式才会收到回调。lookup_only 下平台不会向我们投递，
	// 此时构造验签器只会让一条永远不该被使用的入口看起来是通的。
	if b.GetDeliveryMode() != "webhook" {
		return nil, errors.New("provider callback requires polarstar b2b webhook delivery mode")
	}
	verifier, err := polarstarb2b.NewCallbackVerifierWithSecrets(b.GetCallbackSecret(), b.GetCallbackPreviousSecret(), b.GetTenantId(), b.GetAccountRef())
	if err != nil {
		return nil, err
	}
	return providercallback.NewHandler(verifier, usecase), nil
}

// newConfiguredProviderCallbackHandler 装配本地 rs0 上的 B2B 回调落库依赖。
// 它只写 inbox 与恢复事件，不做终态 CAS；调用方负责在进程退出时执行清理函数。
func newConfiguredProviderCallbackHandler(dataConfig *conf.Data, integrations *conf.Integrations) (http.Handler, func(), error) {
	if err := conf.ValidateConfiguredMongo(dataConfig); err != nil {
		return nil, nil, err
	}
	storage, cleanup, err := data.NewData(dataConfig)
	if err != nil {
		return nil, nil, err
	}
	fail := func(cause error) (http.Handler, func(), error) {
		cleanup()
		return nil, nil, cause
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := data.NewLocalSchemaInitializer(storage).Ensure(ctx); err != nil {
		return fail(fmt.Errorf("initialize local MongoDB schema: %w", err))
	}
	usecase := bizgeneration.NewProviderCallbackUsecase(
		data.NewGenerationProviderInboxRepository(storage),
		data.NewTxRunner(storage),
	)
	handler, err := newProviderCallbackHandler(integrations, usecase)
	if err != nil {
		return fail(err)
	}
	return handler, cleanup, nil
}

// newOptionalProviderCallback 默认不读取配置也不连接 Mongo，确保未显式开启时
// B2B 回调路径继续落入 Gateway 的默认 Node 代理。
func newOptionalProviderCallback(enabled bool, configPath string) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil {
		return nil, nil, err
	}
	if bootstrap == nil {
		return nil, nil, nil
	}
	return newConfiguredProviderCallbackHandler(bootstrap.GetData(), bootstrap.GetIntegrations())
}
