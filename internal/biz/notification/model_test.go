package notification

import (
	"context"
	"testing"
	"time"
)

type memoryRepository struct {
	listQuery ListQuery
	marked    string
	all       string
	deleted   string
	cleared   string
}

func (r *memoryRepository) List(_ context.Context, q ListQuery) ([]Item, int, error) {
	r.listQuery = q
	return []Item{}, 0, nil
}
func (r *memoryRepository) MarkRead(_ context.Context, userID, id string, _ time.Time) error {
	r.marked = userID + ":" + id
	return nil
}
func (r *memoryRepository) MarkAllRead(_ context.Context, userID string, _ time.Time) (int, error) {
	r.all = userID
	return 2, nil
}
func (r *memoryRepository) Delete(_ context.Context, userID, id string) error {
	r.deleted = userID + ":" + id
	return nil
}

func (r *memoryRepository) DeleteRead(_ context.Context, userID string) (int, error) {
	r.cleared = userID
	return 3, nil
}

func TestUsecaseAlwaysScopesByUser(t *testing.T) {
	repository := &memoryRepository{}
	usecase := NewUsecase(repository)
	if _, _, err := usecase.List(context.Background(), ListQuery{UserID: "user-1", Limit: 200}); err != nil {
		t.Fatal(err)
	}
	if repository.listQuery.UserID != "user-1" || repository.listQuery.Limit != 50 {
		t.Fatalf("query = %#v", repository.listQuery)
	}
	if err := usecase.MarkRead(context.Background(), "user-1", "n-1"); err != nil {
		t.Fatal(err)
	}
	if repository.marked != "user-1:n-1" {
		t.Fatalf("marked = %q", repository.marked)
	}
}

func TestUsecaseDeleteReadAlwaysScopesByCurrentUser(t *testing.T) {
	repository := &memoryRepository{}
	usecase := NewUsecase(repository)

	deleted, err := usecase.DeleteRead(context.Background(), "session-user")
	if err != nil || deleted != 3 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if repository.cleared != "session-user" {
		t.Fatalf("clear user=%q", repository.cleared)
	}
}
