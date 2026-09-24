package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	bizmedia "ai-business-service/internal/biz/media"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	transportmedia "ai-business-service/internal/transport/media"
	"ai-business-service/internal/transport/sessionauth"
)

// localMediaDirectory 返回可替换的本地运行目录。它不位于源码 data/ 下，避免上传二进制
// 混入工程文件；生产对象存储接入时只替换 data 层适配器。
func localMediaDirectory() string {
	if configured := strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_MEDIA_DIR")); configured != "" {
		return configured
	}
	return filepath.Join(os.TempDir(), "ai-business-service-media")
}

func newConfiguredLocalMediaHandler(dataConfig *conf.Data, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, fmt.Errorf("Go session authenticator is required for local media")
	}
	if err := conf.ValidateConfiguredMongo(dataConfig); err != nil {
		return nil, nil, err
	}
	storage, cleanup, err := data.NewData(dataConfig)
	if err != nil {
		return nil, nil, err
	}
	fail := func(cause error) (http.Handler, func(), error) { cleanup(); return nil, nil, cause }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := data.NewLocalSchemaInitializer(storage).Ensure(ctx); err != nil {
		return fail(fmt.Errorf("initialize local MongoDB schema: %w", err))
	}
	repository := data.NewLocalUserMediaRepository(storage, localMediaDirectory())
	return transportmedia.NewHandler(authenticator, bizmedia.NewUsecase(repository)), cleanup, nil
}

// newOptionalLocalMediaHandler 保持默认关闭；只有 Go 自有会话和明确素材开关均打开后，
// Gateway 才能接管 Node 的上传路径。
func newOptionalLocalMediaHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil {
		return nil, nil, err
	}
	if bootstrap == nil {
		return nil, nil, nil
	}
	return newConfiguredLocalMediaHandler(bootstrap.GetData(), authenticator)
}
