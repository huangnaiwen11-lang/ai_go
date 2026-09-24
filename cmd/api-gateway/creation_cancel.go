package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"ai-business-service/internal/biz/generationcancel"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	transportcreationcancel "ai-business-service/internal/transport/creationcancel"
	"ai-business-service/internal/transport/sessionauth"
)

// newConfiguredCreationCancelHandler only assembles Go-owned creation facts,
// the durable cancel request and the Go session identity. It has no terminal,
// ledger or provider client dependency, so accepting a user request cannot
// synchronously settle funds or fabricate a provider outcome.
func newConfiguredCreationCancelHandler(dataConfig *conf.Data, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, fmt.Errorf("Go session authenticator is required for creation cancellation")
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
	usecase := generationcancel.NewUsecase(data.NewGenerationCancelStore(storage), data.NewTxRunner(storage))
	return transportcreationcancel.NewHandler(authenticator, usecase), cleanup, nil
}

// newOptionalCreationCancelHandler keeps the user cancellation route entirely
// on Node until both main has enabled its two gates and a valid local bootstrap
// is available. The disabled path must not read config or contact MongoDB.
func newOptionalCreationCancelHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil || bootstrap == nil {
		return nil, nil, err
	}
	return newConfiguredCreationCancelHandler(bootstrap.GetData(), authenticator)
}
