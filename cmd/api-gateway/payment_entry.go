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
	"ai-business-service/internal/integrations/appstore"
	paycores "ai-business-service/internal/integrations/paycores"
	"ai-business-service/internal/transport/localpayment"
	"ai-business-service/internal/transport/sessionauth"

	"github.com/google/uuid"
)

// newConfiguredPaymentEntryHandler 装配仅依赖 Go 自有 Mongo、会话和受控本地回执的支付入口。
// 它不访问 Node 钱包、PayCores、Apple、Google 或任意真实商店密钥。
func newConfiguredPaymentEntryHandler(dataConfig *conf.Data, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	return newConfiguredPaymentEntryHandlerWithCreator(dataConfig, authenticator, nil)
}

// newConfiguredPaymentEntryHandlerWithCreator 复用本地支付存储装配；只有显式传入创建器时
// 才将建单入口切到 PayCores，默认仍保持 local_only 测试收银台。
func newConfiguredPaymentEntryHandlerWithCreator(dataConfig *conf.Data, authenticator *sessionauth.Authenticator, creator payments.PayCoresCheckoutCreator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, errors.New("Go session authenticator is required for local payment entry")
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
	repository, ok := data.NewPaymentRepository(storage).(payments.CheckoutRepository)
	if !ok || repository == nil {
		return fail(errors.New("local payment repository is unavailable"))
	}
	checkout := payments.NewCheckoutServiceWithPayCores(repository, creator, time.Now)
	if creator != nil {
		return localpayment.NewHandler(authenticator, checkout, appstore.LocalVerifier{}, checkout), cleanup, nil
	}
	return localpayment.NewHandler(authenticator, checkout, appstore.LocalVerifier{}), cleanup, nil
}

// paycoresCheckoutCreator 只将领域层冻结的字段转换为 PayCores HTTP 请求。
// 该适配器不读取余额、VIP、Node 用户资料或任何客户端价格。
type paycoresCheckoutCreator struct {
	client    *paycores.Client
	returnURL string
	cancelURL string
}

// newGatewayPayCoresNonce 为每次出站建单生成独立的 V2 防重放 nonce。
func newGatewayPayCoresNonce() string { return uuid.NewString() }

func (creator paycoresCheckoutCreator) CreatePayCoresOrder(ctx context.Context, request payments.PayCoresCheckoutRequest) (payments.PayCoresCheckoutResult, error) {
	result, err := creator.client.CreateOrder(ctx, paycores.CreateOrderRequest{
		UserID: request.UserID, ProductID: request.ProductID, AmountCents: request.AmountCents, Credits: request.Credits,
		Label: request.Label, ClientRequestID: request.ClientRequestID, ReturnURL: creator.returnURL, CancelURL: creator.cancelURL,
	})
	if err != nil {
		return payments.PayCoresCheckoutResult{}, err
	}
	return payments.PayCoresCheckoutResult{ProviderOrderID: result.OrderID, CheckoutURL: result.CheckoutURL}, nil
}

// newConfiguredPayCoresPaymentEntryHandler 只用于受控本地 mock，不允许它隐式启用真实支付。
func newConfiguredPayCoresPaymentEntryHandler(bootstrap *conf.Bootstrap, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if bootstrap == nil || bootstrap.GetSecurity() == nil || bootstrap.GetIntegrations() == nil || bootstrap.GetIntegrations().GetPaycores() == nil {
		return nil, nil, errors.New("PayCores local checkout config is unavailable")
	}
	paycoresConfig := bootstrap.GetIntegrations().GetPaycores()
	client, err := paycores.NewClient(paycoresConfig.GetBaseUrl(), bootstrap.GetSecurity().GetPaycoresRequestHmacKey(), nil, time.Now, newGatewayPayCoresNonce)
	if err != nil {
		return nil, nil, err
	}
	creator := paycoresCheckoutCreator{client: client, returnURL: paycoresConfig.GetReturnUrl(), cancelURL: paycoresConfig.GetCancelUrl()}
	return newConfiguredPaymentEntryHandlerWithCreator(bootstrap.GetData(), authenticator, creator)
}

// newOptionalPaymentEntryHandler 在三重外部开关已被 main 汇总为 enabled 后才加载配置。
// 默认关闭保证现网 Node 钱包、IAP 和收银台请求完全不受 Go 进程影响。
func newOptionalPaymentEntryHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil || bootstrap == nil {
		return nil, nil, err
	}
	return newConfiguredPaymentEntryHandler(bootstrap.GetData(), authenticator)
}

// newOptionalPayCoresPaymentEntryHandler 是本地 mock 的额外门禁，关闭时不读取配置。
func newOptionalPayCoresPaymentEntryHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil || bootstrap == nil {
		return nil, nil, err
	}
	return newConfiguredPayCoresPaymentEntryHandler(bootstrap, authenticator)
}
