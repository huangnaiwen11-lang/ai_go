package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"ai-business-service/internal/biz"
	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/worker"

	"github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	"github.com/go-kratos/kratos/v3"
	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"github.com/go-kratos/kratos/v3/log"
	"github.com/go-kratos/kratos/v3/transport/grpc"
	"github.com/go-kratos/kratos/v3/transport/http"

	_ "go.uber.org/automaxprocs"
)

// 可通过 go build -ldflags "-X main.Version=x.y.z" 注入版本号。
var (
	// Name 是编译产物的服务名。
	Name = "ai-business-service"
	// Version 是构建时注入的服务版本。
	Version string
	// flagconf 保存运行时配置文件路径。
	flagconf string

	id, _ = os.Hostname()
)

// schemaInitializationTimeout 限制启动前 schema 初始化时长，索引必须在 HTTP/gRPC 监听前完成。
const schemaInitializationTimeout = 10 * time.Second

func init() {
	// 默认读取单个配置文件，避免把示例文件误当成运行配置并合并。
	flag.StringVar(&flagconf, "conf", "./configs/config.yaml", "配置文件路径，例如：-conf config.yaml")
}

// newApp 末尾的 *creations.Usecase 参数仅强制 Wire 完整解析创作依赖图。
// 它不注册路由、不调用用例，也不写入数据库。
func newApp(logger *slog.Logger, gs *grpc.Server, hs *http.Server, modules *biz.ModuleRegistry, _ shared.TxRunner, initializer data.LocalSchemaInitializer, _ *creations.Usecase, _ *worker.GenerationSubmissionWorker) (*kratos.App, error) {
	ctx, cancel := context.WithTimeout(context.Background(), schemaInitializationTimeout)
	defer cancel()
	if err := initializer.Ensure(ctx); err != nil {
		return nil, fmt.Errorf("initialize local MongoDB schema: %w", err)
	}

	return kratos.New(
		kratos.ID(id),
		kratos.Name(Name),
		kratos.Version(Version),
		kratos.Metadata(map[string]string{
			"business_modules": strings.Join(modules.Names, ","),
		}),
		kratos.Logger(logger),
		kratos.Server(
			gs,
			hs,
		),
	), nil
}

func main() {
	flag.Parse()
	logger := log.NewLogger(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			AddSource: true,
			Level:     slog.LevelInfo,
		}),
		log.WithExtractor(tracing.TraceAttrs),
	).With(
		slog.String("service.id", id),
		slog.String("service.name", Name),
		slog.String("service.version", Version),
	)
	log.SetDefault(logger)
	c := config.New(
		config.WithSource(
			file.NewSource(flagconf),
			env.NewSource("KRATOS"),
		),
	)
	defer c.Close()

	if err := c.Load(); err != nil {
		panic(err)
	}

	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		panic(err)
	}
	if err := conf.Validate(&bc); err != nil {
		panic(err)
	}

	app, cleanup, err := wireApp(bc.Server, bc.Data, bc.Security, bc.Integrations, logger)
	if err != nil {
		panic(err)
	}
	defer cleanup()

	// 启动服务并等待退出信号，由 Wire cleanup 关闭 MongoDB 连接。
	if err := app.Run(); err != nil {
		panic(err)
	}
}
