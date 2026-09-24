// Package adminimage contains the legacy image-admin contract that still reads
// the explicitly configured staging MongoDB collections.
package adminimage

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidQuery = errors.New("invalid admin image query")
	ErrNotFound     = errors.New("admin image not found")
	ErrUnavailable  = errors.New("admin image dependency unavailable")
)

type ListQuery struct {
	Status, Source, Template, ImagePool, ClientSource string
	MultiImage, DateRange                             string
	Page, Limit, Skip, FetchLimit                     int
	TodayStart                                        time.Time
}

type Creator struct {
	ID, Email, DisplayName, AvatarURL string
}

type Image struct {
	ID, Prompt, ImageURL, Status, Source, APIProvider string
	ImagePool                                         string
	Style, ComfyNode, ImageProfile, TemplateTitle     *string
	IsPublic                                          bool
	Likes, Views, AdditionalImageCount                int64
	GenerationMS                                      *int64
	ClientSource                                      any
	Creator                                           *Creator
	CreatedAt                                         time.Time
	UserID                                            string `json:"-"`
}

type ListResult struct {
	Images                         []Image
	Page, Limit, Total, TotalPages int
}

type Stats struct {
	Active, Hidden, Deleted, Total, Today int64
}

type TemplateOption struct {
	Value string
	Count int64
}

type TemplateOptions struct {
	Options                       []TemplateOption
	WithTemplate, WithoutTemplate int64
}

type ProviderRollup struct {
	Total1H, Completed1H, Failed1H int64
	AvgDurationSec                 *int64
}

type GPUNode struct {
	ID, Label, URL, Role, ImagePool string
	ImageProfile                    *string
	Pools                           []string
	OK                              bool
	GPU                             *string
	VRAMFree, VRAMTotal             *int64
	QueueDepth, MaxQueue            *int64
	CircuitState                    *string
	CircuitCanRequest               *bool
	CircuitFailures                 *int64
	CircuitLastReason               *string
	Schedulable                     bool
}

type GPUStatus struct {
	Nodes                            []GPUNode
	ReportedImageNodes               int64
	FleetSnapshotOK                  bool
	TotalQueueDepth, OnGPUProcessing *int64
	CanAcceptJob                     *bool
}

type RecentThroughput struct {
	Window                            string
	ImagePerHour, VideoPerHour        int64
	CompletedInWindow, FailedInWindow *int64
	FailureRate                       *float64
}

type ProviderStatus struct {
	Providers        map[string]ProviderRollup
	Generating       int64
	GPU              GPUStatus
	RecentThroughput *RecentThroughput
}

type NodeOutput struct {
	NodeURL, Origin, ImagePool string
	Count                      int64
	AvgMS                      *int64
	ImageProfile               *string
	Quality                    any
}

type TodayByGPU struct {
	TodayStart time.Time
	ByNode     []NodeOutput
	External   int64
	Total      int64
}

type Mutation string

const (
	MutationHide    Mutation = "hide"
	MutationRestore Mutation = "restore"
	MutationDelete  Mutation = "delete"
)

type Repository interface {
	List(context.Context, ListQuery) (ListResult, error)
	Stats(context.Context, time.Time) (Stats, error)
	TemplateOptions(context.Context) (TemplateOptions, error)
	ProviderRollup(context.Context, time.Time) (map[string]ProviderRollup, int64, error)
	ExternalToday(context.Context, time.Time) (int64, error)
	Mutate(context.Context, string, Mutation) error
	BatchHide(context.Context, []string) error
}

// Fleet is the read-only generation-fleet source. The local generation
// simulator intentionally does not implement this contract, so callers must
// leave it nil until a real source is available; the endpoints then return 503.
type Fleet interface {
	ImageProviderStatus(context.Context) (GPUStatus, *RecentThroughput, error)
	ImageTodayByGPU(context.Context, time.Time) ([]NodeOutput, error)
}
