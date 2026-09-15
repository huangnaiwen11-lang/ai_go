# Cling `execution.v2` 共享技术快照实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `subagent-driven-development` 或 `executing-plans` 逐任务实施。项目不是 Git 仓库：不得初始化 Git、创建分支、工作树、提交或推送。

**目标：** 让创作预扣与生成客户端使用同一份、可冻结的 `execution.v2` 技术快照合同，阻止下游必拒绝的任务进入 Outbox。

**架构：** `internal/executionv2` 是不依赖 HTTP、MongoDB 或业务模块的纯 Go 合同包。`creations` 在事务前编译一次快照，复用它的摘要和 payload；`integrations/generation` 用同一合同校验并构建执行请求。

**技术栈：** Go 标准库、现有 MongoDB Go Driver v2、本地 `cling_main` / `rs0`。

---

## 固定边界

- 不改 Node/JS、PayCores、生成中台、现网钱包、生产配置、HTTP/gRPC 路由或后台投递循环。
- 不改变 Outbox 领取、租约和结案状态机。
- 只允许 `text_to_image`、`image_edit`、`image_to_video`。
- 所有新增注释使用简体中文；错误不得回显提示词、素材 URL、参数名、JSON、签名或密钥。
- Mongo 测试只用 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'`，并仅对随机精确 `_id` 使用 `DeleteOne` 清理。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/executionv2/contract.go` | capability、SKU、输入结构、素材和参数的单一真相。 |
| `internal/executionv2/snapshot.go` | 冻结原始输入、资源限制、摘要和 Outbox payload。 |
| `internal/executionv2/contract_test.go` | 三原子、素材、参数、重复键和资源限制的单元测试。 |
| `internal/biz/creations/model.go` | 删除重复的技术校验，调用共享合同。 |
| `internal/biz/creations/usecase.go` | 事务前只编译一次快照，复用摘要和 payload。 |
| `internal/integrations/generation/contract.go` | 将重复合同校验替换为共享合同适配。 |
| 相邻 `*_test.go` | 固定创作、客户端和跨包契约行为。 |

## 任务 1：建立冻结快照合同

- [ ] **步骤 1：写共享包红灯测试。**

```go
func TestCompile拒绝图像编辑的空素材与错误SKU(t *testing.T) {
	_, err := executionv2.Compile(
		executionv2.CapabilityImageEdit,
		"ps-image-edit-v1",
		[]byte(`{"prompt":"x","assets":[],"parameters":{}}`),
	)
	if !errors.Is(err, executionv2.ErrInvalidSnapshot) {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestCompile拒绝重复键和超限合法JSON(t *testing.T) {
	_, err := executionv2.Compile(executionv2.CapabilityTextToImage, "ps-image-v1", []byte(`{"prompt":"x","prompt":"y","assets":[],"parameters":{}}`))
	if !errors.Is(err, executionv2.ErrInvalidSnapshot) {
		t.Fatalf("Compile() error = %v", err)
	}
}
```

- [ ] **步骤 2：运行红灯。**

运行：`go test ./internal/executionv2 -count=1`

预期：编译失败，因为共享包尚不存在。

- [ ] **步骤 3：实现最小冻结合同。**

```go
type Snapshot struct {
	Capability Capability
	ModelSKU   string
	Input      []byte
	Digest     string
}

func Compile(capability Capability, modelSKU string, rawInput []byte) (*Snapshot, error) {
	// 先限制字节，再检查 JSON token，最后做 capability 语义校验。
}
```

实现精确 SKU 白名单、输入顶层/素材/参数白名单、HTTPS 素材规则、重复键检查、深度和节点限制。复制输入字节并以长度前缀计算摘要。`MarshalSubmissionPayload` 只输出 `capability`、`model_sku` 与 `input`；工作者必须通过共享合同解析该快照，不能手工伪造完整执行对象。

- [ ] **步骤 4：运行绿色单元验证。**

运行：`go test ./internal/executionv2 -count=1`

预期：PASS。

## 任务 2：让生成客户端适配共享合同

- [ ] **步骤 1：写客户端契约红灯测试。**

```go
func TestBuildExecution与共享快照接受集合一致(t *testing.T) {
	raw := []byte(`{"prompt":"x","assets":[],"parameters":{"width":1024}}`)
	if _, err := executionv2.Compile(executionv2.CapabilityTextToImage, "ps-image-v1", raw); err != nil { t.Fatal(err) }
	if _, err := generation.BuildExecution(generation.Step{ /* 同一 capability、SKU、输入 */ }); err != nil { t.Fatal(err) }
}
```

- [ ] **步骤 2：运行红灯。**

运行：`go test ./internal/integrations/generation -run 'TestBuildExecution与共享快照接受集合一致' -count=1`

预期：失败，当前客户端仍有独立规则。

- [ ] **步骤 3：替换独立校验。**

保留 `generation.Step`、`Input`、`Asset` 的调用形状；将其序列化为输入原始 JSON 后调用 `executionv2.Compile`，再从冻结快照构建 Execution。删除重复 SKU、素材、参数和重复键校验实现，但保留执行 envelope、最终 body 校验、签名和 HTTP 分类。

- [ ] **步骤 4：运行客户端回归。**

运行：`go test ./internal/integrations/generation -count=1`

预期：PASS，既有签名与 HTTP 测试不退化。

## 任务 3：让创作事务只编译一次快照

- [ ] **步骤 1：写创作红灯测试。**

```go
func TestCreateReserved拒绝客户端必拒绝的首步骤快照(t *testing.T) {
	request := validImageRequest()
	request.Plan = []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageEdit}}
	request.InitialSubmission = validInitialSubmission("ps-image-edit-v1", `{"prompt":"x","assets":[],"parameters":{}}`)
	_, err := fixture.usecase.CreateReserved(context.Background(), request)
	if !errors.Is(err, creations.ErrInvalidCreateCommand) { t.Fatalf("err = %v", err) }
	fixture.assertNoSubmissionEvents()
}
```

再覆盖空 prompt、HTTP 素材 URL、重复键、超限合法 JSON 和不同 capability 的 SKU。

- [ ] **步骤 2：运行红灯。**

运行：`go test ./internal/biz/creations -run 'TestCreateReserved拒绝客户端必拒绝的首步骤快照' -count=1`

预期：当前独立创作校验会放行。

- [ ] **步骤 3：一次编译并复用结果。**

在校验步骤计划后取得首个 `ready` 步骤；对 `InitialSubmission` 调用一次 `executionv2.Compile`。用其 Digest 计算私有请求指纹，用其 payload 创建 pending Event。删除 `creations` 内重复的 SKU、素材、参数和 JSON token 校验。

- [ ] **步骤 4：运行创作与本地 Mongo 回归。**

运行：

```bash
go test ./internal/biz/creations -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/biz/creations ./internal/data -run 'Test(CreateReserved.*提交事件|MongoCreateReserved.*Outbox)' -count=1
```

预期：PASS；非法快照在事务前拒绝，相同快照只产生一条稳定事件。

## 任务 4：跨包契约与完整验证

- [ ] **步骤 1：新增跨包契约测试。**

使用三种 capability 的合法 `Snapshot`，断言转换为 `generation.Step` 后均能 `BuildExecution`。对每种非法素材、SKU、参数和重复键断言两个入口都拒绝。

- [ ] **步骤 2：执行完整验证。**

运行：

```bash
go test ./internal/executionv2 ./internal/biz/creations ./internal/integrations/generation -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1
go vet ./internal/executionv2 ./internal/biz/creations ./internal/integrations/generation ./internal/data
go build -o /private/tmp/ai-business-service-executionv2-verify ./cmd/ai-business-service
```

预期：全部 PASS；不启动 worker，不发送真实 HTTP 请求。
