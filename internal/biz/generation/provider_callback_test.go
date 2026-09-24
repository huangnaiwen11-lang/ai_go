package generation

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recordingInboxStore struct {
	records []ProviderInboxRecord
	results []ProviderInboxApplyResult
	err     error
	calls   int
}

func (store *recordingInboxStore) ApplyAndSchedule(_ context.Context, record ProviderInboxRecord) (ProviderInboxApplyResult, error) {
	store.calls++
	store.records = append(store.records, record)
	if store.err != nil {
		return "", store.err
	}
	if len(store.results) == 0 {
		return InboxApplyInserted, nil
	}
	result := store.results[0]
	store.results = store.results[1:]
	return result, nil
}

// retryingTxRunner 按 attempts 次调用事务回调，用来模拟 Mongo 的整笔事务重试。
type retryingTxRunner struct {
	attempts int
	err      error
	calls    int
}

func (runner *retryingTxRunner) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	runner.calls++
	if runner.err != nil {
		return runner.err
	}
	for attempt := 0; attempt < runner.attempts; attempt++ {
		if err := fn(ctx); err != nil {
			return err
		}
	}
	return nil
}

func providerDeliveryFact() ProviderDeliveryFact {
	return ProviderDeliveryFact{
		AccountRef: "account-main", DeliveryID: "delivery-b2b-1", StepID: "step-b2b-1",
		JobID: "job-b2b-1", Attempt: 1, Payload: []byte(`{"status":"completed"}`),
	}
}

func TestProviderCallbackHandle落库并透传合并结果(t *testing.T) {
	for name, want := range map[string]ProviderInboxApplyResult{
		"首次投递": InboxApplyInserted,
		"重复投递": InboxApplyNoop,
		"冲突投递": InboxApplyQuarantined,
	} {
		t.Run(name, func(t *testing.T) {
			store := &recordingInboxStore{results: []ProviderInboxApplyResult{want}}
			usecase := NewProviderCallbackUsecase(store, &retryingTxRunner{attempts: 1})
			result, err := usecase.Handle(context.Background(), providerDeliveryFact())
			if err != nil || result != want {
				t.Fatalf("Handle() = %q, %v; want %q", result, err, want)
			}
			if store.calls != 1 || len(store.records) != 1 {
				t.Fatalf("ApplyAndSchedule 调用 %d 次，记录 %d 条", store.calls, len(store.records))
			}
			record := store.records[0]
			if record.Source != ProviderInboxSource || record.AccountRef != "account-main" ||
				record.DeliveryID != "delivery-b2b-1" || record.StepID != "step-b2b-1" ||
				record.JobID != "job-b2b-1" || record.Attempts != 1 ||
				record.Status != ProviderInboxPending || record.TerminalDigest != "" {
				t.Fatalf("落库记录 = %#v", record)
			}
		})
	}
}

// 业务时刻必须在进入可重试事务之前冻结一次：否则整笔事务重试会写出不同的
// CreatedAt，而 CreatedAt 是「首次见到这条投递」的唯一证据。
func TestProviderCallbackHandle业务时刻在进入事务前冻结(t *testing.T) {
	fixed := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	clockCalls := 0
	store := &recordingInboxStore{}
	usecase := NewProviderCallbackUsecaseWithClock(store, &retryingTxRunner{attempts: 3}, func() time.Time {
		clockCalls++
		return fixed
	})
	if _, err := usecase.Handle(context.Background(), providerDeliveryFact()); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if clockCalls != 1 {
		t.Fatalf("业务时钟被调用 %d 次，want 1", clockCalls)
	}
	if store.calls != 3 {
		t.Fatalf("事务回调执行 %d 次，want 3", store.calls)
	}
	for index, record := range store.records {
		if !record.CreatedAt.Equal(fixed) || !record.UpdatedAt.Equal(fixed) {
			t.Fatalf("第 %d 次落库时间 = %s / %s, want %s", index, record.CreatedAt, record.UpdatedAt, fixed)
		}
	}
}

func TestProviderCallbackHandle拒绝无法持久化的投递(t *testing.T) {
	cases := map[string]ProviderDeliveryFact{
		"缺少账号引用":  factWith(func(fact *ProviderDeliveryFact) { fact.AccountRef = "" }),
		"缺少投递号":   factWith(func(fact *ProviderDeliveryFact) { fact.DeliveryID = "" }),
		"缺少步骤号":   factWith(func(fact *ProviderDeliveryFact) { fact.StepID = "" }),
		"缺少任务号":   factWith(func(fact *ProviderDeliveryFact) { fact.JobID = "" }),
		"投递次数为零":  factWith(func(fact *ProviderDeliveryFact) { fact.Attempt = 0 }),
		"投递次数为负":  factWith(func(fact *ProviderDeliveryFact) { fact.Attempt = -1 }),
		"空载荷":     factWith(func(fact *ProviderDeliveryFact) { fact.Payload = nil }),
		"账号引用含空格": factWith(func(fact *ProviderDeliveryFact) { fact.AccountRef = "account main" }),
	}
	for name, fact := range cases {
		t.Run(name, func(t *testing.T) {
			store := &recordingInboxStore{}
			runner := &retryingTxRunner{attempts: 1}
			usecase := NewProviderCallbackUsecase(store, runner)
			if _, err := usecase.Handle(context.Background(), fact); !errors.Is(err, ErrInvalidProviderDelivery) {
				t.Fatalf("Handle() error = %v, want ErrInvalidProviderDelivery", err)
			}
			// 结构非法的投递不得进入事务，也不得被当成「稍后重试」。
			if store.calls != 0 || runner.calls != 0 {
				t.Fatalf("非法投递仍触发了存储 %d 次、事务 %d 次", store.calls, runner.calls)
			}
		})
	}
}

func TestProviderCallbackHandle存储失败原样上抛(t *testing.T) {
	cause := errors.New("mongo unavailable")
	store := &recordingInboxStore{err: cause}
	usecase := NewProviderCallbackUsecase(store, &retryingTxRunner{attempts: 1})
	if _, err := usecase.Handle(context.Background(), providerDeliveryFact()); !errors.Is(err, cause) {
		t.Fatalf("Handle() error = %v, want %v", err, cause)
	}
}

func TestProviderCallbackHandle依赖缺失时拒绝(t *testing.T) {
	store := &recordingInboxStore{}
	runner := &retryingTxRunner{attempts: 1}
	cases := map[string]*ProviderCallbackUsecase{
		"零值用例": nil,
		"缺少存储": NewProviderCallbackUsecase(nil, runner),
		"缺少事务": NewProviderCallbackUsecase(store, nil),
	}
	for name, usecase := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := usecase.Handle(context.Background(), providerDeliveryFact()); !errors.Is(err, ErrProviderCallbackDependenciesUnavailable) {
				t.Fatalf("Handle() error = %v, want ErrProviderCallbackDependenciesUnavailable", err)
			}
		})
	}
}

func factWith(mutate func(*ProviderDeliveryFact)) ProviderDeliveryFact {
	fact := providerDeliveryFact()
	mutate(&fact)
	return fact
}
