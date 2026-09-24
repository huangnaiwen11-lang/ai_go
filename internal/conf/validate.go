package conf

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"ai-business-service/internal/biz/authcredential"
)

const (
	MongoProfileLocal   = "local"
	MongoProfileStaging = "staging"

	localMongoDatabase   = "cling_main"
	localMongoReplica    = "rs0"
	stagingMongoDatabase = "ai-host-v2-staging"
	minimumHMACKey       = 32
)

// Validate 验证配置的协议隔离与事务前提；生产 Mongo 接入尚未实现。
// 该校验只检查配置边界，不会尝试连接 MongoDB、生成中台或支付渠道。
func Validate(cfg *Bootstrap) error {
	if cfg == nil {
		return errors.New("missing bootstrap config")
	}
	if err := validateServer(cfg.GetServer()); err != nil {
		return err
	}
	if err := ValidateConfiguredMongo(cfg.GetData()); err != nil {
		return err
	}
	if err := ValidateGeneration(cfg.GetIntegrations().GetGeneration(), cfg.GetSecurity(), cfg.GetEnvironment()); err != nil {
		return err
	}
	if err := validateSecurity(cfg.GetSecurity()); err != nil {
		return err
	}
	if err := validateIntegrations(cfg.GetIntegrations()); err != nil {
		return err
	}
	return validateWorker(cfg.GetWorker())
}

// ValidateConfiguredMongo 根据显式 CLING_MONGO_PROFILE 选择校验规则。
// 未显式选择时保持 local，只有 staging profile 才允许 SSH 转发的测试库。
func ValidateConfiguredMongo(data *Data) error {
	profile := strings.TrimSpace(os.Getenv("CLING_MONGO_PROFILE"))
	if profile == "" {
		profile = MongoProfileLocal
	}
	return ValidateMongoProfile(data, profile)
}

// ApplyMongoEnvironmentOverrides 将显式 staging profile 的 URI 注入进程配置。
// local profile 不读取这些变量，避免本地默认配置被测试凭据污染。
func ApplyMongoEnvironmentOverrides(bootstrap *Bootstrap, getenv func(string) string) error {
	if bootstrap == nil || getenv == nil {
		return nil
	}
	profile := strings.TrimSpace(getenv("CLING_MONGO_PROFILE"))
	if profile == "" || profile == MongoProfileLocal {
		return nil
	}
	if profile != MongoProfileStaging {
		return fmt.Errorf("unknown MongoDB profile %q", profile)
	}

	uri := strings.TrimSpace(getenv("CLING_MONGO_URI"))
	if uri == "" {
		return errors.New("CLING_MONGO_URI is required for staging MongoDB profile")
	}
	if bootstrap.Data == nil {
		bootstrap.Data = &Data{}
	}
	if bootstrap.Data.Mongo == nil {
		bootstrap.Data.Mongo = &Data_Mongo{}
	}
	mongo := bootstrap.Data.Mongo
	mongo.Uri = uri
	mongo.Database = strings.TrimSpace(getenv("CLING_MONGO_DATABASE"))
	if mongo.Database == "" {
		mongo.Database = stagingMongoDatabase
	}
	mongo.ReplicaSet = ""
	mongo.TransactionsRequired = true
	mongo.DockerLocalProfile = false
	return nil
}

// ValidateMongoProfile 按命名 profile 校验 Mongo 配置。未知 profile 必须失败，
// 防止拼写错误意外绕过 local/staging 的安全边界。
func ValidateMongoProfile(data *Data, profile string) error {
	switch strings.TrimSpace(profile) {
	case MongoProfileLocal:
		return ValidateLocalMongo(data)
	case MongoProfileStaging:
		return ValidateStagingMongo(data)
	default:
		return fmt.Errorf("unknown MongoDB profile %q", profile)
	}
}

// ValidateLocalMongo 验证 MongoDB 仅指向本地独立的 cling_main 副本集。
// 本函数不会建立网络连接，可供启动入口、独立命令和测试复用同一安全边界。
func ValidateLocalMongo(data *Data) error {
	if data == nil {
		return errors.New("missing MongoDB data config")
	}
	mongo := data.GetMongo()
	if mongo == nil {
		return errors.New("missing mongo config")
	}
	if mongo.GetDatabase() != localMongoDatabase {
		return fmt.Errorf("mongo database must be %q", localMongoDatabase)
	}
	if mongo.GetReplicaSet() != localMongoReplica {
		return fmt.Errorf("mongo replica set must be %q", localMongoReplica)
	}
	if !mongo.GetTransactionsRequired() {
		return errors.New("mongo transactions are required")
	}

	return validateLocalMongoURI(mongo.GetUri(), mongo.GetDockerLocalProfile())
}

// ValidateStagingMongo 验证显式 staging profile 使用 SSH 转发的测试库。
//
// staging 不是默认 profile：调用方必须显式选择它。连接仍被限制到本机
// SSH 转发端口，禁止把公网 Mongo 地址直接写入配置；事务也必须保持开启，
// 因为账本、幂等和 outbox 收敛依赖多文档事务。
func ValidateStagingMongo(data *Data) error {
	if data == nil {
		return errors.New("missing MongoDB data config")
	}
	mongo := data.GetMongo()
	if mongo == nil {
		return errors.New("missing mongo config")
	}
	if mongo.GetDatabase() != stagingMongoDatabase {
		return fmt.Errorf("mongo database must be %q", stagingMongoDatabase)
	}
	if strings.TrimSpace(mongo.GetReplicaSet()) != "" {
		return errors.New("staging mongo replica set must be empty for directConnection")
	}
	if !mongo.GetTransactionsRequired() {
		return errors.New("staging mongo transactions are required")
	}

	uri, err := url.Parse(strings.TrimSpace(mongo.GetUri()))
	if err != nil || uri.Scheme != "mongodb" || strings.TrimSpace(uri.Host) == "" {
		return errors.New("staging mongo uri must be a mongodb connection string")
	}
	if strings.Contains(uri.Host, ",") {
		return errors.New("staging mongo host must contain exactly one SSH-forwarded host")
	}
	host, port, err := net.SplitHostPort(uri.Host)
	if err != nil || host == "" {
		return errors.New("staging mongo host must include a host and port")
	}
	if !isAllowedStagingMongoHost(host) {
		return errors.New("staging mongo host must be host.docker.internal or loopback")
	}
	if !isAllowedStagingMongoPort(port) {
		return errors.New("staging mongo port must be an SSH-forwarded local port (27018 or 27019)")
	}
	if uri.User == nil || uri.User.Username() == "" {
		return errors.New("staging mongo uri must include credentials")
	}
	if authSource := uri.Query().Get("authSource"); authSource != "admin" {
		return errors.New("staging mongo uri must use authSource=admin")
	}
	if directConnection := uri.Query().Get("directConnection"); directConnection != "true" {
		return errors.New("staging mongo uri must use directConnection=true")
	}
	if replicaSets, declared := uri.Query()["replicaSet"]; declared && len(replicaSets) > 0 {
		return errors.New("staging mongo uri must not declare replicaSet")
	}
	uriDatabase := strings.TrimPrefix(uri.Path, "/")
	if uriDatabase != "" && uriDatabase != stagingMongoDatabase {
		return fmt.Errorf("staging mongo uri database must be %q", stagingMongoDatabase)
	}
	return nil
}

func isAllowedStagingMongoHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "host.docker.internal", "127.0.0.1", "localhost":
		return true
	default:
		return false
	}
}

func isAllowedStagingMongoPort(port string) bool {
	return port == "27018" || port == "27019"
}

func validateLocalMongoURI(rawURI string, dockerLocalProfile bool) error {
	uri, err := url.Parse(strings.TrimSpace(rawURI))
	if err != nil || uri.Scheme != "mongodb" || strings.TrimSpace(uri.Host) == "" {
		return errors.New("mongo uri must be a mongodb connection string")
	}
	if strings.Contains(uri.Host, ",") {
		return errors.New("mongo host must contain exactly one local host")
	}

	host, port, err := net.SplitHostPort(uri.Host)
	if err != nil || host == "" {
		return errors.New("mongo host must include a local host and port")
	}
	if strings.EqualFold(strings.TrimSpace(host), "mongo") && !dockerLocalProfile {
		return errors.New("mongo host requires docker local profile")
	}
	if !isAllowedLocalMongoHost(host, dockerLocalProfile) {
		return errors.New("mongo host must be 127.0.0.1, localhost, or mongo")
	}
	if port != "27017" {
		return errors.New("mongo port must be 27017")
	}

	uriDatabase := strings.TrimPrefix(uri.Path, "/")
	if uriDatabase != "" && uriDatabase != localMongoDatabase {
		return fmt.Errorf("mongo uri database must be %q when present", localMongoDatabase)
	}

	replicaSets, declared := uri.Query()["replicaSet"]
	if !declared || len(replicaSets) != 1 || replicaSets[0] != localMongoReplica {
		return fmt.Errorf("mongo uri must declare replicaSet=%s", localMongoReplica)
	}
	return nil
}

func isAllowedLocalMongoHost(host string, dockerLocalProfile bool) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "localhost":
		return true
	case "mongo":
		return dockerLocalProfile
	default:
		return false
	}
}

func validateServer(server *Server) error {
	if server == nil {
		return errors.New("missing server config")
	}
	if server.GetHttp() == nil {
		return errors.New("missing http server config")
	}
	return nil
}

func validateSecurity(security *Security) error {
	if security == nil || strings.TrimSpace(security.GetSessionSigningKey()) == "" {
		return errors.New("missing session signing key")
	}
	paycoresRequestKey := strings.TrimSpace(security.GetPaycoresRequestHmacKey())
	paycoresCallbackKey := strings.TrimSpace(security.GetPaycoresCallbackHmacKey())
	if len(paycoresRequestKey) < minimumHMACKey {
		return fmt.Errorf("paycores request hmac key must be at least %d characters", minimumHMACKey)
	}
	if len(paycoresCallbackKey) < minimumHMACKey {
		return fmt.Errorf("paycores callback hmac key must be at least %d characters", minimumHMACKey)
	}
	if paycoresRequestKey == paycoresCallbackKey {
		return errors.New("paycores request and callback hmac keys must differ")
	}
	if security.GetPasswordMemoryKib() < authcredential.MinimumMemoryKiB {
		return fmt.Errorf("password memory must be at least %d KiB", authcredential.MinimumMemoryKiB)
	}
	if security.GetPasswordTimeCost() < authcredential.MinimumTimeCost {
		return fmt.Errorf("password time cost must be at least %d", authcredential.MinimumTimeCost)
	}
	if security.GetPasswordParallelism() < uint32(authcredential.MinimumParallelism) || security.GetPasswordParallelism() > 255 {
		return errors.New("password parallelism is invalid")
	}
	if security.GetPasswordSaltBytes() < authcredential.MinimumSaltBytes {
		return fmt.Errorf("password salt must be at least %d bytes", authcredential.MinimumSaltBytes)
	}
	if security.GetPasswordKeyBytes() < authcredential.MinimumKeyBytes {
		return fmt.Errorf("password key must be at least %d bytes", authcredential.MinimumKeyBytes)
	}
	return nil
}

func validateIntegrations(integrations *Integrations) error {
	if integrations == nil || integrations.GetGeneration() == nil {
		return errors.New("missing generation integration")
	}
	if integrations.GetPaycores() == nil {
		return errors.New("missing paycores integration")
	}
	if err := validateHTTPURL("paycores base url", integrations.GetPaycores().GetBaseUrl()); err != nil {
		return err
	}
	if err := validateHTTPURL("paycores return url", integrations.GetPaycores().GetReturnUrl()); err != nil {
		return err
	}
	if err := validateHTTPURL("paycores cancel url", integrations.GetPaycores().GetCancelUrl()); err != nil {
		return err
	}
	if integrations.GetAppStore() == nil {
		return errors.New("missing app store integration")
	}
	return validateHTTPURL("app store base url", integrations.GetAppStore().GetBaseUrl())
}

func validateHTTPURL(name, raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("invalid %s", name)
	}
	return nil
}

func validateGenerationBaseURL(raw string) error {
	normalized := strings.TrimSpace(raw)
	parsed, err := url.Parse(normalized)
	if err != nil || strings.Contains(normalized, "#") || parsed.Host == "" || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != "" {
		return errors.New("invalid generation base url")
	}
	return nil
}

func validateCallbackBaseURL(raw string) error {
	normalized := strings.TrimSpace(raw)
	parsed, err := url.Parse(normalized)
	if err != nil || strings.Contains(normalized, "#") || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("invalid callback base url")
	}
	if isDefaultGenerationCallbackHost(parsed.Hostname()) {
		return errors.New("invalid callback base url")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname()) {
		return errors.New("invalid callback base url")
	}
	return nil
}

func isDefaultGenerationCallbackHost(host string) bool {
	normalized := strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
	return normalized == "cling-ai.com" || strings.HasSuffix(normalized, ".cling-ai.com")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func validateWorker(worker *Worker) error {
	if worker == nil || worker.GetPollInterval() == nil || worker.GetPollInterval().AsDuration() <= 0 {
		return errors.New("worker poll interval must be positive")
	}
	if worker.GetBatchSize() <= 0 {
		return errors.New("worker batch size must be positive")
	}
	return nil
}
