// Package adminutm defines the read-only admin UTM projection.
package adminutm

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("admin utm: invalid input")
	ErrNotFound = errors.New("admin utm: link not found")
	ErrConflict = errors.New("admin utm: slug already taken")
)

// 字段长度上限，与 Node 端 UtmLink 模型的 maxlength 逐一对应。
// utmMedium 是 100，其余 UTM 维度字段是 200 —— 不要合并成一个常量。
const (
	SlugMinLen, SlugMaxLen = 3, 50
	LabelMaxLen            = 100
	TargetPathMaxLen       = 300
	UtmSourceMaxLen        = 100
	UtmMediumMaxLen        = 100
	UtmFieldMaxLen         = 200
	NotesMaxLen            = 500
	ExtraParamMaxKeyLen    = 50
	ExtraParamMaxValueLen  = 200
)

// Create 时的默认值，与 Node 端 sanitizeInput 的补全逻辑一致。
const (
	DefaultTargetPath = "/"
	DefaultUtmMedium  = "banner"
)

type Query struct {
	Keyword string
	Enabled *bool
	Limit   int
	Skip    int
}

type Link struct {
	ID, Slug, Label, TargetPath, UtmSource, UtmMedium, UtmCampaign, UtmContent, UtmTerm string
	ExtraParams                                                                         map[string]string
	Enabled                                                                             bool
	Notes                                                                               string
	Clicks                                                                              int64
	ShortURL                                                                            string
	FullURL                                                                             string
	LastClickAt                                                                         *time.Time
	CreatedAt                                                                           time.Time
	UpdatedAt                                                                           time.Time
}

type Page struct {
	Total int64
	Items []Link
}

type Source struct {
	UtmSource   string
	Enabled     bool
	TotalClicks int64
	Links       []SourceLink
}

type SourceLink struct {
	ID, Slug, Label, UtmMedium, UtmCampaign, UtmContent, UtmTerm string
	Enabled                                                      bool
}

type Repository interface {
	List(context.Context, Query) (Page, error)
	Sources(context.Context, bool) ([]Source, error)
	Create(context.Context, Actor, Input) (Link, error)
	Update(context.Context, Actor, string, Input) (Link, error)
	Delete(context.Context, Actor, string) error
}

// Actor 是写操作的审计主体。
type Actor struct{ ID, Role string }

// Input 是 UTM 链接写入载荷。
// 全部字段用指针，区分「未提供」与「显式置空」——
// 后台的启用开关只发 {enabled}，其余字段必须保持原值而不是被清空。
type Input struct {
	Slug, Label, TargetPath, UtmSource                 *string
	UtmMedium, UtmCampaign, UtmContent, UtmTerm, Notes *string
	ExtraParams                                        map[string]string
	Enabled                                            *bool
}

// slugPattern 与 Node 端 UtmLink 模型的 match 保持一致：
// 首尾必须是字母数字，中间允许小写字母、数字、下划线、中划线；长度由模式本身限定为 3–50。
// 注意它允许下划线 —— 早期 Go 版本用的是 ^[a-z0-9]+(-[a-z0-9]+)*$，
// 会把 Node 上完全合法的 tg_community_2026q2 判成非法。
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,48}[a-z0-9]$`)

// NormalizeInput 校验并补全写入载荷。
//
// creating 为真时要求 slug / label / utmSource 必填，并补上 Node 端同款默认值
// （targetPath "/"、utmMedium "banner"）。
//
// 长度越界一律报错而不是静默截断 —— 截断会让运营以为存下了完整值。
// 唯一的例外是 extraParams 的非法键：Node 端是静默丢弃，这里保持同样的行为，
// 否则一个在 Node 上能保存的载荷会在 Go 上 400，属于语义分裂。
func NormalizeInput(in Input, creating bool) (Input, error) {
	trim := func(value *string) *string {
		if value == nil {
			return nil
		}
		trimmed := strings.TrimSpace(*value)
		return &trimmed
	}
	in.Slug, in.Label, in.TargetPath = trim(in.Slug), trim(in.Label), trim(in.TargetPath)
	in.UtmSource, in.UtmMedium, in.UtmCampaign = trim(in.UtmSource), trim(in.UtmMedium), trim(in.UtmCampaign)
	in.UtmContent, in.UtmTerm, in.Notes = trim(in.UtmContent), trim(in.UtmTerm), trim(in.Notes)

	if in.Slug != nil {
		lowered := strings.ToLower(*in.Slug)
		in.Slug = &lowered
		if !slugPattern.MatchString(lowered) {
			return Input{}, ErrInvalid
		}
	}
	if in.Label != nil && (len([]rune(*in.Label)) < 1 || len([]rune(*in.Label)) > LabelMaxLen) {
		return Input{}, ErrInvalid
	}
	if in.TargetPath != nil && len(*in.TargetPath) > TargetPathMaxLen {
		return Input{}, ErrInvalid
	}
	if in.UtmSource != nil && (len(*in.UtmSource) < 1 || len(*in.UtmSource) > UtmSourceMaxLen) {
		return Input{}, ErrInvalid
	}
	if in.UtmMedium != nil && len(*in.UtmMedium) > UtmMediumMaxLen {
		return Input{}, ErrInvalid
	}
	for _, field := range []*string{in.UtmCampaign, in.UtmContent, in.UtmTerm} {
		if field != nil && len(*field) > UtmFieldMaxLen {
			return Input{}, ErrInvalid
		}
	}
	if in.Notes != nil && len([]rune(*in.Notes)) > NotesMaxLen {
		return Input{}, ErrInvalid
	}
	if creating {
		if in.Slug == nil || in.Label == nil || in.UtmSource == nil {
			return Input{}, ErrInvalid
		}
		if in.TargetPath == nil || *in.TargetPath == "" {
			fallback := DefaultTargetPath
			in.TargetPath = &fallback
		}
		if in.UtmMedium == nil || *in.UtmMedium == "" {
			fallback := DefaultUtmMedium
			in.UtmMedium = &fallback
		}
	}
	if in.ExtraParams != nil {
		in.ExtraParams = sanitizeExtraParams(in.ExtraParams)
	}
	return in, nil
}

// sanitizeExtraParams 丢弃非法键、截断超长值，与 Node 端 sanitizeInput 的行为一致。
//
// utm_* 前缀的键会被丢弃：它们由专门的 utmSource/utmMedium/... 字段负责，
// 允许从 extraParams 注入会绕过 buildFullUrl 里「专用字段优先」的赋值顺序。
//
// 值按 rune 截断而不是按字节：Go 的按字节切片会把多字节字符切成半个，
// 编码出的 JSON 会变成 U+FFFD。
func sanitizeExtraParams(params map[string]string) map[string]string {
	out := make(map[string]string, len(params))
	for key, value := range params {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" || len(trimmed) > ExtraParamMaxKeyLen {
			continue
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "utm_") {
			continue
		}
		runes := []rune(value)
		if len(runes) > ExtraParamMaxValueLen {
			value = string(runes[:ExtraParamMaxValueLen])
		}
		out[trimmed] = value
	}
	return out
}

func NormalizeQuery(query Query) (Query, error) {
	if query.Limit == 0 {
		query.Limit = 100
	}
	if query.Limit < 1 || query.Limit > 500 || query.Skip < 0 || query.Skip > 5000 || len(strings.TrimSpace(query.Keyword)) > 100 {
		return Query{}, ErrInvalid
	}
	query.Keyword = strings.TrimSpace(query.Keyword)
	return query, nil
}
