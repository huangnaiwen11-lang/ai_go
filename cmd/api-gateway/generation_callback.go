package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/transport/generationcallback"

	kratosconfig "github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/file"
)

// newGenerationCallbackHandler 只把独立的回调密钥和已装配用例交给 HTTP
// 适配器；它不注册路由，也不启动任何后台循环。
func newGenerationCallbackHandler(security *conf.Security, usecase *bizgeneration.CallbackUsecase) (http.Handler, error) {
	if security == nil || usecase == nil {
		return nil, errors.New("generation callback dependencies are unavailable")
	}
	verifier, err := platform.NewCallbackVerifier(security.GetGenerationCallbackHmacKey(), time.Now)
	if err != nil {
		return nil, err
	}
	return generationcallback.NewHandler(verifier, usecase), nil
}

// newConfiguredGenerationCallbackHandler 装配本地 rs0 所需的回调事务依赖。
// 它不构造 Worker；调用方负责在进程退出时执行返回的清理函数。
func newConfiguredGenerationCallbackHandler(dataConfig *conf.Data, security *conf.Security) (http.Handler, func(), error) {
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
	tx := data.NewTxRunner(storage)
	ledgerUsecase := ledger.NewUsecase(data.NewLedgerRepository(storage), tx)
	callbackStore := data.NewGenerationCallbackRepository(storage)
	usecase := bizgeneration.NewCallbackUsecase(callbackStore, callbackStore, ledgerUsecase, tx)
	handler, err := newGenerationCallbackHandler(security, usecase)
	if err != nil {
		return fail(err)
	}
	return handler, cleanup, nil
}

// newOptionalGenerationCallback 默认不读取配置也不连接 Mongo，确保未显式开启时
// 两条内部回调继续落入 Gateway 的默认 Node 代理。
func newOptionalGenerationCallback(enabled bool, configPath string) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil {
		return nil, nil, err
	}
	if bootstrap == nil {
		return nil, nil, nil
	}
	return newConfiguredGenerationCallbackHandler(bootstrap.GetData(), bootstrap.GetSecurity())
}

// loadOptionalGatewayBootstrap 让开关关闭路径在任何文件读取、密钥处理或 Mongo
// 连接之前返回，开启路径才加载既有本地配置。
func loadOptionalGatewayBootstrap(enabled bool, configPath string) (*conf.Bootstrap, error) {
	if !enabled {
		return nil, nil
	}
	return loadGatewayBootstrap(configPath)
}

// loadGatewayBootstrap 复用受控本地配置校验，避免 Gateway 绕开回调密钥和 rs0
// 隔离约束。它只读取本地文件，不改写任何运行配置。
func loadGatewayBootstrap(path string) (*conf.Bootstrap, error) {
	configSource := kratosconfig.New(kratosconfig.WithSource(file.NewSource(path)))
	defer configSource.Close()
	if err := configSource.Load(); err != nil {
		return nil, err
	}
	bootstrap := new(conf.Bootstrap)
	if err := configSource.Scan(bootstrap); err != nil {
		return nil, err
	}
	if err := conf.Validate(bootstrap); err != nil {
		return nil, err
	}
	return bootstrap, nil
}
