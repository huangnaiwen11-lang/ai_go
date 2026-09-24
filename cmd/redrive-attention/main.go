// redrive-attention 是人工受限重驱的受控运维入口。
//
// 它是一个**一次性的运维命令，不是一个服务**：不监听端口、不注册任何路由、
// 不常驻，执行完即退出。默认只在本地独立的 cling_main / rs0 上执行；显式
// CLING_MONGO_PROFILE=staging 时，复用同一条受限 SSH 转发 staging 配置。
//
// 四个必填参数对应审计幂等键的三个分量与人工原因：
//
//	-actor-id / -event-key / -expected-redrive-count  →  审计幂等键 (actor_id, event_key, count)
//	-reason                                          →  人工原因，落审计
//
// 任何必填项缺失都会在**触库之前**被拒：运维命令不该先连库再告诉用户参数不全。
//
// 示例（在仓库根目录，generation-worker 与 api-gateway 共用网络命名空间）：
//
//	docker compose run --rm generation-worker /app/redrive-attention \
//	  -event-id 'generation.result-materialize:step-xxx' \
//	  -actor-id admin-1 -event-key ticket-42 -expected-redrive-count 0 \
//	  -reason 'provider 已恢复'
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/redrive"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"

	kratosconfig "github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
)

const (
	defaultConfigPath = "/data/conf/config.yaml"
	// redriveTimeout 覆盖一次事务内的全部读写；重驱不发出站请求，因此不需要更长。
	redriveTimeout = 30 * time.Second
)

func main() {
	configPath := flag.String("conf", defaultConfigPath, "本地配置路径")
	eventID := flag.String("event-id", "", "目标发件箱事件 ID（必填）")
	actorID := flag.String("actor-id", "", "操作者身份（必填）")
	eventKey := flag.String("event-key", "", "调用方幂等键（必填）")
	reason := flag.String("reason", "", "人工原因（必填）")
	// 默认 -1 而不是 0：0 是合法值，必须由操作者显式给出，不能靠默认值蒙对。
	expected := flag.Int("expected-redrive-count", -1, "操作者观察到的当前重驱次数（必填，0 起）")
	flag.Parse()

	if err := run(*configPath, outbox.RedriveCommand{
		EventID: *eventID, ExpectedRedriveCount: int32(*expected),
		ActorID: *actorID, Reason: *reason, Key: *eventKey,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "人工重驱失败：%v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, command outbox.RedriveCommand) error {
	// At 是「完整命令」的必备字段（仓储据此写库），因此这里先盖一次进程时钟；
	// 用例随后会用它在事务入口冻结的业务时刻重盖，所以命令行**不接受**时间参数
	// —— 否则操作者可以伪造一个过去/未来的时刻写进审计。
	command.At = time.Now().UTC()
	if err := command.Validate(); err != nil {
		return errors.New("-event-id / -actor-id / -event-key / -reason 必填，-expected-redrive-count 必须显式给出且不能为负")
	}

	bootstrap, closeConfig, err := loadBootstrap(configPath)
	if err != nil {
		return err
	}
	defer closeConfig()

	storage, cleanup, err := data.NewData(bootstrap.GetData())
	if err != nil {
		return fmt.Errorf("连接配置的 MongoDB: %w", err)
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), redriveTimeout)
	defer cancel()

	usecase := redrive.NewUsecase(
		data.NewRedriveEventStore(storage),
		data.NewGenerationCallbackRepository(storage),
		data.NewLedgerRepository(storage),
		data.NewRedriveAuditWriter(storage),
		data.NewTxRunner(storage),
	)
	result, err := usecase.Redrive(ctx, command)
	if err != nil {
		return err
	}

	if result.Replayed {
		fmt.Println("重放：同一 (actor, event_key, expected_redrive_count) 此前已执行，未产生第二次重驱。")
	}
	fmt.Printf("事件            %s\n", result.EventID)
	fmt.Printf("创作            %s\n", result.AggregateID)
	fmt.Printf("事件类型        %s\n", result.EventType)
	fmt.Printf("重驱次数        %d / %d\n", result.RedriveCount, outbox.MaxRedriveCount)
	fmt.Printf("窗口起点        %s\n", result.WindowStartedAt.Format(time.RFC3339))
	fmt.Printf("窗口起始尝试数  %d\n", result.WindowAttemptBase)
	fmt.Println("事件已重新进入自动投递，审计已写 admin_audit。")
	return nil
}

// loadBootstrap 只读取当前进程配置，并在连接 MongoDB 前执行统一的本地隔离校验。
// 与 generation-submission-worker 保持同一份口径（Kratos 文件源 + KRATOS 环境覆盖）。
func loadBootstrap(path string) (*conf.Bootstrap, func(), error) {
	configuration := kratosconfig.New(kratosconfig.WithSource(file.NewSource(path), env.NewSource("KRATOS")))
	if err := configuration.Load(); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("加载本地配置: %w", err)
	}
	bootstrap := &conf.Bootstrap{}
	if err := configuration.Scan(bootstrap); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("解析本地配置: %w", err)
	}
	if err := conf.ApplyMongoEnvironmentOverrides(bootstrap, os.Getenv); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("应用 Mongo 环境覆盖: %w", err)
	}
	if err := conf.Validate(bootstrap); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("校验本地配置: %w", err)
	}
	return bootstrap, func() { _ = configuration.Close() }, nil
}
