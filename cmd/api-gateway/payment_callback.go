package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	paycores "ai-business-service/internal/integrations/paycores"
	"ai-business-service/internal/transport/paymentcallback"
)

const (
	paymentCallbackPathLegacy = "/api/internal/payment-confirmed"
	paymentCallbackPathV1     = "/api/v1/internal/payment-confirmed"
)

// paymentCallbackDispatcher 将两个兼容路径分派到各自固定 nonce 作用域的用例。
// 这样 V1 与 legacy 的防重放记录不会因路径不同而失去可追溯性。
type paymentCallbackDispatcher struct {
	legacy http.Handler
	v1     http.Handler
}

func (dispatcher paymentCallbackDispatcher) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request != nil && request.URL != nil && request.URL.Path == paymentCallbackPathLegacy {
		dispatcher.legacy.ServeHTTP(writer, request)
		return
	}
	dispatcher.v1.ServeHTTP(writer, request)
}

// newPaymentCallbackHandler 只用 PayCores 专属密钥构造单一路径 Handler；禁止复用生成回调密钥。
func newPaymentCallbackHandler(security *conf.Security, callbackPath string, usecase *payments.ConfirmedCallbackUsecase) (http.Handler, error) {
	if security == nil || usecase == nil || (callbackPath != paymentCallbackPathLegacy && callbackPath != paymentCallbackPathV1) {
		return nil, errors.New("payment callback dependencies are unavailable")
	}
	verifier, err := paycores.NewCallbackVerifier(security.GetPaycoresCallbackHmacKey(), time.Now)
	if err != nil {
		return nil, err
	}
	return paymentcallback.NewHandler(verifier, usecase), nil
}

// newConfiguredPaymentCallback 只装配 Go 自有账本、订单、回执和 nonce 集合，绝不访问 Node 钱包。
func newConfiguredPaymentCallback(dataConfig *conf.Data, security *conf.Security) (http.Handler, func(), error) {
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
	repository := data.NewPaymentRepository(storage)
	nonces := data.NewPayCoresCallbackNonceRepository(storage)
	if repository == nil || nonces == nil {
		return fail(errors.New("payment callback repositories are unavailable"))
	}
	settler := payments.NewService(repository, time.Now)
	legacyUsecase := payments.NewConfirmedCallbackUsecase(nonces, repository, settler, paymentCallbackPathLegacy, time.Now)
	v1Usecase := payments.NewConfirmedCallbackUsecase(nonces, repository, settler, paymentCallbackPathV1, time.Now)
	legacyHandler, err := newPaymentCallbackHandler(security, paymentCallbackPathLegacy, legacyUsecase)
	if err != nil {
		return fail(err)
	}
	v1Handler, err := newPaymentCallbackHandler(security, paymentCallbackPathV1, v1Usecase)
	if err != nil {
		return fail(err)
	}
	return paymentCallbackDispatcher{legacy: legacyHandler, v1: v1Handler}, cleanup, nil
}

// newOptionalPaymentCallback 保证默认关闭时既不读配置、也不连接本地 MongoDB。
func newOptionalPaymentCallback(enabled bool, configPath string) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil || bootstrap == nil {
		return nil, nil, err
	}
	return newConfiguredPaymentCallback(bootstrap.GetData(), bootstrap.GetSecurity())
}
