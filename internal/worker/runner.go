package worker

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrRunnerDependenciesUnavailable 表示轮询器没有可安全调用的单次投递单元。
	ErrRunnerDependenciesUnavailable = errors.New("generation submission runner dependencies are unavailable")
	// ErrInvalidPollInterval 表示轮询周期必须为正数，避免忙循环占用本机资源。
	ErrInvalidPollInterval = errors.New("generation submission runner poll interval must be positive")
	// ErrInvalidBatchSize 表示每轮处理上限必须为正数，避免配置把 Worker 变成空转进程。
	ErrInvalidBatchSize = errors.New("generation submission runner batch size must be positive")
)

// onceDeliverer 抽象现有的单次 Worker，确保轮询器不重写任何提交、审核或账本语义。
type onceDeliverer interface {
	DeliverOnce(context.Context, string) error
}

// Runner 是仅供显式本地命令调用的轮询生命周期封装。
// 它不自行连接 MongoDB、不构造生成客户端，也不隐式启动 goroutine。
type Runner struct {
	deliverer onceDeliverer
	interval  time.Duration
	batchSize int
}

// NewRunner 构造本地轮询器。调用者负责决定是否运行，从而保证默认服务行为不变。
func NewRunner(deliverer onceDeliverer, interval time.Duration) (*Runner, error) {
	return NewBatchRunner(deliverer, interval, 1)
}

// NewBatchRunner 构造一次轮询最多处理 batchSize 个事件的本地 Runner。
// 每个事件仍复用既有 DeliverOnce 状态机，批量只影响公平性与单轮工作上限，
// 不改变预扣、冲正、审核或回调的业务语义。
func NewBatchRunner(deliverer onceDeliverer, interval time.Duration, batchSize int) (*Runner, error) {
	if deliverer == nil {
		return nil, ErrRunnerDependenciesUnavailable
	}
	if interval <= 0 {
		return nil, ErrInvalidPollInterval
	}
	if batchSize <= 0 {
		return nil, ErrInvalidBatchSize
	}
	return &Runner{deliverer: deliverer, interval: interval, batchSize: batchSize}, nil
}

// Run 启动后立即尝试一轮投递，随后按固定周期继续。空队列由 DeliverOnce 的 nil 返回表示。
// 上下文取消属于正常退出；其他错误上抛给入口命令记录并终止，避免沉默丢失本地任务。
func (runner *Runner) Run(ctx context.Context) error {
	if runner == nil || runner.deliverer == nil {
		return ErrRunnerDependenciesUnavailable
	}
	if ctx == nil {
		return errors.New("generation submission runner context is required")
	}
	ticker := time.NewTicker(runner.interval)
	defer ticker.Stop()
	for {
		for attempt := 0; attempt < runner.batchSize; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil
			}
			if err := runner.deliverer.DeliverOnce(ctx, ""); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
