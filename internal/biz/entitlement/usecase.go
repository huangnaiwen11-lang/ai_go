package entitlement

import (
	"strings"
	"time"

	"ai-business-service/internal/biz/shared"
)

const (
	imagePriceDiamonds int64 = 20

	videoFiveSecondsPriceDiamonds    int64 = 50
	videoTenSecondsPriceDiamonds     int64 = 100
	videoFifteenSecondsPriceDiamonds int64 = 150

	vipDailyImageLimit   int32 = 10
	vipDailyVideoLimit   int32 = 3
	vipDailyDiamonds     int64 = 50
	firstMonthMultiplier int32 = 2
	firstMonthDuration         = 30 * 24 * time.Hour
)

// Usecase 提供可注入的权益策略入口。
// 核心计算也暴露为带时间参数的函数，便于测试和后续事务在同一时刻使用一致的策略快照。
type Usecase struct {
	clock func() time.Time
}

// NewUsecase 创建权益策略用例。
func NewUsecase() *Usecase {
	return &Usecase{
		clock: func() time.Time {
			return time.Now().UTC()
		},
	}
}

// EvaluateGeneration 用当前时间执行生成门禁与固定计价。
func (usecase *Usecase) EvaluateGeneration(user UserSnapshot, request GenerationRequest) (*GenerationDecision, error) {
	return usecase.EvaluateGenerationAt(user, request, usecase.clock())
}

// ResolveDailyBenefits 用当前时间计算用户的每日权益。
func (usecase *Usecase) ResolveDailyBenefits(user UserSnapshot) (*DailyBenefits, error) {
	return usecase.ResolveDailyBenefitsAt(user, usecase.clock())
}

// EvaluateGenerationAt 使用调用方固定的业务时刻执行生成门禁与固定计价。
// 创作事务重试必须重用同一个时刻，不能在每次回调内重新读取系统时间。
func (usecase *Usecase) EvaluateGenerationAt(user UserSnapshot, request GenerationRequest, now time.Time) (*GenerationDecision, error) {
	return EvaluateGenerationAt(user, request, now)
}

// ResolveDailyBenefitsAt 使用调用方固定的业务时刻计算本地日期和每日权益。
// 该入口保留无时间方法的既有 API，同时允许事务编排保持重试语义稳定。
func (usecase *Usecase) ResolveDailyBenefitsAt(user UserSnapshot, now time.Time) (*DailyBenefits, error) {
	return ResolveDailyBenefitsAt(user, now)
}

// EvaluateGenerationAt 依次执行绑定门禁、产品校验、免费用户能力门禁和固定计价。
// 它刻意不读取余额，也不占用日免或扣钻；这些动作必须留给后续同一 MongoDB 事务。
func EvaluateGenerationAt(user UserSnapshot, request GenerationRequest, now time.Time) (*GenerationDecision, error) {
	if !user.AccountBound {
		return nil, shared.ErrAccountBindingRequired
	}

	switch request.Output {
	case ProductOutputImage:
		return &GenerationDecision{PriceDiamonds: imagePriceDiamonds}, nil
	case ProductOutputVideo:
		price, err := resolveVideoPrice(request.Video.DurationSeconds)
		if err != nil {
			return nil, err
		}
		if !isVIPAt(user.Subscription, now) && requiresVIPVideoCapability(request.Video) {
			return nil, shared.ErrVIPRequired
		}
		return &GenerationDecision{PriceDiamonds: price}, nil
	default:
		return nil, ErrUnsupportedProductOutput
	}
}

// ResolveDailyBenefitsAt 使用已固化的用户时区生成稳定的本地日期与每日权益上限。
// 对非 VIP 用户返回 0 额度，后续结算流程仍可根据固定价格走自有钻石预扣。
func ResolveDailyBenefitsAt(user UserSnapshot, now time.Time) (*DailyBenefits, error) {
	location, err := loadStoredTimezone(user.Timezone)
	if err != nil {
		return nil, ErrInvalidTimezone
	}

	benefits := &DailyBenefits{
		LocalDate: now.In(location).Format("2006-01-02"),
	}
	if !isVIPAt(user.Subscription, now) {
		return benefits, nil
	}

	benefits.ImageLimit = vipDailyImageLimit
	benefits.VideoLimit = vipDailyVideoLimit
	benefits.DailyDiamonds = vipDailyDiamonds
	benefits.FirstMonthDoubled = isYearlyFirstMonth(user.Subscription, now)
	if benefits.FirstMonthDoubled {
		benefits.ImageLimit *= firstMonthMultiplier
		benefits.VideoLimit *= firstMonthMultiplier
	}
	return benefits, nil
}

// loadStoredTimezone 只接受显式存储的 IANA 时区或 UTC。
// Go 会把空字符串解释为 UTC、把 Local 解释为部署机器时区；二者都不是用户首次固化的时区，
// 若静默接受会让日额度的切日随部署环境或缺失数据漂移。
func loadStoredTimezone(timezone string) (*time.Location, error) {
	if strings.TrimSpace(timezone) == "" || timezone == "Local" {
		return nil, ErrInvalidTimezone
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, ErrInvalidTimezone
	}
	return location, nil
}

func resolveVideoPrice(durationSeconds int32) (int64, error) {
	switch durationSeconds {
	case 5:
		return videoFiveSecondsPriceDiamonds, nil
	case 10:
		return videoTenSecondsPriceDiamonds, nil
	case 15:
		return videoFifteenSecondsPriceDiamonds, nil
	default:
		return 0, ErrUnsupportedVideoDuration
	}
}

func requiresVIPVideoCapability(video VideoOptions) bool {
	return video.DurationSeconds > 5 || video.EnableAudio || video.ReferenceImageCount > 1
}

func isVIPAt(subscription *SubscriptionSnapshot, now time.Time) bool {
	return subscription != nil && subscription.Status == SubscriptionStatusActive && subscription.ExpiresAt.After(now)
}

// isYearlyFirstMonth 与现网配额策略保持一致：只有有效年付订阅的首 30 天享受日免双倍。
// 现网优先使用订阅开始时间；历史记录缺失开始时间时回退创建时间。
func isYearlyFirstMonth(subscription *SubscriptionSnapshot, now time.Time) bool {
	if !isVIPAt(subscription, now) || subscription.BillingPeriod != SubscriptionBillingPeriodYearly {
		return false
	}
	startAt := subscription.StartsAt
	if startAt.IsZero() {
		startAt = subscription.CreatedAt
	}
	if startAt.IsZero() {
		return false
	}
	durationSinceStart := now.Sub(startAt)
	return durationSinceStart >= 0 && durationSinceStart <= firstMonthDuration
}
