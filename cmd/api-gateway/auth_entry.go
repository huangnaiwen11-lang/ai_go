package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"ai-business-service/internal/biz/authcredential"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	transportauth "ai-business-service/internal/transport/authentry"
	"ai-business-service/internal/transport/sessionauth"
)

// newConfiguredAuthEntryHandler 装配独立本地 MongoDB 上的账号入口。它只使用 Go
// 自有用户、凭据、会话和账户集合，不读取 Node JWT、用户或钱包。
func newConfiguredAuthEntryHandler(dataConfig *conf.Data, securityConfig *conf.Security, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if authenticator == nil {
		return nil, nil, fmt.Errorf("Go session authenticator is required for local auth entry")
	}
	if err := conf.ValidateLocalMongo(dataConfig); err != nil {
		return nil, nil, err
	}
	policy, err := authcredential.NewPolicy(authcredential.Params{MemoryKiB: securityConfig.GetPasswordMemoryKib(), TimeCost: securityConfig.GetPasswordTimeCost(), Parallelism: uint8(securityConfig.GetPasswordParallelism()), SaltBytes: securityConfig.GetPasswordSaltBytes(), KeyBytes: securityConfig.GetPasswordKeyBytes()})
	if err != nil {
		return nil, nil, fmt.Errorf("initialize password policy: %w", err)
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
	usecase := identity.NewAuthUsecase(data.NewAuthUserRepository(storage), data.NewIdentityRepository(storage), data.NewAuthSessionRepository(storage), data.NewAccountRepository(storage), data.NewCredentialRepository(storage), policy, data.NewTxRunner(storage))
	return transportauth.NewHandlerWithBinding(authenticator, usecase, localBindingVerifier()), cleanup, nil
}

// localBindingVerifier 只在显式本地开关和 local- 前缀密钥同时存在时装配。
// 缺少任一条件时返回 nil，绑定接口安全拒绝，不会降级为裸 subject 信任。
func localBindingVerifier() interface {
	Verify(context.Context, string, string) (identity.ExternalIdentity, error)
} {
	if os.Getenv("GO_LOCAL_BINDING_VERIFIER_ENABLED") != "1" {
		return nil
	}
	secret := strings.TrimSpace(os.Getenv("GO_LOCAL_BINDING_SECRET"))
	if !strings.HasPrefix(secret, "local-") {
		return nil
	}
	return transportauth.LocalBindingVerifier{Secret: []byte(secret)}
}

// newOptionalAuthEntryHandler 只有两个本地开关都已明确打开时才读取配置和连接 MongoDB。
// 默认关闭时返回 nil，Gateway 不会改变任何 Node 认证路径。
func newOptionalAuthEntryHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	bootstrap, err := loadOptionalGatewayBootstrap(enabled, configPath)
	if err != nil {
		return nil, nil, err
	}
	if bootstrap == nil {
		return nil, nil, nil
	}
	return newConfiguredAuthEntryHandler(bootstrap.GetData(), bootstrap.GetSecurity(), authenticator)
}
