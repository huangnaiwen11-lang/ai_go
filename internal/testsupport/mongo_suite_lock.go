// Package testsupport 提供仅供测试代码使用的跨进程辅助能力。
package testsupport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	localMongoURIEnv = "CLING_TEST_MONGO_URI"
	// Mongo 集成测试共用固定 cling_main 数据库，跨包运行时必须互斥，避免通用
	// Outbox 领取测试占用另一个包的随机事件。
	mongoSuiteLockDirectory = "ai-business-service-mongo-integration-suite.lock"
	mongoSuiteLockTimeout   = 5 * time.Minute
	mongoSuiteStaleAfter    = 15 * time.Minute
)

// RunMongoIntegrationSuite 供各 Mongo 集成测试包的 TestMain 调用。
// 未设置测试连接串时不获取锁，保留单元测试可独立、并行执行的默认体验。
func RunMongoIntegrationSuite(m *testing.M) int {
	if m == nil {
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), mongoSuiteLockTimeout)
	defer cancel()
	release, err := acquireMongoSuiteLock(ctx, strings.TrimSpace(os.Getenv(localMongoURIEnv)))
	if err != nil {
		// 禁止回显连接串；错误只说明测试基础设施状态。
		_, _ = fmt.Fprintln(os.Stderr, "无法获取本地 Mongo 集成测试锁:", err)
		return 1
	}

	exitCode := m.Run()
	release()
	return exitCode
}

// acquireMongoSuiteLock 仅在本地 Mongo 集成测试已显式开启时执行跨进程互斥。
// 锁目录位于系统临时目录，测试异常退出后可由后续运行安全回收过期锁。
func acquireMongoSuiteLock(ctx context.Context, mongoURI string) (func(), error) {
	if mongoURI == "" {
		return func() {}, nil
	}

	lockPath := filepath.Join(os.TempDir(), mongoSuiteLockDirectory)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建测试锁: %w", err)
		}
		if err := removeStaleMongoSuiteLock(lockPath); err != nil {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("等待测试锁超时: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// removeStaleMongoSuiteLock 只删除超过保守时限的明确临时锁目录，绝不触及业务数据。
func removeStaleMongoSuiteLock(lockPath string) error {
	info, err := os.Stat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取测试锁: %w", err)
	}
	if !info.IsDir() || time.Since(info.ModTime()) <= mongoSuiteStaleAfter {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("清理过期测试锁: %w", err)
	}
	return nil
}
