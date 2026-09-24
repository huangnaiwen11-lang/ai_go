package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/biz/adminmenu"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const adminMenuCollection = schema.CollectionAdminMenuVisibility

type mongoAdminMenuRepository struct{ data *Data }

func NewAdminMenuRepository(data *Data) adminmenu.Repository {
	return &mongoAdminMenuRepository{data: data}
}

func (r *mongoAdminMenuRepository) collection() (*mongo.Collection, error) {
	if r == nil || r.data == nil || r.data.database == nil {
		return nil, fmt.Errorf("admin menu repository unavailable")
	}
	return r.data.database.Collection(adminMenuCollection), nil
}

// menuDocument 是 admin_menu_visibility 的持久化形态。
//
// Overrides 用 `map[string]bool` 而不是 `bson.D`：覆盖项是**集合**语义，
// 没有顺序含义。而驱动把嵌套文档解码成有序 bson.D（empty_interface_codec 的行为），
// 若这里收 any 再断言，就会踩到和 UTM extraParams 同一个坑 ——
// 写进去能读出来、换个入口读就变空。
type menuDocument struct {
	ID        string          `bson:"_id"`
	Overrides map[string]bool `bson:"overrides"`
	UpdatedBy string          `bson:"updated_by,omitempty"`
	UpdatedAt time.Time       `bson:"updated_at"`
}

// Get 读取覆盖项。文档不存在时返回空配置而不是错误 ——
// 「还没人配置过」是正常状态，不是异常。
func (r *mongoAdminMenuRepository) Get(ctx context.Context) (adminmenu.Visibility, error) {
	collection, err := r.collection()
	if err != nil {
		return adminmenu.Visibility{}, err
	}
	var document menuDocument
	readErr := collection.FindOne(
		ctx,
		bson.M{"_id": adminmenu.SettingsDocumentID},
	).Decode(&document)
	if errors.Is(readErr, mongo.ErrNoDocuments) {
		return adminmenu.Visibility{Overrides: map[string]bool{}}, nil
	}
	if readErr != nil {
		return adminmenu.Visibility{}, fmt.Errorf("read admin menu visibility: %w", readErr)
	}

	overrides := document.Overrides
	if overrides == nil {
		overrides = map[string]bool{}
	}
	updatedAt := document.UpdatedAt.UTC().Truncate(time.Millisecond)
	return adminmenu.Visibility{
		Overrides: overrides,
		UpdatedAt: &updatedAt,
		UpdatedBy: document.UpdatedBy,
	}, nil
}

// Save 覆盖写入整份配置，与 admin_audit 审计同事务。
//
// 整体覆盖而不是逐键合并：前端「节点设置」页提交的是完整草稿，
// 逐键合并会让「把某个节点改回默认」这件事无法表达
// —— 那种情况下覆盖项里该键应当消失，而合并只会保留旧值。
func (r *mongoAdminMenuRepository) Save(
	ctx context.Context,
	actor adminmenu.Actor,
	overrides map[string]bool,
) (adminmenu.Visibility, error) {
	normalized, err := adminmenu.NormalizeOverrides(overrides)
	if err != nil {
		return adminmenu.Visibility{}, err
	}
	collection, err := r.collection()
	if err != nil {
		return adminmenu.Visibility{}, err
	}

	now := menuNow()
	document := menuDocument{
		ID:        adminmenu.SettingsDocumentID,
		Overrides: normalized,
		UpdatedBy: actor.ID,
		UpdatedAt: now,
	}

	writeErr := NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if _, err := collection.ReplaceOne(
			tx,
			bson.M{"_id": adminmenu.SettingsDocumentID},
			document,
			options.Replace().SetUpsert(true),
		); err != nil {
			return fmt.Errorf("save admin menu visibility: %w", err)
		}
		_, err := r.data.database.Collection(schema.CollectionAdminAudit).InsertOne(tx, bson.M{
			"_id":        uuid.NewString(),
			"actor_id":   actor.ID,
			"target_id":  adminmenu.SettingsDocumentID,
			"action":     "admin_menu_visibility_update",
			"created_at": now,
		})
		if err != nil {
			return fmt.Errorf("write admin menu audit: %w", err)
		}
		return nil
	})
	if writeErr != nil {
		return adminmenu.Visibility{}, writeErr
	}

	updatedAt := now
	return adminmenu.Visibility{
		Overrides: normalized,
		UpdatedAt: &updatedAt,
		UpdatedBy: actor.ID,
	}, nil
}

// menuNow 返回截断到毫秒的 UTC 时间。
// BSON DateTime 的精度就是毫秒，不截断会让「刚写完的响应」和「再读一次」
// 在同一个字段上给出不同的值。
func menuNow() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}
