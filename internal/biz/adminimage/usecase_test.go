package adminimage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestListNormalizesPaginationMultiImageAndShanghaiToday(t *testing.T) {
	repository := &recordingRepository{list: ListResult{Total: 101}}
	now := func() time.Time { return time.Date(2026, time.September, 24, 1, 30, 0, 0, time.UTC) }
	usecase := NewUsecase(repository, nil, now)

	result, err := usecase.List(context.Background(), ListQuery{DateRange: "today", MultiImage: "true", Limit: 999})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if repository.query.Page != 1 || repository.query.Limit != 100 || repository.query.MultiImage != "true" || repository.query.Skip != 0 || repository.query.FetchLimit != 100 {
		t.Fatalf("query = %#v", repository.query)
	}
	wantStart := time.Date(2026, time.September, 23, 16, 0, 0, 0, time.UTC)
	if !repository.query.TodayStart.Equal(wantStart) {
		t.Fatalf("today start = %s, want %s", repository.query.TodayStart, wantStart)
	}
	if result.TotalPages != 2 {
		t.Fatalf("total pages = %d, want 2", result.TotalPages)
	}
}

func TestProviderStatusReturnsUnavailableWithoutFleetOrWhenFleetFails(t *testing.T) {
	repository := &recordingRepository{}
	usecase := NewUsecase(repository, nil, nil)
	if _, err := usecase.ProviderStatus(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil fleet error = %v, want ErrUnavailable", err)
	}

	usecase = NewUsecase(repository, failingFleet{}, nil)
	if _, err := usecase.ProviderStatus(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed fleet error = %v, want ErrUnavailable", err)
	}
}

type recordingRepository struct {
	list  ListResult
	query ListQuery
}

func (r *recordingRepository) List(_ context.Context, query ListQuery) (ListResult, error) {
	r.query = query
	return r.list, nil
}
func (*recordingRepository) Stats(context.Context, time.Time) (Stats, error) { return Stats{}, nil }
func (*recordingRepository) TemplateOptions(context.Context) (TemplateOptions, error) {
	return TemplateOptions{}, nil
}
func (*recordingRepository) ProviderRollup(context.Context, time.Time) (map[string]ProviderRollup, int64, error) {
	return nil, 0, nil
}
func (*recordingRepository) ExternalToday(context.Context, time.Time) (int64, error) { return 0, nil }
func (*recordingRepository) Mutate(context.Context, string, Mutation) error          { return nil }
func (*recordingRepository) BatchHide(context.Context, []string) error               { return nil }

type failingFleet struct{}

func (failingFleet) ImageProviderStatus(context.Context) (GPUStatus, *RecentThroughput, error) {
	return GPUStatus{}, nil, errors.New("fleet unavailable")
}
func (failingFleet) ImageTodayByGPU(context.Context, time.Time) ([]NodeOutput, error) {
	return nil, errors.New("fleet unavailable")
}
