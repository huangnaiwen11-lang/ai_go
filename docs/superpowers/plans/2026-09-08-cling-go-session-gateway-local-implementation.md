# Cling Go 自有会话 Gateway 本地接入实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `superpowers:subagent-driven-development`（推荐）或 `superpowers:executing-plans` 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在不接管任何公开路由的前提下，为 Gateway 提供可开关、仅本机 MongoDB 可用的 Go 自有会话验证器，确保账户封禁、删除、撤销和过期会话立即失效。

**架构：** 新增 `internal/transport/sessionauth`，其仅解析 `Authorization` Bearer Token 并调用既有 `identity.Usecase.ValidateSession`。Gateway 启动入口只在 `GATEWAY_GO_SESSION_AUTH_ENABLED=true` 时装配本机身份依赖；默认不读配置、不连 MongoDB，所有请求继续透明代理 Node。身份解析器暂不接入任何公开路径，纯 T2I 路由分流另行设计。

**技术栈：** Go、标准库 `net/http`、Kratos 根层响应编码、MongoDB Go Driver v2、本机 `cling_main/rs0`、Go testing。

---

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `internal/biz/shared/errors.go` | 增加不泄露内部原因的认证失败与依赖不可用 API 错误。 |
| `internal/transport/sessionauth/authenticator.go` | 解析单个 Bearer 会话 ID，调用窄验证器，返回最小 `AuthenticatedIdentity`。 |
| `internal/transport/sessionauth/authenticator_test.go` | 用内存验证器锁定 Header、令牌和会话失效边界。 |
| `internal/transport/sessionauth/mongo_integration_test.go` | 仅用本机 rs0 验证封禁后旧会话立即失效及精确清理。 |
| `cmd/api-gateway/session_auth.go` | 把本机 data 仓储、identity 用例和传输验证器装配为可选依赖。 |
| `cmd/api-gateway/main.go` | 读取独立开关，组合可选依赖 cleanup，并把验证器保留给下一阶段候选 Handler。 |
| `cmd/api-gateway/main_test.go` | 验证开关、配置加载、本地 Mongo 限制和 cleanup 的恰好一次语义。 |

## 任务 1：固定会话认证错误合同

**文件：**

- 修改：`internal/biz/shared/errors.go`
- 修改：`internal/biz/shared/errors_test.go`

- [x] **步骤 1：先写会失败的错误合同测试**

在 `errors_test.go` 增加两个表驱动用例，断言认证失败与依赖失败的 HTTP 语义不依赖传输层：

```go
{
    name: "无效会话固定为未认证",
    err: ErrUnauthenticated,
    wantStatus: http.StatusUnauthorized,
    wantCode: "UNAUTHORIZED",
    wantMessage: "Authentication required",
},
{
    name: "会话存储失败固定为服务不可用",
    err: ErrServiceUnavailable,
    wantStatus: http.StatusServiceUnavailable,
    wantCode: "SERVICE_UNAVAILABLE",
    wantMessage: "Service unavailable",
},
```

测试还必须断言 `Details()` 为 `nil`，避免令牌、会话 ID 或 MongoDB 错误进入响应。

- [x] **步骤 2：运行测试并确认红灯**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/shared -run 'Test.*(Unauthenticated|ServiceUnavailable)' -count=1
```

预期：编译失败，提示 `ErrUnauthenticated` 与 `ErrServiceUnavailable` 未定义。

- [x] **步骤 3：添加最小领域错误**

在 `errors.go` 的既有 `var` 块中增加中文注释和两个不可变错误值：

```go
// ErrUnauthenticated 表示缺失、格式错误或已失效的 Go 自有会话。
ErrUnauthenticated = &APIError{
    status:  http.StatusUnauthorized,
    code:    "UNAUTHORIZED",
    message: "Authentication required",
}
// ErrServiceUnavailable 表示本地会话依赖不可用，不能误判为用户会话无效。
ErrServiceUnavailable = &APIError{
    status:  http.StatusServiceUnavailable,
    code:    "SERVICE_UNAVAILABLE",
    message: "Service unavailable",
}
```

在文件 import 中加入标准库 `net/http`。不要给 `APIError` 增加可变字段，不要增加登录、创建或权益错误。

- [x] **步骤 4：运行目标测试确认绿灯**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/shared -count=1
```

预期：通过。

- [x] **步骤 5：检查本任务范围**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && rg -n 'ErrUnauthenticated|ErrServiceUnavailable' internal/biz/shared
```

预期：只出现在领域错误及其测试中；不应出现 Node、PayCores、钱包或公开路由改动。

## 任务 2：实现纯会话认证器

**文件：**

- 创建：`internal/transport/sessionauth/authenticator.go`
- 创建：`internal/transport/sessionauth/authenticator_test.go`

- [x] **步骤 1：先写会失败的认证器测试**

创建测试包 `sessionauth`，定义可记录输入的内存验证器：

```go
type fakeSessionValidator struct {
    gotSessionID string
    session      *identity.Session
    err          error
}

func (fake *fakeSessionValidator) ValidateSession(_ context.Context, sessionID string, _ time.Time) (*identity.Session, error) {
    fake.gotSessionID = sessionID
    return fake.session, fake.err
}
```

覆盖以下单一行为测试：

1. 只有 `Authorization: Bearer session-1` 时返回 `AuthenticatedIdentity{UserID: "user-1"}`，且验证器只收到 `session-1`。
2. Header 缺失、重复、`Basic`、空 Bearer、含控制字符、超过 128 字符时，返回 `shared.ErrUnauthenticated`，且验证器未被调用。
3. Cookie、query `token`、`X-User-Id` 存在但无 Bearer 时仍返回 `shared.ErrUnauthenticated`。
4. 验证器返回 `identity.ErrSessionInvalid`、`identity.ErrSessionNotFound` 或空 `Session/UserID` 时统一返回 `shared.ErrUnauthenticated`。
5. 验证器返回任意其他错误时返回 `shared.ErrServiceUnavailable`，测试不得在错误文本中比较令牌值。
6. 验证器收到的 `now` 必须来自传入的固定时钟且已转换为 UTC。

- [x] **步骤 2：运行测试并确认红灯**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/transport/sessionauth -count=1
```

预期：编译失败，提示包或 `NewAuthenticator` 未定义。

- [x] **步骤 3：实现最小认证器**

在 `authenticator.go` 中实现以下窄接口与类型：

```go
type sessionValidator interface {
    ValidateSession(context.Context, string, time.Time) (*identity.Session, error)
}

type AuthenticatedIdentity struct {
    UserID string
}

type Authenticator struct {
    validator sessionValidator
    now       func() time.Time
}

func NewAuthenticator(validator sessionValidator) *Authenticator
func NewAuthenticatorWithClock(validator sessionValidator, now func() time.Time) *Authenticator
func (authenticator *Authenticator) Authenticate(request *http.Request) (*AuthenticatedIdentity, error)
```

实现要求：

- 使用 `request.Header.Values("Authorization")`，只允许恰好一个值；`Header.Get` 不能识别重复 Header，因此不得使用。
- 只接受大小写不敏感的 `Bearer`，并用 `strings.Fields` 解析为恰好两个字段；session ID 不做 URL 解码。
- session ID 长度为 `1..128`，并拒绝任意 Unicode 控制字符。
- `request == nil`、验证器为 `nil`、时钟为 `nil` 与空返回 Session/UserID 都必须 fail closed；前两类配置/依赖错误映射为 `shared.ErrServiceUnavailable`，客户端令牌问题映射为 `shared.ErrUnauthenticated`。
- 调用 `validator.ValidateSession(request.Context(), sessionID, now().UTC())`。只将身份的 `UserID` 返回给调用方；不写响应、不把身份写入 Header、不记录或回显机密。

所有导出类型、构造函数和关键安全分支使用简体中文注释。不要在本任务中添加 HTTP 路由、中间件、JWT、Cookie、MongoDB 或登录接口。

- [x] **步骤 4：运行认证器测试确认绿灯**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/transport/sessionauth -count=1
```

预期：通过。

- [x] **步骤 5：格式化并复跑**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && gofmt -w internal/biz/shared/errors.go internal/biz/shared/errors_test.go internal/transport/sessionauth/authenticator.go internal/transport/sessionauth/authenticator_test.go && go test ./internal/biz/shared ./internal/transport/sessionauth -count=1
```

预期：格式化后测试仍通过。

## 任务 3：本机 MongoDB 会话失效回归

**文件：**

- 创建：`internal/transport/sessionauth/mongo_integration_test.go`

- [x] **步骤 1：先写会失败的 rs0 集成测试**

复用 `internal/data` 已有本机测试辅助模式，但在 `sessionauth` 测试中只建立随机测试事实：一个 `normal/bound` 用户和一条有效会话。测试过程如下：

```go
identityUsecase := identity.NewUsecase(
    data.NewUserRepository(storage),
    data.NewIdentityRepository(storage),
    data.NewSessionRepository(storage),
    data.NewTxRunner(storage),
)
authenticator := sessionauth.NewAuthenticator(identityUsecase)

// 第一次请求必须认证成功。
// ChangeAccountStatus(..., identity.AccountStatusBanned) 后，使用同一 Bearer token 必须返回 shared.ErrUnauthenticated。
```

测试必须：

- 通过 `CLING_TEST_MONGO_URI` 读取 URI；未设置则 `t.Skip`，不得回退到生产或外部地址。
- 使用 `conf.ValidateLocalMongo` 验证 `cling_main/rs0` 本地限制。
- 在开始前运行 `data.NewLocalSchemaInitializer(storage).Ensure(ctx)`。
- cleanup 对 `sessions`、`users` 均仅执行带随机 `_id` 的 `DeleteOne`；禁止 `DeleteMany`、删除集合、删除索引或删库。
- 在同一测试中确认会话被撤销或版本失配后不再认证成功。

- [x] **步骤 2：运行集成验证基线**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/transport/sessionauth -run TestMongoSessionAuth封禁后旧会话立即失效 -count=1
```

预期：通过，证明任务 2 的认证器能够与既有身份用例和本机 rs0 正确协作；若本机 rs0 不可用，报告连接失败但不得尝试其他地址。

- [x] **步骤 3：只修正测试装配问题**

若测试无法构造 `data.Data` 所需的本机配置，新增测试私有辅助函数：从 `CLING_TEST_MONGO_URI` 构造 `conf.Data_Mongo`，显式填写 database `cling_main`、replica set `rs0` 和 `transactions_required=true`；然后用 `data.NewData` 获取 cleanup。

不得向生产代码增加测试专用开关，也不得把测试数据写入现网 Node 集合。

- [x] **步骤 4：再次运行目标集成测试**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/transport/sessionauth -run TestMongoSessionAuth封禁后旧会话立即失效 -count=1
```

预期：通过；测试退出后仅随机创建的两条文档被精确删除。

- [x] **步骤 5：复跑包级回归**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/transport/sessionauth ./internal/biz/identity ./internal/data -count=1
```

预期：通过。

## 任务 4：Gateway 可选依赖装配与安全回退

**文件：**

- 创建：`cmd/api-gateway/session_auth.go`
- 修改：`cmd/api-gateway/main.go`
- 修改：`cmd/api-gateway/main_test.go`

- [x] **步骤 1：先写会失败的 Gateway 装配测试**

在 `main_test.go` 添加以下测试：

1. `GATEWAY_GO_SESSION_AUTH_ENABLED` 未设置、`false`、任意非 `true` 值时，`goSessionAuthEnabled()` 都返回 `false`；仅大小写无关的 `true` 返回 `true`。
2. `newOptionalSessionAuthenticator(false, missingPath)` 返回 `(nil, nil, nil)`，证明关闭时不读配置、不连 MongoDB。
3. `newOptionalSessionAuthenticator(true, missingPath)` 返回错误且不返回认证器或 cleanup。
4. `newConfiguredSessionAuthenticator(&conf.Data{})` 拒绝非本地 MongoDB 配置且不返回认证器或 cleanup。
5. `combineCleanups` 对两个非空 cleanup 按注册的逆序各执行一次；空 cleanup 不执行。使用计数器，不启动监听器。
6. 已有 `runGateway` 测试改为通过合并 cleanup，分别验证监听成功与失败时每个已装配资源恰好清理一次。

- [x] **步骤 2：运行测试并确认红灯**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./cmd/api-gateway -run 'Test(GoSessionAuth|NewOptionalSession|NewConfiguredSession|CombineCleanups|RunGateway)' -count=1
```

预期：编译失败，提示开关、装配函数或 cleanup 组合函数未定义。

- [x] **步骤 3：实现最小可选装配**

创建 `session_auth.go`，按现有 `generation_callback.go` 的本地安全模式实现：

```go
func newConfiguredSessionAuthenticator(dataConfig *conf.Data) (*sessionauth.Authenticator, func(), error) {
    if err := conf.ValidateLocalMongo(dataConfig); err != nil {
        return nil, nil, err
    }
    storage, cleanup, err := data.NewData(dataConfig)
    if err != nil {
        return nil, nil, err
    }
    fail := func(cause error) (*sessionauth.Authenticator, func(), error) {
        cleanup()
        return nil, nil, cause
    }
    // 用 10 秒 context 初始化既有本地 schema；之后仅组装仓储与用例。
}
```

完成装配时使用：

```go
identityUsecase := identity.NewUsecase(
    data.NewUserRepository(storage),
    data.NewIdentityRepository(storage),
    data.NewSessionRepository(storage),
    data.NewTxRunner(storage),
)
return sessionauth.NewAuthenticator(identityUsecase), cleanup, nil
```

在 `main.go` 中：

- 新增 `goSessionAuthEnabled()`，只接受 `strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_GO_SESSION_AUTH_ENABLED")), "true")`。
- 使用既有 `loadOptionalGatewayBootstrap` 的关闭短路，不复制配置读取或绕过 `conf.Validate`。
- 当生成回调与会话认证同时开启时，允许分别装配本机依赖，但使用 `combineCleanups` 统一返回一个 cleanup；任一后续装配失败时，必须先清理已成功装配的资源。
- 暂不把认证器传入 `gateway.Config`，也不对现有 Gateway Handler 加身份检查；这是「不新增公开路由、不影响 Node 代理」的关键边界。
- 启动日志只记录开关是否启用，不记录 URI、会话 ID、用户 ID 或密钥。

为 cleanup 实现一个窄函数：

```go
func combineCleanups(cleanups ...func()) func() {
    return func() {
        for index := len(cleanups) - 1; index >= 0; index-- {
            if cleanups[index] != nil {
                cleanups[index]()
            }
        }
    }
}
```

每个导出/关键安全函数增加简体中文注释。不要改 `internal/gateway` 的精确路由表，不要新增公开 HTTP Handler，不要改变 `GATEWAY_GENERATION_CALLBACK_ENABLED` 语义。

- [x] **步骤 4：运行 Gateway 目标测试确认绿灯**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && gofmt -w cmd/api-gateway/main.go cmd/api-gateway/main_test.go cmd/api-gateway/session_auth.go && go test ./cmd/api-gateway -count=1
```

预期：通过；默认关闭路径不需要本地 MongoDB。

- [x] **步骤 5：执行本地 Gateway 启动回退核验**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && GATEWAY_GO_SESSION_AUTH_ENABLED=false BACKEND_UPSTREAM='http://127.0.0.1:1' go run ./cmd/api-gateway
```

在另一个本地终端执行：

```bash
curl -i --max-time 3 http://127.0.0.1:18000/api/chat/image/async
```

预期：请求仍由 Node 上游处理并以 Gateway 的现有 `502 UPSTREAM_BAD_RESPONSE` 失败；不得出现 Go `401`，证明本阶段没有接管公开生成路由。核验结束后通过终端中断命令停止该临时进程。

## 任务 5：全量验证与范围审计

**文件：**

- 修改：仅在前 4 个任务为满足测试所需的文件。

- [x] **步骤 1：运行完整本机测试与竞态检查**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1 && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./... -count=1
```

预期：全部通过。若 MongoDB 未运行，应报告本机依赖不可用，不得替换为任何远程 URI。

- [x] **步骤 2：执行静态检查、构建与语义契约**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && go vet ./... && go build -o /private/tmp/ai-business-service-gateway-session-auth ./cmd/api-gateway && go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json
```

预期：静态检查、Gateway 构建和语义契约均通过。

- [x] **步骤 3：执行范围扫描**

运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && rg -n 'POST /api/chat/image/async|chat/image/async|GATEWAY_GO_SESSION_AUTH_ENABLED|Authorization' internal cmd docs/superpowers/specs/2026-09-08-cling-go-session-gateway-local-design.md docs/superpowers/plans/2026-09-08-cling-go-session-gateway-local-implementation.md
```

预期：`POST /api/chat/image/async` 只能出现在规格、计划或测试回退断言中；实现代码不得注册该公开路由、不得修改 Node/JS、前端、PayCores、钱包、生产配置或 Worker。

- [x] **步骤 4：人工复核失败语义与清理语义**

逐项确认：

- 所有无效 Go 会话对外均为 `401 UNAUTHORIZED`，不会区分封禁、删除、撤销、过期或不存在。
- 数据库意外失败为 `503 SERVICE_UNAVAILABLE`，没有被压成 `401`。
- 登录态解析不读取 Node Token、Cookie、query、`X-User-Id` 或客户端传入用户 ID。
- 默认关闭不连 MongoDB、不开公开路由、所有请求保持 Node 透明代理。
- 所有测试 cleanup 都是随机 `_id` 的 `DeleteOne`，没有任何广泛删除。
