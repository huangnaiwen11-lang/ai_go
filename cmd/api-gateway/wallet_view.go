package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	bizwalletview "ai-business-service/internal/biz/walletview"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/transport/sessionauth"
	transportwalletview "ai-business-service/internal/transport/walletview"
)

// newConfiguredWalletViewHandler 装配钱包读取专用的 Go 自有 MongoDB、会话和只读用例。
// 它不会装配 PayCores、支付订单、Node 钱包或生成预扣路径。
func newConfiguredWalletViewHandler(dataConfig *conf.Data, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, fmt.Errorf("Go session authenticator is required for wallet view")
	}
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
	repository := data.NewWalletViewRepository(storage)
	return transportwalletview.NewHandler(authenticator, bizwalletview.NewUsecase(repository)), cleanup, nil
}

// newOptionalWalletViewHandler 在钱包读取和 Go 会话两个开关均已汇总为 enabled 前，
// 不读取配置、不连接 Mongo；默认完全保留 Node 的钱包读取路径。
func newOptionalWalletViewHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil || bootstrap == nil {
		return nil, nil, err
	}
	return newConfiguredWalletViewHandler(bootstrap.GetData(), authenticator)
}
