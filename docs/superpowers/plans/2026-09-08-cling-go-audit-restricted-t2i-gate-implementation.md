# Cling Go 审核受限 T2I 门禁与没收实施计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `superpowers:subagent-driven-development`（推荐）或 `superpowers:executing-plans` 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在 Go 的生成提交器向中台发送任务前，为审核受限用户执行独立内容审核；审核拒绝没收预留且不退款，审核依赖故障则不发送任务并冲正。

**架构：** 创作仍在同一 MongoDB 事务中占位、预扣并写入生成提交 Outbox。提交器领取事件后从 Go 用户库读取 `content_access`：`standard` 用户直接走既有中台提交，`review_restricted` 用户先调用 Go 直连审核端口。审核拒绝通过同一 MongoDB 事务收敛创作、步骤、预留和 Outbox；审核不可用复用既有“确定未受理”冲正路径。

**技术栈：** Go、Kratos/Wire、MongoDB 单节点副本集 `rs0`、`net/http`、现有 Outbox/账本/生成提交状态机、`httptest`。

> 本项目不是 Git 仓库。禁止初始化 Git、创建 worktree、commit 或 push；每个任务以定向测试和完整回归替代提交检查点。

> **执行状态（2026-09-09）：** 任务 1 至任务 5 已在本地完成并复核。已通过本机 rs0 的全量测试、竞态检测、`go vet` 与构建；未启动 Worker，未请求真实审核或生成中台，未部署到任何环境。

---

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `internal/biz/identity/model.go` | 固定 Go 用户内容访问枚举，避免业务代码散落字符串。 |
| `internal/biz/contentreview/reviewer.go` | 定义审核请求、允许/拒绝/不可用结果和窄接口；不依赖 HTTP 或 MongoDB。 |
| `internal/integrations/contentreview/xai_client.go` | 以 Go 直连方式调用兼容 OpenAI Chat Completions 的审核服务；不调用 Node。 |
| `internal/integrations/contentreview/xai_client_test.go` | 验证请求最小化、超时、响应解析和敏感信息不泄露。 |
| `internal/biz/generation/submission.go` | 为已领取提交补充用户内容访问事实和审核拒绝终态命令。 |
| `internal/biz/creations/model.go` | 增加 `confiscated` 创作和步骤终态。 |
| `internal/data/generation_submission_repository.go` | 原子读取用户内容访问，并以 CAS 持久化审核没收终态。 |
| `internal/data/generation_submission_repository_test.go` | 本机 rs0 验证归属读取、终态 CAS 与精确清理。 |
| `internal/biz/ledger/usecase.go` | 提供 `ConfiscateInTx`，使预留没收可与创作和 Outbox 同事务执行。 |
| `internal/biz/ledger/usecase_test.go` | 验证没收不退款、不恢复日免、并发冲突与事务回滚。 |
| `internal/worker/generation_submission.go` | 在首次中台提交前执行审核，并分流允许、拒绝、不可用。 |
| `internal/worker/generation_submission_test.go` | 验证审核分流，不启动循环、不请求真实中台。 |
| `internal/conf/conf.proto`、`internal/conf/conf.pb.go` | 增加可选的 Go 内容审核集成配置。 |
| `internal/conf/validate.go`、`internal/conf/validate_test.go` | 校验审核配置；未配置时只在审核受限任务发生时安全失败。 |
| `configs/config*.yaml` | 只保留本机无效占位配置，绝不写真实密钥。 |

### 任务 1：冻结内容访问与审核领域合同

**文件：**
- 修改：`internal/biz/identity/model.go`
- 创建：`internal/biz/contentreview/reviewer.go`
- 创建：`internal/biz/contentreview/reviewer_test.go`

- [x] **步骤 1：编写失败的领域测试**

```go
func TestReviewRequest只接受审核所需提示词事实(t *testing.T) {
	request, err := contentreview.NewRequest("creation-1", "step-1", "安全的肖像", "不要血腥")
	if err != nil || request.CreationID != "creation-1" || request.StepID != "step-1" {
		t.Fatalf("NewRequest() = %#v, %v", request, err)
	}
}

func TestDecision拒绝未知状态(t *testing.T) {
	if err := (contentreview.Decision{Outcome: "unknown"}).Validate(); !errors.Is(err, contentreview.ErrInvalidDecision) {
		t.Fatalf("Validate() = %v, want ErrInvalidDecision", err)
	}
}
```

- [x] **步骤 2：运行测试确认失败**

运行：`go test ./internal/biz/contentreview -run 'TestReviewRequest|TestDecision' -count=1`

预期：FAIL，提示 `contentreview` 包或 `NewRequest` 未定义。

- [x] **步骤 3：实现最小领域类型与内容访问枚举**

在 `identity/model.go` 增加固定值；不要把审核规则写进身份模块：

```go
const (
	ContentAccessStandard         = "standard"
	ContentAccessReviewRestricted = "review_restricted"
)
```

创建 `reviewer.go`，只声明最小协议：

```go
type Outcome string
const (
	OutcomeAllowed Outcome = "allowed"
	OutcomeRejected Outcome = "rejected"
)
type Request struct { CreationID, StepID, Prompt, NegativePrompt string }
type Decision struct { Outcome Outcome; ReasonCode string }
type Reviewer interface { Review(context.Context, Request) (Decision, error) }
```

`Request` 不含用户 ID、余额、VIP、模板、资产 URL、会话、支付或账本；`Decision` 不保存模型原文理由。

- [x] **步骤 4：运行领域测试确认通过**

运行：`go test ./internal/biz/contentreview -count=1`

预期：PASS。

### 任务 2：实现 Go 直连审核客户端与可选配置

**文件：**
- 修改：`internal/conf/conf.proto`
- 修改：`internal/conf/validate.go`
- 修改：`internal/conf/validate_test.go`
- 修改：`configs/config.yaml`
- 修改：`configs/config.local.yaml.example`
- 修改：`configs/config.docker.yaml`
- 创建：`internal/integrations/contentreview/xai_client.go`
- 创建：`internal/integrations/contentreview/xai_client_test.go`

- [x] **步骤 1：编写失败的客户端与配置测试**

```go
func TestClient审核请求不发送业务或账本字段(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, forbidden := range []string{"diamond", "vip", "balance", "userId", "callback"} {
			if bytes.Contains(bytes.ToLower(body), []byte(forbidden)) { t.Fatalf("forbidden field %q", forbidden) }
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"allowed\":true}"}}]}`))
	}))
	defer server.Close()
	client, err := contentreviewintegration.NewClient(server.URL, "local-test-review-key", "grok-3-fast", time.Second, server.Client())
	if err != nil { t.Fatal(err) }
	decision, err := client.Review(context.Background(), contentreview.Request{CreationID: "c", StepID: "s", Prompt: "safe"})
	if err != nil || decision.Outcome != contentreview.OutcomeAllowed { t.Fatalf("Review() = %#v, %v", decision, err) }
}

func Test审核配置缺失不阻断普通启动(t *testing.T) {
	cfg := testConfig("mongodb://127.0.0.1:27017/?replicaSet=rs0", "cling_main")
	cfg.Integrations.ContentReview = nil
	if err := conf.Validate(cfg); err != nil { t.Fatal(err) }
}
```

- [x] **步骤 2：运行测试确认失败**

运行：`go test ./internal/integrations/contentreview ./internal/conf -run 'TestClient审核请求不发送业务或账本字段|Test审核配置缺失不阻断普通启动' -count=1`

预期：FAIL，提示 `ContentReview` 或客户端未定义。

- [x] **步骤 3：实现可选配置和严格 HTTP 客户端**

在 `Integrations` 中增加：

```proto
message ContentReview {
  string base_url = 1;
  string api_key = 2;
  string model = 3;
  google.protobuf.Duration timeout = 4;
}
ContentReview content_review = 4;
```

执行 `make config`，禁止手改 `conf.pb.go`。客户端仅接受 HTTP(S) Origin 或以 `/v1` 结尾的固定 API 前缀，并在其后拼接固定 `chat/completions` 路径；短超时和单次 JSON 响应是强制条件。返回结构不完整、网络失败或超时统一返回 `contentreview.ErrUnavailable`。默认配置不填密钥，且不得引用 Node 的 `XAI_API_KEY`。

- [x] **步骤 4：运行客户端和配置测试确认通过**

运行：`go test ./internal/integrations/contentreview ./internal/conf -count=1`

预期：PASS，且测试只访问 `httptest` 服务。

### 任务 3：补齐同事务没收与生成提交状态 CAS

**文件：**
- 修改：`internal/biz/ledger/usecase.go`
- 修改：`internal/biz/ledger/usecase_test.go`
- 修改：`internal/biz/creations/model.go`
- 修改：`internal/biz/generation/submission.go`
- 修改：`internal/biz/generation/submission_test.go`
- 修改：`internal/data/generation_submission_repository.go`
- 修改：`internal/data/generation_submission_repository_test.go`

- [x] **步骤 1：编写失败的账本与状态机测试**

```go
func TestConfiscateInTx不退款也不恢复日免(t *testing.T) {
	beforeBalance, beforeQuota := fixture.balance(), fixture.quotaUsed()
	err := fixture.tx.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		_, err := fixture.ledger.ConfiscateInTx(txCtx, fixture.creationID, "audit_rejected", fixture.now)
		return err
	})
	if err != nil || fixture.balance() != beforeBalance || fixture.quotaUsed() != beforeQuota { t.Fatalf("confiscation changed benefits") }
}

func TestMongoMarkConfiscated只接受持有租约的待提交步骤(t *testing.T) {
	err := fixture.store.MarkConfiscated(fixture.ctx, generation.ConfiscatedCommand{EventID: fixture.eventID, LeaseToken: fixture.leaseToken, At: fixture.now})
	if err != nil { t.Fatal(err) }
	fixture.assertCreationStatus("confiscated")
	fixture.assertStepStatus("confiscated")
	fixture.assertOutboxStatus("failed")
}
```

- [x] **步骤 2：运行测试确认失败**

运行：`CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/biz/ledger ./internal/biz/generation ./internal/data -run 'TestConfiscateInTx|TestMongoMarkConfiscated' -count=1`

预期：FAIL，提示 `ConfiscateInTx`、`ConfiscatedCommand` 或 `MarkConfiscated` 未定义。

- [x] **步骤 3：实现条件没收状态机**

将 `ledger.Confiscate` 重构为对事务包装器的薄调用，并增加：

```go
func (usecase *Usecase) ConfiscateInTx(ctx context.Context, creationID, reason string, at time.Time) (*Reservation, error)
```

它只允许 `reserved -> confiscated`，不写正向账本分录、不加钻石、不恢复日免。新增 `CreationStatusConfiscated` 与 `StepSubmitStatusConfiscated`；`SubmissionRecord` 增加 `UserID`、`ContentAccess`；`SubmissionStore` 增加 `MarkConfiscated`。MongoDB 实现必须在同一事务中用 `event_id + lease_token + dispatching/reconciling` 条件更新步骤、创作和 Outbox；任何 CAS 未命中返回 `generation.ErrSubmissionConflict`。

- [x] **步骤 4：运行定向测试确认通过**

运行：`CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/biz/ledger ./internal/biz/generation ./internal/data -run 'TestConfiscateInTx|TestMongoMarkConfiscated' -count=1`

预期：PASS；所有夹具在 `t.Cleanup` 中仅按随机精确 `_id` 执行 `DeleteOne`。

### 任务 4：在一次性提交器接入审核分流

**文件：**
- 修改：`internal/worker/generation_submission.go`
- 修改：`internal/worker/generation_submission_test.go`
- 修改：`cmd/ai-business-service/wire.go`
- 修改：`cmd/ai-business-service/wire_gen.go`（仅由 `make generate` 生成）

- [x] **步骤 1：编写失败的工作者测试**

```go
func TestDeliverOnce审核拒绝不调用生成中台且没收一次(t *testing.T) {
	fixture.record.ContentAccess = identity.ContentAccessReviewRestricted
	fixture.reviewer.decision = contentreview.Decision{Outcome: contentreview.OutcomeRejected, ReasonCode: "nsfw"}
	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.eventID); err != nil { t.Fatal(err) }
	if fixture.client.submitCalls != 0 || fixture.reverser.confiscateCalls != 1 || fixture.reverser.reverseCalls != 0 { t.Fatal("unexpected review settlement") }
}

func TestDeliverOnce审核不可用不调用生成中台并冲正(t *testing.T) {
	fixture.record.ContentAccess = identity.ContentAccessReviewRestricted
	fixture.reviewer.err = contentreview.ErrUnavailable
	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.eventID); err != nil { t.Fatal(err) }
	if fixture.client.submitCalls != 0 || fixture.reverser.reverseCalls != 1 { t.Fatal("unavailable review must reverse") }
}
```

- [x] **步骤 2：运行测试确认失败**

运行：`go test ./internal/worker -run 'TestDeliverOnce审核拒绝|TestDeliverOnce审核不可用' -count=1`

预期：FAIL，因为提交器尚未接收 `Reviewer`，也没有没收分支。

- [x] **步骤 3：实现审核优先于中台提交的分流**

扩展 `GenerationSubmissionWorker` 构造器注入 `contentreview.Reviewer` 和可同事务调用的 `ConfiscateInTx` 窄接口。首次 `submit` 前：

```go
if record.ContentAccess == identity.ContentAccessReviewRestricted {
	decision, err := worker.reviewer.Review(ctx, reviewRequestFromExecution(record, execution))
	if errors.Is(err, contentreview.ErrUnavailable) { return worker.reject(ctx, record, now) }
	if err != nil { return err }
	if decision.Outcome == contentreview.OutcomeRejected { return worker.confiscate(ctx, record, now) }
}
```

`confiscate` 必须在一个事务内按顺序执行 `submission.MarkConfiscated`、`ledger.ConfiscateInTx`、`outbox.MarkFailed`。审核允许后完全沿用既有 `Submit`、未知结果对账与回调路径。`main` 继续只装配 Worker，绝不启动轮询或真实网络请求。

- [x] **步骤 4：运行工作者测试确认通过**

运行：`go test ./internal/worker -count=1`

预期：PASS；标准用户跳过审核，拒绝不调用中台且不退款，不可用不调用中台且只冲正一次，重放不重复没收或冲正。

### 任务 5：生成代码、全量验证与范围审计

**文件：**
- 修改：由 `make config` 和 `make generate` 自动生成的受影响文件。

- [x] **步骤 1：重新生成配置和 Wire**

运行：`make config && make generate`

预期：生成文件与 `conf.proto`、Wire 提供器一致；不手工编辑 `conf.pb.go` 或 `wire_gen.go`。

- [x] **步骤 2：运行本机 rs0 集成和完整验证**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./... -count=1
go vet ./...
go build -o /private/tmp/ai-business-service-audit-gate ./cmd/ai-business-service
```

预期：全部 PASS；不启动 Worker、不访问真实 Grok、真实生成中台或生产 MongoDB。

- [x] **步骤 3：执行范围审计**

运行：`rg -n 'backend/|frontend/|PayCores|cling-ai\.com' internal cmd configs docs/superpowers/plans/2026-09-08-cling-go-audit-restricted-t2i-gate-implementation.md`

预期：仅文档中的边界说明允许命中；本任务不修改 Node/JS、前端、PayCores、现网钱包或生产配置。
