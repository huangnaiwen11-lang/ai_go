package conf

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestGenerationProviderConfigSchema(t *testing.T) {
	var cfg Bootstrap
	err := protojson.Unmarshal([]byte(`{
		"environment":"local",
		"integrations":{"generation":{
			"provider":"polarstar_b2b_v2",
			"polarstar_b2b":{
				"account_ref":"test-account", "tenant_id":"test-tenant",
				"base_url":"https://api.example.test", "api_key":"test-only-api-key",
				"delivery_mode":"lookup_only", "callback_origin":"", "callback_secret":"",
				"result_host_allowlist":["results.example.test"],
				"http_timeout":"30s", "connect_timeout":"5s", "response_header_timeout":"10s",
				"max_connections_per_host":8, "max_response_bytes":1048576,
				"max_result_bytes":10485760, "max_in_flight":4
			}
		}}
	}`), &cfg)
	if err != nil {
		t.Fatalf("generation provider configuration must decode: %v", err)
	}
}

func TestValidateGenerationProviderBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Bootstrap)
		want   string
	}{
		{"local lookup only", func(*Bootstrap) {}, ""},
		{"independent providers coexist", func(c *Bootstrap) { c.Integrations.Generation.Provider = "local_execution_v2" }, ""},
		{"b2b alone needs no local credentials", func(c *Bootstrap) {
			g := c.Integrations.Generation
			g.BaseUrl, g.CallbackBaseUrl, g.ApiKey = "", "", ""
			c.Security.GenerationRequestHmacKey, c.Security.GenerationCallbackHmacKey = "", ""
		}, ""},
		{"production webhook", func(c *Bootstrap) { c.Environment = "production"; enableTestWebhook(c) }, ""},
		{"unknown provider", func(c *Bootstrap) { c.Integrations.Generation.Provider = "other" }, "provider"},
		{"unknown environment", func(c *Bootstrap) { c.Environment = "staging" }, "environment"},
		{"production lookup only", func(c *Bootstrap) { c.Environment = "production" }, "lookup_only"},
		{"production implicit local", func(c *Bootstrap) { c.Environment = "production"; c.Integrations.Generation.Provider = "" }, "production"},
		{"production explicit local", func(c *Bootstrap) {
			c.Environment = "production"
			c.Integrations.Generation.Provider = "local_execution_v2"
		}, "production"},
		{"missing b2b", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B = nil }, "polarstar b2b"},
		{"missing account", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.AccountRef = "" }, "account_ref"},
		{"account whitespace", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.AccountRef = " test-account" }, "account_ref"},
		{"account control", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.AccountRef = "test\naccount" }, "account_ref"},
		{"account too long", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.AccountRef = strings.Repeat("a", 201) }, "account_ref"},
		{"missing tenant", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.TenantId = "" }, "tenant_id"},
		{"tenant whitespace", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.TenantId = "test-tenant " }, "tenant_id"},
		{"tenant control", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.TenantId = "test\rtenant" }, "tenant_id"},
		{"missing api key", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.ApiKey = "" }, "api key"},
		{"api key whitespace", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.ApiKey = "api key" }, "api key"},
		{"api key control", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.ApiKey = "api\nkey" }, "api key"},
		{"api key non ASCII", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.ApiKey = "密钥" }, "api key"},
		{"api key too long", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.ApiKey = strings.Repeat("a", 4097) }, "api key"},
		{"api key reused", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.ApiKey = c.Integrations.Generation.ApiKey }, "must differ"},
		{"hmac reused as api key", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.ApiKey = c.Security.GenerationRequestHmacKey
		}, "must differ"},
		{"callback hmac reused", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackSecret = c.Security.GenerationCallbackHmacKey
		}, "must differ"},
		{"previous callback hmac reused", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackPreviousSecret = c.Security.GenerationCallbackHmacKey
		}, "must differ"},
		{"b2b key reused as secret", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackSecret = c.Integrations.Generation.PolarstarB2B.ApiKey
		}, "must differ"},
		{"b2b key reused as previous secret", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackPreviousSecret = c.Integrations.Generation.PolarstarB2B.ApiKey
		}, "must differ"},
		{"unknown delivery mode", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.DeliveryMode = "" }, "delivery_mode"},
		{"lookup callback enabled", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.CallbackOrigin = "https://callbacks.example.test"
		}, "lookup_only"},
		{"lookup secret configured", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.CallbackSecret = strings.Repeat("s", 32) }, "lookup_only"},
		{"lookup previous secret configured", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.CallbackPreviousSecret = strings.Repeat("p", 32)
		}, "lookup_only"},
		{"webhook no secret", func(c *Bootstrap) { enableTestWebhook(c); c.Integrations.Generation.PolarstarB2B.CallbackSecret = "" }, "callback secret"},
		{"webhook short secret", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackSecret = "short"
		}, "callback secret"},
		// 长度足够但带首尾空白：验签器用原始字节算 HMAC，若这里放行，
		// 每次回调都会 401，平台重试 8 次后放弃。必须在校验期就拒绝。
		{"webhook secret with surrounding whitespace", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackSecret = strings.Repeat("s", 32) + " "
		}, "callback secret"},
		{"webhook secret with leading whitespace", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackSecret = " " + strings.Repeat("s", 32)
		}, "callback secret"},
		{"webhook previous secret with surrounding whitespace", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackPreviousSecret = strings.Repeat("p", 32) + " "
		}, "previous callback secret"},
		{"webhook valid previous secret", func(c *Bootstrap) {
			enableTestWebhook(c)
			c.Integrations.Generation.PolarstarB2B.CallbackPreviousSecret = strings.Repeat("p", 32)
		}, ""},
		{"webhook no origin", func(c *Bootstrap) { enableTestWebhook(c); c.Integrations.Generation.PolarstarB2B.CallbackOrigin = "" }, "callback origin"},
		{"missing allowlist", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.ResultHostAllowlist = nil }, "result host"},
		{"zero http timeout", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.HttpTimeout = nil }, "http timeout"},
		{"http timeout exceeds client limit", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.HttpTimeout = durationpb.New(2*time.Minute + time.Second)
		}, "http timeout"},
		{"negative connect timeout", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.ConnectTimeout = durationpb.New(-time.Second)
		}, "connect timeout"},
		{"connect timeout exceeds client limit", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.ConnectTimeout = durationpb.New(6 * time.Second)
		}, "connect timeout"},
		{"header timeout exceeds client limit", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.ResponseHeaderTimeout = durationpb.New(16 * time.Second)
		}, "response header timeout"},
		{"invalid proto duration", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.HttpTimeout = &durationpb.Duration{Seconds: 315576000001}
		}, "http timeout"},
		{"header exceeds total timeout", func(c *Bootstrap) {
			c.Integrations.Generation.PolarstarB2B.ResponseHeaderTimeout = durationpb.New(time.Minute)
		}, "response header timeout"},
		{"zero connection bound", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.MaxConnectionsPerHost = 0 }, "max_connections_per_host"},
		{"connections exceed client limit", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.MaxConnectionsPerHost = 33 }, "max_connections_per_host"},
		{"zero response bound", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.MaxResponseBytes = 0 }, "max_response_bytes"},
		{"response bound exceeds client limit", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.MaxResponseBytes = 1048577 }, "max_response_bytes"},
		{"negative result bound", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.MaxResultBytes = -1 }, "max_result_bytes"},
		{"zero concurrency", func(c *Bootstrap) { c.Integrations.Generation.PolarstarB2B.MaxInFlight = 0 }, "max_in_flight"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := b2bTestConfig()
			tc.change(cfg)
			err := Validate(cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateB2BPublicOriginsAndResultHosts(t *testing.T) {
	for _, raw := range []string{"http://api.example.test", "https://localhost", "https://localhost.", "https://api.local", "https://127.0.0.1", "https://10.0.0.1", "https://169.254.169.254", "https://[::1]", "https://[fd00::1]", "https://[fe80::1]", "https://user@api.example.test", "https://api.example.test/path", "https://api.example.test?", "https://api.example.test#", "https://api.example.test:0", "https://api.example.test:99999", "https://2130706433", "https://127.1", "https://100.64.0.1", "https://198.18.0.1", "https://240.0.0.1"} {
		t.Run(raw, func(t *testing.T) {
			cfg := b2bTestConfig()
			cfg.Integrations.Generation.PolarstarB2B.BaseUrl = raw
			if err := Validate(cfg); err == nil {
				t.Fatal("accepted unsafe B2B base URL")
			}
			cfg = b2bTestConfig()
			enableTestWebhook(cfg)
			cfg.Integrations.Generation.PolarstarB2B.CallbackOrigin = raw
			if err := Validate(cfg); err == nil {
				t.Fatal("accepted unsafe callback origin")
			}
		})
	}
	for _, host := range []string{"*.example.test", "https://results.example.test", "results.example.test:443", "10.0.0.1", "localhost", "api.local", "127.1", "[::1]", "results.example.test/path"} {
		t.Run(host, func(t *testing.T) {
			cfg := b2bTestConfig()
			cfg.Integrations.Generation.PolarstarB2B.ResultHostAllowlist = []string{host}
			if err := Validate(cfg); err == nil {
				t.Fatal("accepted unsafe result host")
			}
		})
	}
}

func enableTestWebhook(c *Bootstrap) {
	b := c.Integrations.Generation.PolarstarB2B
	b.DeliveryMode, b.CallbackOrigin, b.CallbackSecret = "webhook", "https://callbacks.example.test", strings.Repeat("s", 32)
}

func b2bTestConfig() *Bootstrap {
	c := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
	c.Environment = "local"
	c.Integrations.Generation.Provider = "polarstar_b2b_v2"
	c.Integrations.Generation.PolarstarB2B = &Integrations_PolarStarB2B{
		AccountRef: "test-account", TenantId: "test-tenant", BaseUrl: "https://api.example.test", ApiKey: strings.Repeat("a", 32),
		DeliveryMode: "lookup_only", ResultHostAllowlist: []string{"results.example.test"},
		HttpTimeout: durationpb.New(30 * time.Second), ConnectTimeout: durationpb.New(5 * time.Second), ResponseHeaderTimeout: durationpb.New(10 * time.Second),
		MaxConnectionsPerHost: 8, MaxResponseBytes: 1048576, MaxResultBytes: 10485760, MaxInFlight: 4,
	}
	return c
}
