package conf

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
)

func TestApplyPolarStarB2BEnvironmentOverridesKeepsProcessesConsistent(t *testing.T) {
	values := map[string]string{
		"POLARSTAR_B2B_PROVIDER":              ProviderPolarStarB2BV2,
		"POLARSTAR_B2B_ACCOUNT_REF":           "account-env",
		"POLARSTAR_B2B_TENANT_ID":             "tenant-env",
		"POLARSTAR_B2B_BASE_URL":              "https://api.polarstar.work",
		"POLARSTAR_B2B_API_KEY":               "key-env",
		"POLARSTAR_B2B_DELIVERY_MODE":         "lookup_only",
		"POLARSTAR_B2B_RESULT_HOST_ALLOWLIST": "cdn.example.test, assets.example.test",
	}
	getenv := func(key string) string { return values[key] }
	bootstrap := &Bootstrap{Integrations: &Integrations{Generation: &Integrations_Generation{}}}
	if err := ApplyPolarStarB2BEnvironmentOverrides(bootstrap, getenv); err != nil {
		t.Fatalf("override: %v", err)
	}
	b2b := bootstrap.GetIntegrations().GetGeneration().GetPolarstarB2B()
	if bootstrap.GetIntegrations().GetGeneration().GetProvider() != ProviderPolarStarB2BV2 || b2b == nil {
		t.Fatalf("B2B selector/config not created: %#v", bootstrap)
	}
	if b2b.GetBaseUrl() != values["POLARSTAR_B2B_BASE_URL"] || b2b.GetApiKey() != values["POLARSTAR_B2B_API_KEY"] || b2b.GetAccountRef() != values["POLARSTAR_B2B_ACCOUNT_REF"] || b2b.GetTenantId() != values["POLARSTAR_B2B_TENANT_ID"] {
		t.Fatalf("B2B identity/config mismatch: %#v", b2b)
	}
	if len(b2b.GetResultHostAllowlist()) != 2 || b2b.GetResultHostAllowlist()[0] != "cdn.example.test" || b2b.GetResultHostAllowlist()[1] != "assets.example.test" {
		t.Fatalf("allowlist = %#v", b2b.GetResultHostAllowlist())
	}
	// 从零创建段时，未显式给出的 7 个限额必须落到安全默认值，而不是 0。
	if b2b.GetHttpTimeout().AsDuration() != defaultB2BHTTPTimeout || b2b.GetConnectTimeout().AsDuration() != defaultB2BConnectTimeout || b2b.GetResponseHeaderTimeout().AsDuration() != defaultB2BResponseHeaderTimeout {
		t.Fatalf("default timeouts = %s %s %s", b2b.GetHttpTimeout().AsDuration(), b2b.GetConnectTimeout().AsDuration(), b2b.GetResponseHeaderTimeout().AsDuration())
	}
	if b2b.GetMaxConnectionsPerHost() != defaultB2BMaxConnectionsPerHost || b2b.GetMaxResponseBytes() != defaultB2BMaxResponseBytes || b2b.GetMaxResultBytes() != defaultB2BMaxResultBytes || b2b.GetMaxInFlight() != defaultB2BMaxInFlight {
		t.Fatalf("default limits = %d %d %d %d", b2b.GetMaxConnectionsPerHost(), b2b.GetMaxResponseBytes(), b2b.GetMaxResultBytes(), b2b.GetMaxInFlight())
	}
}

func TestApplyPolarStarB2BEnvironmentOverridesEmptyValuesLeaveLocalProfile(t *testing.T) {
	bootstrap := &Bootstrap{Integrations: &Integrations{Generation: &Integrations_Generation{Provider: ProviderLocalExecutionV2}}}
	if err := ApplyPolarStarB2BEnvironmentOverrides(bootstrap, func(string) string { return "" }); err != nil {
		t.Fatalf("override: %v", err)
	}
	if bootstrap.GetIntegrations().GetGeneration().GetProvider() != ProviderLocalExecutionV2 || bootstrap.GetIntegrations().GetGeneration().GetPolarstarB2B() != nil {
		t.Fatalf("empty environment changed local profile: %#v", bootstrap)
	}
}

func TestApplyPolarStarB2BEnvironmentOverridesExplicitLimitsReplaceYAML(t *testing.T) {
	b2b := &Integrations_PolarStarB2B{
		HttpTimeout: durationpb.New(20 * time.Second), ConnectTimeout: durationpb.New(2 * time.Second),
		ResponseHeaderTimeout: durationpb.New(8 * time.Second), MaxConnectionsPerHost: 4,
		MaxResponseBytes: 4096, MaxResultBytes: 8192, MaxInFlight: 1,
	}
	bootstrap := &Bootstrap{Integrations: &Integrations{Generation: &Integrations_Generation{Provider: ProviderLocalExecutionV2, PolarstarB2B: b2b}}}
	values := map[string]string{
		"POLARSTAR_B2B_HTTP_TIMEOUT":             "45s",
		"POLARSTAR_B2B_CONNECT_TIMEOUT":          "3s",
		"POLARSTAR_B2B_RESPONSE_HEADER_TIMEOUT":  "12s",
		"POLARSTAR_B2B_MAX_CONNECTIONS_PER_HOST": "16",
		"POLARSTAR_B2B_MAX_RESPONSE_BYTES":       "524288",
		"POLARSTAR_B2B_MAX_RESULT_BYTES":         "2097152",
		"POLARSTAR_B2B_MAX_IN_FLIGHT":            "2",
	}
	if err := ApplyPolarStarB2BEnvironmentOverrides(bootstrap, func(key string) string { return values[key] }); err != nil {
		t.Fatalf("override: %v", err)
	}
	if b2b.GetHttpTimeout().AsDuration() != 45*time.Second || b2b.GetConnectTimeout().AsDuration() != 3*time.Second || b2b.GetResponseHeaderTimeout().AsDuration() != 12*time.Second {
		t.Fatalf("timeouts not overridden: %s %s %s", b2b.GetHttpTimeout().AsDuration(), b2b.GetConnectTimeout().AsDuration(), b2b.GetResponseHeaderTimeout().AsDuration())
	}
	if b2b.GetMaxConnectionsPerHost() != 16 || b2b.GetMaxResponseBytes() != 524288 || b2b.GetMaxResultBytes() != 2097152 || b2b.GetMaxInFlight() != 2 {
		t.Fatalf("limits not overridden: %#v", b2b)
	}
	if bootstrap.GetIntegrations().GetGeneration().GetProvider() != ProviderLocalExecutionV2 {
		t.Fatal("limit overrides changed the provider selector")
	}
}

func TestApplyPolarStarB2BEnvironmentOverridesEmptyLimitsKeepYAML(t *testing.T) {
	b2b := &Integrations_PolarStarB2B{HttpTimeout: durationpb.New(20 * time.Second), MaxInFlight: 3, MaxResponseBytes: 1024}
	bootstrap := &Bootstrap{Integrations: &Integrations{Generation: &Integrations_Generation{PolarstarB2B: b2b}}}
	if err := ApplyPolarStarB2BEnvironmentOverrides(bootstrap, func(string) string { return "" }); err != nil {
		t.Fatalf("override: %v", err)
	}
	if b2b.GetHttpTimeout().AsDuration() != 20*time.Second || b2b.GetMaxInFlight() != 3 || b2b.GetMaxResponseBytes() != 1024 || b2b.GetConnectTimeout() != nil {
		t.Fatalf("empty env changed yaml baseline: %#v", b2b)
	}
}

func TestApplyPolarStarB2BEnvironmentOverridesRejectsInvalidLimitsWithoutValue(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		field string
	}{
		{"POLARSTAR_B2B_HTTP_TIMEOUT", "http_timeout"},
		{"POLARSTAR_B2B_CONNECT_TIMEOUT", "connect_timeout"},
		{"POLARSTAR_B2B_RESPONSE_HEADER_TIMEOUT", "response_header_timeout"},
		{"POLARSTAR_B2B_MAX_CONNECTIONS_PER_HOST", "max_connections_per_host"},
		{"POLARSTAR_B2B_MAX_RESPONSE_BYTES", "max_response_bytes"},
		{"POLARSTAR_B2B_MAX_RESULT_BYTES", "max_result_bytes"},
		{"POLARSTAR_B2B_MAX_IN_FLIGHT", "max_in_flight"},
	} {
		secret := "super-secret-value-must-not-leak"
		getenv := func(key string) string {
			if key == testCase.name {
				return secret
			}
			return ""
		}
		bootstrap := &Bootstrap{Integrations: &Integrations{Generation: &Integrations_Generation{}}}
		err := ApplyPolarStarB2BEnvironmentOverrides(bootstrap, getenv)
		if err == nil || !strings.Contains(err.Error(), testCase.field) {
			t.Fatalf("%s: error = %v, want field %s", testCase.name, err, testCase.field)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), testCase.name) {
			t.Fatalf("%s: error leaked value or env name: %v", testCase.name, err)
		}
	}
}
