package main

import (
	"net/http"

	bizfeedback "ai-business-service/internal/biz/feedback"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	transportfeedback "ai-business-service/internal/transport/feedback"
	"ai-business-service/internal/transport/sessionauth"
)

func newOptionalFeedbackHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
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
	usecase := bizfeedback.NewUsecase(data.NewFeedbackRepository(storage))
	return transportfeedback.NewHandler(authenticator, usecase), cleanup, nil
}
