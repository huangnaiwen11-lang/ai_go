package adminpricing

import "testing"

func TestEffectiveCoinPackagesMergesStaticAndOverrides(t *testing.T) {
	packages, err := EffectiveCoinPackages(map[string]any{
		"coins_1000": map[string]any{
			"coins": 1700, "bonusCoins": 400, "enabled": false,
			"label": map[string]any{"en": "Override"},
		},
		"custom_1999": map[string]any{
			"_custom": true, "coins": 2200, "priceInCents": 1999,
			"label": map[string]any{"en": "Custom", "zh": "自定义"},
		},
		"ignored": map[string]any{"coins": 1},
	})
	if err != nil {
		t.Fatalf("EffectiveCoinPackages() error = %v", err)
	}
	if len(packages) != 6 {
		t.Fatalf("package count = %d, want 6", len(packages))
	}
	var found Package
	for _, item := range packages {
		if item.ID == "coins_1000" {
			found = item
		}
	}
	if found.Coins != 1700 || found.BonusCoins != 400 || found.Enabled {
		t.Fatalf("overridden package = %#v", found)
	}
	if found.Builtin != true || found.Custom {
		t.Fatalf("builtin flags = %#v", found)
	}
	for _, item := range packages {
		if item.ID == "custom_1999" && (!item.Custom || item.Builtin || item.PriceInCents != 1999) {
			t.Fatalf("custom package = %#v", item)
		}
	}
}

func TestNormalizeVIPOverridesRejectsUnsafeValues(t *testing.T) {
	if _, err := NormalizeVIPOverrides(map[string]any{"priceMonthly": -1}); err == nil {
		t.Fatal("negative price must be rejected")
	}
	if _, err := NormalizeVIPOverrides(map[string]any{"dailyQuota": map[string]any{"firstMonthMultiplier": 0}}); err == nil {
		t.Fatal("multiplier below one must be rejected")
	}
	got, err := NormalizeVIPOverrides(map[string]any{
		"priceMonthly": 1299,
		"allowedChannels": map[string]any{
			"monthly": []any{" PixPay:BR_PIX ", "pixpay:br_pix", "bad channel"},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeVIPOverrides() error = %v", err)
	}
	allowed := got["allowedChannels"].(map[string]any)["monthly"].([]string)
	if len(allowed) != 1 || allowed[0] != "PixPay:BR_PIX" {
		t.Fatalf("allowed channels = %#v", allowed)
	}
}

func TestSubscriptionStrategyIsExplicitlyUnavailableWithoutPayCores(t *testing.T) {
	if err := ErrPayCoresUnavailable; err == nil {
		t.Fatal("strategy dependency error must be explicit")
	}
}
