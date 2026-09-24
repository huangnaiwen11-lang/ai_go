// outbox-retention 是 outbox_events 保留期的受控运维命令。
//
// 默认 preflight 只读。create/purge 必须显式传 --confirm，并且 create 会在
// 创建 TTL 前拒绝仍有已过期历史文档的数据库，避免索引创建触发一次性删除。
// 命令不监听端口，也不会被业务服务启动流程自动调用。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/data/migrate"

	kratosconfig "github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
)

const (
	defaultConfigPath = "/data/conf/config.yaml"
	defaultBatchSize  = int64(100)
	retentionTimeout  = 30 * time.Second
)

func main() {
	configPath := flag.String("conf", defaultConfigPath, "本地配置路径")
	mode := flag.String("mode", "preflight", "操作模式：preflight、create 或 purge")
	confirm := flag.Bool("confirm", false, "确认执行 create/purge（必填）")
	batchSize := flag.Int64("batch-size", defaultBatchSize, "purge 单批最多删除数量")
	flag.Parse()

	if err := run(*configPath, *mode, *confirm, *batchSize); err != nil {
		fmt.Fprintf(os.Stderr, "outbox 保留期操作失败：%v\n", err)
		os.Exit(1)
	}
}

func run(configPath, mode string, confirm bool, batchSize int64) error {
	switch mode {
	case "preflight":
	case "create", "purge":
		if !confirm {
			return errors.New("create/purge 必须显式传 --confirm")
		}
		if mode == "purge" && batchSize <= 0 {
			return errors.New("purge 的 --batch-size 必须为正数")
		}
	default:
		return fmt.Errorf("未知 mode %q，只允许 preflight/create/purge", mode)
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

	ctx, cancel := context.WithTimeout(context.Background(), retentionTimeout)
	defer cancel()
	migrator := data.NewOutboxRetentionMigrator(storage)
	now := time.Now().UTC()
	report, err := migrator.Preflight(ctx, now)
	if err != nil {
		return err
	}
	printReport(report)

	switch mode {
	case "preflight":
		return nil
	case "create":
		if report.HasExpired() {
			return errors.New("仍有已过期终态历史文档；先低峰分批 purge，再重新 preflight，禁止直接创建 TTL")
		}
		if err := migrator.CreateIndexes(ctx); err != nil {
			return err
		}
		fmt.Println("三档 outbox TTL 索引已创建；active 状态不在过滤条件内。")
		return nil
	case "purge":
		deleted, err := migrator.PurgeExpired(ctx, now, batchSize)
		if err != nil {
			return err
		}
		fmt.Printf("本次分批清理 %d 条已过期终态事件。\n", deleted)
		return nil
	default:
		panic("validated mode switch is unreachable")
	}
}

func printReport(report migrate.OutboxRetentionReport) {
	fmt.Printf("预检时间 %s\n", report.CheckedAt.Format(time.RFC3339))
	for _, bucket := range report.Buckets {
		oldest := "-"
		if bucket.Oldest != nil {
			oldest = bucket.Oldest.Format(time.RFC3339)
		}
		fmt.Printf("%-16s 总数=%d 已过期=%d 最早=%s 保留=%s\n", bucket.Status, bucket.TotalCount, bucket.ExpiredCount, oldest, bucket.Retention)
	}
}

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
