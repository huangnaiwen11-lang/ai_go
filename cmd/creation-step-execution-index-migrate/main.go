// Command creation-step-execution-index-migrate performs the opt-in, two-phase
// replacement of the legacy global provider-job unique index. It is never
// called by the API or by ordinary schema initialization.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ai-business-service/internal/data/migrate"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	modePreflight    = "preflight"
	modeCreateScoped = "create-scoped"
	modeRetireLegacy = "retire-legacy"
	defaultTimeout   = 15 * time.Minute
)

type creationStepIndexMigration interface {
	Preflight(context.Context) (migrate.CreationStepExecutionIndexPreflight, error)
	EnsureScopedUnique(context.Context) (migrate.CreationStepExecutionIndexPreflight, error)
	RetireLegacyGlobalUnique(context.Context, bool) (migrate.CreationStepExecutionIndexPreflight, error)
}

type creationStepIndexOpener func(context.Context, string, string) (creationStepIndexMigration, func(context.Context), error)

func main() {
	os.Exit(run(os.Args[1:], openCreationStepIndexMigration, os.Stdout, os.Stderr))
}

// run keeps preflight read-only by default. It intentionally emits only
// aggregate counts; connection strings, job IDs and account references must
// never appear in operator logs.
func run(args []string, openMigration creationStepIndexOpener, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("creation-step-execution-index-migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mongoURI := flags.String("mongo-uri", "", "MongoDB connection URI")
	database := flags.String("database", "", "MongoDB database name")
	mode := flags.String("mode", modePreflight, "preflight | create-scoped | retire-legacy")
	confirmWriters := flags.Bool("confirm-scoped-writers", false, "confirm every active writer understands provider/account/job scope")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if strings.TrimSpace(*mongoURI) == "" || strings.TrimSpace(*database) == "" || !validMode(*mode) {
		fmt.Fprintln(stderr, "必须提供 --mongo-uri、--database 和有效 --mode。")
		return 2
	}
	if *mode == modeRetireLegacy && !*confirmWriters {
		fmt.Fprintln(stderr, "撤销旧全局索引必须显式提供 --confirm-scoped-writers。")
		return 2
	}
	if openMigration == nil {
		fmt.Fprintln(stderr, "索引迁移未配置。")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	migration, closeMigration, err := openMigration(ctx, *mongoURI, *database)
	if err != nil || migration == nil {
		fmt.Fprintln(stderr, "无法连接索引迁移目标。")
		return 1
	}
	defer closeMigration(ctx)

	report, err := migration.Preflight(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "索引预检失败。")
		return 1
	}
	printPreflight(stdout, report)
	if !report.Ready() {
		fmt.Fprintln(stderr, "预检未通过：请先完成历史 route 回填与作用域重复处理。")
		return 1
	}
	switch *mode {
	case modePreflight:
		fmt.Fprintln(stdout, "预检通过；未创建或删除索引。")
		return 0
	case modeCreateScoped:
		if _, err := migration.EnsureScopedUnique(ctx); err != nil {
			fmt.Fprintln(stderr, "创建作用域唯一索引失败。")
			return 1
		}
		fmt.Fprintln(stdout, "已确认作用域唯一索引；旧全局索引仍保留。")
		return 0
	case modeRetireLegacy:
		if _, err := migration.RetireLegacyGlobalUnique(ctx, true); err != nil {
			fmt.Fprintln(stderr, "撤销旧全局索引失败。")
			return 1
		}
		fmt.Fprintln(stdout, "旧全局索引已撤销；作用域唯一索引保持生效。")
		return 0
	default:
		// validMode above keeps this unreachable, but keep the command
		// fail-closed if a future mode is added incompletely.
		fmt.Fprintln(stderr, "无效索引迁移模式。")
		return 2
	}
}

func validMode(mode string) bool {
	switch mode {
	case modePreflight, modeCreateScoped, modeRetireLegacy:
		return true
	default:
		return false
	}
}

func printPreflight(writer io.Writer, report migrate.CreationStepExecutionIndexPreflight) {
	fmt.Fprintf(writer, "预检：含 job 步骤=%d，身份无效=%d，重复作用域组=%d。\n", report.DocumentsWithJob, report.InvalidRouteDocuments, report.DuplicateScopedJobGroups)
}

func openCreationStepIndexMigration(ctx context.Context, mongoURI, database string) (creationStepIndexMigration, func(context.Context), error) {
	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, err
	}
	return migrate.NewCreationStepExecutionIndexMigration(client.Database(database)), func(disconnectContext context.Context) {
		_ = client.Disconnect(disconnectContext)
	}, nil
}
