// r2-orphan-audit is a bounded, read-only comparison of B2B-owned R2 result
// objects and their MongoDB asset records. It never deletes R2 objects or
// edits MongoDB; a nonzero count is an operator investigation signal only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"ai-business-service/internal/biz/r2audit"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/integrations/r2"

	kratosconfig "github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
)

const (
	defaultConfigPath = "/data/conf/config.yaml"
	defaultMaxKeys    = 1000
	auditTimeout      = 30 * time.Second
)

func main() {
	configPath := flag.String("conf", defaultConfigPath, "本地配置路径")
	cursor := flag.String("continuation-token", "", "上一次审计返回的 R2 continuation token")
	maxKeys := flag.Int("max-keys", defaultMaxKeys, "本次最多审计的 R2 对象数（1..1000）")
	flag.Parse()

	if err := run(*configPath, *cursor, int32(*maxKeys), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "R2 孤儿审计失败：", err)
		os.Exit(1)
	}
}

// run performs exactly one bounded page. The continuation token is returned
// only to allow a privileged operator to resume a long audit; object keys and
// provider URLs are intentionally absent from the report.
func run(configPath, cursor string, maxKeys int32, stdout io.Writer) error {
	if err := (r2audit.Command{Cursor: cursor, Limit: maxKeys}).Validate(); err != nil {
		return fmt.Errorf("校验 max-keys 或 continuation-token: %w", err)
	}
	if stdout == nil {
		return errors.New("审计输出不可用")
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
	configuration, enabled, err := r2.LoadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("加载 R2 配置: %w", err)
	}
	if !enabled {
		return errors.New("R2 配置缺失；孤儿审计不会回退到本地存储")
	}
	client, err := r2.NewClient(configuration)
	if err != nil {
		return fmt.Errorf("构造 R2 客户端: %w", err)
	}
	defer client.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	result, err := r2audit.NewUsecase(r2.NewAuditInventory(client), data.NewR2AuditAssetIndex(storage)).Audit(ctx, r2audit.Command{
		Cursor: cursor, Limit: maxKeys,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		Scanned          int    `json:"scanned"`
		PotentialOrphans int    `json:"potential_orphans"`
		Complete         bool   `json:"complete"`
		NextCursor       string `json:"next_cursor,omitempty"`
	}{
		Scanned: result.Scanned, PotentialOrphans: result.Orphaned, Complete: result.Complete, NextCursor: result.NextCursor,
	})
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
