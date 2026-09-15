package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/ledger"
	bizmedia "ai-business-service/internal/biz/media"
	bizvideo "ai-business-service/internal/biz/video"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/transport/sessionauth"
	transportvideo "ai-business-service/internal/transport/video"
)

// newConfiguredPublicVideoHandler 装配独立本地 MongoDB 上的模板视频业务依赖。
// 它只写创作、预扣与 Outbox，不启动 Worker，也不请求真实生成中台。
func newConfiguredPublicVideoHandler(dataConfig *conf.Data, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, errors.New("Go session authenticator is required for public video")
	}
	if err := conf.ValidateLocalMongo(dataConfig); err != nil {
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
	tx := data.NewTxRunner(storage)
	ledgerUsecase := ledger.NewUsecase(data.NewLedgerRepository(storage), tx)
	creationUsecase := creations.NewUsecase(
		creations.NewUserReaderAdapter(data.NewUserRepository(storage)),
		data.NewSubscriptionRepository(storage),
		entitlement.NewUsecase(),
		data.NewCreationRepository(storage),
		creations.NewReservationUsecaseAdapter(ledgerUsecase),
		data.NewOutboxRepository(storage),
		tx,
	)
	// 与 I2I 复用同一份 Go 自有素材仓储：I2V 不能把任意外链直接编译进生成快照。
	userMedia := bizmedia.NewUsecase(data.NewLocalUserMediaRepository(storage, localMediaDirectory()))
	creator := bizvideo.NewUsecase(data.NewVideoTemplateRepository(storage), creationUsecase, userMedia)
	statuses := bizvideo.NewStatusUsecase(data.NewVideoStatusRepository(storage))
	return newLocalVideoTemplateCatalogHandler(authenticator, catalog.NewUsecase(data.NewTemplateRepository(storage)), transportvideo.NewHandler(authenticator, creator, statuses)), cleanup, nil
}

// newOptionalPublicVideoHandler 仅在 Go 会话与公开视频开关都启用后读取配置并连接 MongoDB。
// 任一开关关闭时返回 nil，Gateway 对所有视频请求保持透明 Node 代理。
func newOptionalPublicVideoHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil {
		return nil, nil, err
	}
	if bootstrap == nil {
		return nil, nil, nil
	}
	return newConfiguredPublicVideoHandler(bootstrap.GetData(), authenticator)
}
