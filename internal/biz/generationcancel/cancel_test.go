package generationcancel

import (
	"context"
	"errors"
	"testing"
	"time"
)

var cancelNow = time.Date(2026, time.September, 21, 10, 0, 0, 0, time.UTC)

type fakeStore struct {
	command Command
	result  Result
	err     error
	calls   int
}

func (store *fakeStore) RequestCancellation(_ context.Context, command Command) (Result, error) {
	store.calls++
	store.command = command
	return store.result, store.err
}

type immediateTx struct{ calls int }

func (tx *immediateTx) WithinTx(ctx context.Context, operation func(context.Context) error) error {
	tx.calls++
	return operation(ctx)
}

func TestCancel在同一事务中交给持久化边界(t *testing.T) {
	store := &fakeStore{result: Result{CreationID: "creation-1", Accepted: true, CancelEventCount: 1}}
	tx := &immediateTx{}
	usecase := NewUsecaseWithClock(store, tx, func() time.Time { return cancelNow })

	result, err := usecase.Cancel(context.Background(), Command{CreationID: "creation-1", UserID: "user-1"})
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if tx.calls != 1 || store.calls != 1 {
		t.Fatalf("transaction/store calls = %d/%d, want 1/1", tx.calls, store.calls)
	}
	if !store.command.At.Equal(cancelNow) || store.command.CreationID != "creation-1" || store.command.UserID != "user-1" {
		t.Fatalf("store command = %#v", store.command)
	}
	if result != store.result {
		t.Fatalf("result = %#v, want %#v", result, store.result)
	}
}

func TestCancel拒绝无效命令且没有副作用(t *testing.T) {
	store := &fakeStore{}
	tx := &immediateTx{}
	usecase := NewUsecaseWithClock(store, tx, func() time.Time { return cancelNow })

	_, err := usecase.Cancel(context.Background(), Command{CreationID: "creation-1", UserID: " user-1"})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("Cancel() error = %v, want ErrInvalidCommand", err)
	}
	if tx.calls != 0 || store.calls != 0 {
		t.Fatalf("invalid command calls = %d/%d, want 0/0", tx.calls, store.calls)
	}
}

func TestCancel缺少依赖明确失败(t *testing.T) {
	_, err := (*Usecase)(nil).Cancel(context.Background(), Command{CreationID: "creation-1", UserID: "user-1"})
	if !errors.Is(err, ErrDependenciesUnavailable) {
		t.Fatalf("nil usecase Cancel() error = %v, want ErrDependenciesUnavailable", err)
	}
}
