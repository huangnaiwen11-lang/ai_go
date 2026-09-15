package entitlement_test

import (
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/shared"
)

func TestEvaluateGenerationRequiresAccountBinding(t *testing.T) {
	_, err := entitlement.EvaluateGenerationAt(
		entitlement.UserSnapshot{Timezone: "Asia/Shanghai"},
		entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
		time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC),
	)
	if !errors.Is(err, shared.ErrAccountBindingRequired) {
		t.Fatalf("EvaluateGenerationAt() error = %v, want ErrAccountBindingRequired", err)
	}
}

func TestEvaluateGenerationRestrictsFreeVideoCapabilities(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	freeUser := entitlement.UserSnapshot{AccountBound: true, Timezone: "Asia/Shanghai"}

	cases := []struct {
		name  string
		video entitlement.VideoOptions
	}{
		{
			name:  "10 秒视频",
			video: entitlement.VideoOptions{DurationSeconds: 10},
		},
		{
			name:  "开启音频",
			video: entitlement.VideoOptions{DurationSeconds: 5, EnableAudio: true},
		},
		{
			name:  "多图输入",
			video: entitlement.VideoOptions{DurationSeconds: 5, ReferenceImageCount: 2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := entitlement.EvaluateGenerationAt(
				freeUser,
				entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo, Video: tc.video},
				now,
			)
			if !errors.Is(err, shared.ErrVIPRequired) {
				t.Fatalf("EvaluateGenerationAt() error = %v, want ErrVIPRequired", err)
			}
		})
	}
}

func TestEvaluateGenerationAllowsFreeStandardFiveSecondVideo(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	freeUser := entitlement.UserSnapshot{AccountBound: true, Timezone: "Asia/Shanghai"}

	for _, referenceImageCount := range []int32{0, 1} {
		decision, err := entitlement.EvaluateGenerationAt(freeUser, entitlement.GenerationRequest{
			Output: entitlement.ProductOutputVideo,
			Video: entitlement.VideoOptions{
				DurationSeconds:     5,
				ReferenceImageCount: referenceImageCount,
			},
		}, now)
		if err != nil {
			t.Fatalf("EvaluateGenerationAt(reference image count %d) error = %v", referenceImageCount, err)
		}
		if decision.PriceDiamonds != 50 {
			t.Fatalf("PriceDiamonds = %d, want 50", decision.PriceDiamonds)
		}
	}
}

func TestEvaluateGenerationReturnsFixedPrices(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	vipUser := activeVIPUser(now, entitlement.SubscriptionBillingPeriodMonthly)

	cases := []struct {
		name  string
		input entitlement.GenerationRequest
		want  int64
	}{
		{
			name:  "图片",
			input: entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
			want:  20,
		},
		{
			name:  "5 秒视频",
			input: entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo, Video: entitlement.VideoOptions{DurationSeconds: 5}},
			want:  50,
		},
		{
			name:  "10 秒视频",
			input: entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo, Video: entitlement.VideoOptions{DurationSeconds: 10}},
			want:  100,
		},
		{
			name:  "15 秒视频",
			input: entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo, Video: entitlement.VideoOptions{DurationSeconds: 15, EnableAudio: true, ReferenceImageCount: 2}},
			want:  150,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := entitlement.EvaluateGenerationAt(vipUser, tc.input, now)
			if err != nil {
				t.Fatalf("EvaluateGenerationAt() error = %v", err)
			}
			if decision.PriceDiamonds != tc.want {
				t.Fatalf("PriceDiamonds = %d, want %d", decision.PriceDiamonds, tc.want)
			}
		})
	}
}

func TestEvaluateGenerationRejectsUnsupportedVideoDuration(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	_, err := entitlement.EvaluateGenerationAt(
		activeVIPUser(now, entitlement.SubscriptionBillingPeriodMonthly),
		entitlement.GenerationRequest{
			Output: entitlement.ProductOutputVideo,
			Video:  entitlement.VideoOptions{DurationSeconds: 8},
		},
		now,
	)
	if !errors.Is(err, entitlement.ErrUnsupportedVideoDuration) {
		t.Fatalf("EvaluateGenerationAt() error = %v, want ErrUnsupportedVideoDuration", err)
	}
}

func TestResolveDailyBenefitsAppliesFirstMonthDoubleOnlyToYearlyVIP(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name                  string
		user                  entitlement.UserSnapshot
		wantImageLimit        int32
		wantVideoLimit        int32
		wantFirstMonthDoubled bool
	}{
		{
			name:                  "年付首 30 天",
			user:                  activeVIPUser(now, entitlement.SubscriptionBillingPeriodYearly),
			wantImageLimit:        20,
			wantVideoLimit:        6,
			wantFirstMonthDoubled: true,
		},
		{
			name: "月付首月不翻倍",
			user: entitlement.UserSnapshot{
				AccountBound: true,
				Timezone:     "Asia/Shanghai",
				Subscription: &entitlement.SubscriptionSnapshot{
					Status:        entitlement.SubscriptionStatusActive,
					BillingPeriod: entitlement.SubscriptionBillingPeriodMonthly,
					StartsAt:      now.Add(-24 * time.Hour),
					ExpiresAt:     now.Add(24 * time.Hour),
				},
			},
			wantImageLimit:        10,
			wantVideoLimit:        3,
			wantFirstMonthDoubled: false,
		},
		{
			name: "年付超过首 30 天不翻倍",
			user: entitlement.UserSnapshot{
				AccountBound: true,
				Timezone:     "Asia/Shanghai",
				Subscription: &entitlement.SubscriptionSnapshot{
					Status:        entitlement.SubscriptionStatusActive,
					BillingPeriod: entitlement.SubscriptionBillingPeriodYearly,
					StartsAt:      now.Add(-((30*24 + 1) * time.Hour)),
					ExpiresAt:     now.Add(24 * time.Hour),
				},
			},
			wantImageLimit:        10,
			wantVideoLimit:        3,
			wantFirstMonthDoubled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			benefits, err := entitlement.ResolveDailyBenefitsAt(tc.user, now)
			if err != nil {
				t.Fatalf("ResolveDailyBenefitsAt() error = %v", err)
			}
			if benefits.ImageLimit != tc.wantImageLimit {
				t.Fatalf("ImageLimit = %d, want %d", benefits.ImageLimit, tc.wantImageLimit)
			}
			if benefits.VideoLimit != tc.wantVideoLimit {
				t.Fatalf("VideoLimit = %d, want %d", benefits.VideoLimit, tc.wantVideoLimit)
			}
			if benefits.DailyDiamonds != 50 {
				t.Fatalf("DailyDiamonds = %d, want 50; 首月双倍不应影响日赠钻石", benefits.DailyDiamonds)
			}
			if benefits.FirstMonthDoubled != tc.wantFirstMonthDoubled {
				t.Fatalf("FirstMonthDoubled = %t, want %t", benefits.FirstMonthDoubled, tc.wantFirstMonthDoubled)
			}
		})
	}
}

func TestResolveDailyBenefitsUsesSubscriptionCreatedAtWhenStartIsMissing(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	benefits, err := entitlement.ResolveDailyBenefitsAt(entitlement.UserSnapshot{
		Timezone: "Asia/Shanghai",
		Subscription: &entitlement.SubscriptionSnapshot{
			Status:        entitlement.SubscriptionStatusActive,
			BillingPeriod: entitlement.SubscriptionBillingPeriodYearly,
			CreatedAt:     now.Add(-24 * time.Hour),
			ExpiresAt:     now.Add(24 * time.Hour),
		},
	}, now)
	if err != nil {
		t.Fatalf("ResolveDailyBenefitsAt() error = %v", err)
	}
	if !benefits.FirstMonthDoubled || benefits.ImageLimit != 20 || benefits.VideoLimit != 6 {
		t.Fatalf("benefits = %#v, want yearly first-month double based on CreatedAt", benefits)
	}
}

func TestResolveDailyBenefitsPrefersSubscriptionStartOverCreatedAt(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	benefits, err := entitlement.ResolveDailyBenefitsAt(entitlement.UserSnapshot{
		Timezone: "Asia/Shanghai",
		Subscription: &entitlement.SubscriptionSnapshot{
			Status:        entitlement.SubscriptionStatusActive,
			BillingPeriod: entitlement.SubscriptionBillingPeriodYearly,
			StartsAt:      now.Add(-31 * 24 * time.Hour),
			CreatedAt:     now.Add(-24 * time.Hour),
			ExpiresAt:     now.Add(24 * time.Hour),
		},
	}, now)
	if err != nil {
		t.Fatalf("ResolveDailyBenefitsAt() error = %v", err)
	}
	if benefits.FirstMonthDoubled || benefits.ImageLimit != 10 || benefits.VideoLimit != 3 {
		t.Fatalf("benefits = %#v, want non-doubled yearly quota based on StartsAt", benefits)
	}
}

func TestResolveDailyBenefitsIncludesExactThirtyDayBoundary(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	benefits, err := entitlement.ResolveDailyBenefitsAt(entitlement.UserSnapshot{
		Timezone: "Asia/Shanghai",
		Subscription: &entitlement.SubscriptionSnapshot{
			Status:        entitlement.SubscriptionStatusActive,
			BillingPeriod: entitlement.SubscriptionBillingPeriodYearly,
			StartsAt:      now.Add(-30 * 24 * time.Hour),
			ExpiresAt:     now.Add(24 * time.Hour),
		},
	}, now)
	if err != nil {
		t.Fatalf("ResolveDailyBenefitsAt() error = %v", err)
	}
	if !benefits.FirstMonthDoubled || benefits.ImageLimit != 20 || benefits.VideoLimit != 6 {
		t.Fatalf("benefits = %#v, want doubled quota at exact 30-day boundary", benefits)
	}
}

func TestResolveDailyBenefitsUsesStoredTimezoneForLocalDate(t *testing.T) {
	now := time.Date(2026, time.September, 5, 0, 30, 0, 0, time.UTC)

	shanghai, err := entitlement.ResolveDailyBenefitsAt(entitlement.UserSnapshot{
		Timezone: "Asia/Shanghai",
	}, now)
	if err != nil {
		t.Fatalf("ResolveDailyBenefitsAt(Shanghai) error = %v", err)
	}
	losAngeles, err := entitlement.ResolveDailyBenefitsAt(entitlement.UserSnapshot{
		Timezone: "America/Los_Angeles",
	}, now)
	if err != nil {
		t.Fatalf("ResolveDailyBenefitsAt(Los Angeles) error = %v", err)
	}
	if shanghai.LocalDate != "2026-09-05" {
		t.Fatalf("Shanghai LocalDate = %q, want 2026-09-05", shanghai.LocalDate)
	}
	if losAngeles.LocalDate != "2026-09-04" {
		t.Fatalf("Los Angeles LocalDate = %q, want 2026-09-04", losAngeles.LocalDate)
	}
}

func TestResolveDailyBenefitsRejectsNonCanonicalTimezone(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	for _, timezone := range []string{"", "Local", "Mars/Olympus_Mons"} {
		t.Run(timezone, func(t *testing.T) {
			_, err := entitlement.ResolveDailyBenefitsAt(entitlement.UserSnapshot{Timezone: timezone}, now)
			if !errors.Is(err, entitlement.ErrInvalidTimezone) {
				t.Fatalf("ResolveDailyBenefitsAt(%q) error = %v, want ErrInvalidTimezone", timezone, err)
			}
		})
	}
}

func TestResolveDailyBenefitsReturnsNoDailyQuotaForInactiveSubscription(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	benefits, err := entitlement.ResolveDailyBenefitsAt(entitlement.UserSnapshot{
		Timezone: "Asia/Shanghai",
		Subscription: &entitlement.SubscriptionSnapshot{
			BillingPeriod: entitlement.SubscriptionBillingPeriodYearly,
			StartsAt:      now.Add(-24 * time.Hour),
			ExpiresAt:     now.Add(24 * time.Hour),
		},
	}, now)
	if err != nil {
		t.Fatalf("ResolveDailyBenefitsAt() error = %v", err)
	}
	if benefits.ImageLimit != 0 || benefits.VideoLimit != 0 || benefits.DailyDiamonds != 0 || benefits.FirstMonthDoubled {
		t.Fatalf("benefits = %#v, want no daily quota for inactive subscription", benefits)
	}
}

func TestEvaluateGenerationRejectsUnsupportedProductOutput(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	_, err := entitlement.EvaluateGenerationAt(entitlement.UserSnapshot{AccountBound: true}, entitlement.GenerationRequest{
		Output: entitlement.ProductOutput("unknown"),
	}, now)
	if !errors.Is(err, entitlement.ErrUnsupportedProductOutput) {
		t.Fatalf("EvaluateGenerationAt() error = %v, want ErrUnsupportedProductOutput", err)
	}
}

func activeVIPUser(now time.Time, billingPeriod entitlement.SubscriptionBillingPeriod) entitlement.UserSnapshot {
	return entitlement.UserSnapshot{
		AccountBound: true,
		Timezone:     "Asia/Shanghai",
		Subscription: &entitlement.SubscriptionSnapshot{
			Status:        entitlement.SubscriptionStatusActive,
			BillingPeriod: billingPeriod,
			StartsAt:      now.Add(-24 * time.Hour),
			ExpiresAt:     now.Add(24 * time.Hour),
		},
	}
}

func TestUsecase带业务时刻的权益入口委托纯函数(t *testing.T) {
	now := time.Date(2026, time.September, 5, 15, 59, 59, 0, time.UTC)
	user := activeVIPUser(now, entitlement.SubscriptionBillingPeriodMonthly)
	usecase := entitlement.NewUsecase()

	decision, err := usecase.EvaluateGenerationAt(user, entitlement.GenerationRequest{
		Output: entitlement.ProductOutputVideo,
		Video:  entitlement.VideoOptions{DurationSeconds: 10},
	}, now)
	if err != nil {
		t.Fatalf("EvaluateGenerationAt() error = %v", err)
	}
	if decision.PriceDiamonds != 100 {
		t.Fatalf("PriceDiamonds = %d, want 100", decision.PriceDiamonds)
	}
	benefits, err := usecase.ResolveDailyBenefitsAt(user, now)
	if err != nil {
		t.Fatalf("ResolveDailyBenefitsAt() error = %v", err)
	}
	if benefits.LocalDate != "2026-09-05" || benefits.VideoLimit != 3 {
		t.Fatalf("benefits = %#v, want local date 2026-09-05 and video limit 3", benefits)
	}
}
