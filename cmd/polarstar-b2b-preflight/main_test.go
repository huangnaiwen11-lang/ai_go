package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/conf"
	"ai-business-service/internal/integrations/polarstarb2b"

	"google.golang.org/protobuf/types/known/durationpb"
)

func TestPreflight默认不调用供应商Lookup(t *testing.T) {
	setCompleteR2Environment(t)
	report := preflight(context.Background(), preflightBootstrap(), false, func(context.Context, *polarstarb2b.Client, polarstarb2b.LookupKey) (polarstarb2b.Job, error) {
		t.Fatal("default preflight must not call PolarStar")
		return polarstarb2b.Job{}, nil
	})

	if report.CreateReadiness != createReadinessUnverified {
		t.Fatalf("create_readiness = %q, want %q; findings = %#v", report.CreateReadiness, createReadinessUnverified, report.Findings)
	}
	if report.Lookup.Attempted || report.Lookup.Status != lookupStatusNotRequested {
		t.Fatalf("default lookup = %#v, want not requested", report.Lookup)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"polarstar-api-key-must-not-leak",
		"r2-secret-must-not-leak",
		"mongodb://127.0.0.1:27017/cling_main?replicaSet=rs0",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("report leaks secret %q: %s", secret, encoded)
		}
	}
}

func TestPreflight显式Lookup使用随机幂等键(t *testing.T) {
	setCompleteR2Environment(t)
	var keys []polarstarb2b.LookupKey
	lookup := func(_ context.Context, _ *polarstarb2b.Client, key polarstarb2b.LookupKey) (polarstarb2b.Job, error) {
		keys = append(keys, key)
		return polarstarb2b.Job{}, &polarstarb2b.ClientError{HTTPStatus: 404, Code: "NOT_FOUND"}
	}

	first := preflight(context.Background(), preflightBootstrap(), true, lookup)
	second := preflight(context.Background(), preflightBootstrap(), true, lookup)

	if first.Lookup.Status != lookupStatusNotFound || second.Lookup.Status != lookupStatusNotFound {
		t.Fatalf("lookup status = %q / %q, want %q", first.Lookup.Status, second.Lookup.Status, lookupStatusNotFound)
	}
	if len(keys) != 2 {
		t.Fatalf("lookup calls = %d, want 2", len(keys))
	}
	for _, key := range keys {
		if key.IdempotencyKey != "cling-step:"+key.ExternalID || key.Capability != "text_to_image" || key.AccountRef != "account-preflight" {
			t.Fatalf("unsafe lookup key: %#v", key)
		}
	}
	if keys[0].IdempotencyKey == keys[1].IdempotencyKey {
		t.Fatalf("lookup idempotency keys must be fresh: %#v", keys)
	}
}

func TestPreflight配置未通过校验时不执行显式Lookup(t *testing.T) {
	setCompleteR2Environment(t)
	bootstrap := preflightBootstrap()
	bootstrap.Security.PaycoresCallbackHmacKey = bootstrap.Security.PaycoresRequestHmacKey
	called := false
	report := preflight(context.Background(), bootstrap, true, func(context.Context, *polarstarb2b.Client, polarstarb2b.LookupKey) (polarstarb2b.Job, error) {
		called = true
		return polarstarb2b.Job{}, nil
	})

	if called || report.Lookup.Attempted || report.Lookup.Code != "bootstrap_validation_failed" {
		t.Fatalf("invalid bootstrap must prevent lookup: called=%v lookup=%#v", called, report.Lookup)
	}
}

func TestPreflight默认不拨号时明确标记配方和映射未验证(t *testing.T) {
	setCompleteR2Environment(t)
	report := preflight(context.Background(), preflightBootstrap(), false, nil)

	if report.CreateReadiness != createReadinessUnverified {
		t.Fatalf("create_readiness = %q, want %q", report.CreateReadiness, createReadinessUnverified)
	}
	if !hasFinding(report.Findings, "mapping_catalog_not_inspected", "not_checked", createImpactUnknown) ||
		!hasFinding(report.Findings, "product_recipes_not_inspected", "not_checked", createImpactUnknown) {
		t.Fatalf("unverified catalog/recipes findings absent: %#v", report.Findings)
	}
}

func preflightBootstrap() *conf.Bootstrap {
	return &conf.Bootstrap{
		Environment: "local",
		Server:      &conf.Server{Http: &conf.Server_HTTP{}},
		Data: &conf.Data{Mongo: &conf.Data_Mongo{
			Uri: "mongodb://127.0.0.1:27017/cling_main?replicaSet=rs0", Database: "cling_main", ReplicaSet: "rs0", TransactionsRequired: true,
		}},
		Security: &conf.Security{
			SessionSigningKey: "session", PaycoresRequestHmacKey: strings.Repeat("p", 32), PaycoresCallbackHmacKey: strings.Repeat("c", 32),
			PasswordMemoryKib: 19456, PasswordTimeCost: 2, PasswordParallelism: 1, PasswordSaltBytes: 16, PasswordKeyBytes: 32,
		},
		Integrations: &conf.Integrations{
			Generation: &conf.Integrations_Generation{
				Provider: "polarstar_b2b_v2",
				PolarstarB2B: &conf.Integrations_PolarStarB2B{
					AccountRef: "account-preflight", TenantId: "tenant-preflight", BaseUrl: "https://api.example.com", ApiKey: "polarstar-api-key-must-not-leak",
					DeliveryMode: "lookup_only", ResultHostAllowlist: []string{"cdn.example.com"},
					HttpTimeout: durationpb.New(30 * time.Second), ConnectTimeout: durationpb.New(5 * time.Second), ResponseHeaderTimeout: durationpb.New(10 * time.Second),
					MaxConnectionsPerHost: 8, MaxResponseBytes: 1 << 20, MaxResultBytes: 10 << 20, MaxInFlight: 4,
				},
			},
			Paycores: &conf.Integrations_Paycores{BaseUrl: "http://127.0.0.1:19082", ReturnUrl: "http://127.0.0.1:5173/payment/success", CancelUrl: "http://127.0.0.1:5173/payment/cancel"},
			AppStore: &conf.Integrations_AppStore{BaseUrl: "http://127.0.0.1:19083"},
		},
		Worker: &conf.Worker{PollInterval: durationpb.New(time.Second), BatchSize: 1},
	}
}

func setCompleteR2Environment(t *testing.T) {
	t.Helper()
	t.Setenv("CLING_MONGO_PROFILE", "local")
	t.Setenv("R2_ACCOUNT_ID", "account123")
	t.Setenv("R2_ACCESS_KEY_ID", "access-key")
	t.Setenv("R2_SECRET_ACCESS_KEY", "r2-secret-must-not-leak")
	t.Setenv("R2_BUCKET_NAME", "cling-ai")
	t.Setenv("R2_PUBLIC_URL", "https://media.example.com")
	t.Setenv("R2_PRIVATE_BUCKET_NAME", "")
	t.Setenv("R2_PRIVATE_PUBLIC_URL", "")
	t.Setenv("LEGAL_PAGE_R2_BUCKET_NAME", "")
}

func hasFinding(findings []finding, code, status, impact string) bool {
	for _, finding := range findings {
		if finding.Code == code && finding.Status == status && finding.CreateImpact == impact {
			return true
		}
	}
	return false
}
