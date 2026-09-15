package works_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/works"
)

func TestList仅查询会话所属作品且Cursor优先于Skip(t *testing.T) {
	repository := &recordingRepository{page: &works.Page{Items: []works.Work{{ID: "work-1", Kind: works.KindImage}}}}
	usecase := works.NewUsecase(repository)
	cursor, err := works.EncodeCursor(time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC), "work-previous")
	if err != nil {
		t.Fatal(err)
	}

	page, err := usecase.List(context.Background(), works.ListQuery{UserID: "session-user", Kind: works.KindImage, Limit: 20, Skip: 9, Cursor: cursor})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Items) != 1 || repository.listQuery.UserID != "session-user" || repository.listQuery.Kind != works.KindImage || repository.listQuery.Skip != 0 || repository.listQuery.Cursor != cursor {
		t.Fatalf("page=%#v, query=%#v", page, repository.listQuery)
	}
}

// 全部作品是前端的默认筛选；空 Kind 必须被保留给仓储层表示“不追加输出类型过滤”。
func TestList全部作品筛选允许空Kind(t *testing.T) {
	repository := &recordingRepository{page: &works.Page{Items: []works.Work{}}}
	usecase := works.NewUsecase(repository)

	if _, err := usecase.List(context.Background(), works.ListQuery{UserID: "session-user", Limit: 20}); err != nil {
		t.Fatalf("空 Kind 的全部作品查询 error = %v", err)
	}
	if repository.listCalls != 1 || repository.listQuery.Kind != "" {
		t.Fatalf("全部作品查询 = %#v，期望以空 Kind 进入仓储", repository.listQuery)
	}
}

func TestGet跨用户与缺失作品统一为未找到(t *testing.T) {
	repository := &recordingRepository{getErr: works.ErrWorkNotFound}
	usecase := works.NewUsecase(repository)

	work, err := usecase.Get(context.Background(), "session-user", "other-user-work")
	if !errors.Is(err, works.ErrWorkNotFound) || work != nil {
		t.Fatalf("Get() work=%#v err=%v，跨用户必须统一为未找到", work, err)
	}
	if repository.getUserID != "session-user" || repository.getID != "other-user-work" {
		t.Fatalf("查询边界错误：user=%q id=%q", repository.getUserID, repository.getID)
	}
}

func TestList非法Cursor不访问仓储(t *testing.T) {
	repository := &recordingRepository{}
	usecase := works.NewUsecase(repository)

	_, err := usecase.List(context.Background(), works.ListQuery{UserID: "session-user", Kind: works.KindVideo, Limit: 1, Cursor: "not-a-cursor"})
	if !errors.Is(err, works.ErrInvalidQuery) {
		t.Fatalf("List() error = %v，want ErrInvalidQuery", err)
	}
	if repository.listCalls != 0 {
		t.Fatalf("非法游标不得查询仓储，calls=%d", repository.listCalls)
	}
}

type recordingRepository struct {
	page      *works.Page
	listErr   error
	getWork   *works.Work
	getErr    error
	listCalls int
	listQuery works.ListQuery
	getUserID string
	getID     string
}

func (repository *recordingRepository) List(_ context.Context, query works.ListQuery) (*works.Page, error) {
	repository.listCalls++
	repository.listQuery = query
	return repository.page, repository.listErr
}

func (repository *recordingRepository) FindByID(_ context.Context, userID, id string) (*works.Work, error) {
	repository.getUserID = userID
	repository.getID = id
	return repository.getWork, repository.getErr
}
