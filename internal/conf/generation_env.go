package conf

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
)

// 仅在「环境变量新建了 B2B 段、YAML 没有基线」时填入。数值与
// internal/conf/generation_provider_test.go 的夹具一致，并且都落在
// validateB2B 的合法区间内。已有 YAML 值不会被这些默认值覆盖。
const (
	defaultB2BHTTPTimeout           = 30 * time.Second
	defaultB2BConnectTimeout        = 5 * time.Second
	defaultB2BResponseHeaderTimeout = 10 * time.Second
	defaultB2BMaxConnectionsPerHost = int32(8)
	defaultB2BMaxResponseBytes      = int64(1 << 20)
	defaultB2BMaxResultBytes        = int64(10 << 20)
	defaultB2BMaxInFlight           = int32(4)
)

// ApplyPolarStarB2BEnvironmentOverrides applies only explicitly supplied
// POLARSTAR_B2B_* values. Empty values leave the mounted configuration intact,
// so the default local profile remains unchanged. The same helper is called by
// Gateway, business service and submission worker to keep provider selection
// and credentials identical across all processes.
//
// 7 个超时/连接/限额字段同样可被环境变量覆盖。非法值在这里直接失败，错误只含
// 字段名，不含变量值。当环境变量从零创建 B2B 段时，未显式给出的这 7 个字段
// 使用上面的安全默认值；YAML 已有的值保持不变。
func ApplyPolarStarB2BEnvironmentOverrides(bootstrap *Bootstrap, getenv func(string) string) error {
	if bootstrap == nil || getenv == nil || bootstrap.Integrations == nil || bootstrap.Integrations.Generation == nil {
		return nil
	}
	generation := bootstrap.Integrations.Generation
	b2b := generation.PolarstarB2B
	set := func(name string, target *string) {
		if value := getenv(name); value != "" {
			*target = value
		}
	}
	set("POLARSTAR_B2B_PROVIDER", &generation.Provider)
	created := false
	if b2b == nil {
		if !hasPolarStarB2BEnvironment(getenv) {
			return nil
		}
		b2b = &Integrations_PolarStarB2B{}
		generation.PolarstarB2B = b2b
		created = true
	}
	set("POLARSTAR_B2B_ACCOUNT_REF", &b2b.AccountRef)
	set("POLARSTAR_B2B_TENANT_ID", &b2b.TenantId)
	set("POLARSTAR_B2B_BASE_URL", &b2b.BaseUrl)
	set("POLARSTAR_B2B_API_KEY", &b2b.ApiKey)
	set("POLARSTAR_B2B_DELIVERY_MODE", &b2b.DeliveryMode)
	set("POLARSTAR_B2B_CALLBACK_ORIGIN", &b2b.CallbackOrigin)
	set("POLARSTAR_B2B_CALLBACK_SECRET", &b2b.CallbackSecret)
	set("POLARSTAR_B2B_CALLBACK_PREVIOUS_SECRET", &b2b.CallbackPreviousSecret)
	if value := getenv("POLARSTAR_B2B_RESULT_HOST_ALLOWLIST"); value != "" {
		parts := strings.Split(value, ",")
		allowlist := make([]string, 0, len(parts))
		for _, part := range parts {
			if host := strings.TrimSpace(part); host != "" {
				allowlist = append(allowlist, host)
			}
		}
		b2b.ResultHostAllowlist = allowlist
	}
	if err := applyPolarStarB2BLimitOverrides(b2b, getenv, created); err != nil {
		return err
	}
	return nil
}

// applyPolarStarB2BLimitOverrides 覆盖 7 个必须为正的字段。空环境变量不改动
// 目标；从零新建段时，空环境变量改用安全默认值，避免只配凭据就在 Validate 里 panic。
func applyPolarStarB2BLimitOverrides(b2b *Integrations_PolarStarB2B, getenv func(string) string, fillDefaults bool) error {
	setDuration := func(name, field string, target **durationpb.Duration, fallback time.Duration) error {
		value := getenv(name)
		if value == "" {
			if fillDefaults && *target == nil {
				*target = durationpb.New(fallback)
			}
			return nil
		}
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("invalid polarstar b2b %s", field)
		}
		*target = durationpb.New(parsed)
		return nil
	}
	setInt32 := func(name, field string, target *int32, fallback int32) error {
		value := getenv(name)
		if value == "" {
			if fillDefaults && *target == 0 {
				*target = fallback
			}
			return nil
		}
		parsed, err := strconv.ParseInt(value, 10, 32)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("invalid polarstar b2b %s", field)
		}
		*target = int32(parsed)
		return nil
	}
	setInt64 := func(name, field string, target *int64, fallback int64) error {
		value := getenv(name)
		if value == "" {
			if fillDefaults && *target == 0 {
				*target = fallback
			}
			return nil
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("invalid polarstar b2b %s", field)
		}
		*target = parsed
		return nil
	}
	if err := setDuration("POLARSTAR_B2B_HTTP_TIMEOUT", "http_timeout", &b2b.HttpTimeout, defaultB2BHTTPTimeout); err != nil {
		return err
	}
	if err := setDuration("POLARSTAR_B2B_CONNECT_TIMEOUT", "connect_timeout", &b2b.ConnectTimeout, defaultB2BConnectTimeout); err != nil {
		return err
	}
	if err := setDuration("POLARSTAR_B2B_RESPONSE_HEADER_TIMEOUT", "response_header_timeout", &b2b.ResponseHeaderTimeout, defaultB2BResponseHeaderTimeout); err != nil {
		return err
	}
	if err := setInt32("POLARSTAR_B2B_MAX_CONNECTIONS_PER_HOST", "max_connections_per_host", &b2b.MaxConnectionsPerHost, defaultB2BMaxConnectionsPerHost); err != nil {
		return err
	}
	if err := setInt64("POLARSTAR_B2B_MAX_RESPONSE_BYTES", "max_response_bytes", &b2b.MaxResponseBytes, defaultB2BMaxResponseBytes); err != nil {
		return err
	}
	if err := setInt64("POLARSTAR_B2B_MAX_RESULT_BYTES", "max_result_bytes", &b2b.MaxResultBytes, defaultB2BMaxResultBytes); err != nil {
		return err
	}
	return setInt32("POLARSTAR_B2B_MAX_IN_FLIGHT", "max_in_flight", &b2b.MaxInFlight, defaultB2BMaxInFlight)
}

func hasPolarStarB2BEnvironment(getenv func(string) string) bool {
	for _, name := range []string{
		"POLARSTAR_B2B_PROVIDER", "POLARSTAR_B2B_ACCOUNT_REF", "POLARSTAR_B2B_TENANT_ID",
		"POLARSTAR_B2B_BASE_URL", "POLARSTAR_B2B_API_KEY", "POLARSTAR_B2B_DELIVERY_MODE",
		"POLARSTAR_B2B_CALLBACK_ORIGIN", "POLARSTAR_B2B_CALLBACK_SECRET",
		"POLARSTAR_B2B_CALLBACK_PREVIOUS_SECRET", "POLARSTAR_B2B_RESULT_HOST_ALLOWLIST",
		"POLARSTAR_B2B_HTTP_TIMEOUT", "POLARSTAR_B2B_CONNECT_TIMEOUT",
		"POLARSTAR_B2B_RESPONSE_HEADER_TIMEOUT", "POLARSTAR_B2B_MAX_CONNECTIONS_PER_HOST",
		"POLARSTAR_B2B_MAX_RESPONSE_BYTES", "POLARSTAR_B2B_MAX_RESULT_BYTES",
		"POLARSTAR_B2B_MAX_IN_FLIGHT",
	} {
		if getenv(name) != "" {
			return true
		}
	}
	return false
}
