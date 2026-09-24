package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ai-business-service/internal/biz/runtimeapp"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
)

// newOptionalAppScopeResolver opens a dedicated read-only App lookup only
// after the scope feature and its target App have both been explicitly set.
// When disabled it must not load configuration or contact MongoDB.
func newOptionalAppScopeResolver(enabled bool, configPath string) (runtimeapp.Resolver, string, func(), error) {
	if !enabled {
		return nil, "", nil, nil
	}
	appID := strings.TrimSpace(os.Getenv("CLING_APP_ID"))
	if appID == "" {
		return nil, "", nil, fmt.Errorf("CLING_APP_ID must be set when GATEWAY_APP_SCOPE_ENABLED=true")
	}
	bootstrap, err := loadGatewayBootstrap(configPath)
	if err != nil {
		return nil, "", nil, err
	}
	if err := conf.ValidateConfiguredMongo(bootstrap.GetData()); err != nil {
		return nil, "", nil, err
	}
	storage, cleanup, err := data.NewData(bootstrap.GetData())
	if err != nil {
		return nil, "", nil, err
	}
	return newInitializedAppScopeResolver(storage, appID, cleanup)
}

func newInitializedAppScopeResolver(storage *data.Data, appID string, cleanup func()) (runtimeapp.Resolver, string, func(), error) {
	fail := func(cause error) (runtimeapp.Resolver, string, func(), error) {
		if cleanup != nil {
			cleanup()
		}
		return nil, "", nil, cause
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := data.NewRuntimeAppSchemaInitializer(storage).Ensure(ctx); err != nil {
		return fail(fmt.Errorf("initialize runtime App MongoDB schema: %w", err))
	}
	return data.NewRuntimeAppResolver(storage), appID, cleanup, nil
}
