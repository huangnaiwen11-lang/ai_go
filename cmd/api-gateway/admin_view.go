package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	bizadminimage "ai-business-service/internal/biz/adminimage"
	bizadminpricing "ai-business-service/internal/biz/adminpricing"
	bizadminreview "ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	integrationsga4 "ai-business-service/internal/integrations/ga4"
	"ai-business-service/internal/integrations/paycores"
	transportadminanalyticsga4 "ai-business-service/internal/transport/adminanalyticsga4"
	transportadminapps "ai-business-service/internal/transport/adminapps"
	"ai-business-service/internal/transport/adminauth"
	transportadminblog "ai-business-service/internal/transport/adminblog"
	transportadminimage "ai-business-service/internal/transport/adminimage"
	transportadminmenu "ai-business-service/internal/transport/adminmenu"
	transportadminmux "ai-business-service/internal/transport/adminmux"
	transportadminpaycores "ai-business-service/internal/transport/adminpaycores"
	transportadminpricing "ai-business-service/internal/transport/adminpricing"
	transportadminreview "ai-business-service/internal/transport/adminreview"
	transportadminsubscription "ai-business-service/internal/transport/adminsubscription"
	transportadminutm "ai-business-service/internal/transport/adminutm"
	transportadminvideo "ai-business-service/internal/transport/adminvideo"
	transportadmin "ai-business-service/internal/transport/adminview"
	"ai-business-service/internal/transport/sessionauth"
)

func newConfiguredAdminViewHandler(dataConfig *conf.Data, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, fmt.Errorf("Go session authenticator is required for admin view")
	}
	staging := conf.ValidateStagingMongo(dataConfig) == nil
	var (
		storage *data.Data
		cleanup func()
		err     error
	)
	if staging {
		storage, cleanup, err = data.NewAdminData(dataConfig)
	} else {
		if err := conf.ValidateLocalMongo(dataConfig); err != nil {
			return nil, nil, err
		}
		storage, cleanup, err = data.NewData(dataConfig)
	}
	if err != nil {
		return nil, nil, err
	}
	if !staging {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := data.NewLocalSchemaInitializer(storage).Ensure(ctx); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("initialize local MongoDB schema: %w", err)
		}
	}
	authorize := adminauth.Authorizer(authenticator)
	// GA4 的配置来源与出站客户端都收敛在 integrations/ga4：
	// 本层只看两件事实（属性 ID 与凭据是否给出），不读环境变量、不碰凭据内容。
	ga4Environment := integrationsga4.FromEnvironment()
	var paycoresChannels *paycores.AdminClient
	if strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_PAYCORES_ENABLED")), "true") {
		paycoresChannels, err = paycores.NewAdminClient(os.Getenv("PAYCORES_BASE_URL"), os.Getenv("PAYCORES_REQUEST_HMAC_KEY"), nil, time.Now)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("configure PayCores admin channels overview: %w", err)
		}
	}
	// 组合入口只做路径分派：Apps / UTM / 博客 / 订阅分析各自持有自己的投影，
	// 其余 /api/admin/ 路径仍由 adminview 处理，未迁移的返回 501。
	//
	// UTM 与节点设置直接拿 authenticator 而不是 authorize：两者都只放行
	// super_admin（比 admin 更严），需要自己看角色才能照搬。
	reviewRepository := data.NewAdminReviewRepository(storage)
	return transportadminmux.New(transportadminmux.Options{
		// Apps 直接拿 authenticator 而不是 authorize：它的写入口要求 super_admin
		// （比 admin 更严），需要自己看角色才能照搬。
		Apps:         transportadminapps.NewHandler(data.NewAdminAppsRepository(storage), authenticator),
		UTM:          transportadminutm.NewHandler(data.NewAdminUTMRepository(storage), authenticator),
		Funnel:       transportadminutm.NewFunnelUnavailableHandler(authenticator),
		Subscription: transportadminsubscription.NewHandler(data.NewAdminSubscriptionRepository(storage), authorize),
		Blog:         transportadminblog.NewHandler(authenticator, data.NewAdminBlogRepository(storage)),
		Menu:         transportadminmenu.NewHandler(data.NewAdminMenuRepository(storage), authenticator),
		Pricing:      transportadminpricing.NewHandler(bizadminpricing.NewOperations(data.NewAdminPricingRepository(storage)), authenticator),
		PayCores:     transportadminpaycores.NewHandler(authenticator, paycoresChannels),
		ImageReview:  transportadminreview.NewHandler(reviewRepository, authorize),
		Images:       transportadminimage.NewHandler(bizadminimage.NewUsecase(data.NewAdminImageRepository(storage), nil, time.Now), authorize),
		VideoReview:  transportadminvideo.NewHandler(bizadminreview.NewUsecase(reviewRepository), adminauth.AuthorizerWithActor(authenticator)),
		GA4: transportadminanalyticsga4.NewHandler(authenticator, transportadminanalyticsga4.Config{
			PropertyID:        ga4Environment.PropertyID,
			CredentialPresent: ga4Environment.CredentialSource != "",
		}, newGa4Reporter(ga4Environment)),
		Fallback: transportadmin.NewHandler(authenticator, data.NewAdminViewRepository(storage)),
	}), cleanup, nil
}

func newOptionalAdminViewHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil || bootstrap == nil {
		return nil, nil, err
	}
	adminConfig := bootstrap.GetData()
	if adminStagingMongoEnabled() {
		adminConfig, err = adminStagingMongoConfigFromEnvironment()
		if err != nil {
			return nil, nil, err
		}
	}
	return newConfiguredAdminViewHandler(adminConfig, authenticator)
}

// adminStagingMongoEnabled is separate from the local admin handler switch:
// session/authentication storage always stays on the bootstrap's local
// cling_main database.
func adminStagingMongoEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_MONGO_ENABLED")), "true")
}

// adminStagingMongoConfigFromEnvironment keeps staging credentials out of the
// checked-in bootstrap YAML. Both values are required when enabled; an
// incomplete configuration fails closed before opening any network connection.
func adminStagingMongoConfigFromEnvironment() (*conf.Data, error) {
	uri := strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_MONGO_URI"))
	database := strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_MONGO_DATABASE"))
	if uri == "" {
		return nil, fmt.Errorf("GATEWAY_ADMIN_MONGO_URI must be injected when GATEWAY_ADMIN_MONGO_ENABLED=true")
	}
	if database == "" {
		return nil, fmt.Errorf("GATEWAY_ADMIN_MONGO_DATABASE must be injected when GATEWAY_ADMIN_MONGO_ENABLED=true")
	}
	config := &conf.Data{Mongo: &conf.Data_Mongo{
		Uri:                  uri,
		Database:             database,
		TransactionsRequired: true,
	}}
	if err := conf.ValidateStagingMongo(config); err != nil {
		return nil, fmt.Errorf("validate admin staging MongoDB config: %w", err)
	}
	return config, nil
}
