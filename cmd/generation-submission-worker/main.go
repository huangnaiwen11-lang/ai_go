// generation-submission-worker 是显式启动的本地生成投递进程。
// 它不提供 HTTP/gRPC 服务，也不会由主站 HTTP 进程隐式启动。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ai-business-service/internal/biz/contentreview"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	contentreviewintegration "ai-business-service/internal/integrations/contentreview"
	generationintegration "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/r2"
	"ai-business-service/internal/worker"

	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"

	_ "go.uber.org/automaxprocs"
)

const (
	schemaInitializationTimeout         = 10 * time.Second
	runtimeObservabilityShutdownTimeout = 5 * time.Second
)

var flagconf string

func init() {
	flag.StringVar(&flagconf, "conf", "./configs/config.yaml", "配置文件路径，例如：-conf config.yaml")
}

func main() {
	flag.Parse()
	bootstrap, closeConfig, err := loadBootstrap(flagconf)
	if err != nil {
		panic(err)
	}
	defer closeConfig()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	observability, err := worker.NewRuntimeObservability(os.Getenv("GENERATION_WORKER_OBSERVABILITY_ADDR"), logger)
	if err != nil {
		panic(err)
	}
	runner, cleanup, err := newRunner(bootstrap, logger, observability)
	if err != nil {
		panic(err)
	}
	defer cleanup()
	if err := observability.Start(); err != nil {
		panic(err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), runtimeObservabilityShutdownTimeout)
		defer cancel()
		if err := observability.Shutdown(shutdownCtx); err != nil {
			logger.Warn("关闭 Worker 运行观测端点失败", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("本地生成投递 Worker 已启动", "poll_interval", bootstrap.GetWorker().GetPollInterval().AsDuration(), "batch_size", bootstrap.GetWorker().GetBatchSize(), "observability_addr", observability.Address())
	if err := runner.Run(ctx); err != nil {
		logger.Error("本地生成投递 Worker 异常退出", "error", err)
		panic(err)
	}
	logger.Info("本地生成投递 Worker 已停止")
}

// loadBootstrap 只读取当前进程配置，并在连接 MongoDB 前执行统一的本地隔离校验。
func loadBootstrap(path string) (*conf.Bootstrap, func(), error) {
	configuration := config.New(config.WithSource(file.NewSource(path), env.NewSource("KRATOS")))
	if err := configuration.Load(); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("load worker config: %w", err)
	}
	bootstrap := &conf.Bootstrap{}
	if err := configuration.Scan(bootstrap); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("scan worker config: %w", err)
	}
	if err := conf.ApplyMongoEnvironmentOverrides(bootstrap, os.Getenv); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("apply Mongo environment: %w", err)
	}
	if err := conf.ApplyPolarStarB2BEnvironmentOverrides(bootstrap, os.Getenv); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("apply polarstar b2b environment: %w", err)
	}
	if err := conf.Validate(bootstrap); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("validate worker config: %w", err)
	}
	// 配置资源关闭失败不影响进程退出路径；与其他启动入口一致显式忽略该返回值。
	return bootstrap, func() { _ = configuration.Close() }, nil
}

// newRunner 只装配既有的 Outbox 投递状态机；它不注册任何路由，也不在构造阶段发送请求。
func newRunner(bootstrap *conf.Bootstrap, logger *slog.Logger, observability *worker.RuntimeObservability) (*worker.Runner, func(), error) {
	if bootstrap == nil {
		return nil, nil, errors.New("worker bootstrap config is required")
	}
	database, cleanup, err := data.NewData(bootstrap.GetData())
	if err != nil {
		return nil, nil, err
	}

	fail := func(cause error) (*worker.Runner, func(), error) {
		cleanup()
		return nil, nil, cause
	}
	initializer := data.NewLocalSchemaInitializer(database)
	ctx, cancel := context.WithTimeout(context.Background(), schemaInitializationTimeout)
	err = initializer.Ensure(ctx)
	cancel()
	if err != nil {
		return fail(fmt.Errorf("initialize local MongoDB schema: %w", err))
	}

	// 注册表同时持有本地与 B2B 出站能力，并保留「按冻结路由解析」的语义：
	// Worker 不再从当前 selector 推断历史步骤该走哪个 provider。
	registry, closeRegistry, err := generationintegration.NewProviderRegistry(bootstrap.GetSecurity(), bootstrap.GetIntegrations(), data.NewMappingCatalogRepository(database))
	if err != nil {
		return fail(fmt.Errorf("build generation provider registry: %w", err))
	}
	closeAll := func() {
		closeRegistry()
		cleanup()
	}
	txRunner := data.NewTxRunner(database)
	ledgerUsecase := ledger.NewUsecase(data.NewLedgerRepository(database), txRunner)
	baseOutbox := data.NewOutboxRepository(database)
	submissionWorker := worker.NewDefaultGenerationSubmissionWorker(
		worker.NewObservedOutboxRepository(baseOutbox, observability, outbox.EventTypeGenerationSubmission),
		data.NewGenerationSubmissionRepository(database),
		ledgerUsecase,
		txRunner,
		registry,
		configuredReviewer(bootstrap),
	)
	// B2B 取消、投递消费、对账恢复、失败结算与提交共用同一个轮询进程：它们按各自的事件类型
	// 领取，互不占用对方的事件，也不需要新增部署单元。
	cancellationWorker, err := newProviderCancelWorker(bootstrap, database, registry, worker.NewObservedOutboxRepository(baseOutbox, observability, outbox.EventType(generation.ProviderCancelEventType)))
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("build provider cancel worker: %w", err)
	}
	consumerWorker, err := newInboxConsumerWorker(bootstrap, database, txRunner, worker.NewObservedOutboxRepository(baseOutbox, observability, outbox.EventType(generation.ProviderInboxConsumeEventType)))
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("build provider inbox consumer: %w", err)
	}
	reconcileWorker, err := newReconcileWorker(bootstrap, database, txRunner, registry, worker.NewObservedOutboxRepository(baseOutbox, observability, outbox.EventType(generation.ProviderReconcileEventType)))
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("build provider reconcile worker: %w", err)
	}
	settlementWorker, err := newProviderTerminalSettlementWorker(bootstrap, database, ledgerUsecase, txRunner, logger, worker.NewObservedOutboxRepository(baseOutbox, observability, outbox.EventType(generation.ProviderTerminalSettlementEventType)))
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("build provider terminal settlement worker: %w", err)
	}
	materializerWorker, closeMaterializer, err := newProviderResultMaterializerWorker(bootstrap, database, txRunner, registry, logger, worker.NewObservedOutboxRepository(baseOutbox, observability, outbox.EventType(generation.ProviderResultMaterializeEventType)))
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("build provider result materializer: %w", err)
	}
	if closeMaterializer != nil {
		previousCloseAll := closeAll
		closeAll = func() {
			closeMaterializer()
			previousCloseAll()
		}
	}
	deliverer := generationDeliverer{
		submission:    newObservedGenerationUnit(worker.WorkerUnitSubmission, submissionWorker, observability),
		backlog:       data.NewOutboxBacklogReader(database),
		observability: observability,
	}
	// 未配置 B2B 时两个 Worker 都是 nil 指针；直接赋给接口会得到一个
	// 「非 nil 接口持有 nil 指针」的值，nil 判断会失效并在每次轮询里报错。
	if consumerWorker != nil {
		deliverer.consumer = newObservedGenerationUnit(worker.WorkerUnitInbox, consumerWorker, observability)
	}
	if cancellationWorker != nil {
		deliverer.cancellation = newObservedGenerationUnit(worker.WorkerUnitCancel, cancellationWorker, observability)
	}
	if reconcileWorker != nil {
		deliverer.reconcile = newObservedGenerationUnit(worker.WorkerUnitReconcile, reconcileWorker, observability)
	}
	if settlementWorker != nil {
		deliverer.settlement = newObservedGenerationUnit(worker.WorkerUnitSettlement, settlementWorker, observability)
	}
	if materializerWorker != nil {
		deliverer.materializer = newObservedGenerationUnit(worker.WorkerUnitMaterialize, materializerWorker, observability)
	}
	runner, err := worker.NewBatchRunner(
		deliverer,
		bootstrap.GetWorker().GetPollInterval().AsDuration(),
		int(bootstrap.GetWorker().GetBatchSize()),
	)
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("build generation runner: %w", err)
	}
	return runner, closeAll, nil
}

// generationUnit 是一个可被轮询驱动的单次投递单元。
type generationUnit interface {
	DeliverOnce(context.Context, string) error
}

// generationDeliverer 在一个轮询周期内依次驱动提交、取消投递、投递消费、
// 对账恢复、失败结算与素材发布六个单元。
//
// 各单元按自己的事件类型领取，因此互不占用对方的事件。任一单元报错都会上抛，让入口按
// 既有 fail-fast 语义处理，而不是让某一条队列静默停摆。
type generationDeliverer struct {
	submission    generationUnit
	cancellation  generationUnit
	consumer      generationUnit
	reconcile     generationUnit
	settlement    generationUnit
	materializer  generationUnit
	backlog       outbox.BacklogReader
	observability *worker.RuntimeObservability
}

func (deliverer generationDeliverer) DeliverOnce(ctx context.Context, expectedEventID string) (err error) {
	defer func() { deliverer.observeCycle(ctx, err) }()
	if err = deliverer.submission.DeliverOnce(ctx, expectedEventID); err != nil {
		return err
	}
	if deliverer.cancellation != nil {
		if err = deliverer.cancellation.DeliverOnce(ctx, expectedEventID); err != nil {
			return err
		}
	}
	if deliverer.consumer != nil {
		if err = deliverer.consumer.DeliverOnce(ctx, expectedEventID); err != nil {
			return err
		}
	}
	for _, unit := range []generationUnit{deliverer.reconcile, deliverer.settlement, deliverer.materializer} {
		if unit != nil {
			if err = unit.DeliverOnce(ctx, expectedEventID); err != nil {
				return err
			}
		}
	}
	return nil
}

// observeCycle is deliberately best-effort: an aggregate metrics read must not
// change the existing fail-fast delivery semantics or strand user work. A
// delivery error still propagates unchanged through Runner and restarts the
// process as before.
func (deliverer generationDeliverer) observeCycle(ctx context.Context, deliveryErr error) {
	if deliverer.observability == nil || deliveryErr != nil {
		return
	}
	deliverer.observability.MarkReady()
	if deliverer.backlog == nil {
		return
	}
	now := time.Now().UTC()
	if !deliverer.observability.ShouldSampleBacklog(now) {
		return
	}
	snapshot, err := deliverer.backlog.ReadOutboxBacklog(ctx)
	if err != nil {
		deliverer.observability.ObserveBacklogFailure(err)
		return
	}
	deliverer.observability.ObserveBacklog(snapshot, now)
}

type observedGenerationUnit struct {
	unit          worker.WorkerUnit
	delegate      generationUnit
	observability *worker.RuntimeObservability
}

func newObservedGenerationUnit(unit worker.WorkerUnit, delegate generationUnit, observability *worker.RuntimeObservability) generationUnit {
	if delegate == nil || observability == nil {
		return delegate
	}
	return observedGenerationUnit{unit: unit, delegate: delegate, observability: observability}
}

func (unit observedGenerationUnit) DeliverOnce(ctx context.Context, expectedEventID string) error {
	startedAt := time.Now()
	err := unit.delegate.DeliverOnce(ctx, expectedEventID)
	unit.observability.ObserveUnit(unit.unit, startedAt, err)
	return err
}

// newProviderCancelWorker wires cancellation delivery only when this process
// has an explicit B2B configuration. Local-only profiles have no cancel
// outbox events and must not construct a B2B-capable worker by accident.
func newProviderCancelWorker(
	bootstrap *conf.Bootstrap,
	database *data.Data,
	registry *generationintegration.ProviderRegistry,
	outboxRepository outbox.Repository,
) (*worker.ProviderCancelWorker, error) {
	if bootstrap == nil || bootstrap.GetIntegrations() == nil || bootstrap.GetIntegrations().GetGeneration() == nil {
		return nil, errors.New("generation integration config is required")
	}
	if bootstrap.GetIntegrations().GetGeneration().GetPolarstarB2B() == nil {
		return nil, nil
	}
	if database == nil || registry == nil || outboxRepository == nil {
		return nil, errors.New("provider cancel worker dependencies are required")
	}
	return worker.NewDefaultProviderCancelWorker(outboxRepository, registry), nil
}

// newInboxConsumerWorker 装配 B2B 投递消费者；未配置 B2B 时返回 nil。
//
// 返回 nil 而不是错误：本地 profile 是合法配置，此时根本不存在 B2B 投递。
// 「没配 B2B」与「B2B 配错了」因此不会混在一起——后者在构造注册表时就已经
// 失败，组合根不需要在这里再区分一次。
func newInboxConsumerWorker(bootstrap *conf.Bootstrap, database *data.Data, txRunner shared.TxRunner, outboxRepository outbox.Repository) (*worker.ProviderInboxConsumerWorker, error) {
	b2b := bootstrap.GetIntegrations().GetGeneration().GetPolarstarB2B()
	if b2b == nil {
		return nil, nil
	}
	// 重放解析需要租户身份：报文体自报的 tenantId 与账号归属都是合同的一部分，
	// 少了它们就只剩一份无法核对来源的字节。
	parser, err := generationintegration.NewB2BObservationParser(b2b.GetTenantId(), b2b.GetAccountRef())
	if err != nil {
		return nil, err
	}
	store := data.NewGenerationProviderInboxConsumerStore(database)
	return worker.NewDefaultProviderInboxConsumerWorker(
		outboxRepository,
		store,
		generation.NewProviderInboxConsumerUsecase(
			store,
			data.NewGenerationProviderSubmissionStore(database),
			data.NewGenerationProviderTerminalStore(data.NewGenerationCallbackRepository(database)),
			txRunner,
		),
		parser,
	), nil
}

// newReconcileWorker 装配 B2B 对账恢复工作者；未配置 B2B 时返回 nil。
//
// 对账是「终态只能靠投递到达」这一假设的唯一破局点：平台的有界投递会在 8 次
// 后放弃，本地崩溃窗口也会整段吞掉投递。没有它，那些任务的创作永远停在
// submitted，用户预扣的钻石也永远不结算。
func newReconcileWorker(
	bootstrap *conf.Bootstrap,
	database *data.Data,
	txRunner shared.TxRunner,
	registry *generationintegration.ProviderRegistry,
	outboxRepository outbox.Repository,
) (*worker.ProviderReconcileWorker, error) {
	b2b := bootstrap.GetIntegrations().GetGeneration().GetPolarstarB2B()
	if b2b == nil {
		return nil, nil
	}
	// 与投递消费共用同一份租户身份：两条路径必须用同一个解析口径把供应商
	// 字节归一成结论，否则同一结论经两条路到达会被下游当成两件事。
	parser, err := generationintegration.NewB2BObservationParser(b2b.GetTenantId(), b2b.GetAccountRef())
	if err != nil {
		return nil, err
	}
	return worker.NewDefaultProviderReconcileWorker(
		outboxRepository,
		data.NewGenerationProviderSubmissionStore(database),
		data.NewGenerationProviderTerminalStore(data.NewGenerationCallbackRepository(database)),
		txRunner,
		registry,
		parser,
	), nil
}

// newProviderTerminalSettlementWorker 装配 B2B 失败/取消终态的账本结算器。
// 没有 B2B 账号时不存在这类事件，保持 local profile 完全不读取 R2 或 B2B 配置。
func newProviderTerminalSettlementWorker(
	bootstrap *conf.Bootstrap,
	database *data.Data,
	ledgerUsecase *ledger.Usecase,
	txRunner shared.TxRunner,
	logger *slog.Logger,
	outboxRepository outbox.Repository,
) (*worker.ProviderTerminalSettlementWorker, error) {
	if bootstrap == nil || bootstrap.GetIntegrations() == nil || bootstrap.GetIntegrations().GetGeneration() == nil {
		return nil, errors.New("generation integration config is required")
	}
	if bootstrap.GetIntegrations().GetGeneration().GetPolarstarB2B() == nil {
		return nil, nil
	}
	if ledgerUsecase == nil {
		return nil, errors.New("ledger usecase is required for B2B terminal settlement")
	}
	settlement := worker.NewDefaultProviderTerminalSettlementWorker(
		outboxRepository,
		data.NewGenerationProviderTerminalSettlementStore(data.NewGenerationCallbackRepository(database)),
		ledgerUsecase,
		txRunner,
	)
	// 给出局必须有出口：转入 needs_attention 的事件不再被自动认领，若不打告警，
	// 只能靠人去翻库才发现。告警在此处安装，Worker 本身仍不依赖日志实现。
	settlement.SetAttentionAlerter(worker.NewLogAttentionAlerter(logger))
	return settlement, nil
}

// newProviderResultMaterializerWorker only opts into R2 when this process has
// a B2B account to drain. The local profile has no B2B block and therefore
// does not parse, construct, or dial R2. Once B2B is configured, missing or
// partial R2 settings are a startup error: accepting a provider job without an
// owned-result storage path would strand a terminal completed task forever.
func newProviderResultMaterializerWorker(
	bootstrap *conf.Bootstrap,
	database *data.Data,
	txRunner shared.TxRunner,
	registry *generationintegration.ProviderRegistry,
	logger *slog.Logger,
	outboxRepository outbox.Repository,
) (*worker.ProviderResultMaterializerWorker, func(), error) {
	if bootstrap == nil || bootstrap.GetIntegrations() == nil || bootstrap.GetIntegrations().GetGeneration() == nil {
		return nil, nil, errors.New("generation integration config is required")
	}
	if bootstrap.GetIntegrations().GetGeneration().GetPolarstarB2B() == nil {
		return nil, nil, nil
	}
	storage, enabled, err := r2.LoadConfig(os.Getenv)
	if err != nil {
		return nil, nil, fmt.Errorf("load R2 config for B2B result materialization: %w", err)
	}
	if !enabled {
		return nil, nil, errors.New("R2 config is required when PolarStar B2B is configured")
	}
	objects, err := r2.NewClient(storage)
	if err != nil {
		return nil, nil, fmt.Errorf("build R2 client for B2B result materialization: %w", err)
	}
	materializer, err := worker.NewProviderResultMaterializerWorker(
		outboxRepository,
		data.NewGenerationCallbackRepository(database),
		txRunner,
		registry,
		worker.NewSafeProviderResultFetcher(),
		objects,
		storage,
		"provider-result-materializer-worker",
		time.Now,
	)
	if err != nil {
		objects.CloseIdleConnections()
		return nil, nil, err
	}
	// 与结算 Worker 同一个理由：转入 needs_attention 后不再有自动出口，
	// 必须有可被外部采集的告警，否则只能靠人翻库。
	materializer.SetAttentionAlerter(worker.NewLogAttentionAlerter(logger))
	return materializer, objects.CloseIdleConnections, nil
}

// configuredReviewer 把审核适配器收敛在进程组合根，Worker 仍只依赖领域接口。
func configuredReviewer(bootstrap *conf.Bootstrap) contentreview.Reviewer {
	return contentreviewintegration.NewConfiguredReviewer(bootstrap.GetIntegrations())
}
