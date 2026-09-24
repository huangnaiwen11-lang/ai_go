// Package adminview 定义管理后台只读总览的领域投影。
package adminview

import (
	"strings"
	"time"
)

// 后台日期与 ai-admin 的 ADMIN_TIMEZONE 一致；不依赖宿主机器时区。
var Location = time.FixedZone("Asia/Shanghai", 8*60*60)

type Trend struct {
	Date         string
	Count, Cents int64
}
type Window struct{ From, Until time.Time }
type RevenueBucket struct {
	Key, Label     string
	Cents, D0Cents int64
}
type RevenueBreakdown struct{ Country, Source, Client []RevenueBucket }

func DayStart(now time.Time) time.Time {
	n := now.In(Location)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, Location)
}

// Overview 只包含 Go 自有 Mongo 可证明的运营事实；不混入 GA4、广告或旧 Node 数据。
type Overview struct {
	TotalUsers, TodayUsers, WeeklyUsers, MonthlyUsers int64
	TotalImages, TodayImages, PendingImages           int64
	TotalVideos, TodayVideos, PendingVideos           int64
	TotalRevenueCents, TodayRevenueCents              int64
}

// SubscriptionGrant 是后台手工发放/续期订阅权益的输入。
type SubscriptionGrant struct {
	UserID string
	Tier   string
	Days   int
	Reason string
}

// SubscriptionGrantResult 描述发放结果。
// Action 为 created（新建）或 extended（在既有有效期内顺延）。
type SubscriptionGrantResult struct {
	Action   string
	Tier     string
	EndDate  time.Time
	UserName string
}

// 订阅发放的天数上限与默认值，与后台表单一致。
const (
	SubscriptionGrantDefaultDays = 30
	SubscriptionGrantMaxDays     = 365
	// SubscriptionGrantTier 是唯一可发放的层级。
	// Go 的权益模型（subscriptions 集合）只有单一订阅层级，没有 vip/svip 之分；
	// 前端类型也只允许 'vip'。显式拒绝其他取值，避免把 svip 静默当成 vip 发放。
	SubscriptionGrantTier = "vip"
)

// NormalizeSubscriptionGrant 校验并补全发放输入。
// days 允许缺省（按默认值），但显式传入越界值必须报错而不是静默夹取 ——
// 静默夹取会让运营以为发放了 400 天，实际只发了 365 天。
func NormalizeSubscriptionGrant(grant SubscriptionGrant) (SubscriptionGrant, error) {
	grant.UserID = strings.TrimSpace(grant.UserID)
	grant.Tier = strings.ToLower(strings.TrimSpace(grant.Tier))
	grant.Reason = strings.TrimSpace(grant.Reason)
	if grant.UserID == "" {
		return SubscriptionGrant{}, ErrInvalid
	}
	if grant.Tier == "" {
		grant.Tier = SubscriptionGrantTier
	}
	if grant.Tier != SubscriptionGrantTier {
		return SubscriptionGrant{}, ErrInvalid
	}
	if grant.Days == 0 {
		grant.Days = SubscriptionGrantDefaultDays
	}
	if grant.Days < 1 || grant.Days > SubscriptionGrantMaxDays {
		return SubscriptionGrant{}, ErrInvalid
	}
	if len([]rune(grant.Reason)) > 500 {
		return SubscriptionGrant{}, ErrInvalid
	}
	return grant, nil
}
