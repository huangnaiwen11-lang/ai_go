// polarstar-b2b-preflight validates the local prerequisites for PolarStar B2B
// without creating a provider job. Its default path performs no network I/O:
// mapping catalogs and product recipes remain explicitly unverified because
// they are Mongo-backed. --lookup is the sole opt-in network operation and
// uses the existing GET-only Lookup client with a fresh random identity.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"ai-business-service/internal/conf"
	"ai-business-service/internal/integrations/polarstarb2b"
	"ai-business-service/internal/integrations/r2"

	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"github.com/google/uuid"
)

const (
	defaultConfigPath = "/data/conf/config.yaml"

	createReadinessReady      = "ready"
	createReadinessBlocked    = "blocked"
	createReadinessUnverified = "unverified"

	createImpactNo      = "no"
	createImpactYes     = "yes"
	createImpactUnknown = "unknown"

	lookupStatusNotRequested = "not_requested"
	lookupStatusNotFound     = "not_found"
	lookupStatusMatched      = "matched"
	lookupStatusFailed       = "failed"
)

type report struct {
	CreateReadiness string       `json:"create_readiness"`
	Findings        []finding    `json:"findings"`
	Lookup          lookupReport `json:"lookup"`
}

// finding contains bounded operator classifications only. It never includes
// raw configuration errors, URLs, Mongo URIs, API keys, secrets or recipes.
type finding struct {
	Check        string `json:"check"`
	Status       string `json:"status"`
	Code         string `json:"code"`
	CreateImpact string `json:"blocks_real_create"`
	Detail       string `json:"detail"`
}

type lookupReport struct {
	Requested  bool   `json:"requested"`
	Attempted  bool   `json:"attempted"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Code       string `json:"code,omitempty"`
}

type lookupCaller func(context.Context, *polarstarb2b.Client, polarstarb2b.LookupKey) (polarstarb2b.Job, error)

func main() {
	configPath := flag.String("conf", defaultConfigPath, "本地 Bootstrap 配置路径")
	lookup := flag.Bool("lookup", false, "显式发起一次随机幂等键的 GET /api/v1/jobs/lookup；绝不建单")
	flag.Parse()

	result := run(*configPath, *lookup, os.Stdout)
	if result != 0 {
		os.Exit(result)
	}
}

func run(configPath string, lookup bool, stdout io.Writer) int {
	if stdout == nil {
		return 2
	}
	bootstrap, err := loadBootstrap(configPath)
	if err != nil {
		return writeReport(stdout, blockedLoadReport(lookup))
	}
	report := preflight(context.Background(), bootstrap, lookup, callLookup)
	return writeReport(stdout, report)
}

func writeReport(stdout io.Writer, result report) int {
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return 2
	}
	if result.CreateReadiness == createReadinessBlocked {
		return 1
	}
	return 0
}

func blockedLoadReport(lookup bool) report {
	result := report{Lookup: lookupReport{Requested: lookup, Status: lookupStatusNotRequested}}
	result.add("bootstrap", "fail", "bootstrap_load_failed", createImpactYes, "无法读取或解析本地 Bootstrap 配置。")
	result.add("polarstar_b2b", "skipped", "bootstrap_unavailable", createImpactUnknown, "Bootstrap 不可用，未检查 B2B 配置。")
	result.add("r2", "skipped", "bootstrap_unavailable", createImpactUnknown, "Bootstrap 不可用，未检查 R2 配置。")
	result.add("mapping_catalog", "not_checked", "mapping_catalog_not_inspected", createImpactUnknown, "已发布映射在 MongoDB 中；此命令默认不拨号，因此未读取。")
	result.add("product_recipes", "not_checked", "product_recipes_not_inspected", createImpactUnknown, "已发布配方在 MongoDB 中；此命令默认不拨号，因此未读取。")
	result.finish()
	return result
}

// preflight is deliberately local by default. NewClient validates only local
// transport configuration; it does not resolve DNS or issue HTTP requests.
func preflight(ctx context.Context, bootstrap *conf.Bootstrap, lookup bool, caller lookupCaller) report {
	result := report{Lookup: lookupReport{Requested: lookup, Status: lookupStatusNotRequested}}
	bootstrapValid := conf.Validate(bootstrap) == nil
	if !bootstrapValid {
		result.add("bootstrap", "fail", "bootstrap_validation_failed", createImpactYes, "本地 Bootstrap 未通过共享配置校验。")
	} else {
		result.add("bootstrap", "pass", "bootstrap_valid", createImpactNo, "本地 Bootstrap 已通过共享配置校验。")
	}

	b2b := bootstrap.GetIntegrations().GetGeneration().GetPolarstarB2B()
	var client *polarstarb2b.Client
	if b2b == nil {
		result.add("polarstar_b2b", "fail", "polarstar_b2b_missing", createImpactYes, "未配置 PolarStar B2B；真实 B2B 创建会被阻断。")
	} else {
		var err error
		client, err = newB2BClient(b2b)
		if err != nil {
			result.add("polarstar_b2b", "fail", "polarstar_b2b_invalid", createImpactYes, "PolarStar B2B 配置不能构造受限客户端；真实 B2B 创建会被阻断。")
		} else {
			defer client.CloseIdleConnections()
			result.add("polarstar_b2b", "pass", "polarstar_b2b_valid", createImpactNo, "PolarStar B2B 配置已通过本地客户端校验；未发起网络请求。")
		}
	}

	if b2b == nil {
		result.add("r2", "skipped", "polarstar_b2b_missing", createImpactUnknown, "未配置 B2B，未判定 R2 是否满足 B2B 结果落盘要求。")
	} else {
		_, enabled, err := r2.LoadConfig(os.Getenv)
		switch {
		case err != nil:
			result.add("r2", "fail", "r2_invalid_or_incomplete", createImpactYes, "R2 配置不完整或不安全；真实 B2B 创建会被阻断。")
		case !enabled:
			result.add("r2", "fail", "r2_missing", createImpactYes, "缺少 R2 配置；真实 B2B 创建会被阻断。")
		default:
			result.add("r2", "pass", "r2_valid", createImpactNo, "R2 配置已通过本地形状校验；未连接 R2。")
		}
	}

	// Both artifacts are intentionally Mongo-backed. Inspecting them would
	// require a local database connection, which violates the default no-dial
	// contract. Do not turn this into an implicit Mongo health check.
	result.add("mapping_catalog", "not_checked", "mapping_catalog_not_inspected", createImpactUnknown, "已发布映射在 MongoDB 中；默认预检不拨号，无法判定是否缺失。")
	result.add("product_recipes", "not_checked", "product_recipes_not_inspected", createImpactUnknown, "已发布配方在 MongoDB 中；默认预检不拨号，无法判定是否缺失。")

	if lookup {
		if !bootstrapValid {
			result.Lookup = lookupReport{Requested: true, Status: lookupStatusFailed, Code: "bootstrap_validation_failed"}
		} else {
			result.Lookup = performLookup(ctx, b2b.GetAccountRef(), client, caller)
		}
		if result.Lookup.Status == lookupStatusFailed {
			result.add("polarstar_lookup", "fail", "polarstar_lookup_failed", createImpactYes, "显式 GET lookup 未获得预期供应商响应；真实 B2B 创建可能被阻断。")
		} else {
			result.add("polarstar_lookup", "pass", "polarstar_lookup_responded", createImpactNo, "显式 GET lookup 已收到供应商响应；未创建供应商任务。")
		}
	}

	result.finish()
	return result
}

func newB2BClient(b2b *conf.Integrations_PolarStarB2B) (*polarstarb2b.Client, error) {
	if b2b == nil {
		return nil, errors.New("missing b2b config")
	}
	return polarstarb2b.NewClient(polarstarb2b.ClientOptions{
		BaseURL: b2b.GetBaseUrl(), APIKey: b2b.GetApiKey(), AccountRef: b2b.GetAccountRef(), ExpectedTenantID: b2b.GetTenantId(),
		Timeout: duration(b2b.GetHttpTimeout()), ConnectTimeout: duration(b2b.GetConnectTimeout()),
		ResponseHeaderTimeout: duration(b2b.GetResponseHeaderTimeout()), MaxConnectionsPerHost: int(b2b.GetMaxConnectionsPerHost()),
		MaxResponseBytes: b2b.GetMaxResponseBytes(),
	})
}

func duration(value interface{ AsDuration() time.Duration }) time.Duration {
	if value == nil {
		return 0
	}
	return value.AsDuration()
}

func performLookup(ctx context.Context, accountRef string, client *polarstarb2b.Client, caller lookupCaller) lookupReport {
	result := lookupReport{Requested: true, Status: lookupStatusFailed}
	if client == nil || caller == nil {
		result.Code = "local_precondition_failed"
		return result
	}
	key := polarstarb2b.LookupKey{
		AccountRef: accountRef,
		ExternalID: "preflight-" + uuid.NewString(),
		Capability: "text_to_image",
	}
	key.IdempotencyKey = "cling-step:" + key.ExternalID
	result.Attempted = true
	_, err := caller(ctx, client, key)
	if err == nil {
		result.Status = lookupStatusMatched
		return result
	}
	var clientError *polarstarb2b.ClientError
	if errors.As(err, &clientError) {
		result.HTTPStatus, result.Code = clientError.HTTPStatus, clientError.Code
		if clientError.HTTPStatus == 404 && clientError.Code == "NOT_FOUND" {
			result.Status = lookupStatusNotFound
		}
		return result
	}
	result.Code = "local_lookup_error"
	return result
}

func callLookup(ctx context.Context, client *polarstarb2b.Client, key polarstarb2b.LookupKey) (polarstarb2b.Job, error) {
	return client.Lookup(ctx, key)
}

func (report *report) add(check, status, code, impact, detail string) {
	report.Findings = append(report.Findings, finding{Check: check, Status: status, Code: code, CreateImpact: impact, Detail: detail})
}

func (report *report) finish() {
	report.CreateReadiness = createReadinessReady
	for _, finding := range report.Findings {
		switch finding.CreateImpact {
		case createImpactYes:
			report.CreateReadiness = createReadinessBlocked
			return
		case createImpactUnknown:
			report.CreateReadiness = createReadinessUnverified
		}
	}
}

func loadBootstrap(path string) (*conf.Bootstrap, error) {
	configuration := config.New(config.WithSource(file.NewSource(path), env.NewSource("KRATOS")))
	if err := configuration.Load(); err != nil {
		_ = configuration.Close()
		return nil, fmt.Errorf("load config: %w", err)
	}
	defer configuration.Close()
	bootstrap := &conf.Bootstrap{}
	if err := configuration.Scan(bootstrap); err != nil {
		return nil, fmt.Errorf("scan config: %w", err)
	}
	if err := conf.ApplyMongoEnvironmentOverrides(bootstrap, os.Getenv); err != nil {
		return nil, fmt.Errorf("apply mongo environment: %w", err)
	}
	if err := conf.ApplyPolarStarB2BEnvironmentOverrides(bootstrap, os.Getenv); err != nil {
		return nil, fmt.Errorf("apply polarstar environment: %w", err)
	}
	return bootstrap, nil
}
