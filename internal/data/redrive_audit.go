package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/biz/redrive"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// NewRedriveEventStore 返回人工重驱所需的最小发件箱边界。
//
// 它比 NewOutboxRepository 多一个只读的 FindForRedrive，但**不**并进
// outbox.Repository：那个接口被大量普通调用方与替身实现，重驱是唯一的窄用例，
// 不该为它扩大公共契约。
func NewRedriveEventStore(data *Data) redrive.EventStore {
	return &mongoOutboxRepository{collection: data.database.Collection(schema.CollectionOutboxEvents)}
}

// redriveAuditDocument 是 admin_audit 里的人工重驱记录。
//
// 它刻意不含 payload：事件载荷是未受信的 provider 数据，审计表没有理由持有它。
// 记录本身自足到能回放一次重驱结果（次数、窗口基线、时刻、事件身份），
// 因此重放不需要再读事件。
type redriveAuditDocument struct {
	ID                string    `bson:"_id"`
	Action            string    `bson:"action"`
	ActorID           string    `bson:"actor_id"`
	EventKey          string    `bson:"event_key"`
	ExpectedCount     int32     `bson:"expected_redrive_count"`
	TargetID          string    `bson:"target_id"`
	AggregateID       string    `bson:"aggregate_id"`
	EventType         string    `bson:"event_type"`
	Reason            string    `bson:"reason"`
	WindowAttemptBase int32     `bson:"window_attempt_base"`
	RedriveCount      int32     `bson:"redrive_count"`
	CreatedAt         time.Time `bson:"created_at"`
}

type mongoRedriveAuditWriter struct {
	collection *mongo.Collection
}

// NewRedriveAuditWriter 返回人工重驱审计的实现。
//
// 它复用既有的 admin_audit 集合而不是另开一张表：房规就是「敏感人工动作进
// admin_audit」。该集合不设 TTL —— 它是人工介入的永久证据，与 outbox 的分档
// 保留期完全分离。
func NewRedriveAuditWriter(data *Data) redrive.AuditWriter {
	return &mongoRedriveAuditWriter{collection: data.database.Collection(schema.CollectionAdminAudit)}
}

// FindAudit 按幂等键读取既有审计。它同时限定 action，避免别的入口恰好用了
// 同一个 _id 前缀时被误认为重驱记录。
func (writer *mongoRedriveAuditWriter) FindAudit(ctx context.Context, key redrive.AuditKey) (redrive.Audit, bool, error) {
	var document redriveAuditDocument
	err := writer.collection.FindOne(ctx, bson.D{
		{Key: "_id", Value: redrive.AuditID(key)},
		{Key: "action", Value: redrive.RedriveAuditAction},
	}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return redrive.Audit{}, false, nil
	}
	if err != nil {
		return redrive.Audit{}, false, fmt.Errorf("find redrive audit: %w", err)
	}
	return document.toBiz(key), true, nil
}

// WriteAudit 写入审计。它与事件 CAS 共用调用方传入的事务上下文，
// 因此两者要么一起成功，要么一起回滚。
func (writer *mongoRedriveAuditWriter) WriteAudit(ctx context.Context, audit redrive.Audit) error {
	if err := audit.Validate(); err != nil {
		return err
	}
	_, err := writer.collection.InsertOne(ctx, redriveAuditDocument{
		ID:                redrive.AuditID(audit.Key),
		Action:            redrive.RedriveAuditAction,
		ActorID:           audit.Key.ActorID,
		EventKey:          audit.Key.EventKey,
		ExpectedCount:     audit.Key.ExpectedRedriveCount,
		TargetID:          audit.EventID,
		AggregateID:       audit.AggregateID,
		EventType:         audit.EventType,
		Reason:            audit.Reason,
		WindowAttemptBase: audit.WindowAttemptBase,
		RedriveCount:      audit.RedriveCount,
		CreatedAt:         audit.At.UTC(),
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// 幂等键是审计的唯一载体：重复键说明并发请求抢到了同一条记录。
			// 绝不覆盖既有证据，由调用方按冲突处理。
			return fmt.Errorf("%w: duplicate redrive audit", redrive.ErrRedriveAuditConflict)
		}
		return fmt.Errorf("write redrive audit: %w", err)
	}
	return nil
}

func (document redriveAuditDocument) toBiz(key redrive.AuditKey) redrive.Audit {
	return redrive.Audit{
		Key: key, EventID: document.TargetID, AggregateID: document.AggregateID, EventType: document.EventType,
		Reason: document.Reason, WindowAttemptBase: document.WindowAttemptBase,
		RedriveCount: document.RedriveCount, At: document.CreatedAt,
	}
}

var _ redrive.AuditWriter = (*mongoRedriveAuditWriter)(nil)
