// Package adminreview defines the Go-owned moderation projection contract.
//
// The projection is deliberately separate from CreationDocument: generation
// lifecycle state is not a content-review decision.  Until the projection has
// been populated by a trusted importer/new-generation writer, readers must
// fail closed instead of returning a fabricated empty queue.
package adminreview

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	MediaTypeImage = "image"
	MediaTypeVideo = "video"

	ReviewStatusPending  = "pending"
	ReviewStatusApproved = "approved"
	ReviewStatusRejected = "rejected"
	ReviewStatusDeleted  = "deleted"

	GenerationStatusGenerating = "generating"
	GenerationStatusSucceeded  = "succeeded"
	GenerationStatusFailed     = "failed"
	GenerationStatusCancelled  = "cancelled"
)

var (
	// ErrProjectionUnavailable means the read model has not been populated or
	// its readiness marker cannot be trusted.  HTTP callers map this to 503.
	ErrProjectionUnavailable = errors.New("admin review projection unavailable")
	ErrInvalidQuery          = errors.New("admin review invalid query")
)

type ReviewItem struct {
	ID               string
	MediaType        string
	Source           string
	LegacySourceID   string
	CreationID       string
	UserID           string
	AssetID          string
	OutputRef        string
	ReviewStatus     string
	VisibilityStatus string
	GenerationStatus string
	Prompt           string
	NegativePrompt   string
	TemplateID       string
	TemplateTitle    string
	ReviewedBy       string
	ReviewedAt       time.Time
	RejectReason     string
	Version          int64
	CreatedAt        time.Time
	OutputAt         time.Time
	UpdatedAt        time.Time
}

type ListQuery struct {
	MediaType string
	Status    string
	Source    string
	Template  string
	DateRange string
	SortBy    string
	VideoPool string
	Page      int
	Limit     int
}

type ListResult struct {
	Items      []ReviewItem
	Total      int64
	Page       int
	Limit      int
	TotalPages int
}

type StatsQuery struct{ MediaType string }

type StatusCounts struct {
	Pending  int64
	Approved int64
	Rejected int64
	Deleted  int64
	Total    int64
	Today    int64
}

// ImageTemplateOption is the filter value required by the frozen admin ImagesPage.
type ImageTemplateOption struct {
	Value string
	Count int64
}

// ImageTemplateOptions is the image-only template filter projection.
type ImageTemplateOptions struct {
	Options         []ImageTemplateOption
	WithTemplate    int64
	WithoutTemplate int64
}

// ImageRuntimeQuery scopes the small set of image runtime facts retained by
// the Go-owned moderation projection.
type ImageRuntimeQuery struct {
	GeneratingSince time.Time
	TodayStart      time.Time
}

// ImageRuntime contains only facts the Go projection can establish. GPU-node
// attribution deliberately stays out because the projection does not own it.
type ImageRuntime struct {
	Generating    int64
	ExternalToday int64
}

// VideoTemplateOption is the filter value required by the frozen admin
// VideosPage. Counts only cover completed items represented in the review
// projection; FaceSwap names are not homepage templates and are excluded.
type VideoTemplateOption struct {
	Value string
	Count int64
}

type VideoTemplateOptions struct {
	Options         []VideoTemplateOption
	WithTemplate    int64
	WithoutTemplate int64
}

type VideoRuntimeQuery struct {
	GeneratingSince time.Time
	TodayStart      time.Time
}

// VideoRuntime contains the video facts retained by the review projection.
// Physical GPU attribution deliberately remains absent: it belongs to the
// generation platform, which the Gateway does not query here.
type VideoRuntime struct {
	Generating    int64
	ExternalToday int64
}

type Repository interface {
	ListReviewItems(context.Context, ListQuery) (ListResult, error)
	CountReviewStatuses(context.Context, StatsQuery) (StatusCounts, error)
}

// ImageOverviewRepository is optional so the existing review queue contract
// remains usable by smaller projections and tests.
type ImageOverviewRepository interface {
	ImageTemplateOptions(context.Context) (ImageTemplateOptions, error)
	ImageRuntime(context.Context, ImageRuntimeQuery) (ImageRuntime, error)
}

type VideoOverviewRepository interface {
	VideoTemplateOptions(context.Context) (VideoTemplateOptions, error)
	VideoRuntime(context.Context, VideoRuntimeQuery) (VideoRuntime, error)
}

// ReviewAction is the write-side moderation operation.  It is deliberately
// separate from generation lifecycle state: an operator can edit a prompt or
// hide an already completed output without changing generationStatus.
type ReviewAction string

const (
	ReviewActionApprove ReviewAction = "approve"
	ReviewActionReject  ReviewAction = "reject"
	ReviewActionPrompt  ReviewAction = "prompt"
	ReviewActionDelete  ReviewAction = "delete"
)

var (
	ErrReviewConflict       = errors.New("admin review compare-and-swap conflict")
	ErrReviewNotFound       = errors.New("admin review item not found")
	ErrReviewInvalidCommand = errors.New("admin review invalid command")
	ErrReviewIdempotency    = errors.New("admin review idempotency conflict")
)

// ReviewCommand is the canonical single-item write contract.  ExpectedVersion
// is required for a strict CAS; -1 is accepted only for legacy admin clients
// that do not send a version, in which case the repository snapshots the item
// and still applies the final update with a version predicate in one tx.
type ReviewCommand struct {
	ItemID          string
	MediaType       string
	Action          ReviewAction
	ActorID         string
	ExpectedVersion int64
	IdempotencyKey  string
	Reason          string
	Prompt          *string
	NegativePrompt  *string
}

// ReviewMutation is the storage-facing CAS mutation used by the contract
// tests and by non-Mongo repositories.
type ReviewMutation struct {
	ItemID          string
	MediaType       string
	Action          ReviewAction
	ActorID         string
	ExpectedVersion int64
	Reason          string
	Prompt          *string
	NegativePrompt  *string
	At              time.Time
}

type ReviewAudit struct {
	IdempotencyKey  string
	ItemID          string
	MediaType       string
	Action          ReviewAction
	ActorID         string
	ExpectedVersion int64
	Version         int64
	Reason          string
	At              time.Time
	ItemIDs         []string
}

// ReviewWriteRepository is the narrow fallback contract used by unit tests
// and alternative stores.  Mongo uses AtomicReviewWriter below so the CAS,
// audit row and outbox event share one transaction.
type ReviewWriteRepository interface {
	FindReviewItem(context.Context, string) (ReviewItem, error)
	CompareAndSwapReview(context.Context, ReviewMutation) (ReviewItem, bool, error)
	WriteReviewAudit(context.Context, ReviewAudit) error
	FindReviewAudit(context.Context, string) (ReviewAudit, bool, error)
}

type ReviewResult struct {
	Item          ReviewItem
	Message       string
	ModifiedCount int64
}

type BatchReviewCommand struct {
	ItemIDs         []string
	MediaType       string
	Action          ReviewAction
	ActorID         string
	ExpectedVersion map[string]int64
	IdempotencyKey  string
	Reason          string
}

type BatchReviewResult struct {
	Items         []ReviewItem
	Message       string
	ModifiedCount int64
}

// AtomicReviewWriter is implemented by the Mongo projection repository.  The
// method owns the transaction boundary so no caller can accidentally commit a
// review status without its audit and outbox facts.
type AtomicReviewWriter interface {
	ApplyReview(context.Context, ReviewCommand) (ReviewResult, error)
	ApplyBatchReview(context.Context, BatchReviewCommand) (BatchReviewResult, error)
}

type Usecase struct{ repository any }

func NewUsecase(repository any) *Usecase { return &Usecase{repository: repository} }

func (u *Usecase) Review(ctx context.Context, command ReviewCommand) (ReviewResult, error) {
	if err := validateReviewCommand(command); err != nil {
		return ReviewResult{}, err
	}
	if u == nil || u.repository == nil {
		return ReviewResult{}, ErrProjectionUnavailable
	}
	if writer, ok := u.repository.(AtomicReviewWriter); ok {
		return writer.ApplyReview(ctx, command)
	}
	writer, ok := u.repository.(ReviewWriteRepository)
	if !ok {
		return ReviewResult{}, ErrProjectionUnavailable
	}
	if audit, found, err := writer.FindReviewAudit(ctx, command.IdempotencyKey); err != nil {
		return ReviewResult{}, err
	} else if found {
		return ReviewResult{Message: reviewMessage(command.Action), Item: ReviewItem{ID: audit.ItemID, Version: audit.Version}}, nil
	}
	item, err := writer.FindReviewItem(ctx, command.ItemID)
	if err != nil {
		return ReviewResult{}, err
	}
	expected := command.ExpectedVersion
	if expected < 0 {
		expected = item.Version
	}
	updated, ok, err := writer.CompareAndSwapReview(ctx, ReviewMutation{ItemID: command.ItemID, MediaType: command.MediaType, Action: command.Action, ActorID: command.ActorID, ExpectedVersion: expected, Reason: command.Reason, Prompt: command.Prompt, NegativePrompt: command.NegativePrompt, At: time.Now().UTC()})
	if err != nil {
		return ReviewResult{}, err
	}
	if !ok {
		return ReviewResult{}, ErrReviewConflict
	}
	if err := writer.WriteReviewAudit(ctx, ReviewAudit{IdempotencyKey: command.IdempotencyKey, ItemID: command.ItemID, MediaType: command.MediaType, Action: command.Action, ActorID: command.ActorID, ExpectedVersion: expected, Version: updated.Version, Reason: command.Reason, At: time.Now().UTC()}); err != nil {
		return ReviewResult{}, err
	}
	return ReviewResult{Item: updated, Message: reviewMessage(command.Action)}, nil
}

func (u *Usecase) BatchReview(ctx context.Context, command BatchReviewCommand) (BatchReviewResult, error) {
	if err := validateBatchReviewCommand(command); err != nil {
		return BatchReviewResult{}, err
	}
	if u == nil || u.repository == nil {
		return BatchReviewResult{}, ErrProjectionUnavailable
	}
	if writer, ok := u.repository.(AtomicReviewWriter); ok {
		return writer.ApplyBatchReview(ctx, command)
	}
	// A fallback store cannot guarantee all items and their audit/outbox facts
	// commit together, so fail closed rather than pretending batch atomicity.
	return BatchReviewResult{}, ErrProjectionUnavailable
}

func validateReviewCommand(command ReviewCommand) error {
	if strings.TrimSpace(command.ItemID) == "" || strings.TrimSpace(command.ActorID) == "" || strings.TrimSpace(command.IdempotencyKey) == "" {
		return ErrReviewInvalidCommand
	}
	if command.ExpectedVersion < -1 {
		return ErrReviewInvalidCommand
	}
	if command.MediaType != "" && command.MediaType != MediaTypeImage && command.MediaType != MediaTypeVideo {
		return ErrReviewInvalidCommand
	}
	switch command.Action {
	case ReviewActionApprove, ReviewActionReject, ReviewActionPrompt, ReviewActionDelete:
	default:
		return ErrReviewInvalidCommand
	}
	if command.Action == ReviewActionReject && len(command.Reason) > 500 {
		return ErrReviewInvalidCommand
	}
	if command.Action == ReviewActionPrompt && command.Prompt == nil && command.NegativePrompt == nil {
		return ErrReviewInvalidCommand
	}
	if command.Prompt != nil && len([]rune(*command.Prompt)) > 2000 {
		return ErrReviewInvalidCommand
	}
	if command.NegativePrompt != nil && len([]rune(*command.NegativePrompt)) > 5000 {
		return ErrReviewInvalidCommand
	}
	return nil
}

func validateBatchReviewCommand(command BatchReviewCommand) error {
	if len(command.ItemIDs) == 0 || len(command.ItemIDs) > 500 || strings.TrimSpace(command.ActorID) == "" || strings.TrimSpace(command.IdempotencyKey) == "" {
		return ErrReviewInvalidCommand
	}
	if command.MediaType != "" && command.MediaType != MediaTypeImage && command.MediaType != MediaTypeVideo {
		return ErrReviewInvalidCommand
	}
	if command.Action != ReviewActionApprove && command.Action != ReviewActionReject {
		return ErrReviewInvalidCommand
	}
	if command.Action == ReviewActionReject && len(command.Reason) > 500 {
		return ErrReviewInvalidCommand
	}
	seen := make(map[string]struct{}, len(command.ItemIDs))
	for _, itemID := range command.ItemIDs {
		if strings.TrimSpace(itemID) == "" {
			return ErrReviewInvalidCommand
		}
		if _, ok := seen[itemID]; ok {
			return ErrReviewInvalidCommand
		}
		seen[itemID] = struct{}{}
	}
	return nil
}

func reviewMessage(action ReviewAction) string {
	switch action {
	case ReviewActionApprove:
		return "Review approved"
	case ReviewActionReject:
		return "Review rejected"
	case ReviewActionPrompt:
		return "Prompt updated successfully"
	case ReviewActionDelete:
		return "Review item deleted"
	default:
		return "Review updated"
	}
}

func (u *Usecase) List(ctx context.Context, query ListQuery) (ListResult, error) {
	if u == nil || u.repository == nil {
		return ListResult{}, ErrProjectionUnavailable
	}
	normalized, err := normalizeListQuery(query)
	if err != nil {
		return ListResult{}, err
	}
	if repository, ok := u.repository.(Repository); ok {
		return repository.ListReviewItems(ctx, normalized)
	}
	// Compatibility with the original red contract while stores migrate to the
	// richer paged ListResult shape.
	if repository, ok := u.repository.(interface {
		ListReviewItems(context.Context, ListQuery) ([]ReviewItem, int64, error)
	}); ok {
		items, total, err := repository.ListReviewItems(ctx, normalized)
		if err != nil {
			return ListResult{}, err
		}
		if total == 0 {
			total = int64(len(items))
		}
		filtered := items[:0]
		for _, item := range items {
			if item.MediaType != normalized.MediaType {
				continue
			}
			if normalized.Status != "all" && item.ReviewStatus != normalized.Status {
				continue
			}
			filtered = append(filtered, item)
		}
		items = filtered
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].ID > items[j].ID
			}
			return items[i].CreatedAt.After(items[j].CreatedAt)
		})
		start := (normalized.Page - 1) * normalized.Limit
		if start > len(items) {
			start = len(items)
		}
		end := start + normalized.Limit
		if end > len(items) {
			end = len(items)
		}
		items = items[start:end]
		total = int64(len(filtered))
		pages := int(math.Ceil(float64(total) / float64(normalized.Limit)))
		return ListResult{Items: items, Total: total, Page: normalized.Page, Limit: normalized.Limit, TotalPages: pages}, nil
	}
	return ListResult{}, ErrProjectionUnavailable
}

func (u *Usecase) Stats(ctx context.Context, query StatsQuery) (StatusCounts, error) {
	if u == nil || u.repository == nil {
		return StatusCounts{}, ErrProjectionUnavailable
	}
	if query.MediaType != "" && query.MediaType != MediaTypeImage && query.MediaType != MediaTypeVideo {
		return StatusCounts{}, ErrInvalidQuery
	}
	if repository, ok := u.repository.(interface {
		CountReviewStatuses(context.Context, StatsQuery) (StatusCounts, error)
	}); ok {
		return repository.CountReviewStatuses(ctx, query)
	}
	return StatusCounts{}, ErrProjectionUnavailable
}

func (u *Usecase) ImageTemplateOptions(ctx context.Context) (ImageTemplateOptions, error) {
	if u == nil || u.repository == nil {
		return ImageTemplateOptions{}, ErrProjectionUnavailable
	}
	repository, ok := u.repository.(ImageOverviewRepository)
	if !ok {
		return ImageTemplateOptions{}, ErrProjectionUnavailable
	}
	return repository.ImageTemplateOptions(ctx)
}

func (u *Usecase) ImageRuntime(ctx context.Context, query ImageRuntimeQuery) (ImageRuntime, error) {
	if u == nil || u.repository == nil || query.GeneratingSince.IsZero() || query.TodayStart.IsZero() {
		return ImageRuntime{}, ErrProjectionUnavailable
	}
	repository, ok := u.repository.(ImageOverviewRepository)
	if !ok {
		return ImageRuntime{}, ErrProjectionUnavailable
	}
	return repository.ImageRuntime(ctx, query)
}

func (u *Usecase) VideoTemplateOptions(ctx context.Context) (VideoTemplateOptions, error) {
	if u == nil || u.repository == nil {
		return VideoTemplateOptions{}, ErrProjectionUnavailable
	}
	repository, ok := u.repository.(VideoOverviewRepository)
	if !ok {
		return VideoTemplateOptions{}, ErrProjectionUnavailable
	}
	return repository.VideoTemplateOptions(ctx)
}

func (u *Usecase) VideoRuntime(ctx context.Context, query VideoRuntimeQuery) (VideoRuntime, error) {
	if u == nil || u.repository == nil || query.GeneratingSince.IsZero() || query.TodayStart.IsZero() {
		return VideoRuntime{}, ErrProjectionUnavailable
	}
	repository, ok := u.repository.(VideoOverviewRepository)
	if !ok {
		return VideoRuntime{}, ErrProjectionUnavailable
	}
	return repository.VideoRuntime(ctx, query)
}

func normalizeListQuery(query ListQuery) (ListQuery, error) {
	if query.MediaType == "" {
		// The initial projection contract omitted a media selector and described
		// the pending image queue.  Keep that call shape source-compatible;
		// transport handlers always pass an explicit media type.
		query.MediaType = MediaTypeImage
	}
	if query.MediaType != MediaTypeImage && query.MediaType != MediaTypeVideo {
		return ListQuery{}, ErrInvalidQuery
	}
	if query.Status == "" {
		query.Status = ReviewStatusPending
	}
	switch query.Status {
	case ReviewStatusPending, ReviewStatusApproved, ReviewStatusRejected, ReviewStatusDeleted, "all":
	default:
		return ListQuery{}, ErrInvalidQuery
	}
	if query.Source == "" {
		query.Source = "all"
	}
	switch query.Source {
	case "all", "generated", "animate", "faceswap":
	default:
		return ListQuery{}, ErrInvalidQuery
	}
	if query.Template == "" {
		query.Template = "all"
	}
	if query.DateRange == "" {
		query.DateRange = "all"
	}
	if query.DateRange != "all" && query.DateRange != "today" {
		return ListQuery{}, ErrInvalidQuery
	}
	if query.SortBy == "" {
		query.SortBy = "createdAt"
	}
	if query.SortBy != "createdAt" && query.SortBy != "outputAt" {
		return ListQuery{}, ErrInvalidQuery
	}
	if query.VideoPool == "" {
		query.VideoPool = "all"
	}
	if query.VideoPool != "all" && query.VideoPool != "external" && query.VideoPool != "unknown" {
		return ListQuery{}, ErrInvalidQuery
	}
	if query.Page == 0 {
		query.Page = 1
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Page < 1 || query.Limit < 1 || query.Limit > 100 {
		return ListQuery{}, ErrInvalidQuery
	}
	return query, nil
}
