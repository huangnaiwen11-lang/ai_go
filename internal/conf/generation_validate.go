package conf

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	ProviderLocalExecutionV2 = "local_execution_v2"
	ProviderPolarStarB2BV2   = "polarstar_b2b_v2"
)

// IsGlobalUnicast includes shared, benchmark and reserved IPv4 space.
var nonPublicGenerationNetworks = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// EffectiveGenerationProvider 返回新任务选择器；历史步骤必须使用自己的冻结身份。
// 空值只为旧本地配置兼容，非法值留给 ValidateGeneration 拒绝。
func EffectiveGenerationProvider(g *Integrations_Generation) string {
	if g.GetProvider() == "" {
		return ProviderLocalExecutionV2
	}
	return g.GetProvider()
}

// ValidateGeneration 只检查配置，不解析 DNS、不建立连接或读取 Secret。
// 选择器只控制新任务；已配置的 local/B2B 账号均校验，允许独立凭据同时排空。
func ValidateGeneration(g *Integrations_Generation, security *Security, environment string) error {
	if g == nil {
		return errors.New("missing generation integration")
	}
	if environment == "" {
		environment = "local"
	}
	if environment != "local" && environment != "production" {
		return errors.New("environment must be local or production")
	}
	provider := EffectiveGenerationProvider(g)
	if provider != ProviderLocalExecutionV2 && provider != ProviderPolarStarB2BV2 {
		return errors.New("invalid generation provider")
	}
	if environment == "production" && provider == ProviderLocalExecutionV2 {
		return errors.New("production cannot select local generation provider")
	}
	localConfigured := g.GetBaseUrl() != "" || g.GetCallbackBaseUrl() != "" || g.GetApiKey() != "" || security.GetGenerationRequestHmacKey() != "" || security.GetGenerationCallbackHmacKey() != ""
	if provider == ProviderLocalExecutionV2 || localConfigured {
		if err := validateLocalGeneration(g, security); err != nil {
			return err
		}
	}
	b := g.GetPolarstarB2B()
	if b == nil {
		if provider == ProviderPolarStarB2BV2 {
			return errors.New("missing polarstar b2b config")
		}
		return nil
	}
	if err := validateB2B(b, environment); err != nil {
		return err
	}
	localKeys := []string{g.GetApiKey(), security.GetGenerationRequestHmacKey(), security.GetGenerationCallbackHmacKey()}
	for _, b2bKey := range []string{b.GetApiKey(), b.GetCallbackSecret(), b.GetCallbackPreviousSecret()} {
		if strings.TrimSpace(b2bKey) == "" {
			continue
		}
		for _, localKey := range localKeys {
			if strings.TrimSpace(localKey) == strings.TrimSpace(b2bKey) {
				return errors.New("local and polarstar b2b credentials must differ")
			}
		}
	}
	return nil
}

func validateLocalGeneration(g *Integrations_Generation, security *Security) error {
	requestKey, callbackKey := strings.TrimSpace(security.GetGenerationRequestHmacKey()), strings.TrimSpace(security.GetGenerationCallbackHmacKey())
	if requestKey == "" {
		return errors.New("missing generation request hmac key")
	}
	if callbackKey == "" {
		return errors.New("missing generation callback hmac key")
	}
	if len(requestKey) < minimumHMACKey {
		return errors.New("generation request hmac key must be at least 32 characters")
	}
	if len(callbackKey) < minimumHMACKey {
		return errors.New("generation callback hmac key must be at least 32 characters")
	}
	if requestKey == callbackKey {
		return errors.New("generation request and callback hmac keys must differ")
	}
	if err := validateGenerationBaseURL(g.GetBaseUrl()); err != nil {
		return err
	}
	if err := validateCallbackBaseURL(g.GetCallbackBaseUrl()); err != nil {
		return err
	}
	if strings.TrimSpace(g.GetApiKey()) == "" {
		return errors.New("missing generation api key")
	}
	return nil
}

func validateB2B(b *Integrations_PolarStarB2B, environment string) error {
	if !validGenerationAccountRef(b.GetAccountRef()) {
		return errors.New("polarstar b2b account_ref must be a safe identifier of 1 to 200 bytes")
	}
	if !validGenerationTenantID(b.GetTenantId()) {
		return errors.New("invalid polarstar b2b tenant_id")
	}
	if !validGenerationAPIKey(b.GetApiKey()) {
		return errors.New("polarstar b2b api key must contain 1 to 4096 printable ASCII characters without whitespace")
	}
	if err := validatePublicHTTPSOrigin("polarstar b2b base url", b.GetBaseUrl()); err != nil {
		return err
	}
	switch b.GetDeliveryMode() {
	case "lookup_only":
		if environment != "local" {
			return errors.New("lookup_only is only allowed in local environment")
		}
		if b.GetCallbackOrigin() != "" || b.GetCallbackSecret() != "" || b.GetCallbackPreviousSecret() != "" {
			return errors.New("lookup_only must disable callback origin and secret")
		}
	case "webhook":
		// 验签器用原始字节计算 HMAC，而这里若只按 trim 后的长度判合法性，
		// 一个带首尾空白的 secret 会通过校验却在运行期永远验签失败：平台按
		// 自己那份干净的 secret 签名，我们却拿带空白的字节去比，结果是每次
		// 投递都 401、平台重试 8 次后放弃，而创作永远停在 submitted。
		// 因此这里直接拒绝未 trim 的取值，让「secret 只能有一种解释」成立。
		secret := b.GetCallbackSecret()
		if secret != strings.TrimSpace(secret) {
			return errors.New("polarstar b2b callback secret must not contain surrounding whitespace")
		}
		if len(secret) < minimumHMACKey {
			return errors.New("polarstar b2b callback secret must be at least 32 characters")
		}
		previous := b.GetCallbackPreviousSecret()
		if previous != strings.TrimSpace(previous) || (previous != "" && len(previous) < minimumHMACKey) {
			return errors.New("polarstar b2b previous callback secret must be empty or at least 32 characters without surrounding whitespace")
		}
		if previous != "" && previous == secret {
			return errors.New("polarstar b2b callback secrets must differ")
		}
		if secret == b.GetApiKey() || (previous != "" && previous == b.GetApiKey()) {
			return errors.New("polarstar b2b api key and callback secret must differ")
		}
		if err := validatePublicHTTPSOrigin("polarstar b2b callback origin", b.GetCallbackOrigin()); err != nil {
			return err
		}
	default:
		return errors.New("polarstar b2b delivery_mode must be webhook or lookup_only")
	}
	if len(b.GetResultHostAllowlist()) == 0 {
		return errors.New("polarstar b2b result host allowlist is required")
	}
	for _, host := range b.GetResultHostAllowlist() {
		if !isPublicHost(host) {
			return errors.New("invalid polarstar b2b result host allowlist")
		}
	}
	for _, timeout := range []struct {
		name  string
		value *durationpb.Duration
		limit time.Duration
	}{
		{"http timeout", b.GetHttpTimeout(), 2 * time.Minute},
		{"connect timeout", b.GetConnectTimeout(), 5 * time.Second},
		{"response header timeout", b.GetResponseHeaderTimeout(), 15 * time.Second},
	} {
		if timeout.value == nil || timeout.value.CheckValid() != nil || timeout.value.AsDuration() <= 0 || timeout.value.AsDuration() > timeout.limit {
			return fmt.Errorf("polarstar b2b %s must be positive and at most %s", timeout.name, timeout.limit)
		}
		if timeout.value.AsDuration() > b.GetHttpTimeout().AsDuration() {
			return fmt.Errorf("polarstar b2b %s exceeds http timeout", timeout.name)
		}
	}
	for _, bound := range []struct {
		name  string
		value int64
	}{
		{"max_connections_per_host", int64(b.GetMaxConnectionsPerHost())}, {"max_response_bytes", b.GetMaxResponseBytes()},
		{"max_result_bytes", b.GetMaxResultBytes()}, {"max_in_flight", int64(b.GetMaxInFlight())},
	} {
		if bound.value <= 0 {
			return fmt.Errorf("polarstar b2b %s must be positive", bound.name)
		}
	}
	if b.GetMaxResponseBytes() > 1<<20 {
		return errors.New("polarstar b2b max_response_bytes must not exceed 1 MiB")
	}
	if b.GetMaxConnectionsPerHost() > 32 {
		return errors.New("polarstar b2b max_connections_per_host must not exceed 32")
	}
	return nil
}

func validGenerationAccountRef(value string) bool {
	if len(value) == 0 || len(value) > 200 {
		return false
	}
	for i, ch := range value {
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			continue
		}
		if i == 0 || (ch != '.' && ch != '_' && ch != ':' && ch != '-') {
			return false
		}
	}
	return true
}

func validGenerationTenantID(value string) bool {
	if len(value) == 0 || len(value) > 200 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func validGenerationAPIKey(value string) bool {
	if len(value) == 0 || len(value) > 4096 {
		return false
	}
	for _, ch := range value {
		if ch < 33 || ch > 126 {
			return false
		}
	}
	return true
}

func validatePublicHTTPSOrigin(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || !isPublicHost(u.Hostname()) {
		return fmt.Errorf("invalid %s: public HTTPS origin required", name)
	}
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("invalid %s port", name)
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid %s port", name)
		}
	}
	return nil
}

// Only syntactic checks occur here. Runtime transports must independently reject
// private DNS answers and revalidate every redirect at the time of connection.
func isPublicHost(host string) bool {
	if host == "" || strings.TrimSpace(host) != host {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		addr, _ := netip.AddrFromSlice(ip)
		for _, prefix := range nonPublicGenerationNetworks {
			if prefix.Contains(addr.Unmap()) {
				return false
			}
		}
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, suffix := range []string{"localhost", "local", "internal", "lan", "home"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	labels := strings.Split(host, ".")
	if len(host) > 253 || len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	// Reject noncanonical numeric IP forms, e.g. 127.1.
	last := labels[len(labels)-1]
	for _, ch := range last {
		if ch >= 'a' && ch <= 'z' {
			return true
		}
	}
	return false
}
