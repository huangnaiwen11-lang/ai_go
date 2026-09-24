package adminimage

import (
	"context"
	"math"
	"strings"
	"time"
)

const (
	defaultPage  = 1
	defaultLimit = 20
	maxLimit     = 100
	maxFetch     = 5000
)

type Usecase struct {
	repository Repository
	fleet      Fleet
	now        func() time.Time
}

func NewUsecase(repository Repository, fleet Fleet, now func() time.Time) *Usecase {
	if now == nil {
		now = time.Now
	}
	return &Usecase{repository: repository, fleet: fleet, now: now}
}

func (u *Usecase) List(ctx context.Context, query ListQuery) (ListResult, error) {
	if u == nil || u.repository == nil {
		return ListResult{}, ErrUnavailable
	}
	query = normalizeListQuery(query, u.now())
	result, err := u.repository.List(ctx, query)
	if err != nil {
		return ListResult{}, err
	}
	result.Page, result.Limit = query.Page, query.Limit
	result.TotalPages = int((int64(result.Total) + int64(query.Limit) - 1) / int64(query.Limit))
	return result, nil
}

func (u *Usecase) Stats(ctx context.Context) (Stats, error) {
	if u == nil || u.repository == nil {
		return Stats{}, ErrUnavailable
	}
	return u.repository.Stats(ctx, shanghaiStartOfDay(u.now()))
}

func (u *Usecase) TemplateOptions(ctx context.Context) (TemplateOptions, error) {
	if u == nil || u.repository == nil {
		return TemplateOptions{}, ErrUnavailable
	}
	return u.repository.TemplateOptions(ctx)
}

func (u *Usecase) ProviderStatus(ctx context.Context) (ProviderStatus, error) {
	if u == nil || u.repository == nil || u.fleet == nil {
		return ProviderStatus{}, ErrUnavailable
	}
	fleet, throughput, err := u.fleet.ImageProviderStatus(ctx)
	if err != nil {
		return ProviderStatus{}, ErrUnavailable
	}
	rollup, generating, err := u.repository.ProviderRollup(ctx, u.now().Add(-time.Hour))
	if err != nil {
		return ProviderStatus{}, err
	}
	return ProviderStatus{Providers: rollup, Generating: generating, GPU: fleet, RecentThroughput: throughput}, nil
}

func (u *Usecase) TodayByGPU(ctx context.Context) (TodayByGPU, error) {
	if u == nil || u.repository == nil || u.fleet == nil {
		return TodayByGPU{}, ErrUnavailable
	}
	start := shanghaiStartOfDay(u.now())
	byNode, err := u.fleet.ImageTodayByGPU(ctx, start)
	if err != nil {
		return TodayByGPU{}, ErrUnavailable
	}
	external, err := u.repository.ExternalToday(ctx, start)
	if err != nil {
		return TodayByGPU{}, err
	}
	result := TodayByGPU{TodayStart: start, ByNode: byNode, External: external, Total: external}
	for _, node := range byNode {
		result.Total += node.Count
	}
	return result, nil
}

func (u *Usecase) Mutate(ctx context.Context, id string, mutation Mutation) error {
	if u == nil || u.repository == nil {
		return ErrUnavailable
	}
	if strings.TrimSpace(id) == "" || (mutation != MutationHide && mutation != MutationRestore && mutation != MutationDelete) {
		return ErrInvalidQuery
	}
	return u.repository.Mutate(ctx, id, mutation)
}

func (u *Usecase) BatchHide(ctx context.Context, ids []string) error {
	if u == nil || u.repository == nil {
		return ErrUnavailable
	}
	if len(ids) == 0 {
		return ErrInvalidQuery
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return ErrInvalidQuery
		}
	}
	return u.repository.BatchHide(ctx, ids)
}

func normalizeListQuery(query ListQuery, now time.Time) ListQuery {
	query.Status = defaultString(query.Status, "all")
	query.Source = defaultString(query.Source, "all")
	query.Template = defaultString(query.Template, "all")
	query.ImagePool = defaultString(query.ImagePool, "all")
	query.ClientSource = defaultString(query.ClientSource, "all")
	multiImage := query.MultiImage
	query.MultiImage = "all"
	if strings.EqualFold(strings.TrimSpace(multiImage), "true") {
		query.MultiImage = "true"
	}
	query.DateRange = defaultString(query.DateRange, "all")
	if query.Page < 1 {
		query.Page = defaultPage
	}
	if query.Limit < 1 {
		query.Limit = defaultLimit
	}
	if query.Limit > maxLimit {
		query.Limit = maxLimit
	}
	if query.DateRange == "today" {
		query.TodayStart = shanghaiStartOfDay(now)
	} else {
		query.TodayStart = time.Time{}
	}
	if query.Page > math.MaxInt/query.Limit+1 {
		query.Skip, query.FetchLimit = math.MaxInt, maxFetch
		return query
	}
	query.Skip = (query.Page - 1) * query.Limit
	query.FetchLimit = query.Skip + query.Limit
	if query.FetchLimit > maxFetch {
		query.FetchLimit = maxFetch
	}
	return query
}

func defaultString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func shanghaiStartOfDay(now time.Time) time.Time {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		location = time.FixedZone("CST", 8*60*60)
	}
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).UTC()
}
