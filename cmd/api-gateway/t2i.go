package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/ledger"
	bizmedia "ai-business-service/internal/biz/media"
	"ai-business-service/internal/biz/t2i"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/transport/sessionauth"
	transportt2i "ai-business-service/internal/transport/t2i"
)

// newConfiguredPublicT2IHandler 装配独立本地 MongoDB 上的公开 T2I 业务依赖。
// 它不启动 Worker，也不请求审核或生成中台；投递仍由既有 Outbox 后续处理。
func newConfiguredPublicT2IHandler(dataConfig *conf.Data, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, errors.New("Go session authenticator is required for public T2I")
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
	creator := t2i.NewUsecase(data.NewFreeformRecipeRepository(storage), creationUsecase)
	// I2I 与素材上传共用同一份 Go 自有元数据仓储；创建前必须由它确认图片归属。
	userMedia := bizmedia.NewUsecase(data.NewLocalUserMediaRepository(storage, localMediaDirectory()))
	imageEditor := t2i.NewImageEditUsecase(data.NewImageEditRecipeRepository(storage), creationUsecase, userMedia)
	statuses := t2i.NewStatusUsecase(data.NewImageStatusRepository(storage))
	return newLocalImageTemplateCatalogHandler(transportt2i.NewHandler(authenticator, creator, statuses, imageEditor)), cleanup, nil
}

// newOptionalPublicT2IHandler 仅在会话和公开路由两个开关均启用后才读取配置和连接 MongoDB。
// 任何一个开关关闭都返回 nil，Gateway 将请求完全代理给 Node。
func newOptionalPublicT2IHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil {
		return nil, nil, err
	}
	if bootstrap == nil {
		return nil, nil, nil
	}
	return newConfiguredPublicT2IHandler(bootstrap.GetData(), authenticator)
}
