package data

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminblog"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoAdminBlogRepository 只读写 Go 自有的 blog_posts 集合。
// 它不读取 Node 端内容表，也不把站点统计混入文章事实。
type mongoAdminBlogRepository struct{ data *Data }

func NewAdminBlogRepository(data *Data) adminblog.Repository {
	return &mongoAdminBlogRepository{data: data}
}

func (r *mongoAdminBlogRepository) collection() (*mongo.Collection, error) {
	if r == nil || r.data == nil || r.data.database == nil {
		return nil, fmt.Errorf("admin blog repository unavailable")
	}
	return r.data.database.Collection(schema.CollectionBlogPosts), nil
}

type adminBlogRow struct {
	ID              string     `bson:"_id"`
	Slug            string     `bson:"slug"`
	Title           string     `bson:"title"`
	MetaDescription string     `bson:"meta_description"`
	Keywords        []string   `bson:"keywords"`
	Excerpt         string     `bson:"excerpt"`
	Content         string     `bson:"content"`
	CoverImage      string     `bson:"cover_image"`
	AuthorName      string     `bson:"author_name"`
	AuthorAvatar    string     `bson:"author_avatar"`
	Category        string     `bson:"category"`
	Tags            []string   `bson:"tags"`
	Status          string     `bson:"status"`
	PublishedAt     *time.Time `bson:"published_at"`
	ReadingTime     int64      `bson:"reading_time"`
	ViewCount       int64      `bson:"view_count"`
	IsFeatured      bool       `bson:"is_featured"`
	Language        string     `bson:"language"`
	CreatedAt       time.Time  `bson:"created_at"`
	UpdatedAt       time.Time  `bson:"updated_at"`
}

func (row adminBlogRow) post() adminblog.Post {
	post := adminblog.Post{
		ID:              row.ID,
		Slug:            row.Slug,
		Title:           row.Title,
		MetaDescription: row.MetaDescription,
		Keywords:        append([]string{}, row.Keywords...),
		Excerpt:         row.Excerpt,
		Content:         row.Content,
		CoverImage:      row.CoverImage,
		Category:        row.Category,
		Tags:            append([]string{}, row.Tags...),
		Status:          row.Status,
		PublishedAt:     row.PublishedAt,
		ReadingTime:     row.ReadingTime,
		ViewCount:       row.ViewCount,
		IsFeatured:      row.IsFeatured,
		Language:        row.Language,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
	if row.AuthorName != "" || row.AuthorAvatar != "" {
		post.Author = &adminblog.Author{Name: row.AuthorName, Avatar: row.AuthorAvatar}
	}
	return post
}

func blogNotFound(err error) error {
	if errors.Is(err, mongo.ErrNoDocuments) {
		return adminblog.ErrNotFound
	}
	if mongo.IsDuplicateKeyError(err) {
		return adminblog.ErrConflict
	}
	return err
}

func blogFilter(query adminblog.Query) bson.M {
	filter := bson.M{}
	if query.Status != "" {
		filter["status"] = query.Status
	}
	if query.Category != "" {
		filter["category"] = query.Category
	}
	if query.Search != "" {
		pattern := regexp.QuoteMeta(query.Search)
		filter["$or"] = bson.A{
			bson.M{"title": bson.M{"$regex": pattern, "$options": "i"}},
			bson.M{"slug": bson.M{"$regex": pattern, "$options": "i"}},
			bson.M{"excerpt": bson.M{"$regex": pattern, "$options": "i"}},
		}
	}
	return filter
}

func (r *mongoAdminBlogRepository) List(ctx context.Context, query adminblog.Query) (adminblog.Page, error) {
	query, err := adminblog.NormalizeQuery(query)
	if err != nil {
		return adminblog.Page{}, err
	}
	collection, err := r.collection()
	if err != nil {
		return adminblog.Page{}, err
	}
	filter := blogFilter(query)
	total, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return adminblog.Page{}, fmt.Errorf("count blog posts: %w", err)
	}
	skip := int64((query.Page - 1) * query.Limit)
	cursor, err := collection.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(skip).
		SetLimit(int64(query.Limit)))
	if err != nil {
		return adminblog.Page{}, fmt.Errorf("list blog posts: %w", err)
	}
	defer cursor.Close(ctx)
	var rows []adminBlogRow
	if err := cursor.All(ctx, &rows); err != nil {
		return adminblog.Page{}, fmt.Errorf("decode blog posts: %w", err)
	}
	posts := make([]adminblog.Post, 0, len(rows))
	for _, row := range rows {
		posts = append(posts, row.post())
	}
	return adminblog.Page{Posts: posts, Total: total}, nil
}

func (r *mongoAdminBlogRepository) Get(ctx context.Context, id string) (adminblog.Post, error) {
	collection, err := r.collection()
	if err != nil {
		return adminblog.Post{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return adminblog.Post{}, adminblog.ErrInvalid
	}
	var row adminBlogRow
	if err := collection.FindOne(ctx, bson.M{"_id": id}).Decode(&row); err != nil {
		return adminblog.Post{}, blogNotFound(err)
	}
	return row.post(), nil
}

func (r *mongoAdminBlogRepository) Create(ctx context.Context, actor adminblog.Actor, input adminblog.Input) (adminblog.Post, error) {
	input, err := adminblog.NormalizeInput(input, true)
	if err != nil {
		return adminblog.Post{}, err
	}
	slug := ""
	if input.Slug != nil {
		slug = *input.Slug
	} else {
		slug = adminblog.SlugFromTitle(*input.Title)
	}
	if slug == "" {
		return adminblog.Post{}, adminblog.ErrInvalid
	}
	now := time.Now().UTC()
	status := "draft"
	if input.Status != nil {
		status = *input.Status
	}
	document := bson.M{
		"_id":          uuid.NewString(),
		"slug":         slug,
		"title":        *input.Title,
		"category":     *input.Category,
		"content":      *input.Content,
		"status":       status,
		"reading_time": int64(0),
		"view_count":   int64(0),
		"is_featured":  false,
		"language":     "en",
		"keywords":     []string{},
		"tags":         []string{},
		"created_at":   now,
		"updated_at":   now,
	}
	applyBlogInput(document, input)
	if status == "published" {
		document["published_at"] = now
	}
	collection, err := r.collection()
	if err != nil {
		return adminblog.Post{}, err
	}
	var created adminBlogRow
	err = NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if _, err := collection.InsertOne(tx, document); err != nil {
			return blogNotFound(err)
		}
		if err := r.audit(tx, actor, valueString(document["_id"]), "blog_create"); err != nil {
			return err
		}
		return collection.FindOne(tx, bson.M{"_id": document["_id"]}).Decode(&created)
	})
	if err != nil {
		return adminblog.Post{}, err
	}
	return created.post(), nil
}

func (r *mongoAdminBlogRepository) Update(ctx context.Context, actor adminblog.Actor, id string, input adminblog.Input) (adminblog.Post, error) {
	input, err := adminblog.NormalizeInput(input, false)
	if err != nil {
		return adminblog.Post{}, err
	}
	updates := bson.M{"updated_at": time.Now().UTC()}
	applyBlogInput(updates, input)
	if input.Author != nil && *input.Author == nil {
		updates["author_name"] = ""
		updates["author_avatar"] = ""
	}
	if input.Status != nil && *input.Status == "published" {
		updates["published_at"] = time.Now().UTC()
	}
	collection, err := r.collection()
	if err != nil {
		return adminblog.Post{}, err
	}
	var updated adminBlogRow
	err = NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if err := collection.FindOneAndUpdate(tx, bson.M{"_id": id}, bson.M{"$set": updates}, adminAfter).Decode(&updated); err != nil {
			return blogNotFound(err)
		}
		return r.audit(tx, actor, id, "blog_update")
	})
	if err != nil {
		return adminblog.Post{}, err
	}
	return updated.post(), nil
}

func (r *mongoAdminBlogRepository) Delete(ctx context.Context, actor adminblog.Actor, id string) error {
	collection, err := r.collection()
	if err != nil {
		return err
	}
	return NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		result, err := collection.DeleteOne(tx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		if result.DeletedCount == 0 {
			return adminblog.ErrNotFound
		}
		return r.audit(tx, actor, id, "blog_delete")
	})
}

func (r *mongoAdminBlogRepository) Publish(ctx context.Context, actor adminblog.Actor, id string) (adminblog.Post, error) {
	return r.setStatus(ctx, actor, id, "published", "blog_publish")
}

func (r *mongoAdminBlogRepository) Unpublish(ctx context.Context, actor adminblog.Actor, id string) (adminblog.Post, error) {
	return r.setStatus(ctx, actor, id, "draft", "blog_unpublish")
}

func (r *mongoAdminBlogRepository) setStatus(ctx context.Context, actor adminblog.Actor, id, status, action string) (adminblog.Post, error) {
	collection, err := r.collection()
	if err != nil {
		return adminblog.Post{}, err
	}
	now := time.Now().UTC()
	updates := bson.M{"status": status, "updated_at": now}
	if status == "published" {
		updates["published_at"] = now
	}
	var updated adminBlogRow
	err = NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if err := collection.FindOneAndUpdate(tx, bson.M{"_id": id}, bson.M{"$set": updates}, adminAfter).Decode(&updated); err != nil {
			return blogNotFound(err)
		}
		return r.audit(tx, actor, id, action)
	})
	if err != nil {
		return adminblog.Post{}, err
	}
	return updated.post(), nil
}

func (r *mongoAdminBlogRepository) Stats(ctx context.Context) (adminblog.Stats, error) {
	collection, err := r.collection()
	if err != nil {
		return adminblog.Stats{}, err
	}
	result := adminblog.Stats{ByCategory: map[string]int64{}}
	cursor, err := collection.Aggregate(ctx, bson.A{
		bson.M{"$group": bson.M{
			"_id":   bson.M{"status": "$status", "category": "$category"},
			"n":     bson.M{"$sum": 1},
			"views": bson.M{"$sum": "$view_count"},
		}},
	})
	if err != nil {
		return adminblog.Stats{}, fmt.Errorf("aggregate blog stats: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var row struct {
			Key struct {
				Status   string `bson:"status"`
				Category string `bson:"category"`
			} `bson:"_id"`
			Count int64 `bson:"n"`
			Views int64 `bson:"views"`
		}
		if err := cursor.Decode(&row); err != nil {
			return adminblog.Stats{}, fmt.Errorf("decode blog stats: %w", err)
		}
		result.Total += row.Count
		result.TotalViews += row.Views
		switch row.Key.Status {
		case "published":
			result.Published += row.Count
		case "draft":
			result.Draft += row.Count
		}
		if row.Key.Category != "" {
			result.ByCategory[row.Key.Category] += row.Count
		}
	}
	return result, cursor.Err()
}

func (r *mongoAdminBlogRepository) audit(ctx context.Context, actor adminblog.Actor, targetID, action string) error {
	_, err := r.data.database.Collection(adminAuditCollection).InsertOne(ctx, bson.M{
		"_id":        uuid.NewString(),
		"actor_id":   actor.ID,
		"target_id":  targetID,
		"action":     action,
		"created_at": time.Now().UTC(),
	})
	return err
}

// applyBlogInput 只写入调用方显式提供的字段，未提供的保持原值。
func applyBlogInput(target bson.M, input adminblog.Input) {
	set := func(key string, value any) { target[key] = value }
	if input.Slug != nil {
		set("slug", *input.Slug)
	}
	if input.Title != nil {
		set("title", *input.Title)
	}
	if input.MetaDescription != nil {
		set("meta_description", *input.MetaDescription)
	}
	if input.Keywords != nil {
		set("keywords", append([]string{}, (*input.Keywords)...))
	}
	if input.Excerpt != nil {
		set("excerpt", *input.Excerpt)
	}
	if input.Content != nil {
		set("content", *input.Content)
	}
	if input.CoverImage != nil {
		set("cover_image", *input.CoverImage)
	}
	if input.Author != nil && *input.Author != nil {
		set("author_name", (**input.Author).Name)
		set("author_avatar", (**input.Author).Avatar)
	}
	if input.Category != nil {
		set("category", *input.Category)
	}
	if input.Tags != nil {
		set("tags", append([]string{}, (*input.Tags)...))
	}
	if input.Status != nil {
		set("status", *input.Status)
	}
	if input.Language != nil {
		set("language", *input.Language)
	}
	if input.IsFeatured != nil {
		set("is_featured", *input.IsFeatured)
	}
	if input.ReadingTime != nil {
		set("reading_time", *input.ReadingTime)
	}
}

var _ adminblog.Repository = (*mongoAdminBlogRepository)(nil)
