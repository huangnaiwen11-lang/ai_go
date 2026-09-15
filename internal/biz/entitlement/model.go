// Package entitlement 管理价格、VIP、每日额度和用户时区。
package entitlement

import "time"

// ProductOutput 表示用户面向模板选择后得到的产品输出。
// 它刻意不包含生成中台的原子名称，避免把内部实现泄露到业务边界。
type ProductOutput string

const (
	// ProductOutputImage 表示图片产品输出。
	ProductOutputImage ProductOutput = "image"
	// ProductOutputVideo 表示视频产品输出。
	ProductOutputVideo ProductOutput = "video"
)

// VideoOptions 是与用户可见视频能力相关的权益输入。
// 引用图数量只用于判断“多图”权限，不表达任何中台请求结构。
type VideoOptions struct {
	DurationSeconds     int32
	EnableAudio         bool
	ReferenceImageCount int32
}

// SubscriptionStatus 表示订阅在账本投影中的有效状态。
type SubscriptionStatus string

const (
	// SubscriptionStatusActive 表示订阅已生效。
	SubscriptionStatusActive SubscriptionStatus = "active"
)

// SubscriptionBillingPeriod 表示订阅的计费周期。
type SubscriptionBillingPeriod string

const (
	// SubscriptionBillingPeriodMonthly 表示月付 VIP。
	SubscriptionBillingPeriodMonthly SubscriptionBillingPeriod = "monthly"
	// SubscriptionBillingPeriodYearly 表示年付 VIP。
	SubscriptionBillingPeriodYearly SubscriptionBillingPeriod = "yearly"
)

// SubscriptionSnapshot 是上层从订阅投影读取后传入的不可变快照。
// 权益模块不查询钱包或支付服务，避免把跨模块读取隐藏在策略计算中。
type SubscriptionSnapshot struct {
	Status        SubscriptionStatus
	BillingPeriod SubscriptionBillingPeriod
	StartsAt      time.Time
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// UserSnapshot 是生成门禁和每日权益计算所需的最小用户上下文。
// Timezone 由身份模块首次写入，本模块只读取它来计算本地日界。
type UserSnapshot struct {
	AccountBound bool
	Timezone     string
	Subscription *SubscriptionSnapshot
}

// GenerationRequest 表示权益模块接收的产品能力请求。
type GenerationRequest struct {
	Output ProductOutput
	Video  VideoOptions
}

// GenerationDecision 是后续创作预留事务可使用的固定计价结果。
// 它不包含余额、日免是否已占用或任何扣款状态。
type GenerationDecision struct {
	PriceDiamonds int64
}

// DailyBenefits 是某用户在本地日期内可参与后续原子占用的权益上限。
// 该结果只是策略快照；真正的占用必须由 reservations、daily_quotas 与账本在同一事务完成。
type DailyBenefits struct {
	LocalDate         string
	ImageLimit        int32
	VideoLimit        int32
	DailyDiamonds     int64
	FirstMonthDoubled bool
}
