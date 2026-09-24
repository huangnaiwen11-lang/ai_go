// Package adminpricing contains the pricing contracts owned by the admin API.
//
// The defaults intentionally mirror the old Node pricing.config.js. Runtime
// values are only overrides stored as SystemConfig-like documents; the Go
// service never invents payment products when the configuration is absent.
package adminpricing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	CoinPackagesKey = "wallet.coinPackagesOverrides"
	VIPConfigKey    = "subscription.vipConfigOverrides"
	StrategyKey     = "payment.subscriptionStrategy"
)

var (
	ErrInvalid                 = errors.New("admin pricing: invalid input")
	ErrNotFound                = errors.New("admin pricing: config not found")
	ErrPayCoresUnavailable     = errors.New("admin pricing: PayCores subscription strategy unavailable")
	ErrDependenciesUnavailable = errors.New("admin pricing: dependencies unavailable")
)

type Actor struct{ ID, Role string }

// SystemConfig is the subset of the old Node SystemConfig contract used by
// pricing. Value stays untyped because coin package overrides are a map while
// firstRechargeBonusCoins is a scalar.
type SystemConfig struct {
	Key         string
	Value       any
	Category    string
	Description string
	Environment string
	Enabled     bool
	UpdatedAt   *time.Time
	CreatedAt   *time.Time
	UpdatedBy   string
}

type SystemConfigInput struct {
	Key          string
	Value        any
	Category     string
	Description  string
	Environment  string
	ChangeReason string
}

type Package struct {
	ID              string
	Coins           int64
	BonusCoins      int64
	PriceInCents    int64
	Label           map[string]string
	Icon            string
	Popular         bool
	Badge           *string
	Enabled         bool
	AllowedChannels []string
	Builtin         bool
	Custom          bool
}

type CreditBundle struct {
	VideoCredits  int64 `json:"videoCredits"`
	ImageCredits  int64 `json:"imageCredits"`
	DailyDiamonds int64 `json:"dailyDiamonds"`
}

type DailyQuota struct {
	Images               int64   `json:"images"`
	Videos               int64   `json:"videos"`
	FirstMonthMultiplier float64 `json:"firstMonthMultiplier"`
}

type VIPConfig struct {
	ID              string                  `json:"id"`
	Name            map[string]string       `json:"name"`
	Tagline         map[string]string       `json:"tagline"`
	PriceMonthly    int64                   `json:"priceMonthly"`
	PriceYearly     int64                   `json:"priceYearly"`
	CreditBundle    map[string]CreditBundle `json:"creditBundle"`
	DailyQuota      DailyQuota              `json:"dailyQuota"`
	Features        map[string]any          `json:"features"`
	AllowedChannels map[string][]string     `json:"allowedChannels"`
	Icon            string                  `json:"icon"`
	Color           string                  `json:"color"`
	Popular         bool                    `json:"popular"`
	Badge           *string                 `json:"badge"`
}

type VIPSnapshot struct {
	Static     VIPConfig        `json:"static"`
	Overrides  map[string]any   `json:"overrides"`
	Effective  VIPConfig        `json:"effective"`
	Benefits   []map[string]any `json:"benefits"`
	Highlights []map[string]any `json:"highlights"`
}

type Repository interface {
	LoadSystemConfig(ctx context.Context, key, environment string) (*SystemConfig, error)
	SaveSystemConfig(ctx context.Context, actor Actor, input SystemConfigInput) (SystemConfig, error)
	DeleteSystemConfig(ctx context.Context, actor Actor, key, environment string) (SystemConfig, error)
}

// Operations composes static defaults with stored overrides.
type Operations struct{ repository Repository }

func NewOperations(repository Repository) *Operations { return &Operations{repository: repository} }

func (o *Operations) Packages(ctx context.Context) ([]Package, error) {
	if o == nil || o.repository == nil {
		return nil, ErrDependenciesUnavailable
	}
	doc, err := o.repository.LoadSystemConfig(ctx, CoinPackagesKey, "all")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var overrides map[string]any
	if doc != nil {
		overrides, _ = asMap(doc.Value)
	}
	return EffectiveCoinPackages(overrides)
}

func (o *Operations) VIP(ctx context.Context) (VIPSnapshot, error) {
	if o == nil || o.repository == nil {
		return VIPSnapshot{}, ErrDependenciesUnavailable
	}
	doc, err := o.repository.LoadSystemConfig(ctx, VIPConfigKey, "all")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return VIPSnapshot{}, err
	}
	var overrides map[string]any
	if doc != nil {
		overrides, _ = asMap(doc.Value)
	}
	effective, err := EffectiveVIPConfig(overrides)
	if err != nil {
		return VIPSnapshot{}, err
	}
	benefits, highlights := copyRows(vipBenefits), copyRows(vipHighlights)
	if rows, ok := asRows(overrides["benefits"]); ok {
		benefits = rows
	}
	if rows, ok := asRows(overrides["highlights"]); ok {
		highlights = rows
	}
	return VIPSnapshot{Static: StaticVIPConfig(), Overrides: overrides, Effective: effective, Benefits: benefits, Highlights: highlights}, nil
}

func (o *Operations) SystemConfig(ctx context.Context, key, environment string) (SystemConfig, error) {
	if o == nil || o.repository == nil {
		return SystemConfig{}, ErrDependenciesUnavailable
	}
	doc, err := o.repository.LoadSystemConfig(ctx, key, environment)
	if err != nil {
		return SystemConfig{}, err
	}
	if doc == nil {
		return SystemConfig{}, ErrNotFound
	}
	return *doc, nil
}

func (o *Operations) SaveVIP(ctx context.Context, actor Actor, overrides map[string]any, reason string) (VIPSnapshot, error) {
	normalized, err := NormalizeVIPOverrides(overrides)
	if err != nil {
		return VIPSnapshot{}, err
	}
	if o == nil || o.repository == nil {
		return VIPSnapshot{}, ErrDependenciesUnavailable
	}
	if _, err := o.repository.SaveSystemConfig(ctx, actor, SystemConfigInput{Key: VIPConfigKey, Value: normalized, Category: "subscription", Description: "VIP pricing & benefits overrides (admin-configurable)", Environment: "all", ChangeReason: reason}); err != nil {
		return VIPSnapshot{}, err
	}
	return o.VIP(ctx)
}

func (o *Operations) SaveSystemConfig(ctx context.Context, actor Actor, input SystemConfigInput) (SystemConfig, error) {
	if o == nil || o.repository == nil {
		return SystemConfig{}, ErrDependenciesUnavailable
	}
	if !allowedSystemKey(input.Key) || input.Environment == "" {
		return SystemConfig{}, ErrInvalid
	}
	return o.repository.SaveSystemConfig(ctx, actor, input)
}

func (o *Operations) DeleteSystemConfig(ctx context.Context, actor Actor, key, environment string) (SystemConfig, error) {
	if o == nil || o.repository == nil {
		return SystemConfig{}, ErrDependenciesUnavailable
	}
	if !allowedSystemKey(key) || environment == "" {
		return SystemConfig{}, ErrInvalid
	}
	return o.repository.DeleteSystemConfig(ctx, actor, key, environment)
}

func (o *Operations) SubscriptionStrategy(context.Context) (map[string]any, error) {
	return nil, ErrPayCoresUnavailable
}

func allowedSystemKey(key string) bool {
	return key == CoinPackagesKey || key == "wallet.firstRechargeBonusCoins"
}

func StaticCoinPackages() map[string]Package {
	return map[string]Package{
		"coins_200":   {ID: "coins_200", Coins: 300, BonusCoins: 25, PriceInCents: 199, Label: map[string]string{"en": "Small", "zh": "小额包"}, Icon: "💎"},
		"coins_500":   {ID: "coins_500", Coins: 750, BonusCoins: 75, PriceInCents: 499, Label: map[string]string{"en": "Starter", "zh": "入门包"}, Icon: "💎"},
		"coins_1000":  {ID: "coins_1000", Coins: 1500, BonusCoins: 300, PriceInCents: 999, Label: map[string]string{"en": "Basic", "zh": "基础包"}, Icon: "💎", Popular: true, Badge: strptr("POPULAR")},
		"coins_5000":  {ID: "coins_5000", Coins: 7500, BonusCoins: 3000, PriceInCents: 4999, Label: map[string]string{"en": "Pro", "zh": "进阶包"}, Icon: "💎", Badge: strptr("BEST VALUE")},
		"coins_10000": {ID: "coins_10000", Coins: 15000, BonusCoins: 7500, PriceInCents: 9999, Label: map[string]string{"en": "Max", "zh": "至尊包"}, Icon: "💎"},
	}
}

func EffectiveCoinPackages(overrides map[string]any) ([]Package, error) {
	defaults := StaticCoinPackages()
	result := make(map[string]Package, len(defaults))
	for id, pkg := range defaults {
		pkg.Builtin, pkg.Enabled = true, true
		if override, ok := asMap(overrides[id]); ok {
			applyPackageOverride(&pkg, override)
		}
		result[id] = pkg
	}
	for id, raw := range overrides {
		if _, exists := result[id]; exists {
			continue
		}
		entry, ok := asMap(raw)
		if !ok || !asBool(entry["_custom"]) {
			continue
		}
		pkg := Package{ID: id, Enabled: true, Icon: "💎", Label: map[string]string{"en": id, "zh": id}, Custom: true}
		applyPackageOverride(&pkg, entry)
		pkg.Custom, pkg.Builtin = true, false
		result[id] = pkg
	}
	out := make([]Package, 0, len(result))
	for _, pkg := range result {
		out = append(out, pkg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PriceInCents != out[j].PriceInCents {
			return out[i].PriceInCents < out[j].PriceInCents
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func applyPackageOverride(pkg *Package, override map[string]any) {
	if value, ok := nonNegativeInt(override["coins"]); ok {
		pkg.Coins = value
	}
	if value, ok := nonNegativeInt(override["bonusCoins"]); ok {
		pkg.BonusCoins = value
	}
	if value, ok := nonNegativeInt(override["priceInCents"]); ok {
		pkg.PriceInCents = value
	}
	if value, ok := override["enabled"].(bool); ok {
		pkg.Enabled = value
	}
	if value, ok := override["popular"].(bool); ok {
		pkg.Popular = value
	}
	if value, ok := override["icon"].(string); ok {
		pkg.Icon = value
	}
	if value, ok := override["badge"]; ok {
		if value == nil {
			pkg.Badge = nil
		} else if s, ok := value.(string); ok {
			pkg.Badge = strptr(s)
		}
	}
	if value, ok := asMap(override["label"]); ok {
		pkg.Label = map[string]string{}
		for key, raw := range value {
			if s, ok := raw.(string); ok {
				pkg.Label[key] = s
			}
		}
	}
	if value, ok := override["allowedChannels"].([]any); ok {
		pkg.AllowedChannels = normalizeAllowedChannels(value)
	}
	if value, ok := override["allowedChannels"].([]string); ok {
		pkg.AllowedChannels = normalizeAllowedChannelsString(value)
	}
}

func StaticVIPConfig() VIPConfig {
	return VIPConfig{ID: "sub_vip", Name: map[string]string{"en": "VIP Member", "zh": "VIP会员"}, Tagline: map[string]string{"en": "Unlock advanced features + daily diamonds", "zh": "解锁高级功能 + 每日钻石"}, PriceMonthly: 1299, PriceYearly: 6999, CreditBundle: map[string]CreditBundle{"monthly": {DailyDiamonds: 50}, "yearly": {DailyDiamonds: 50}}, DailyQuota: DailyQuota{Images: 10, Videos: 3, FirstMonthMultiplier: 2}, Features: map[string]any{"canCreateAgent": true, "canGenerateImage": true, "canGenerateVideo": true, "videoMaxDurationSeconds": int64(15), "advancedSettings": true, "enableAudio": true, "priorityQueue": true, "prioritySupport": true, "earlyAccess": true}, AllowedChannels: map[string][]string{"monthly": {}, "yearly": {}}, Icon: "👑", Color: "amber", Popular: true, Badge: strptr("RECOMMENDED")}
}

func EffectiveVIPConfig(overrides map[string]any) (VIPConfig, error) {
	effective := StaticVIPConfig()
	if overrides == nil {
		return effective, nil
	}
	if value, ok := nonNegativeInt(overrides["priceMonthly"]); ok {
		effective.PriceMonthly = value
	}
	if value, ok := nonNegativeInt(overrides["priceYearly"]); ok {
		effective.PriceYearly = value
	}
	if bundle, ok := asMap(overrides["creditBundle"]); ok {
		for _, period := range []string{"monthly", "yearly"} {
			if values, ok := asMap(bundle[period]); ok {
				current := effective.CreditBundle[period]
				if v, ok := nonNegativeInt(values["videoCredits"]); ok {
					current.VideoCredits = v
				}
				if v, ok := nonNegativeInt(values["imageCredits"]); ok {
					current.ImageCredits = v
				}
				if v, ok := nonNegativeInt(values["dailyDiamonds"]); ok {
					current.DailyDiamonds = v
				}
				effective.CreditBundle[period] = current
			}
		}
	}
	if quota, ok := asMap(overrides["dailyQuota"]); ok {
		if v, ok := nonNegativeInt(quota["images"]); ok {
			effective.DailyQuota.Images = v
		}
		if v, ok := nonNegativeInt(quota["videos"]); ok {
			effective.DailyQuota.Videos = v
		}
		if v, ok := positiveNumber(quota["firstMonthMultiplier"]); ok {
			effective.DailyQuota.FirstMonthMultiplier = v
		}
	}
	if features, ok := asMap(overrides["features"]); ok {
		for key, value := range features {
			effective.Features[key] = value
		}
	}
	if channels, ok := asMap(overrides["allowedChannels"]); ok {
		effective.AllowedChannels = normalizeAllowedChannelMap(channels)
	}
	if value, ok := overrideString(overrides["icon"]); ok {
		effective.Icon = value
	}
	if value, ok := overrideString(overrides["color"]); ok {
		effective.Color = value
	}
	if value, ok := overrides["badge"]; ok {
		if value == nil {
			effective.Badge = nil
		} else if s, ok := value.(string); ok {
			effective.Badge = strptr(s)
		}
	}
	if value, ok := overrides["popular"].(bool); ok {
		effective.Popular = value
	}
	return effective, nil
}

func NormalizeVIPOverrides(overrides map[string]any) (map[string]any, error) {
	if overrides == nil {
		return nil, ErrInvalid
	}
	out := cloneMap(overrides)
	for _, key := range []string{"priceMonthly", "priceYearly"} {
		if value, present := overrides[key]; present {
			if n, ok := nonNegativeInt(value); !ok {
				return nil, fmt.Errorf("%w: invalid %s", ErrInvalid, key)
			} else {
				out[key] = n
			}
		}
	}
	if raw, present := overrides["dailyQuota"]; present {
		quota, ok := asMap(raw)
		if !ok {
			return nil, fmt.Errorf("%w: invalid dailyQuota", ErrInvalid)
		}
		normalized := cloneMap(quota)
		if value, present := quota["images"]; present {
			n, ok := nonNegativeInt(value)
			if !ok {
				return nil, fmt.Errorf("%w: invalid dailyQuota.images", ErrInvalid)
			}
			normalized["images"] = n
		}
		if value, present := quota["videos"]; present {
			n, ok := nonNegativeInt(value)
			if !ok {
				return nil, fmt.Errorf("%w: invalid dailyQuota.videos", ErrInvalid)
			}
			normalized["videos"] = n
		}
		if value, present := quota["firstMonthMultiplier"]; present {
			n, ok := positiveNumber(value)
			if !ok || n < 1 {
				return nil, fmt.Errorf("%w: invalid dailyQuota.firstMonthMultiplier", ErrInvalid)
			}
			normalized["firstMonthMultiplier"] = n
		}
		out["dailyQuota"] = normalized
	}
	if raw, present := overrides["allowedChannels"]; present {
		channels, ok := asMap(raw)
		if !ok {
			return nil, fmt.Errorf("%w: invalid allowedChannels", ErrInvalid)
		}
		normalized := map[string]any{}
		for _, period := range []string{"monthly", "yearly"} {
			if value, exists := channels[period]; exists {
				list, ok := channelList(value)
				if !ok {
					return nil, fmt.Errorf("%w: invalid allowedChannels.%s", ErrInvalid, period)
				}
				normalized[period] = list
			}
		}
		out["allowedChannels"] = normalized
	}
	return out, nil
}

func channelList(raw any) ([]string, bool) {
	switch value := raw.(type) {
	case []string:
		return normalizeAllowedChannelsString(value), true
	case []any:
		return normalizeAllowedChannels(value), true
	case nil:
		return []string{}, true
	default:
		return nil, false
	}
}
func normalizeAllowedChannels(values []any) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, raw := range values {
		s, ok := raw.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" || !strings.Contains(s, ":") {
			continue
		}
		parts := strings.Split(s, ":")
		if len(parts) != 2 || !isChannelPart(parts[0]) || !isChannelPart(parts[1]) {
			continue
		}
		lower := strings.ToLower(s)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, s)
	}
	return out
}
func normalizeAllowedChannelsString(values []string) []string {
	rows := make([]any, len(values))
	for i, value := range values {
		rows[i] = value
	}
	return normalizeAllowedChannels(rows)
}
func normalizeAllowedChannelMap(values map[string]any) map[string][]string {
	out := map[string][]string{"monthly": {}, "yearly": {}}
	for _, period := range []string{"monthly", "yearly"} {
		if value, ok := values[period]; ok {
			if list, ok := channelList(value); ok {
				out[period] = list
			}
		}
	}
	return out
}
func isChannelPart(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

var vipBenefits = []map[string]any{{"key": "dailyImages", "label": map[string]string{"en": "Daily Free Images", "zh": "每日免费图片"}, "free": map[string]any{"value": "Pay with diamonds", "valueZh": "使用钻石", "highlight": false}, "vip": map[string]any{"value": "10 / day (first month 20)", "valueZh": "10张/天 (首月20张)", "highlight": true}}, {"key": "dailyVideos", "label": map[string]string{"en": "Daily Free Videos", "zh": "每日免费视频"}, "free": map[string]any{"value": "—", "valueZh": "—", "highlight": false}, "vip": map[string]any{"value": "3 / day (first month 6)", "valueZh": "3个/天 (首月6个)", "highlight": true}}, {"key": "dailyDiamonds", "label": map[string]string{"en": "Daily Diamonds", "zh": "每日钻石"}, "free": map[string]any{"value": "0", "highlight": false}, "vip": map[string]any{"value": "50 💎/day", "valueZh": "50 💎/天", "highlight": true}}, {"key": "videoDuration", "label": map[string]string{"en": "Video Duration", "zh": "视频时长"}, "free": map[string]any{"value": "5s only", "valueZh": "仅 5s", "highlight": false}, "vip": map[string]any{"value": "5s / 10s / 15s", "highlight": true}}, {"key": "queuePriority", "label": map[string]string{"en": "Queue Priority", "zh": "队列优先级"}, "free": map[string]any{"value": "Standard", "valueZh": "标准", "highlight": false}, "vip": map[string]any{"value": "Priority", "valueZh": "优先", "highlight": true}}}
var vipHighlights = []map[string]any{{"icon": "image", "label": map[string]string{"en": "10 imgs/day free", "zh": "每日10张免费图片"}}, {"icon": "video", "label": map[string]string{"en": "3 vids/day free", "zh": "每日3个免费视频"}}, {"icon": "bolt", "label": map[string]string{"en": "First month 2X", "zh": "首月双倍"}}}

func copyRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i] = cloneMap(row)
	}
	return out
}

func asRows(value any) ([]map[string]any, bool) {
	var raw []any
	switch typed := value.(type) {
	case []any:
		raw = typed
	case []map[string]any:
		return copyRows(typed), true
	default:
		return nil, false
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := asMap(item)
		if !ok {
			return nil, false
		}
		out = append(out, cloneMap(row))
	}
	return out, true
}
func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		if nested, ok := asMap(value); ok {
			out[key] = cloneMap(nested)
		} else if list, ok := value.([]any); ok {
			copied := make([]any, len(list))
			for i, item := range list {
				if nested, ok := asMap(item); ok {
					copied[i] = cloneMap(nested)
				} else {
					copied[i] = item
				}
			}
			out[key] = copied
		} else {
			out[key] = value
		}
	}
	return out
}
func asMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[string]string:
		out := map[string]any{}
		for key, value := range typed {
			out[key] = value
		}
		return out, true
	default:
		return nil, false
	}
}
func nonNegativeInt(value any) (int64, bool) {
	n, ok := number(value)
	if !ok || n < 0 {
		if n == 0 && ok {
			return 0, true
		}
		return 0, false
	}
	return int64(math.Floor(n)), true
}
func positiveNumber(value any) (float64, bool) {
	n, ok := number(value)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, true
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case string:
		if strings.TrimSpace(typed) == "" {
			return 0, true
		}
		var n float64
		_, err := fmt.Sscan(strings.TrimSpace(typed), &n)
		return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
	default:
		return 0, false
	}
}
func overrideString(value any) (string, bool) { s, ok := value.(string); return s, ok }
func asBool(value any) bool                   { typed, _ := value.(bool); return typed }
func strptr(value string) *string             { return &value }
