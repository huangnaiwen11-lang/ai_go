package generation

import (
	"context"
	"errors"
	"strings"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/outbox"
)

var (
	// ErrInvalidProviderJobBinding 表示供应商任务绑定缺少稳定身份或与冻结路由不一致。
	ErrInvalidProviderJobBinding = errors.New("generation: invalid provider job binding")
	// ErrProviderJobBindingConflict 表示租约、步骤或任务身份的 CAS 未命中。
	// It aliases ErrSubmissionConflict so existing worker conflict recovery keeps
	// treating stale B2B leases as a normal durable-state race.
	ErrProviderJobBindingConflict = ErrSubmissionConflict
)

// ProviderJobBinding 是 PolarStar Submit/Lookup 成功后的最小绑定事实。
// Route 与 Capability 必须和创建步骤中冻结的值完全一致；LeaseToken、Fence
// 用于阻止过期 worker 覆盖新租约下已经绑定的任务。
type ProviderJobBinding struct {
	EventID    string
	CreationID string
	StepID     string
	LeaseToken string
	LeaseOwner string
	Fence      int32
	Route      creations.ExecutionRoute
	ExternalID string
	Capability string
	At         time.Time
}

// ProviderJobBinder 在外层 Mongo transaction 中执行绑定和 recovery 唤醒。
type ProviderJobBinder interface {
	BindProviderJob(context.Context, ProviderJobBinding) error
}

// Normalize 校验并标准化供应商任务绑定命令。
func (binding ProviderJobBinding) Normalize() (ProviderJobBinding, error) {
	route, err := creations.NormalizeExecutionRoute(binding.Route)
	if err != nil || route.Provider != creations.PolarStarB2BProvider {
		return ProviderJobBinding{}, ErrInvalidProviderJobBinding
	}
	if !providerJobIdentifier(binding.EventID) || !providerJobIdentifier(binding.CreationID) ||
		!providerJobIdentifier(binding.StepID) || !providerJobIdentifier(binding.LeaseToken) ||
		!providerJobIdentifier(binding.LeaseOwner) || binding.Fence <= 0 ||
		!providerJobIdentifier(binding.ExternalID) || binding.At.IsZero() || !validProviderCapability(binding.Capability) {
		return ProviderJobBinding{}, ErrInvalidProviderJobBinding
	}
	if outbox.SubmissionEventID(binding.StepID) != binding.EventID || route.AccountRef == creations.DefaultLocalAccount {
		return ProviderJobBinding{}, ErrInvalidProviderJobBinding
	}
	binding.Route = route
	binding.Capability = strings.TrimSpace(binding.Capability)
	binding.At = binding.At.UTC()
	return binding, nil
}

func (binding ProviderJobBinding) Validate() error {
	_, err := binding.Normalize()
	return err
}

func providerJobIdentifier(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && len(value) <= maxSubmissionIdentifierLength
}

func validProviderCapability(value string) bool {
	switch value {
	case string(creations.AtomTextToImage), string(creations.AtomImageEdit), string(creations.AtomImageToVideo):
		return true
	default:
		return false
	}
}
