package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/worker"
)

type stubGenerationUnit struct {
	calls []string
	err   error
}

func (stub *stubGenerationUnit) DeliverOnce(_ context.Context, expectedEventID string) error {
	stub.calls = append(stub.calls, expectedEventID)
	return stub.err
}

func TestGenerationDeliverer按顺序驱动三个单元(t *testing.T) {
	submission := &stubGenerationUnit{}
	consumer := &stubGenerationUnit{}
	reconcile := &stubGenerationUnit{}
	deliverer := generationDeliverer{submission: submission, consumer: consumer, reconcile: reconcile}

	if err := deliverer.DeliverOnce(context.Background(), "event-1"); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(submission.calls) != 1 || len(consumer.calls) != 1 || len(reconcile.calls) != 1 {
		t.Fatalf("提交 %v / 消费 %v / 对账 %v，期望各 1 次", submission.calls, consumer.calls, reconcile.calls)
	}
	// 三个单元必须拿到同一个待投递标识：否则受控单次投递只会命中其中一条队列。
	for name, calls := range map[string][]string{"提交": submission.calls, "消费": consumer.calls, "对账": reconcile.calls} {
		if calls[0] != "event-1" {
			t.Fatalf("%s 待投递标识 = %q", name, calls[0])
		}
	}
}

// 取消必须紧随提交单元：一次 Submit 已在同轮绑定 job 并发现此前的取消意图时，
// ProviderCancelWorker 才能在同轮领取新建的 cancel outbox，而不必等下一次轮询。
func TestGenerationDeliverer提交后驱动取消单元(t *testing.T) {
	submission := &stubGenerationUnit{}
	cancellation := &stubGenerationUnit{}
	consumer := &stubGenerationUnit{}
	deliverer := generationDeliverer{submission: submission, cancellation: cancellation, consumer: consumer}

	if err := deliverer.DeliverOnce(context.Background(), "cancel-event-1"); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(submission.calls) != 1 || len(cancellation.calls) != 1 || len(consumer.calls) != 1 {
		t.Fatalf("提交/取消/消费调用 = %d/%d/%d，期望各 1 次", len(submission.calls), len(cancellation.calls), len(consumer.calls))
	}
	if cancellation.calls[0] != "cancel-event-1" {
		t.Fatalf("取消待投递标识 = %q，want cancel-event-1", cancellation.calls[0])
	}
}

func TestGenerationDeliverer取消失败时不再驱动后续单元(t *testing.T) {
	failure := errors.New("cancel delivery failed")
	submission := &stubGenerationUnit{}
	cancellation := &stubGenerationUnit{err: failure}
	consumer := &stubGenerationUnit{}
	deliverer := generationDeliverer{submission: submission, cancellation: cancellation, consumer: consumer}

	if err := deliverer.DeliverOnce(context.Background(), ""); !errors.Is(err, failure) {
		t.Fatalf("错误 = %v，期望 %v", err, failure)
	}
	if len(submission.calls) != 1 || len(cancellation.calls) != 1 || len(consumer.calls) != 0 {
		t.Fatalf("提交/取消/消费调用 = %d/%d/%d，期望 1/1/0", len(submission.calls), len(cancellation.calls), len(consumer.calls))
	}
}

func TestGenerationDeliverer提交失败时不再驱动后续单元(t *testing.T) {
	failure := errors.New("claim failed")
	submission := &stubGenerationUnit{err: failure}
	consumer := &stubGenerationUnit{}
	reconcile := &stubGenerationUnit{}
	deliverer := generationDeliverer{submission: submission, consumer: consumer, reconcile: reconcile}

	if err := deliverer.DeliverOnce(context.Background(), ""); !errors.Is(err, failure) {
		t.Fatalf("错误 = %v，期望 %v", err, failure)
	}
	if len(consumer.calls) != 0 || len(reconcile.calls) != 0 {
		t.Fatalf("消费/对账被驱动 %d/%d 次，期望 0/0 次", len(consumer.calls), len(reconcile.calls))
	}
}

// 消费失败同样必须挡住对账：让对账照跑会把「一条队列已经报错」掩盖成一次
// 正常轮询，而入口的 fail-fast 语义正是靠这个上抛才成立。
func TestGenerationDeliverer消费失败时不再驱动对账(t *testing.T) {
	failure := errors.New("consume failed")
	submission := &stubGenerationUnit{}
	consumer := &stubGenerationUnit{err: failure}
	reconcile := &stubGenerationUnit{}
	deliverer := generationDeliverer{submission: submission, consumer: consumer, reconcile: reconcile}

	if err := deliverer.DeliverOnce(context.Background(), ""); !errors.Is(err, failure) {
		t.Fatalf("错误 = %v，期望 %v", err, failure)
	}
	if len(submission.calls) != 1 || len(reconcile.calls) != 0 {
		t.Fatalf("提交/对账被驱动 %d/%d 次，期望 1/0 次", len(submission.calls), len(reconcile.calls))
	}
}

// 未配置 B2B 时消费与对账单元缺席：轮询必须照常驱动提交，而不是整体停摆。
func TestGenerationDeliverer缺少B2B单元时仍驱动提交(t *testing.T) {
	submission := &stubGenerationUnit{}
	deliverer := generationDeliverer{submission: submission}

	if err := deliverer.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(submission.calls) != 1 {
		t.Fatalf("提交被驱动 %d 次，期望 1 次", len(submission.calls))
	}
}

// 只缺对账（例如 B2B 配置存在但注册表未解析出对账能力）时，消费仍要被驱动。
func TestGenerationDeliverer缺少对账单元时仍驱动消费(t *testing.T) {
	submission := &stubGenerationUnit{}
	consumer := &stubGenerationUnit{}
	deliverer := generationDeliverer{submission: submission, consumer: consumer}

	if err := deliverer.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(submission.calls) != 1 || len(consumer.calls) != 1 {
		t.Fatalf("提交/消费被驱动 %d/%d 次，期望 1/1 次", len(submission.calls), len(consumer.calls))
	}
}

func TestGenerationDeliverer成功循环后才就绪并刷新积压快照(t *testing.T) {
	observability, err := worker.NewRuntimeObservability("127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRuntimeObservability() error = %v", err)
	}
	if err := observability.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := observability.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	})
	backlog := &stubOutboxBacklogReader{snapshot: outbox.BacklogSnapshot{Pending: 3}}
	deliverer := generationDeliverer{
		submission:    &stubGenerationUnit{},
		backlog:       backlog,
		observability: observability,
	}
	if err := deliverer.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if backlog.calls != 1 {
		t.Fatalf("ReadOutboxBacklog() calls = %d, want 1", backlog.calls)
	}
	if err := deliverer.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("第二次 DeliverOnce() error = %v", err)
	}
	if backlog.calls != 1 {
		t.Fatalf("同一采样窗口内 ReadOutboxBacklog() calls = %d, want 1", backlog.calls)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	observability.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("成功完整循环后 ready status = %d, want 200", response.Code)
	}
}

type stubOutboxBacklogReader struct {
	snapshot outbox.BacklogSnapshot
	err      error
	calls    int
}

func (stub *stubOutboxBacklogReader) ReadOutboxBacklog(context.Context) (outbox.BacklogSnapshot, error) {
	stub.calls++
	return stub.snapshot, stub.err
}
