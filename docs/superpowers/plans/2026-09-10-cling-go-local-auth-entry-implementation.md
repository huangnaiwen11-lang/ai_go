# Cling Go 本地账号入口一期实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `superpowers:subagent-driven-development`（推荐）或 `superpowers:executing-plans` 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在本机 `cling_main/rs0` 中接管已冻结的邮箱密码注册、邮箱密码登录、移动端游客登录和当前用户读取四条精确路由，同时保持其余认证请求继续透明代理 Node。

**架构：** `identity` 负责用户、会话、游客和账号状态；新增 `authcredential` 负责密码规则与 Argon2id；`data` 只保存领域对象并在 MongoDB 事务中执行写入；`transport/authentry` 将严格 JSON 与固定 HTTP 合同映射到用例。Gateway 仅在两个显式开关与精确路由开关同时打开时注入本地 Handler，任何其它形态回退 Node。

**技术栈：** Go 1.25、MongoDB Go Driver v2、本机单节点副本集 `rs0`、`golang.org/x/crypto/argon2`、`crypto/rand`、`crypto/subtle`、`net/http`。

---

## 文件结构

- 新增 `internal/biz/authcredential/model.go`：密码凭据领域值、校验结果与错误。
- 新增 `internal/biz/authcredential/policy.go`：Argon2id 参数校验、编码、验证和需要重哈希的判断。
- 新增 `internal/biz/authcredential/repository.go`：不泄露 MongoDB 的凭据读写反转接口。
- 新增 `internal/biz/authcredential/policy_test.go`：密码和编码策略的红绿测试。
- 修改 `internal/biz/identity/{model.go,repository.go,usecase.go,usecase_test.go}`：注册、密码登录、游客登录、当前用户读取、会话签发及删除账号的凭据停用。
- 新增 `internal/data/credential_repository.go` 与测试：MongoDB 凭据、用户、账户、会话的原子存取实现。
- 修改 `internal/data/{model/identity.go,identity_repository.go,data.go,schema/collections.go,schema/indexes.go,migrate/local_schema.go,migrate/local_schema_test.go}`：集合、局部唯一索引、持久化模型和依赖注入。
- 新增 `internal/transport/authentry/{handler.go,handler_test.go}`：四条账号入口的严格 HTTP 适配器。
- 修改 `internal/biz/shared/errors.go`：账号入口专用、可稳定编码的错误。
- 修改 `internal/gateway/{proxy.go,route_switch.go,route_switch_test.go,proxy_test.go}`：账号路由精确匹配、请求分类与透明代理保护。
- 新增 `cmd/api-gateway/auth_entry.go` 并修改 `cmd/api-gateway/{main.go,main_test.go,session_auth.go}`：受控装配、生命周期清理与双开关。
- 修改 `internal/conf/{conf.proto,validate.go,validate_test.go}`、运行 `make config`，并修改 `configs/config.local.yaml.example`：本地密码策略安全下限。
- 新增 `cmd/api-gateway/auth_entry_e2e_test.go`：只对 `127.0.0.1/cling_main/rs0` 运行的端到端闭环。

### 任务 1：密码凭据领域与安全配置

**文件：**

- 创建：`internal/biz/authcredential/model.go`
- 创建：`internal/biz/authcredential/policy.go`
- 创建：`internal/biz/authcredential/policy_test.go`
- 修改：`internal/conf/conf.proto`
- 修改：`internal/conf/validate.go`
- 测试：`internal/conf/validate_test.go`

- [x] **步骤 1：先写密码策略和配置的失败测试。**

```go
func TestPolicyRejectsPasswordShorterThanSixCharacters(t *testing.T) {
    policy := authcredential.MustNewPolicy(authcredential.Params{MemoryKiB: 19 * 1024, TimeCost: 2, Parallelism: 1, SaltBytes: 16, KeyBytes: 32})
    if _, err := policy.Hash("12345"); !errors.Is(err, authcredential.ErrWeakPassword) {
        t.Fatalf("Hash() error = %v, want ErrWeakPassword", err)
    }
}

func TestPolicyHashVerifyAndRehash(t *testing.T) {
    old := authcredential.MustNewPolicy(authcredential.Params{MemoryKiB: 19 * 1024, TimeCost: 2, Parallelism: 1, SaltBytes: 16, KeyBytes: 32})
    encoded, err := old.Hash("correct-horse")
    if err != nil { t.Fatal(err) }
    current := authcredential.MustNewPolicy(authcredential.Params{MemoryKiB: 32 * 1024, TimeCost: 3, Parallelism: 1, SaltBytes: 16, KeyBytes: 32})
    verified, rehash, err := current.Verify(encoded, "correct-horse")
    if err != nil || !verified || !rehash { t.Fatalf("Verify() = %t, %t, %v", verified, rehash, err) }
}
```

- [x] **步骤 2：运行失败测试，确认失败原因是缺少 `authcredential`。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/authcredential ./internal/conf -run 'TestPolicy|TestPassword' -count=1`

预期：FAIL，编译错误指出 `internal/biz/authcredential` 不存在或密码配置字段不存在。

- [x] **步骤 3：实现最小且固定的 Argon2id 领域策略。**

```go
type Params struct { MemoryKiB uint32; TimeCost uint32; Parallelism uint8; SaltBytes uint32; KeyBytes uint32 }
type Policy struct { params Params; random io.Reader }

func (p *Policy) Hash(password string) (string, error) {
    if utf8.RuneCountInString(password) < 6 { return "", ErrWeakPassword }
    salt := make([]byte, p.params.SaltBytes)
    if _, err := io.ReadFull(p.random, salt); err != nil { return "", fmt.Errorf("read password salt: %w", err) }
    key := argon2.IDKey([]byte(password), salt, p.params.TimeCost, p.params.MemoryKiB, p.params.Parallelism, p.params.KeyBytes)
    return encodePHC(p.params, salt, key), nil
}

func (p *Policy) Verify(encoded, password string) (matched bool, needsRehash bool, err error) {
    parsed, err := decodePHC(encoded); if err != nil { return false, false, ErrInvalidCredential }
    candidate := argon2.IDKey([]byte(password), parsed.salt, parsed.timeCost, parsed.memoryKiB, parsed.parallelism, uint32(len(parsed.key)))
    if subtle.ConstantTimeCompare(candidate, parsed.key) != 1 { return false, false, nil }
    return true, parsed.params != p.params, nil
}
```

在 `Security` 中增加 `password_memory_kib`、`password_time_cost`、`password_parallelism`、`password_salt_bytes`、`password_key_bytes`；`Validate` 必须拒绝低于 `19 MiB / 2 / 1 / 16 / 32` 的值。执行 `make config`，只提交由 proto 重新生成的配置代码。

- [x] **步骤 4：运行密码与配置测试，确认通过。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/authcredential ./internal/conf -count=1`

预期：PASS。

- [x] **步骤 5：记录本地验证结果（本项目不是 Git 仓库）。**

本项目不是 Git 仓库；不执行 `git add`、`git commit`、初始化 Git 或创建 worktree。记录本地验证输出即可。

### 任务 2：账号入口领域用例与错误合同

**文件：**

- 修改：`internal/biz/identity/model.go`
- 修改：`internal/biz/identity/repository.go`
- 修改：`internal/biz/identity/usecase.go`
- 修改：`internal/biz/identity/usecase_test.go`
- 修改：`internal/biz/shared/errors.go`
- 测试：`internal/biz/shared/errors_test.go`

- [x] **步骤 1：编写失败的领域测试。**

```go
func TestRegisterCreatesBoundUserZeroBalanceAndThirtyDaySession(t *testing.T) {
    fixture := newAuthFixture()
    result, err := fixture.usecase.Register(context.Background(), identity.RegisterInput{
        Email: "  USER@example.test ", Password: "correct-horse", Timezone: "Asia/Shanghai", DisplayName: "User",
    })
    if err != nil { t.Fatal(err) }
    if result.User.BindingState != identity.BindingStateBound || result.AccountBalance != 0 { t.Fatalf("unexpected result: %#v", result) }
    if got := result.Session.ExpiresAt.Sub(fixture.now); got != 30*24*time.Hour { t.Fatalf("TTL = %s", got) }
}

func TestRegisterDeletedEmailCreatesNewUserButBannedEmailConflicts(t *testing.T) {
    fixture := newAuthFixture()
    fixture.seedCredential("deleted@example.test", identity.AccountStatusDeleted, false)
    if _, err := fixture.usecase.Register(context.Background(), identity.RegisterInput{Email: "deleted@example.test", Password: "correct-horse", Timezone: "UTC"}); err != nil { t.Fatal(err) }
    fixture.seedCredential("banned@example.test", identity.AccountStatusBanned, true)
    if _, err := fixture.usecase.Register(context.Background(), identity.RegisterInput{Email: "banned@example.test", Password: "correct-horse", Timezone: "UTC"}); !errors.Is(err, identity.ErrEmailAlreadyRegistered) { t.Fatalf("err = %v", err) }
}

func TestGuestLoginRejectsWebAndReusesSameMobileDeviceUser(t *testing.T) { /* iOS 两次为同一 userID；web 返回 ErrWebGuestLoginDisabled */ }
func TestPasswordLoginDoesNotDistinguishUnknownEmailFromWrongPassword(t *testing.T) { /* 两条路径均为 ErrInvalidCredentials */ }
```

- [x] **步骤 2：运行领域测试，确认它们因 API 尚不存在而失败。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/identity ./internal/biz/shared -run 'Test(Register|GuestLogin|PasswordLogin|AuthEntry)' -count=1`

预期：FAIL，缺少 `Register`、`LoginWithPassword`、`LoginGuest` 和账号入口错误。

- [x] **步骤 3：扩展领域模型和仓储边界。**

```go
type RegisterInput struct { Email, Password, Timezone, DisplayName string }
type PasswordLoginInput struct { Email, Password string }
type GuestLoginInput struct { Platform, DeviceID, Timezone string }
type LoginResult struct { User *User; Session *Session; AccountBalance int64 }

type UserRepository interface {
    Find(context.Context, string) (*User, error)
    Create(context.Context, User) error
    FindGuestByDevice(context.Context, string, string) (*User, error)
    ChangeAccountStatus(context.Context, string, AccountStatus, time.Time) (*User, error)
}
type SessionRepository interface { Find(context.Context, string) (*Session, error); Create(context.Context, Session) error; RevokeActiveByUser(context.Context, string, time.Time) error }
type AccountRepository interface { CreateZero(context.Context, string, time.Time) error }
```

`Register` 在一次 `WithinTx` 中调用 `credentials.Create`、`users.Create`、`accounts.CreateZero`、`sessions.Create`。`LoginWithPassword` 对未知邮箱使用固定 dummy PHC 做一次比对，两个失败情形均返回 `ErrInvalidCredentials`。`LoginGuest` 只接受 `ios`/`android` 与长度至少 8 的 `deviceId`；Web 在进入事务前返回 `ErrWebGuestLoginDisabled`。`ChangeAccountStatus` 在目标为 `deleted` 时调用 `credentials.DeactivateByUser`，并仍撤销会话、递增版本。

在 `shared/errors.go` 增加下面固定 HTTP 语义，transport 不自行猜测状态：

```go
ErrInvalidRequest = newAPIError(400, "INVALID_REQUEST", "Invalid request")
ErrInvalidCredentials = newAPIError(401, "INVALID_CREDENTIALS", "Invalid credentials")
ErrEmailAlreadyRegistered = newAPIError(409, "EMAIL_ALREADY_REGISTERED", "Email already registered")
ErrWebGuestLoginDisabled = newAPIError(403, "WEB_GUEST_LOGIN_DISABLED", "Web guest login is disabled")
```

- [x] **步骤 4：运行领域测试，确认通过且既有绑定测试不回归。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/identity ./internal/biz/shared -count=1`

预期：PASS。

- [x] **步骤 5：记录本地验证结果（本项目不是 Git 仓库）。**

本项目不是 Git 仓库；不执行 Git 写操作。

### 任务 3：本地 MongoDB 持久化、事务和局部唯一约束

**文件：**

- 修改：`internal/data/model/identity.go`
- 修改：`internal/data/identity_repository.go`
- 创建：`internal/data/credential_repository.go`
- 修改：`internal/data/data.go`
- 修改：`internal/data/schema/collections.go`
- 修改：`internal/data/schema/indexes.go`
- 修改：`internal/data/migrate/local_schema.go`
- 修改：`internal/data/migrate/local_schema_test.go`
- 测试：`internal/data/identity_repository_test.go`
- 测试：`internal/data/credential_repository_test.go`

- [ ] **步骤 1：先写 MongoDB 集成测试。**

```go
func TestMongoRegisterConcurrentSameActiveEmailCreatesExactlyOneUserAccountCredentialAndSession(t *testing.T) { /* 两个 goroutine，同一 email；一个成功，一个 ErrEmailAlreadyRegistered；四集合精确计数 */ }
func TestMongoDeletedCredentialStopsParticipatingInEmailUniqueness(t *testing.T) { /* delete 后 active=false；同邮箱注册成功且 userID 不同 */ }
func TestMongoGuestSamePlatformAndDeviceCreatesOneUserAndAccount(t *testing.T) { /* 事务并发执行，验证唯一 deviceKey */ }
```

每个测试先读取 `CLING_TEST_MONGO_URI`，仅接受 `conf.ValidateLocalMongo` 通过的 `127.0.0.1:27017`、`cling_main`、`rs0`；通过 UUID 记录每个文档 `_id`，在 `t.Cleanup` 用精确 `_id` 删除。

- [ ] **步骤 2：运行集成测试，确认失败。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run 'TestMongo(Register|DeletedCredential|Guest)' -count=1`

预期：FAIL，`credentials` 集合、局部索引或仓储方法缺失。

- [ ] **步骤 3：实现持久化对象、仓储和 schema。**

```go
type CredentialDocument struct {
    ID string `bson:"_id"`; UserID string `bson:"user_id"`; EmailNormalized string `bson:"email_normalized"`
    PasswordHash string `bson:"password_hash"`; Active bool `bson:"active"`; CreatedAt time.Time `bson:"created_at"`; UpdatedAt time.Time `bson:"updated_at"`
}
type UserDocument struct { /* 既有字段 */ DisplayName string `bson:"display_name,omitempty"`; GuestPlatform string `bson:"guest_platform,omitempty"`; GuestDeviceID string `bson:"guest_device_id,omitempty"` }
```

把 `credentials` 加入 `CollectionCredentials` 和 `AllCollections`。扩展 `IndexSpec`：

```go
PartialFilter bson.D
{Collection: CollectionCredentials, Name: "ux_credentials_active_email", Keys: bson.D{{Key: "email_normalized", Value: 1}}, Unique: true, PartialFilter: bson.D{{Key: "active", Value: true}}}
{Collection: CollectionUsers, Name: "ux_users_guest_platform_device", Keys: bson.D{{Key: "guest_platform", Value: 1}, {Key: "guest_device_id", Value: 1}}, Unique: true, Sparse: true}
```

`migrate.Initializer` 必须把 `PartialFilter` 映射为 `options.Index().SetPartialFilterExpression`，迁移测试必须断言它。`mongoCredentialRepository.Create` 把重复键转换为 `identity.ErrEmailAlreadyRegistered`，`DeactivateByUser` 仅更新当前用户的 `active=true` 文档。`CreateZero` 向既有 `accounts` 插入 `{_id:userID, diamond_balance:0}`；任何重复键都作为事务冲突回传，不能静默覆写余额。

- [ ] **步骤 4：运行 data 测试，确认通过。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data ./internal/data/migrate -count=1`

预期：PASS。

- [ ] **步骤 5：提交本任务。**

本项目不是 Git 仓库；不执行 Git 写操作。

### 任务 4：账号 HTTP Handler 的严格合同

**文件：**

- 创建：`internal/transport/authentry/handler.go`
- 创建：`internal/transport/authentry/handler_test.go`

- [x] **步骤 1：编写 Handler 失败测试。**

```go
func TestRegisterReturnsCreatedEnvelopeWithoutPasswordOrHash(t *testing.T) {
    handler := authentry.NewHandler(fakeUsecase{registerResult: loginResult("user-1", "session-secret")})
    request := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"email":"u@example.test","password":"correct-horse","timezone":"UTC"}`))
    recorder := httptest.NewRecorder(); handler.ServeHTTP(recorder, request)
    if recorder.Code != http.StatusCreated || strings.Contains(recorder.Body.String(), "correct-horse") || strings.Contains(recorder.Body.String(), "password_hash") { t.Fatalf("response = %s", recorder.Body.String()) }
}

func TestAuthEntryRejectsUnknownFieldsAndInvalidMethods(t *testing.T) { /* 400 INVALID_REQUEST；没有调用 usecase */ }
func TestMeUsesOnlyBearerSessionAndReturnsSafeProjection(t *testing.T) { /* Session auth 是唯一身份来源；余额、session ID 不出现 */ }
```

- [x] **步骤 2：运行 Handler 测试，确认失败。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/transport/authentry -count=1`

预期：FAIL，包不存在。

- [x] **步骤 3：实现严格 JSON Handler。**

```go
const maxRequestBytes = 64 << 10
const (registerPath = "/api/auth/register"; loginPath = "/api/auth/login"; guestPath = "/api/auth/guest"; mePath = "/api/auth/me")

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    switch { case isExact(r, http.MethodPost, registerPath): h.register(w,r)
    case isExact(r, http.MethodPost, loginPath): h.login(w,r)
    case isExact(r, http.MethodPost, guestPath): h.guest(w,r)
    case isExact(r, http.MethodGet, mePath): h.me(w,r)
    default: writeError(w, shared.ErrInvalidRequest) }
}
```

JSON 解码使用 `io.LimitReader`、`DisallowUnknownFields` 和 EOF 检查。注册成功只返回 `{user, token}`，游客登录只返回同形响应，`me` 只返回规格定义的用户安全投影；不要返回余额、密码、哈希、盐、会话版本、外部 subject 或内部 session ID。所有错误经 `shared.APIError` 输出根层 `{success:false,code,message,details:null}`。

- [x] **步骤 4：运行 Handler 测试，确认通过。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/transport/authentry -count=1`

预期：PASS。

- [x] **步骤 5：记录本地验证结果（本项目不是 Git 仓库）。**

本项目不是 Git 仓库；不执行 Git 写操作。

### 任务 5：Gateway 精确接管与 fail-closed 装配

**文件：**

- 修改：`internal/gateway/proxy.go`
- 修改：`internal/gateway/route_switch.go`
- 修改：`internal/gateway/route_switch_test.go`
- 修改：`internal/gateway/proxy_test.go`
- 创建：`cmd/api-gateway/auth_entry.go`
- 修改：`cmd/api-gateway/main.go`
- 修改：`cmd/api-gateway/main_test.go`
- 修改：`cmd/api-gateway/session_auth.go`

- [x] **步骤 1：编写 Gateway 和装配失败测试。**

```go
func TestAuthRoutesProxyUnlessSessionAndAuthEntrySwitchesAndExactRouteAreAllEnabled(t *testing.T) { /* 逐项关闭均打到 upstream；三项打开才进入 local handler */ }
func TestAuthEntryDoesNotClaimOAuthQueryOrEncodedPath(t *testing.T) { /* /api/auth/login?provider=google、/api/auth%2Flogin 都代理 Node */ }
func TestNewOptionalAuthEntryHandlerDoesNotReadConfigWhenDisabled(t *testing.T) { /* disabled + missing file => nil,nil,nil */ }
```

- [x] **步骤 2：运行 Gateway 测试，确认失败。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/gateway ./cmd/api-gateway -run 'Test(AuthRoutes|AuthEntry|OptionalAuth)' -count=1`

预期：FAIL，本地账号 Handler 尚未装入 `gateway.Config`。

- [x] **步骤 3：实现精确匹配与受控装配。**

```go
type Config struct { /* 既有字段 */ AuthEntryHandler http.Handler }
var authEntryRoutes = [...]exactRouteKey{
    {method:http.MethodPost, path:"/api/auth/register"}, {method:http.MethodPost, path:"/api/auth/login"},
    {method:http.MethodPost, path:"/api/auth/guest"}, {method:http.MethodGet, path:"/api/auth/me"},
}

func matchesAuthEntry(r *http.Request) bool {
    return r != nil && r.URL != nil && r.URL.RawQuery == "" && !r.URL.ForceQuery && r.URL.EscapedPath() == r.URL.Path && isAuthEntryRoute(exactRouteKey{method:r.Method,path:r.URL.Path})
}
```

在 `main.go` 新增 `GATEWAY_LOCAL_AUTH_ENTRY_ENABLED`，只有该值严格为 `true`、`GATEWAY_GO_SESSION_AUTH_ENABLED=true`、文件开关为对应精确路由 `true`、且本地 Handler 不为 nil 时才分流。`newConfiguredAuthEntryHandler` 与 session authenticator 共享**同一个** `Data` 生命周期，避免两个独立 Mongo 客户端和不一致 schema；因此将现有 `newConfiguredSessionAuthenticator` 调整为返回认证器与存储依赖的构造器，或新增专用 `newConfiguredLocalAuthDependencies`，由 auth、T2I、video 各自仍保持独立可选装配。关任何开关、配置读取失败、路径/查询不精确时全部代理 Node，永不将已接管注册请求重放到 Node。

- [x] **步骤 4：运行 Gateway 和所有既有本地路由测试，确认通过。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/gateway ./cmd/api-gateway -count=1`

预期：PASS。

- [x] **步骤 5：记录本地验证结果（本项目不是 Git 仓库）。**

本项目不是 Git 仓库；不执行 Git 写操作。

### 任务 6：本机端到端验收与回归验证

**文件：**

- 创建：`cmd/api-gateway/auth_entry_e2e_test.go`
- 修改：`configs/config.local.yaml.example`
- 修改：`README.md`

- [ ] **步骤 1：编写端到端失败测试。**

```go
func TestLocalAuthEntryE2ERegisterLoginGuestMeAndStatusInvalidation(t *testing.T) {
    fixture := newLocalAuthGatewayFixture(t)
    registered := fixture.post("/api/auth/register", `{"email":"auth-...@example.test","password":"correct-horse","timezone":"Asia/Shanghai"}`)
    fixture.assertStatus(registered, http.StatusCreated)
    fixture.assertMeWithBearer(registered.Token)
    fixture.assertGuestReusesIOSDevice("device-" + uuid.NewString())
    fixture.banRegisteredUser(); fixture.assertUnauthorized(registered.Token)
}
```

- [ ] **步骤 2：运行端到端测试，确认失败。**

运行：`cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./cmd/api-gateway -run TestLocalAuthEntryE2E -count=1`

预期：FAIL，直至 Handler、路由和真实 MongoDB 事务均已完成。

- [ ] **步骤 3：完成 fixture 和本地运行说明。**

fixture 只允许本机 rs0，所有创建的 user、credential、account、session 均以 UUID 精确追踪和清理。`README.md` 新增本地运行环境变量示例，其中 `BACKEND_UPSTREAM` 必须为本机 Node 地址、`GATEWAY_GO_SESSION_AUTH_ENABLED=true`、`GATEWAY_LOCAL_AUTH_ENTRY_ENABLED=true`，路由文件只列出四条账号路由；明确这些值不能用于生产。

- [ ] **步骤 4：运行完整验证。**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' GOCACHE=/private/tmp/ai-business-service-go-cache go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' GOCACHE=/private/tmp/ai-business-service-go-cache go test -race ./... -count=1
GOCACHE=/private/tmp/ai-business-service-go-cache go vet ./...
GOCACHE=/private/tmp/ai-business-service-go-cache go build -o /private/tmp/ai-business-service-api-gateway ./cmd/api-gateway
```

预期：全部 PASS；构建生成 `/private/tmp/ai-business-service-api-gateway`，不得写入仓库。

- [ ] **步骤 5：提交本任务。**

本项目不是 Git 仓库；不执行 Git 写操作。交付测试命令和输出摘要，并声明不连接真实中台、真实 PayCores、生产 MongoDB 或生产环境。

## 计划自检

- 规格中的 4 条路由、账号状态、删除邮箱重用、游客复用、30 天会话、零余额、Argon2id、透明代理、错误合同和本机 rs0 约束，分别由任务 1 至 6 覆盖。
- 没有未完成占位文本或未定义的方法作为实现指令。
- 所有跨层引用均遵守 `biz` 声明接口、`data` 实现接口、`transport` 仅调用用例、`cmd` 装配依赖的既有工程边界。
