package conf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kratosconfig "github.com/go-kratos/kratos/v3/config"
	kratosfile "github.com/go-kratos/kratos/v3/config/file"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestValidateRejectsSharedNodeMongoDatabase(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "node_business")

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "mongo database") {
		t.Fatalf("Validate() error = %v, want mongo database isolation error", err)
	}
}

func TestValidateRejectsNonLocalMongoHost(t *testing.T) {
	cfg := testConfig("mongodb://node-mongo.example.test:27017/?replicaSet=rs0", "cling_main")

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "mongo host") {
		t.Fatalf("Validate() error = %v, want MongoDB host isolation error", err)
	}
}

func TestValidateLocalMongoRequiresDockerProfileForMongoHost(t *testing.T) {
	cases := []struct {
		name                string
		uri                 string
		enableDockerProfile bool
		wantError           string
	}{
		{
			name:      "默认拒绝 Docker 服务名",
			uri:       "mongodb://mongo:27017/?replicaSet=rs0",
			wantError: "docker local profile",
		},
		{
			name:                "显式 Docker 本地配置放行服务名",
			uri:                 "mongodb://mongo:27017/?replicaSet=rs0",
			enableDockerProfile: true,
		},
		{
			name: "localhost 不受 Docker 配置影响",
			uri:  "mongodb://localhost:27017/?replicaSet=rs0",
		},
		{
			name: "127 地址不受 Docker 配置影响",
			uri:  "mongodb://127.0.0.1:27017/?replicaSet=rs0",
		},
		{
			name:                "外部主机即使启用 Docker 配置仍拒绝",
			uri:                 "mongodb://remote.example.test:27017/?replicaSet=rs0",
			enableDockerProfile: true,
			wantError:           "mongo host",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(tc.uri, "cling_main")
			if tc.enableDockerProfile {
				setDockerLocalProfile(t, cfg.GetData().GetMongo())
			}

			err := ValidateLocalMongo(cfg.GetData())
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateLocalMongo() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("ValidateLocalMongo() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateLocalMongoRejectsUnsafeURIComponents(t *testing.T) {
	cases := []struct {
		name      string
		uri       string
		wantError string
	}{
		{
			name:      "多个主机",
			uri:       "mongodb://127.0.0.1:27017,mongo:27017/?replicaSet=rs0",
			wantError: "mongo host",
		},
		{
			name:      "错误端口",
			uri:       "mongodb://127.0.0.1:27018/?replicaSet=rs0",
			wantError: "mongo port",
		},
		{
			name:      "IPv6 环回地址",
			uri:       "mongodb://[::1]:27017/?replicaSet=rs0",
			wantError: "mongo host",
		},
		{
			name:      "错误副本集参数",
			uri:       "mongodb://127.0.0.1:27017/?replicaSet=other",
			wantError: "replicaSet",
		},
		{
			name:      "非独立数据库路径",
			uri:       "mongodb://127.0.0.1:27017/node_business?replicaSet=rs0",
			wantError: "mongo uri database",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(tc.uri, "cling_main")

			err := ValidateLocalMongo(cfg.GetData())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("ValidateLocalMongo() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateStagingMongoAcceptsSSHForwardedConnection(t *testing.T) {
	cfg := testConfig(
		"mongodb://staging-user:staging-pass@host.docker.internal:27019/ai-host-v2-staging?authSource=admin&directConnection=true",
		"ai-host-v2-staging",
	)
	cfg.Data.Mongo.ReplicaSet = ""
	cfg.Data.Mongo.TransactionsRequired = true

	if err := ValidateStagingMongo(cfg.GetData()); err != nil {
		t.Fatalf("ValidateStagingMongo() error = %v", err)
	}
}

func TestValidateStagingMongoRejectsUnsafeConnection(t *testing.T) {
	cases := []struct {
		name      string
		uri       string
		database  string
		wantError string
	}{
		{
			name:      "公网主机",
			uri:       "mongodb://staging-user:staging-pass@mongo.example.test:27019/ai-host-v2-staging?authSource=admin&directConnection=true",
			database:  "ai-host-v2-staging",
			wantError: "mongo host",
		},
		{
			name:      "错误数据库",
			uri:       "mongodb://staging-user:staging-pass@host.docker.internal:27019/other?authSource=admin&directConnection=true",
			database:  "other",
			wantError: "mongo database",
		},
		{
			name:      "关闭事务",
			uri:       "mongodb://staging-user:staging-pass@host.docker.internal:27019/ai-host-v2-staging?authSource=admin&directConnection=true",
			database:  "ai-host-v2-staging",
			wantError: "transactions",
		},
		{
			name:      "错误连接模式",
			uri:       "mongodb://staging-user:staging-pass@host.docker.internal:27019/ai-host-v2-staging?authSource=admin&replicaSet=rs0",
			database:  "ai-host-v2-staging",
			wantError: "directConnection",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(tc.uri, tc.database)
			cfg.Data.Mongo.ReplicaSet = ""
			cfg.Data.Mongo.TransactionsRequired = tc.wantError != "transactions"

			err := ValidateStagingMongo(cfg.GetData())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("ValidateStagingMongo() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateConfiguredMongoDefaultsToLocal(t *testing.T) {
	t.Setenv("CLING_MONGO_PROFILE", "")
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")

	if err := ValidateConfiguredMongo(cfg.GetData()); err != nil {
		t.Fatalf("ValidateConfiguredMongo() error = %v", err)
	}
}

func TestValidateConfiguredMongoUsesExplicitStagingProfile(t *testing.T) {
	t.Setenv("CLING_MONGO_PROFILE", "staging")
	cfg := testConfig(
		"mongodb://staging-user:staging-pass@host.docker.internal:27019/ai-host-v2-staging?authSource=admin&directConnection=true",
		"ai-host-v2-staging",
	)
	cfg.Data.Mongo.ReplicaSet = ""
	cfg.Data.Mongo.TransactionsRequired = true

	if err := ValidateConfiguredMongo(cfg.GetData()); err != nil {
		t.Fatalf("ValidateConfiguredMongo() error = %v", err)
	}
}

func TestValidateUsesConfiguredStagingProfile(t *testing.T) {
	t.Setenv("CLING_MONGO_PROFILE", MongoProfileStaging)
	cfg := testConfig(
		"mongodb://staging-user:staging-pass@host.docker.internal:27019/ai-host-v2-staging?authSource=admin&directConnection=true",
		"ai-host-v2-staging",
	)
	cfg.Data.Mongo.ReplicaSet = ""

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateConfiguredMongoRejectsUnknownProfile(t *testing.T) {
	t.Setenv("CLING_MONGO_PROFILE", "production")
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")

	err := ValidateConfiguredMongo(cfg.GetData())
	if err == nil || !strings.Contains(err.Error(), "unknown MongoDB profile") {
		t.Fatalf("ValidateConfiguredMongo() error = %v, want unknown profile error", err)
	}
}

func TestApplyMongoEnvironmentOverridesConfiguresStaging(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
	values := map[string]string{
		"CLING_MONGO_PROFILE":  "staging",
		"CLING_MONGO_URI":      "mongodb://staging-user:staging-pass@host.docker.internal:27019/ai-host-v2-staging?authSource=admin&directConnection=true",
		"CLING_MONGO_DATABASE": "ai-host-v2-staging",
	}

	if err := ApplyMongoEnvironmentOverrides(cfg, func(name string) string { return values[name] }); err != nil {
		t.Fatalf("ApplyMongoEnvironmentOverrides() error = %v", err)
	}

	mongo := cfg.GetData().GetMongo()
	if mongo.GetUri() != values["CLING_MONGO_URI"] || mongo.GetDatabase() != values["CLING_MONGO_DATABASE"] || mongo.GetReplicaSet() != "" || !mongo.GetTransactionsRequired() || mongo.GetDockerLocalProfile() {
		t.Fatalf("staging Mongo override = %+v", mongo)
	}
}

func TestApplyMongoEnvironmentOverridesRequiresStagingURI(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
	err := ApplyMongoEnvironmentOverrides(cfg, func(name string) string {
		if name == "CLING_MONGO_PROFILE" {
			return "staging"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "CLING_MONGO_URI") {
		t.Fatalf("ApplyMongoEnvironmentOverrides() error = %v, want missing URI", err)
	}
}

func TestValidateRequiresOwnCallbackBaseURL(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
	cfg.Integrations.Generation.CallbackBaseUrl = ""

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "callback base url") {
		t.Fatalf("Validate() error = %v, want callback base url error", err)
	}
}

// 生成中台 API Key 用于租户准入，缺失时不能依赖 HMAC 签名侥幸发送请求。
func TestValidateRequiresIndependentGenerationAPIKey(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
	cfg.Integrations.Generation.ApiKey = ""

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "generation api key") {
		t.Fatalf("Validate() error = %v, want generation api key error", err)
	}
}

func TestValidateRequiresDirectionalGenerationHMACKeys(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*Security)
		wantError string
	}{
		{
			name: "缺少生成请求密钥",
			configure: func(security *Security) {
				security.GenerationRequestHmacKey = ""
			},
			wantError: "generation request hmac key",
		},
		{
			name: "缺少生成回调密钥",
			configure: func(security *Security) {
				security.GenerationCallbackHmacKey = ""
			},
			wantError: "generation callback hmac key",
		},
		{
			name: "两个方向不能复用同一密钥",
			configure: func(security *Security) {
				security.GenerationCallbackHmacKey = security.GenerationRequestHmacKey
			},
			wantError: "must differ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
			tc.configure(cfg.Security)

			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateRequiresGenerationHMACKeyMinimumLength(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*Security)
	}{
		{
			name: "请求密钥过短",
			configure: func(security *Security) {
				security.GenerationRequestHmacKey = strings.Repeat("a", 31)
			},
		},
		{
			name: "回调密钥过短",
			configure: func(security *Security) {
				security.GenerationCallbackHmacKey = strings.Repeat("b", 31)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
			tc.configure(cfg.Security)
			if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "at least 32") {
				t.Fatalf("Validate() error = %v, want minimum key length error", err)
			}
		})
	}
}

// 密码哈希参数属于账号入口的安全边界；缺失或降级配置必须在启动前失败，不能悄然
// 退回到弱哈希或依赖机器默认值。
func TestValidateRejectsWeakPasswordHashPolicy(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*Security)
		wantError string
	}{
		{
			name: "内存低于安全下限",
			configure: func(security *Security) {
				security.PasswordMemoryKib = 19*1024 - 1
			},
			wantError: "password memory",
		},
		{
			name: "盐长度低于安全下限",
			configure: func(security *Security) {
				security.PasswordSaltBytes = 15
			},
			wantError: "password salt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
			tc.configure(cfg.Security)
			if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

// TestRuntimeConfigsPassValidation 防止普通本地、Docker 及可复制示例配置遗漏新增的启动安全字段。
// 测试只解析仓库内的 YAML，不会连接 MongoDB、支付渠道或生成中台。
func TestRuntimeConfigsPassValidation(t *testing.T) {
	configFiles := []string{"config.yaml", "config.docker.yaml", "config.local.yaml.example"}
	for _, configFile := range configFiles {
		t.Run(configFile, func(t *testing.T) {
			configPath := filepath.Join("..", "..", "configs", configFile)
			// Kratos 以文件扩展名选择解码器，示例文件必须先复制为临时 .yaml
			// 才能验证“用户复制后能否启动”的内容契约。
			if strings.HasSuffix(configFile, ".example") {
				raw, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatalf("读取示例配置失败: %v", err)
				}
				configPath = filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(configPath, raw, 0o600); err != nil {
					t.Fatalf("写入临时示例配置失败: %v", err)
				}
			}
			loaded := kratosconfig.New(kratosconfig.WithSource(kratosfile.NewSource(configPath)))
			t.Cleanup(func() { _ = loaded.Close() })
			if err := loaded.Load(); err != nil {
				t.Fatalf("加载运行配置失败: %v", err)
			}

			var bootstrap Bootstrap
			if err := loaded.Scan(&bootstrap); err != nil {
				t.Fatalf("解析运行配置失败: %v", err)
			}
			if err := Validate(&bootstrap); err != nil {
				t.Fatalf("运行配置未通过启动校验: %v", err)
			}
		})
	}
}

func TestValidateGenerationBaseURLRequiresOrigin(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		wantError string
	}{
		{
			name:    "纯 Origin 可用",
			baseURL: "http://generation.local",
		},
		{
			name:      "不能带路径",
			baseURL:   "http://generation.local/api",
			wantError: "generation base url",
		},
		{
			name:      "不能带根路径",
			baseURL:   "https://generation.local/",
			wantError: "generation base url",
		},
		{
			name:      "不能带查询参数",
			baseURL:   "http://generation.local?next=value",
			wantError: "generation base url",
		},
		{
			name:      "不能带裸查询符",
			baseURL:   "http://generation.local?",
			wantError: "generation base url",
		},
		{
			name:      "不能带片段",
			baseURL:   "http://generation.local#fragment",
			wantError: "generation base url",
		},
		{
			name:      "不能带裸片段符",
			baseURL:   "http://generation.local#",
			wantError: "generation base url",
		},
		{
			name:      "不能带用户信息",
			baseURL:   "http://user@generation.local",
			wantError: "generation base url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
			cfg.Integrations.Generation.BaseUrl = tc.baseURL
			err := Validate(cfg)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateCallbackBaseURLRequiresSecurePureOrigin(t *testing.T) {
	cases := []struct {
		name      string
		callback  string
		wantError string
	}{
		{
			name:     "本地回环地址可使用 HTTP",
			callback: "http://127.0.0.1:18000",
		},
		{
			name:      "外部地址必须使用 HTTPS",
			callback:  "http://callbacks.example.test",
			wantError: "callback base url",
		},
		{
			name:     "外部 HTTPS 地址可用",
			callback: "https://callbacks.example.test",
		},
		{
			name:      "拒绝默认域名",
			callback:  "https://cling-ai.com",
			wantError: "callback base url",
		},
		{
			name:      "拒绝默认域名尾随点",
			callback:  "https://cling-ai.com.",
			wantError: "callback base url",
		},
		{
			name:      "拒绝默认子域和多个尾随点",
			callback:  "https://CALLBACK.CLING-AI.COM..",
			wantError: "callback base url",
		},
		{
			name:      "回调地址不能带路径",
			callback:  "http://127.0.0.1:18000/internal/v1/generation/callbacks",
			wantError: "callback base url",
		},
		{
			name:      "回调地址不能带查询参数",
			callback:  "http://127.0.0.1:18000?next=value",
			wantError: "callback base url",
		},
		{
			name:      "回调地址不能带裸查询符",
			callback:  "http://127.0.0.1:18000?",
			wantError: "callback base url",
		},
		{
			name:      "回调地址不能带片段",
			callback:  "http://127.0.0.1:18000#fragment",
			wantError: "callback base url",
		},
		{
			name:      "回调地址不能带裸片段符",
			callback:  "http://127.0.0.1:18000#",
			wantError: "callback base url",
		},
		{
			name:      "回调地址不能带用户信息",
			callback:  "http://user@127.0.0.1:18000",
			wantError: "callback base url",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
			cfg.Integrations.Generation.CallbackBaseUrl = tc.callback

			err := Validate(cfg)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateRequiresMongoReplicaSetTransactions(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017", "cling_main")
	cfg.Data.Mongo.TransactionsRequired = false

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "transactions") {
		t.Fatalf("Validate() error = %v, want transaction requirement error", err)
	}
}

func TestValidateRequiresHTTPServerConfig(t *testing.T) {
	cases := []struct {
		name      string
		server    *Server
		wantError string
	}{
		{
			name:      "缺少 Server",
			wantError: "server config",
		},
		{
			name:      "缺少 Server HTTP",
			server:    &Server{},
			wantError: "http server config",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
			cfg.Server = tc.server

			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateAcceptsIsolatedLocalMongoConfiguration(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

// 审核服务是受限用户投递阶段的可选依赖，缺失时不能阻断普通用户启动。
func Test审核配置缺失不阻断普通启动(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
	cfg.Integrations.ContentReview = nil

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func setDockerLocalProfile(t *testing.T, mongo *Data_Mongo) {
	t.Helper()
	field := mongo.ProtoReflect().Descriptor().Fields().ByName("docker_local_profile")
	if field == nil {
		t.Fatal("mongo 配置缺少 docker_local_profile")
	}
	mongo.ProtoReflect().Set(field, protoreflect.ValueOfBool(true))
}

// testConfig 构造不含真实密钥的本地配置，只用于验证配置边界。
func testConfig(uri, database string) *Bootstrap {
	return &Bootstrap{
		Server: &Server{
			Http: &Server_HTTP{Addr: "127.0.0.1:18000"},
		},
		Data: &Data{
			Mongo: &Data_Mongo{
				Uri:                  uri,
				Database:             database,
				ReplicaSet:           "rs0",
				TransactionsRequired: true,
			},
		},
		Security: &Security{
			SessionSigningKey:         "local-session-key-for-test-only",
			GenerationRequestHmacKey:  "local-generation-request-key-for-test-only",
			GenerationCallbackHmacKey: "local-generation-callback-key-for-test-only",
			PaycoresRequestHmacKey:    "local-paycores-request-key-for-test-only",
			PaycoresCallbackHmacKey:   "local-paycores-callback-key-for-test-only",
			PasswordMemoryKib:         19 * 1024,
			PasswordTimeCost:          2,
			PasswordParallelism:       1,
			PasswordSaltBytes:         16,
			PasswordKeyBytes:          32,
		},
		Integrations: &Integrations{
			Generation: &Integrations_Generation{
				BaseUrl:         "http://generation.local",
				CallbackBaseUrl: "http://127.0.0.1:18000",
				ApiKey:          "local-generation-api-key-for-test-only",
			},
			Paycores: &Integrations_Paycores{BaseUrl: "http://paycores.local", ReturnUrl: "http://127.0.0.1:5173/payment/success", CancelUrl: "http://127.0.0.1:5173/payment/cancel"},
			AppStore: &Integrations_AppStore{BaseUrl: "http://appstore.local"},
		},
		Worker: &Worker{
			PollInterval: durationpb.New(time.Second),
			BatchSize:    20,
		},
	}
}
