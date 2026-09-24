package data

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// mongoGenerationProviderInboxConsumerStore 是 inbox consumer 侧的持久化适配器。
//
// 它与 ACK 侧的 mongoGenerationProviderInboxRepository 共用同一个集合，却实现
// 两个不同的接口：ACK 只负责写入待消费事实，消费侧只负责读回事实并单向推进
// 本地生命周期。合并成一个接口会让任何一个替身实现都必须同时满足两套语义，
// 接口隔离就此失效。
type mongoGenerationProviderInboxConsumerStore struct {
	inbox *mongo.Collection
}

// NewGenerationProviderInboxConsumerStore 返回 inbox consumer 存储实现。
func NewGenerationProviderInboxConsumerStore(data *Data) generation.ProviderInboxConsumerStore {
	if data == nil || data.database == nil {
		return &mongoGenerationProviderInboxConsumerStore{}
	}
	return &mongoGenerationProviderInboxConsumerStore{inbox: data.database.Collection(schema.CollectionGenerationProviderInbox)}
}

func (store *mongoGenerationProviderInboxConsumerStore) ready() error {
	if store == nil || store.inbox == nil {
		return errors.New("generation provider inbox consumer store is not configured")
	}
	return nil
}

// ReadProviderInbox 按 receipt 身份读回已持久化的投递事实。
//
// 未找到返回 (nil, nil)：消费用例据此把「工作项存在但投递不存在」判定为显式
// 失败，而不是当成一次可重试的存储故障。查询条件与 ACK 侧写入共用同一组字段，
// 消费侧因此不需要知道 inbox 文档 ID 的构造方式。
func (store *mongoGenerationProviderInboxConsumerStore) ReadProviderInbox(ctx context.Context, key generation.ProviderInboxKey) (*generation.ProviderInboxRecord, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if err := store.ready(); err != nil {
		return nil, err
	}
	var document model.ProviderInboxDocument
	if err := store.inbox.FindOne(ctx, providerInboxKeyFilter(key)).Decode(&document); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("read provider inbox: %w", err)
	}
	record, err := providerInboxDocumentToDomain(document)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// MarkProviderInboxApplied 把一条 pending 投递单向推进为 applied。
//
// 幂等规则分三层，缺一不可：
//   - pending → applied：唯一允许的转换，写入终态摘要与业务时刻；
//   - 已 applied 且摘要一致：成功，且**不改写任何字段**（重复消费是正常重放）；
//   - 已 applied 但摘要不同、或已 quarantined：冲突，必须让调用方看见。
//
// 这里刻意不做「先读后判再写」：读-改-写会与并发消费互相覆盖，条件更新才能
// 保证只有第一次转换生效。命中 0 条时才回读一次用于区分上面三种情形。
func (store *mongoGenerationProviderInboxConsumerStore) MarkProviderInboxApplied(ctx context.Context, key generation.ProviderInboxKey, terminalDigest string, at time.Time) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if err := store.ready(); err != nil {
		return err
	}
	// 推进投递状态必须与终态 CAS 同一个事务：分开提交会让投递被标成已消费
	// 而终态并未落地，之后再也没有工作项会重放它。
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() {
		return errors.New("provider inbox consumption requires an active transaction")
	}
	if !isProviderInboxTerminalDigest(terminalDigest) || at.IsZero() {
		return errors.New("provider inbox consumption requires a terminal digest and a business time")
	}
	now := at.UTC()

	result, err := store.inbox.UpdateOne(
		ctx,
		append(providerInboxKeyFilter(key), bson.E{Key: "status", Value: string(generation.ProviderInboxPending)}),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: string(generation.ProviderInboxApplied)},
			{Key: "terminal_digest", Value: terminalDigest},
			{Key: "updated_at", Value: now},
		}}},
	)
	if err != nil {
		return fmt.Errorf("mark provider inbox applied: %w", err)
	}
	if result.MatchedCount == 1 {
		return nil
	}

	// 条件更新没命中：要么同一结论已经消费过（幂等成功），要么是一份不能被
	// 覆盖的记录。必须回读区分——一律报成功会让「冲突被静默吞掉」，一律报
	// 冲突会让正常重放变成永久失败。
	var document model.ProviderInboxDocument
	if err := store.inbox.FindOne(ctx, providerInboxKeyFilter(key)).Decode(&document); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return generation.ErrInvalidProviderInboxKey
		}
		return fmt.Errorf("read provider inbox after mark: %w", err)
	}
	if generation.ProviderInboxStatus(document.Status) == generation.ProviderInboxApplied && document.TerminalDigest == terminalDigest {
		return nil
	}
	return generation.ErrProviderInboxConflict
}

// providerInboxKeyFilter 是 receipt 身份的三元组条件，与 ACK 侧写入使用的
// 完全一致。两侧共用一个构造函数，避免「刚 ACK 的记录消费侧读不到」这类
// 只会在运行期暴露的字段漂移。
func providerInboxKeyFilter(key generation.ProviderInboxKey) bson.D {
	return bson.D{
		{Key: "source", Value: key.Source},
		{Key: "account_ref", Value: key.AccountRef},
		{Key: "delivery_id", Value: key.DeliveryID},
	}
}

// isProviderInboxTerminalDigest 与领域侧摘要格式保持一致：小写 SHA-256 十六进制。
// 在写入点重复校验一次，是为了让「写坏了摘要」立刻在这里失败，而不是等到
// 下一次读回时由领域校验报出一个与真实原因无关的通用错误。
func isProviderInboxTerminalDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

var _ generation.ProviderInboxConsumerStore = (*mongoGenerationProviderInboxConsumerStore)(nil)
