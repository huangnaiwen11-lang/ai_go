package main

import (
	"net/http"

	biznotification "ai-business-service/internal/biz/notification"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	transportnotification "ai-business-service/internal/transport/notification"
	"ai-business-service/internal/transport/sessionauth"
)

// newOptionalNotificationHandler 只在本地会话和通知开关同时开启时构造 Mongo 处理器。
// 未开启时返回 nil，由 Gateway 继续透明代理旧 Node，避免半切流读到两套通知事实。
func newOptionalNotificationHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if !enabled || authenticator == nil {
		return nil, nil, nil
	}
	bootstrap, err := loadGatewayBootstrap(configPath)
	if err != nil {
		return nil, nil, err
	}
	if err := conf.ValidateLocalMongo(bootstrap.GetData()); err != nil {
		return nil, nil, err
	}
	storage, cleanup, err := data.NewData(bootstrap.GetData())
	if err != nil {
		return nil, nil, err
	}
	return transportnotification.NewHandler(authenticator, biznotification.NewUsecase(data.NewNotificationRepository(storage))), cleanup, nil
}

