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
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	contentreviewintegration "ai-business-service/internal/integrations/contentreview"
	generationintegration "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/worker"

	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"

	_ "go.uber.org/automaxprocs"
)

const schemaInitializationTimeout = 10 * time.Second

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
	runner, cleanup, err := newRunner(bootstrap)
	if err != nil {
		panic(err)
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("本地生成投递 Worker 已启动", "poll_interval", bootstrap.GetWorker().GetPollInterval().AsDuration(), "batch_size", bootstrap.GetWorker().GetBatchSize())
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
	if err := conf.Validate(bootstrap); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("validate worker config: %w", err)
	}
	// 配置资源关闭失败不影响进程退出路径；与其他启动入口一致显式忽略该返回值。
	return bootstrap, func() { _ = configuration.Close() }, nil
}

// newRunner 只装配既有的 Outbox 投递状态机；它不注册任何路由，也不在构造阶段发送请求。
func newRunner(bootstrap *conf.Bootstrap) (*worker.Runner, func(), error) {
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

	client, err := generationintegration.NewConfiguredClient(bootstrap.GetSecurity(), bootstrap.GetIntegrations())
	if err != nil {
		return fail(fmt.Errorf("build generation client: %w", err))
	}
	txRunner := data.NewTxRunner(database)
	ledgerUsecase := ledger.NewUsecase(data.NewLedgerRepository(database), txRunner)
	submissionWorker := worker.NewDefaultGenerationSubmissionWorker(
		data.NewOutboxRepository(database),
		data.NewGenerationSubmissionRepository(database),
		ledgerUsecase,
		txRunner,
		client,
		configuredReviewer(bootstrap),
	)
	runner, err := worker.NewBatchRunner(
		submissionWorker,
		bootstrap.GetWorker().GetPollInterval().AsDuration(),
		int(bootstrap.GetWorker().GetBatchSize()),
	)
	if err != nil {
		return fail(fmt.Errorf("build generation runner: %w", err))
	}
	return runner, cleanup, nil
}

// configuredReviewer 把审核适配器收敛在进程组合根，Worker 仍只依赖领域接口。
func configuredReviewer(bootstrap *conf.Bootstrap) contentreview.Reviewer {
	return contentreviewintegration.NewConfiguredReviewer(bootstrap.GetIntegrations())
}
