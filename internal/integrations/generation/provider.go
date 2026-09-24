package generation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"ai-business-service/internal/conf"

	"github.com/google/wire"
)

const configuredClientTimeout = 15 * time.Second

// NewConfiguredClient 按已校验的本地配置构造生成中台客户端。
// 构造阶段不会发送请求；实际提交只能由显式调用的工作者触发。
func NewConfiguredClient(security *conf.Security, integrations *conf.Integrations) (*Client, error) {
	if security == nil || integrations == nil || integrations.GetGeneration() == nil {
		return nil, errors.New("generation client config is unavailable")
	}
	// This factory only speaks execution.v2. Never fall back to it when a
	// different provider is selected, even if legacy drain credentials remain.
	provider := integrations.GetGeneration().GetProvider()
	if provider != "" && provider != "local_execution_v2" {
		return nil, errors.New("local generation client cannot serve the selected provider")
	}
	return NewConfiguredLocalClient(security, integrations)
}

// NewConfiguredLocalClient constructs the execution.v2 client from the local
// credentials regardless of the new-task selector.  The selector guard lives
// in NewConfiguredClient so callers that intentionally resolve a historical
// local route can still do so while the active selector is B2B.  It does not
// make local the fallback for a B2B route; the registry performs route checks.
func NewConfiguredLocalClient(security *conf.Security, integrations *conf.Integrations) (*Client, error) {
	if security == nil || integrations == nil || integrations.GetGeneration() == nil {
		return nil, errors.New("generation client config is unavailable")
	}
	return NewClientWithCallbackOriginAndAPIKey(
		integrations.GetGeneration().GetBaseUrl(),
		security.GetGenerationRequestHmacKey(),
		integrations.GetGeneration().GetCallbackBaseUrl(),
		integrations.GetGeneration().GetApiKey(),
		&http.Client{Timeout: configuredClientTimeout},
		time.Now,
		newRequestNonce,
	)
}

// newRequestNonce 为每次出站请求生成独立随机 nonce。
// 随机源不可用时返回空值，由 Client 在请求发送前安全拒绝该调用。
func newRequestNonce() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(raw[:])
}

// ProviderSet 只提供受控客户端构造器与按运行配置构造的归属解析器，不发起真实网络调用。
// 归属解析器必须在这里（而不是在创作模块内）提供：只有组合根同时掌握运行选择器与
// 已发布目录来源，才能在选择 B2B 却没有可用来源时拒绝启动。
var ProviderSet = wire.NewSet(NewConfiguredClient, NewB2BAdmissionStorageReadiness, NewAdmissionResolverWithStorageReadiness, NewProviderRegistry)
