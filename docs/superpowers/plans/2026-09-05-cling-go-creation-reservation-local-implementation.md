# Cling Go 本地创作占位与预留编排实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（- [ ]）语法跟踪进度。本项目没有 Git 仓库：不得创建提交、分支或工作树。

**目标：** 在本地 MongoDB 数据库 cling_main 中实现创作占位、内部步骤计划和账本预留的单个原子事务。

**架构：** creations.Usecase 是用户创建操作的总编排者，在同一 shared.TxRunner 事务中读取用户、调用权益策略、写入创作和步骤，并调用账本的事务内预留入口。data 只实现创作与步骤仓储；账本继续拥有日免条件占用、钻石预扣、预留和账本分录。

**技术栈：** Go 1.25、Kratos Wire、MongoDB Go Driver v2、本地单节点副本集 rs0、Go 标准库 crypto/sha256。

---

## 固定边界

- 只允许 text_to_image、image_edit、image_to_video 三个内部原子；领域层拒绝 Animate。
- 用户只选择模板；步骤计划是服务端内部命令，未来 HTTP 客户端不得传入技术原子。
- 不注册 HTTP/gRPC 业务路由，不调用生成中台，不写 Outbox，不接收回调，不修改 Node/JS、PayCores、现网钱包或生产配置。
- 账本占用只能依赖条件更新；不得新增先读余额或日免再创建任务的逻辑。
- MongoDB 集成测试只能使用：

~~~bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'
~~~

- 测试清理只允许按随机 _id 对单个文档执行 DeleteOne；禁止 DeleteMany、空过滤删除、删除集合和删除数据库。
- 所有新增 Go 注释使用简体中文；不得手改 cmd/ai-business-service/wire_gen.go。

## 文件结构

| 文件 | 职责 |
| --- | --- |
| internal/biz/ledger/usecase.go | 拆出事务内预留入口，保留现有独立 Reserve 行为。 |
| internal/biz/ledger/usecase_test.go | 锁定事务内入口不创建嵌套事务。 |
| internal/biz/creations/model.go | 创作、步骤、原子、状态、内部创建命令和返回对象。 |
| internal/biz/creations/errors.go | 创建命令、计划、幂等和账号状态领域错误。 |
| internal/biz/creations/repository.go | 创作和步骤的反转依赖接口。 |
| internal/biz/creations/usecase.go | 指纹、计划校验、权益映射、原子创建和重复键收敛。 |
| internal/biz/creations/usecase_test.go | 内存事务下的门禁、日免、扣钻、回滚和幂等测试。 |
| internal/biz/creations/provider.go | 注册创作创建用例。 |
| internal/data/model/creations.go | 为创作文档增加请求指纹。 |
| internal/data/model/model_test.go | 锁定请求指纹 BSON 字段名。 |
| internal/data/creation_repository.go | MongoDB 创作和步骤仓储。 |
| internal/data/creation_repository_test.go | 本地 rs0 原子事务、幂等和回滚集成测试。 |
| internal/data/data.go | 注册创作仓储构造器。 |
| internal/biz/biz.go | 注册创作模块 ProviderSet。 |
| cmd/ai-business-service/main.go | 显式接收已装配的创作用例，强制 Wire 解析其依赖图，但不注册路由。 |
| cmd/ai-business-service/main_test.go | 锁定应用构造函数可接收创作用例依赖。 |

## 任务 1：为账本提供事务内预留入口

**文件：**

- 修改：internal/biz/ledger/usecase.go
- 修改：internal/biz/ledger/usecase_test.go

- [ ] **步骤 1：编写失败的账本事务边界测试**

在现有内存仓储辅助对象旁增加 countingTxRunner，并写入下列测试。测试直接调用新入口，证明入口不自行创建事务，但仍能写出完整预留。

~~~go
func TestReserveInTx复用调用方事务且不创建嵌套事务(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	runner := &countingTxRunner{}
	usecase := ledger.NewUsecase(repository, runner)

	reservation, err := usecase.ReserveInTx(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-in-existing-transaction",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         disabledQuota(),
	})

	if err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}
	if reservation.Status != ledger.ReservationStatusReserved {
		t.Fatalf("Reservation.Status = %q, want reserved", reservation.Status)
	}
	if runner.calls != 0 {
		t.Fatalf("WithinTx calls = %d, want 0", runner.calls)
	}
}
~~~

为 countingTxRunner.WithinTx 实现 calls 递增后执行回调的行为；沿用测试文件现有 memoryRepository。

- [ ] **步骤 2：运行测试确认失败**

运行：

~~~bash
go test ./internal/biz/ledger -run TestReserveInTx复用调用方事务且不创建嵌套事务 -count=1
~~~

预期：编译失败，提示 Usecase 没有 ReserveInTx 方法。

- [ ] **步骤 3：提取无事务状态机主体**

在 usecase.go 中将现有 Reserve 的事务回调主体提取为私有 reserveInTx。保留原有 Reserve 的外部事务和返回行为；新增入口只使用调用者给出的事务 context。

~~~go
func (usecase *Usecase) Reserve(ctx context.Context, request ReserveRequest) (*Reservation, error) {
	if err := usecase.ready(); err != nil {
		return nil, err
	}
	var reservation *Reservation
	err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		var err error
		reservation, err = usecase.ReserveInTx(txCtx, request)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

// ReserveInTx 在调用方已经开启的事务中写入预留事实。
// 调用方必须把同一 txCtx 传给创作、账本和其他写入操作。
func (usecase *Usecase) ReserveInTx(ctx context.Context, request ReserveRequest) (*Reservation, error) {
	if usecase == nil || usecase.repository == nil {
		return nil, ErrLedgerDependenciesUnavailable
	}
	return usecase.reserveInTx(ctx, request)
}
~~~

把原有预留主体移动到 reserveInTx，完整保留命令一致性判断、日免优先、条件扣钻、预留写入和账本分录写入。Reverse 与 Confiscate 不改动。

- [ ] **步骤 4：运行账本单元测试验证通过**

运行：

~~~bash
go test ./internal/biz/ledger -count=1
~~~

预期：PASS；预留、冲正、没收和 CAS 测试均继续通过。

- [ ] **步骤 5：运行账本本地集成回归**

运行：

~~~bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run 'TestMongo账本' -count=1
~~~

预期：PASS；余额 20 到 0 再到 20、最后一份日免竞争和冲正事务回滚语义不变。

## 任务 2：先定义创作领域对象、指纹和步骤计划

**文件：**

- 创建：internal/biz/creations/model.go
- 创建：internal/biz/creations/errors.go
- 创建：internal/biz/creations/repository.go
- 创建：internal/biz/creations/usecase_test.go

- [ ] **步骤 1：编写失败的指纹与计划校验测试**

在新的 usecase_test.go 先写入下列测试，并补充空计划、非连续序号、图片误用 image_to_video、视频误用单步 text_to_image、非法输入摘要的子测试。

~~~go
func TestRequestFingerprint区分提示词素材和能力参数(t *testing.T) {
	base := validImageRequest()
	base.InputDigest = strings.Repeat("a", 64)

	changedInput := base
	changedInput.InputDigest = strings.Repeat("b", 64)
	changedVideo := base
	changedVideo.Product = entitlement.GenerationRequest{
		Output: entitlement.ProductOutputVideo,
		Video:  entitlement.VideoOptions{DurationSeconds: 5},
	}
	changedVideo.Plan = []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageToVideo}}

	baseFingerprint, err := creations.RequestFingerprint(base)
	if err != nil {
		t.Fatalf("RequestFingerprint(base) error = %v", err)
	}
	inputFingerprint, err := creations.RequestFingerprint(changedInput)
	if err != nil {
		t.Fatalf("RequestFingerprint(changedInput) error = %v", err)
	}
	videoFingerprint, err := creations.RequestFingerprint(changedVideo)
	if err != nil {
		t.Fatalf("RequestFingerprint(changedVideo) error = %v", err)
	}
	if baseFingerprint == inputFingerprint {
		t.Fatal("不同素材摘要不得得到相同请求指纹")
	}
	if baseFingerprint == videoFingerprint {
		t.Fatal("不同能力和步骤计划不得得到相同请求指纹")
	}
}

func TestValidatePlan拒绝Animate并接受文生视频两步计划(t *testing.T) {
	err := creations.ValidatePlan(
		entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
		[]creations.StepPlan{{Sequence: 1, Atom: "animate"}},
	)
	if !errors.Is(err, creations.ErrInvalidStepPlan) {
		t.Fatalf("ValidatePlan(animate) error = %v, want ErrInvalidStepPlan", err)
	}

	plan := []creations.StepPlan{
		{Sequence: 1, Atom: creations.AtomTextToImage},
		{Sequence: 2, Atom: creations.AtomImageToVideo},
	}
	if err := creations.ValidatePlan(entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo}, plan); err != nil {
		t.Fatalf("ValidatePlan(text-to-video) error = %v", err)
	}
}
~~~

- [ ] **步骤 2：运行测试确认失败**

运行：

~~~bash
go test ./internal/biz/creations -run 'TestRequestFingerprint|TestValidatePlan' -count=1
~~~

预期：编译失败，提示 StepPlan、RequestFingerprint、ValidatePlan 和 ErrInvalidStepPlan 未定义。

- [ ] **步骤 3：实现纯领域类型和校验**

在 model.go 定义下列类型。不得导入 MongoDB、HTTP、PayCores 或生成中台客户端。

~~~go
type StepAtom string

const (
	AtomTextToImage StepAtom = "text_to_image"
	AtomImageEdit   StepAtom = "image_edit"
	AtomImageToVideo StepAtom = "image_to_video"
)

type CreationStatus string

const CreationStatusPendingSubmission CreationStatus = "pending_submission"

type StepSubmitStatus string

const (
	StepSubmitStatusReady   StepSubmitStatus = "ready"
	StepSubmitStatusBlocked StepSubmitStatus = "blocked"
)

type StepPlan struct {
	Sequence int32
	Atom     StepAtom
}

type CreateReservedRequest struct {
	UserID          string
	IdempotencyKey  string
	TemplateID      string
	TemplateVersion int64
	Product         entitlement.GenerationRequest
	InputDigest     string
	Plan            []StepPlan
}
~~~

再定义 Creation、CreationStep 与 CreateReservedResult：

- Creation 含 ID、IdempotencyKey、UserID、TemplateID、TemplateVersion、RequestFingerprint、Status、Version、CreatedAt、UpdatedAt。
- CreationStep 含 ID、CreationID、Sequence、Atom、SubmitStatus、CallbackVersion、CreatedAt。
- CreateReservedResult 只返回 Creation 和按序 Steps，不把账本余额或预扣金额暴露给用户侧调用者。

在 errors.go 定义 ErrInvalidCreateCommand、ErrInvalidStepPlan、ErrCreationCommandConflict、ErrCreationAlreadyExists、ErrAccountUnavailable。

在 repository.go 定义：

~~~go
type Repository interface {
	FindByIdempotencyKey(context.Context, string) (*Creation, error)
	ListSteps(context.Context, string) ([]CreationStep, error)
	Create(context.Context, *Creation, []CreationStep) error
}
~~~

实现 RequestFingerprint：返回 (string, error)，使用 SHA-256 计算长度前缀的规范化字段串，顺序为用户、模板、版本、产品输出、视频时长、音频开关、引用图数量、输入摘要、每个步骤数量、序号和原子。InputDigest 只能接受 64 位小写十六进制摘要；非法摘要返回 ErrInvalidCreateCommand，提示词和素材原文绝不进入领域对象或持久化文档。用表驱动测试逐项只改变上述每个字段，并验证每一种变化都会得到不同指纹。

实现 ValidatePlan：仅接受图片的一步 text_to_image 或 image_edit、视频的一步 image_to_video、视频的两步 text_to_image 到 image_to_video。计划序号从 1 连续递增。

- [ ] **步骤 4：运行领域模型测试验证通过**

运行：

~~~bash
go test ./internal/biz/creations -run 'TestRequestFingerprint|TestValidatePlan' -count=1
~~~

预期：PASS；相同命令的指纹稳定，不同输入摘要、视频参数或步骤计划的指纹不同，Animate 不能通过校验。

## 任务 3：以 TDD 实现创作、步骤与预留总事务

**文件：**

- 修改：internal/biz/creations/usecase_test.go
- 创建：internal/biz/creations/usecase.go

- [ ] **步骤 1：编写失败的总事务编排测试**

在 usecase_test.go 建立共享状态的内存夹具。创作仓储、账本预留器和事务运行器共用一个状态对象；事务运行器在回调报错时还原创作、步骤、日免、账户、预留和分录快照。先写入：

~~~go
func TestCreateReserved以日免原子创建创作和步骤(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundVIP("user-1", "Asia/Shanghai")
	fixture.addDailyQuota("user-1", "vip_daily_image", fixture.localDate, 10, 0)

	result, err := fixture.usecase.CreateReserved(context.Background(), validImageRequest())

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if result.Creation.Status != creations.CreationStatusPendingSubmission || len(result.Steps) != 1 {
		t.Fatalf("result = %#v, want one pending creation and one ready step", result)
	}
	if fixture.reservationSource(result.Creation.ID) != ledger.BenefitSourceDailyQuota {
		t.Fatalf("reservation source = %q, want daily_quota", fixture.reservationSource(result.Creation.ID))
	}
	if fixture.accountBalance("user-1") != 0 || fixture.quotaUsed("user-1", "vip_daily_image", fixture.localDate) != 1 {
		t.Fatal("日免创建必须占用一个图片单位且不能扣减零余额账户")
	}
}

func TestCreateReserved账本预留失败时整体回滚(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundFreeUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	fixture.failReservation = errors.New("模拟账本预留失败")

	_, err := fixture.usecase.CreateReserved(context.Background(), validImageRequest())

	if !errors.Is(err, fixture.failReservation) {
		t.Fatalf("CreateReserved() error = %v, want reservation failure", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
	if fixture.accountBalance("user-1") != 20 {
		t.Fatalf("balance = %d, want 20 after rollback", fixture.accountBalance("user-1"))
	}
}
~~~

再写独立测试：同键同命令重试不重复扣费；同键不同输入摘要或模板版本返回 ErrCreationCommandConflict；游客返回 shared.ErrAccountBindingRequired；封禁用户返回 ErrAccountUnavailable；免费用户 10 秒视频返回 shared.ErrVIPRequired；日免耗尽且余额不足返回 shared.ErrInsufficientFunds；文生视频创建第 1 步 ready 和第 2 步 blocked；5/10/15 秒视频向账本传入 1/2/3 个视频单位。

- [ ] **步骤 2：运行测试确认失败**

运行：

~~~bash
go test ./internal/biz/creations -run TestCreateReserved -count=1
~~~

预期：编译失败，提示 NewUsecase、CreateReserved 和相关依赖接口未定义。

- [ ] **步骤 3：实现依赖接口、总事务和幂等收敛**

在 usecase.go 定义窄接口，防止 creations 依赖 data 或 MongoDB：

~~~go
type userReader interface {
	Find(context.Context, string) (*identity.User, error)
}

// subscriptionReader 从服务端自有订阅投影读取权益快照。
// 缺失快照表示普通用户；创作命令不得携带订阅或 VIP 状态。
type subscriptionReader interface {
	FindByUserID(context.Context, string) (*entitlement.SubscriptionSnapshot, error)
}

type entitlementEvaluator interface {
	EvaluateGenerationAt(entitlement.UserSnapshot, entitlement.GenerationRequest, time.Time) (*entitlement.GenerationDecision, error)
	ResolveDailyBenefitsAt(entitlement.UserSnapshot, time.Time) (*entitlement.DailyBenefits, error)
}

type reservationUsecase interface {
	ReserveInTx(context.Context, ledger.ReserveRequest) (*ledger.Reservation, error)
}
~~~

实现构造器和入口：

~~~go
func NewUsecase(
	users userReader,
	subscriptions subscriptionReader,
	entitlements entitlementEvaluator,
	creations Repository,
	reservations reservationUsecase,
	tx shared.TxRunner,
) *Usecase

func (usecase *Usecase) CreateReserved(ctx context.Context, request CreateReservedRequest) (*CreateReservedResult, error)
~~~

CreateReserved 的实现顺序固定为：

1. 校验必填字段和步骤计划；调用 RequestFingerprint 并在其返回错误时立即结束命令，不能用空字符串继续幂等查询或进入事务。
2. 读取用户并拒绝非 normal 状态；查询既有幂等键。已有创作仅在 UserID 与 RequestFingerprint 相等时返回其创作和按序步骤，否则返回 ErrCreationCommandConflict。
3. 在进入 `tx.WithinTx` 前只读取一次 UTC 业务时刻，并在事务内再次读取用户和自有订阅投影。使用绑定状态、时区和服务端订阅快照构造 `entitlement.UserSnapshot`，依次调用带该业务时刻的 `EvaluateGenerationAt` 和 `ResolveDailyBenefitsAt`。订阅缺失按普通用户处理，读取失败原样返回。
4. 组装 ledger.QuotaReservation：图片为 vip_daily_image 和 1 单位；视频为 vip_daily_video 和 DurationSeconds/5 单位；LocalDate 与 Limit 均来自 DailyBenefits。
5. 在进入事务前生成创作 ID 和步骤 ID，并固定业务时刻。事务中先执行 Repository.Create，再执行 ReserveInTx。创作状态为 pending_submission、版本为 1；第一步为 ready，文生视频的第二步为 blocked。事务重试必须复用相同 ID、业务时刻、价格和额度命令。
6. Repository.Create 返回 ErrCreationAlreadyExists 时，在事务外重新读取同键创作，使用同一 UserID 与 RequestFingerprint 判断并返回获胜事务结果或冲突。

不得在此用例创建账户、充值、发放每日钻石、发送 HTTP、写 Outbox、调用 Reverse 或 Confiscate。

权益冻结约束：基础 VIP 每日图片/视频额度分别为 10/3 个单位；有效年付首月仅将图片/视频额度翻倍为 20/6 个单位。每日赠钻始终为 50 钻，不参与首月翻倍；其发放仍不属于本任务。

- [ ] **步骤 4：运行创作领域测试验证通过**

运行：

~~~bash
go test ./internal/biz/creations -count=1
~~~

预期：PASS；门禁错误无副作用，日免、预扣、文生视频步骤、幂等重放和事务回滚断言均通过。

- [ ] **步骤 5：执行关联领域回归测试**

运行：

~~~bash
go test ./internal/biz/identity ./internal/biz/entitlement ./internal/biz/ledger ./internal/biz/creations -count=1
~~~

预期：PASS；身份、权益和账本既有语义不因创作总编排而改变。

## 任务 4：以 TDD 落地 MongoDB 创作仓储和本地集成测试

> 任务 4 还必须为任务 3 已定义的 `subscriptionReader` 落地本地 MongoDB 的自有订阅读模型。该读模型只向创作用例返回服务端订阅快照；缺失记录返回 `nil` 表示普通用户。不得从创建命令、Node/JS、现网钱包或 PayCores 临时透传 VIP 状态。

> 订阅读模型的精确文件边界、TDD 用例与本地 rs0 验证命令见[补充计划](2026-09-05-cling-go-creation-mongo-subscription-readmodel-addendum.md)。该补充计划是本任务 4 的一部分；任务 5 前不得装配 Wire。

**文件：**

- 修改：internal/data/model/creations.go
- 修改：internal/data/model/model_test.go
- 创建：internal/data/creation_repository.go
- 创建：internal/data/creation_repository_test.go

- [ ] **步骤 1：编写失败的 BSON 与 MongoDB 集成测试**

先在 model_test.go 的创作字段期望中加入 request_fingerprint。再创建 creation_repository_test.go，复用 newLocalMongoClient、newMongoTestContext、migrate.NewInitializer，写入：

~~~go
func TestMongoCreateReserved日免占用与创作步骤同事务提交(t *testing.T) {
	fixture := newMongoCreationFixture(t)
	fixture.seedBoundUser(0)
	fixture.seedDailyQuota("vip_daily_image", fixture.localDate, 10, 0)

	result, err := fixture.usecase.CreateReserved(fixture.ctx, fixture.imageRequest())

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	assertCreationDocument(t, fixture.ctx, fixture.creations, result.Creation.ID, "pending_submission", result.Creation.RequestFingerprint)
	assertCreationStepDocument(t, fixture.ctx, fixture.steps, stepID(result.Creation.ID, 1), "text_to_image", "ready")
	assertReservationAndLedger(t, fixture.ctx, fixture.reservations, fixture.ledgerEntries, result.Creation.ID, "daily_quota", 0)
	assertDailyQuotaUsed(t, fixture.ctx, fixture.dailyQuotas, fixture.dailyQuotaID, 1)
}
~~~

增加同键重试只保存一条创作、一个步骤、一条预留、一条分录；余额不足时创作和步骤均为 0 条；10 秒 VIP 视频写入 quota_units=2；两次并发相同幂等键返回同一创作 ID 且只留下单套事实。

每个测试生成随机用户、创作、幂等键、账户、额度、预留、账本和步骤 ID。为每一个已知 ID 注册 t.Cleanup 并逐条执行：

~~~go
result, err := collection.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
if err != nil {
	t.Errorf("清理 %s %q: %v", collection.Name(), id, err)
}
if result.DeletedCount > 1 {
	t.Errorf("清理 %s %q 删除了 %d 条", collection.Name(), id, result.DeletedCount)
}
~~~

- [ ] **步骤 2：运行测试确认失败**

运行：

~~~bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run 'TestMongoCreateReserved|Test关键持久化对象的BSON字段名' -count=1
~~~

预期：编译失败，提示 CreationDocument 缺少 RequestFingerprint，且 NewCreationRepository 未定义。

- [ ] **步骤 3：实现 PO 转换和 MongoDB 仓储**

在 CreationDocument 中增加 RequestFingerprint 字段。实现下面的构造器和仓储方法：

~~~go
func NewCreationRepository(data *Data) creations.Repository
func (repository *mongoCreationRepository) FindByIdempotencyKey(ctx context.Context, key string) (*creations.Creation, error)
func (repository *mongoCreationRepository) ListSteps(ctx context.Context, creationID string) ([]creations.CreationStep, error)
func (repository *mongoCreationRepository) Create(ctx context.Context, creation *creations.Creation, steps []creations.CreationStep) error
~~~

实现规则：

- FindByIdempotencyKey 未命中返回 nil、nil，其他驱动错误保留操作上下文。
- ListSteps 使用 sequence 升序查询，只返回领域对象。
- Create 先 InsertOne 创作文档，再 InsertMany 步骤文档；所有调用使用传入 context，仓储不创建 Session。
- 创作或步骤写入产生的重复键错误映射为 creations.ErrCreationAlreadyExists；其他错误保留集合与操作名称。
- 转换函数为 newCreationDocument、newCreationStepDocument、toBizCreation、toBizCreationStep，全部驻留 data 层。

- [ ] **步骤 4：运行 MongoDB 测试验证通过**

运行：

~~~bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run 'TestMongoCreateReserved|Test关键持久化对象的BSON字段名' -count=1
~~~

预期：PASS；提交路径同时存在创作、步骤、预留、账本和额度事实；余额不足路径不残留创作或步骤。

- [ ] **步骤 5：运行 MongoDB 竞争与账本回归**

运行：

~~~bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run 'TestMongoCreateReserved|TestMongo账本' -count=1
~~~

预期：PASS；相同幂等键并发只保留一套事实，账本既有最后日免和冲正测试继续通过。

## 任务 5：注册依赖、生成 Wire 并完成全量本地验证

**文件：**

- 修改：internal/biz/creations/provider.go
- 修改：internal/biz/biz.go
- 修改：internal/data/data.go
- 修改：cmd/ai-business-service/main.go
- 修改：cmd/ai-business-service/main_test.go
- 生成：cmd/ai-business-service/wire_gen.go，仅通过 go generate

- [ ] **步骤 1：编写失败的装配编译检查**

先在 main_test.go 增加一条直接向 newApp 传入最后一个 nil 参数的测试，并让现有 newTestApp 使用同一参数位置：

~~~go
func TestNewAppAcceptsCreationUsecaseDependency(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := newApp(
		logger,
		grpc.NewServer(),
		http.NewServer(),
		biz.NewModuleRegistry(),
		nil,
		&fakeSchemaInitializer{},
		nil,
	)
	if err != nil || app == nil {
		t.Fatalf("newApp() = (%#v, %v), want initialized app", app, err)
	}
}
~~~

- [ ] **步骤 2：运行装配检查确认失败**

运行：

~~~bash
go test ./cmd/ai-business-service -run TestNewAppAcceptsCreationUsecaseDependency -count=1
~~~

预期：编译失败，提示 newApp 的参数数量不足，说明应用尚未显式接收创作用例。

- [ ] **步骤 3：完成 ProviderSet 和数据构造器注册**

完成任务 3、任务 4 后写入下列装配关系：

- creations.ProviderSet 仅注册 creations.NewUsecase。
- biz.ProviderSet 包含 creations.ProviderSet，不增加 HTTP/gRPC Service。
- data.ProviderSet 包含 NewCreationRepository 与 NewSubscriptionRepository，并继续保留用户、身份、会话、模板和账本仓储构造器。订阅读取仓储只为创作用例提供本服务自有的订阅快照，不注册任何传输入口。
- main.go 在 newApp 的最后一个参数显式接收未使用的 *creations.Usecase；main_test.go 的 newTestApp 为该参数传入 nil。该依赖只强制 Wire 构建并校验业务图，不注册路由、不发起任务、不写数据库。
- 不修改 ModuleRegistry.Names；它已经包含稳定的 creations 边界。

对应代码为：

~~~go
var ProviderSet = wire.NewSet(NewUsecase)
~~~

~~~go
var ProviderSet = wire.NewSet(
	NewModuleRegistry,
	identity.ProviderSet,
	catalog.ProviderSet,
	entitlement.ProviderSet,
	ledger.ProviderSet,
	creations.ProviderSet,
)
~~~

~~~go
func newApp(
	logger *slog.Logger,
	gs *grpc.Server,
	hs *http.Server,
	modules *biz.ModuleRegistry,
	_ shared.TxRunner,
	initializer data.LocalSchemaInitializer,
	_ *creations.Usecase,
) (*kratos.App, error)
~~~

- [ ] **步骤 4：生成 Wire 代码并验证构建**

运行：

~~~bash
go generate ./cmd/ai-business-service
go test ./cmd/ai-business-service -run TestNewAppAcceptsCreationUsecaseDependency -count=1
go build ./cmd/ai-business-service
~~~

预期：全部 PASS；wire_gen.go 仅由生成器更新，没有新增业务路由。

- [ ] **步骤 5：执行全量质量验证**

运行：

~~~bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./... -count=1
go vet ./...
go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json
~~~

预期：所有 Go 测试、竞态检测、静态检查和 14 条既有语义用例通过；测试与构建不连接生产、不调用中台、不修改 Node/JS、PayCores 或现网钱包。

## 规格覆盖自检

| 设计要求 | 覆盖任务 |
| --- | --- |
| 创作、步骤和预留同事务提交或回滚 | 任务 1、任务 3、任务 4。 |
| 不预读余额或日免 | 任务 1、任务 3、任务 4。 |
| 游客、VIP、余额不足的独立门禁 | 任务 3。 |
| 封禁与删除用户不能创建 | 任务 3。 |
| T2I、模板图编辑、I2V 与文生视频两步计划 | 任务 2、任务 3、任务 4。 |
| Animate 明确拒绝 | 任务 2、任务 3。 |
| 同键幂等、同键冲突和并发重复键收敛 | 任务 2、任务 3、任务 4。 |
| 图片和视频的固定价格、日免和时长单位 | 任务 3、任务 4。 |
| 自有余额、无中台调用、无路由、无 Node/PayCores 修改 | 任务 3、任务 5。 |
| MongoDB 精确清理与全量验证 | 任务 4、任务 5。 |
