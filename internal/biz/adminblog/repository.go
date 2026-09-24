// Package adminblog 定义后台博客内容的管理合同。
//
// 该域只读写 Go 自有的 blog_posts 集合，不依赖 Node 端内容表，
// 也不做跨域推断（例如从阅读量反推站点统计）。
package adminblog

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("admin blog: invalid request")
	ErrNotFound = errors.New("admin blog: not found")
	ErrConflict = errors.New("admin blog: slug already exists")
)

var (
	categories = []string{"guides", "news", "tutorials", "comparisons", "stories", "updates"}
	statuses   = []string{"draft", "published", "archived"}
	languages  = []string{"en", "zh", "ja", "ko"}
)

// Author 是文章作者的可选展示信息。
type Author struct {
	Name   string
	Avatar string
}

// Post 是后台可见的博客文章投影。
type Post struct {
	ID              string
	Slug            string
	Title           string
	MetaDescription string
	Keywords        []string
	Excerpt         string
	Content         string
	CoverImage      string
	Author          *Author
	Category        string
	Tags            []string
	Status          string
	PublishedAt     *time.Time
	ReadingTime     int64
	ViewCount       int64
	IsFeatured      bool
	Language        string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Input 是创建/更新入参。指针字段区分「未提供」与「显式置空」。
type Input struct {
	Slug            *string
	Title           *string
	MetaDescription *string
	Keywords        *[]string
	Excerpt         *string
	Content         *string
	CoverImage      *string
	Author          **Author
	Category        *string
	Tags            *[]string
	Status          *string
	Language        *string
	IsFeatured      *bool
	ReadingTime     *int64
}

type Query struct {
	Status   string
	Category string
	Search   string
	Page     int
	Limit    int
}

type Page struct {
	Posts []Post
	Total int64
}

type Stats struct {
	Total      int64
	Published  int64
	Draft      int64
	TotalViews int64
	ByCategory map[string]int64
}

// Actor 是执行写入的管理员身份，仅用于审计归属。
type Actor struct{ ID string }

type Repository interface {
	List(context.Context, Query) (Page, error)
	Get(context.Context, string) (Post, error)
	Create(context.Context, Actor, Input) (Post, error)
	Update(context.Context, Actor, string, Input) (Post, error)
	Delete(context.Context, Actor, string) error
	Publish(context.Context, Actor, string) (Post, error)
	Unpublish(context.Context, Actor, string) (Post, error)
	Stats(context.Context) (Stats, error)
}

// NormalizeQuery 收敛分页与筛选，越界一律拒绝而不是静默截断。
func NormalizeQuery(query Query) (Query, error) {
	if query.Page == 0 {
		query.Page = 1
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Page < 1 || query.Page > 10000 || query.Limit < 1 || query.Limit > 100 {
		return Query{}, ErrInvalid
	}
	query.Status = strings.TrimSpace(query.Status)
	query.Category = strings.TrimSpace(query.Category)
	query.Search = strings.TrimSpace(query.Search)
	if query.Status != "" && !contains(statuses, query.Status) {
		return Query{}, ErrInvalid
	}
	if query.Category != "" && !contains(categories, query.Category) {
		return Query{}, ErrInvalid
	}
	if len(query.Search) > 200 {
		return Query{}, ErrInvalid
	}
	return query, nil
}

// NormalizeInput 校验创建/更新入参，返回规范化副本。
func NormalizeInput(input Input, requireContent bool) (Input, error) {
	trim := func(value *string) *string {
		if value == nil {
			return nil
		}
		trimmed := strings.TrimSpace(*value)
		return &trimmed
	}
	input.Slug = trim(input.Slug)
	// 后台表单把未填的 slug 传成空串；按「未提供」处理并回退到标题派生，
	// 不把空 slug 当成一次显式置空。
	if input.Slug != nil && *input.Slug == "" {
		input.Slug = nil
	}
	input.Title = trim(input.Title)
	input.MetaDescription = trim(input.MetaDescription)
	input.Excerpt = trim(input.Excerpt)
	input.CoverImage = trim(input.CoverImage)
	input.Category = trim(input.Category)
	input.Status = trim(input.Status)
	input.Language = trim(input.Language)
	if input.Slug != nil && (len(*input.Slug) > 200 || strings.ContainsAny(*input.Slug, " /?#")) {
		return Input{}, ErrInvalid
	}
	if input.Title != nil && (*input.Title == "" || len(*input.Title) > 300) {
		return Input{}, ErrInvalid
	}
	if requireContent && (input.Title == nil || input.Content == nil || input.Category == nil) {
		return Input{}, ErrInvalid
	}
	if input.Category != nil && !contains(categories, *input.Category) {
		return Input{}, ErrInvalid
	}
	if input.Status != nil && !contains(statuses, *input.Status) {
		return Input{}, ErrInvalid
	}
	if input.Language != nil && !contains(languages, *input.Language) {
		return Input{}, ErrInvalid
	}
	if input.ReadingTime != nil && (*input.ReadingTime < 0 || *input.ReadingTime > 24*60) {
		return Input{}, ErrInvalid
	}
	if input.MetaDescription != nil && len(*input.MetaDescription) > 500 {
		return Input{}, ErrInvalid
	}
	if input.Excerpt != nil && len(*input.Excerpt) > 2000 {
		return Input{}, ErrInvalid
	}
	if input.Keywords != nil && len(*input.Keywords) > 50 {
		return Input{}, ErrInvalid
	}
	if input.Tags != nil && len(*input.Tags) > 50 {
		return Input{}, ErrInvalid
	}
	if input.Author != nil && *input.Author != nil {
		author := **input.Author
		author.Name = strings.TrimSpace(author.Name)
		author.Avatar = strings.TrimSpace(author.Avatar)
		if len(author.Name) > 200 || len(author.Avatar) > 1000 {
			return Input{}, ErrInvalid
		}
		*input.Author = &author
	}
	return input, nil
}

// SlugFromTitle 在未显式提供 slug 时派生一个稳定标识。
func SlugFromTitle(title string) string {
	var builder strings.Builder
	for _, symbol := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case symbol >= 'a' && symbol <= 'z', symbol >= '0' && symbol <= '9':
			builder.WriteRune(symbol)
		case symbol == ' ' || symbol == '-' || symbol == '_' || symbol == '.':
			builder.WriteRune('-')
		}
	}
	slug := strings.Trim(builder.String(), "-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	if len(slug) > 200 {
		slug = strings.Trim(slug[:200], "-")
	}
	return slug
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
