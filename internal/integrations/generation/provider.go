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

// ProviderSet 只提供受控客户端构造器，不发起真实网络调用。
var ProviderSet = wire.NewSet(NewConfiguredClient)
