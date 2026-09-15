package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Runner 必须在启动后立即尝试一次投递，而不是等待第一个周期，避免新任务无故滞留。
func TestRunner启动后立即投递并在取消后退出(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deliverer := &deliveryFunc{call: func(context.Context, string) error {
		cancel()
		return nil
	}}
	runner, err := NewRunner(deliverer, time.Hour)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if err := runner.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if deliverer.calls != 1 {
		t.Fatalf("投递次数 = %d，期望启动后立即一次", deliverer.calls)
	}
}

// Worker 的可观测错误必须上抛给独立命令，不能被循环悄悄吞掉。
func TestRunner返回投递错误(t *testing.T) {
	sentinel := errors.New("local generation endpoint unavailable")
	deliverer := &deliveryFunc{call: func(context.Context, string) error { return sentinel }}
	runner, err := NewRunner(deliverer, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v，期望 %v", err, sentinel)
	}
}

// batchSize 限制每轮最多处理的事件数量，避免独立进程在单个轮询周期无限占用。
// 取消发生后必须立即停止后续批量投递，保证退出不会扩大处理范围。
func TestNewBatchRunner每轮最多投递指定次数(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	deliverer := &deliveryFunc{call: func(context.Context, string) error {
		calls++
		if calls == 3 {
			cancel()
		}
		return nil
	}}

	runner, err := NewBatchRunner(deliverer, time.Hour, 3)
	if err != nil {
		t.Fatalf("NewBatchRunner() error = %v", err)
	}
	if err := runner.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls != 3 {
		t.Fatalf("投递次数 = %d，期望本轮最多 3 次", calls)
	}
}

func TestNewBatchRunner拒绝非正批量(t *testing.T) {
	if _, err := NewBatchRunner(&deliveryFunc{call: func(context.Context, string) error { return nil }}, time.Second, 0); !errors.Is(err, ErrInvalidBatchSize) {
		t.Fatalf("NewBatchRunner() error = %v，期望 %v", err, ErrInvalidBatchSize)
	}
}

type deliveryFunc struct {
	call  func(context.Context, string) error
	calls int
}

func (deliverer *deliveryFunc) DeliverOnce(ctx context.Context, eventID string) error {
	deliverer.calls++
	return deliverer.call(ctx, eventID)
}
