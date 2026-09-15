package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	bizworks "ai-business-service/internal/biz/works"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/transport/sessionauth"
	transportworks "ai-business-service/internal/transport/works"
)

// newConfiguredWorksViewHandler 装配作品历史专用的 Go 自有 MongoDB、会话和只读用例。
// 它不装配生成提交器、回调客户端、支付或旧 Node 作品读取。
func newConfiguredWorksViewHandler(dataConfig *conf.Data, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, fmt.Errorf("Go session authenticator is required for works view")
	}
	if err := conf.ValidateLocalMongo(dataConfig); err != nil {
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
	repository := data.NewWorksRepository(storage)
	return transportworks.NewHandler(authenticator, bizworks.NewUsecase(repository)), cleanup, nil
}

// newOptionalWorksViewHandler 只有 Go 会话和作品读取独立开关同时汇总为 enabled 后才连接 MongoDB。
// 默认不读取配置、不接管 Node 的作品历史路径。
func newOptionalWorksViewHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil || bootstrap == nil {
		return nil, nil, err
	}
	return newConfiguredWorksViewHandler(bootstrap.GetData(), authenticator)
}
