//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.

package main

import (
	"log/slog"

	"ai-business-service/internal/biz"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	contentreviewintegration "ai-business-service/internal/integrations/contentreview"
	generationintegration "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/server"
	"ai-business-service/internal/worker"

	"github.com/go-kratos/kratos/v3"
	"github.com/google/wire"
)

// wireApp init kratos application.
func wireApp(*conf.Server, *conf.Data, *conf.Security, *conf.Integrations, *slog.Logger) (*kratos.App, func(), error) {
	panic(wire.Build(server.ProviderSet, biz.ProviderSet, data.ProviderSet, generationintegration.ProviderSet, contentreviewintegration.ProviderSet, worker.ProviderSet, newApp))
}
