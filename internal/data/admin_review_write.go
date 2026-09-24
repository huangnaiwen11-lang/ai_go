package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ApplyReview is the atomic Mongo implementation of the review write
// contract.  The projection readiness fence, CAS mutation, permanent admin
// audit fact and an outbox notification all share one transaction context.
func (r *mongoAdminReviewRepository) ApplyReview(ctx context.Context, command adminreview.ReviewCommand) (adminreview.ReviewResult, error) {
	if err := validateWriteStore(r, command); err != nil {
		return adminreview.ReviewResult{}, err
	}
	var result adminreview.ReviewResult
	err := NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if err := r.ready(tx); err != nil {
			return err
		}
		if existing, found, err := r.findAudit(tx, command.IdempotencyKey, false); err != nil {
			return err
		} else if found {
			item, itemErr := r.findItem(tx, command.ItemID, command.MediaType)
			if itemErr != nil {
				return itemErr
			}
			result = adminreview.ReviewResult{Item: item, Message: reviewMessage(command.Action), ModifiedCount: 1}
			if existing.Action != command.Action || existing.ActorID != command.ActorID {
				return adminreview.ErrReviewIdempotency
			}
			return nil
		}
		item, err := r.findItem(tx, command.ItemID, command.MediaType)
		if err != nil {
			return err
		}
		expected := command.ExpectedVersion
		if expected < 0 {
			expected = item.Version
		}
		updated, ok, err := r.cas(tx, command, expected)
		if err != nil {
			return err
		}
		if !ok {
			return adminreview.ErrReviewConflict
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		audit := adminreview.ReviewAudit{IdempotencyKey: command.IdempotencyKey, ItemID: command.ItemID, MediaType: command.MediaType, Action: command.Action, ActorID: command.ActorID, ExpectedVersion: expected, Version: updated.Version, Reason: command.Reason, At: now}
		if err := r.insertAudit(tx, audit, false, nil); err != nil {
			return err
		}
		if err := r.insertOutbox(tx, command, updated, now); err != nil {
			return err
		}
		result = adminreview.ReviewResult{Item: updated, Message: reviewMessage(command.Action), ModifiedCount: 1}
		return nil
	})
	if err != nil {
		return adminreview.ReviewResult{}, err
	}
	return result, nil
}

func (r *mongoAdminReviewRepository) ApplyBatchReview(ctx context.Context, command adminreview.BatchReviewCommand) (adminreview.BatchReviewResult, error) {
	if r == nil || r.data == nil || r.data.database == nil {
		return adminreview.BatchReviewResult{}, adminreview.ErrProjectionUnavailable
	}
	if len(command.ItemIDs) == 0 || strings.TrimSpace(command.IdempotencyKey) == "" {
		return adminreview.BatchReviewResult{}, adminreview.ErrReviewInvalidCommand
	}
	var result adminreview.BatchReviewResult
	err := NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if err := r.ready(tx); err != nil {
			return err
		}
		if audit, found, err := r.findAudit(tx, command.IdempotencyKey, true); err != nil {
			return err
		} else if found {
			if audit.Action != command.Action || audit.ActorID != command.ActorID || !sameReviewIDs(audit.ItemIDs, command.ItemIDs) {
				return adminreview.ErrReviewIdempotency
			}
			items := make([]adminreview.ReviewItem, 0, len(command.ItemIDs))
			for _, id := range command.ItemIDs {
				item, err := r.findItem(tx, id, command.MediaType)
				if err != nil {
					return err
				}
				items = append(items, item)
			}
			result = adminreview.BatchReviewResult{Items: items, Message: reviewMessage(command.Action), ModifiedCount: int64(len(items))}
			return nil
		}
		items := make([]adminreview.ReviewItem, 0, len(command.ItemIDs))
		for _, id := range command.ItemIDs {
			item, err := r.findItem(tx, id, command.MediaType)
			if err != nil {
				return err
			}
			expected := int64(-1)
			if command.ExpectedVersion != nil {
				if value, ok := command.ExpectedVersion[id]; ok {
					expected = value
				}
			}
			if expected < 0 {
				expected = item.Version
			}
			single := adminreview.ReviewCommand{ItemID: id, MediaType: command.MediaType, Action: command.Action, ActorID: command.ActorID, ExpectedVersion: expected, IdempotencyKey: command.IdempotencyKey + ":" + id, Reason: command.Reason}
			updated, ok, err := r.cas(tx, single, expected)
			if err != nil {
				return err
			}
			if !ok {
				return adminreview.ErrReviewConflict
			}
			if err := r.insertAudit(tx, adminreview.ReviewAudit{IdempotencyKey: command.IdempotencyKey + ":" + id, ItemID: id, MediaType: command.MediaType, Action: command.Action, ActorID: command.ActorID, ExpectedVersion: expected, Version: updated.Version, Reason: command.Reason, At: time.Now().UTC()}, false, nil); err != nil {
				return err
			}
			if err := r.insertOutbox(tx, single, updated, time.Now().UTC()); err != nil {
				return err
			}
			items = append(items, updated)
		}
		if err := r.insertAudit(tx, adminreview.ReviewAudit{IdempotencyKey: command.IdempotencyKey, ItemID: "batch", Action: command.Action, ActorID: command.ActorID, At: time.Now().UTC()}, true, command.ItemIDs); err != nil {
			return err
		}
		result = adminreview.BatchReviewResult{Items: items, Message: reviewMessage(command.Action), ModifiedCount: int64(len(items))}
		return nil
	})
	if err != nil {
		return adminreview.BatchReviewResult{}, err
	}
	return result, nil
}

func validateWriteStore(r *mongoAdminReviewRepository, command adminreview.ReviewCommand) error {
	if r == nil || r.data == nil || r.data.database == nil {
		return adminreview.ErrProjectionUnavailable
	}
	if strings.TrimSpace(command.ItemID) == "" || strings.TrimSpace(command.IdempotencyKey) == "" {
		return adminreview.ErrReviewInvalidCommand
	}
	return nil
}

func (r *mongoAdminReviewRepository) findItem(ctx context.Context, id, mediaType string) (adminreview.ReviewItem, error) {
	filter := bson.M{"_id": strings.TrimSpace(id)}
	if mediaType != "" {
		filter["media_type"] = mediaType
	}
	var document model.AdminReviewItemDocument
	err := r.data.database.Collection(schema.CollectionAdminReviewItems).FindOne(ctx, filter).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return adminreview.ReviewItem{}, adminreview.ErrReviewNotFound
	}
	if err != nil {
		return adminreview.ReviewItem{}, fmt.Errorf("load admin review item: %w", err)
	}
	return reviewItemFromDocument(document), nil
}

func (r *mongoAdminReviewRepository) cas(ctx context.Context, command adminreview.ReviewCommand, expected int64) (adminreview.ReviewItem, bool, error) {
	set := bson.M{"updated_at": time.Now().UTC().Truncate(time.Millisecond), "version": expected + 1}
	unset := bson.M{}
	switch command.Action {
	case adminreview.ReviewActionApprove:
		set["review_status"], set["visibility_status"] = adminreview.ReviewStatusApproved, "visible"
		set["reviewed_by"], set["reviewed_at"] = command.ActorID, time.Now().UTC()
		unset["reject_reason"] = ""
	case adminreview.ReviewActionReject:
		set["review_status"], set["visibility_status"] = adminreview.ReviewStatusRejected, "hidden"
		set["reviewed_by"], set["reviewed_at"] = command.ActorID, time.Now().UTC()
		if strings.TrimSpace(command.Reason) != "" {
			set["reject_reason"] = strings.TrimSpace(command.Reason)
		}
	case adminreview.ReviewActionDelete:
		set["review_status"], set["visibility_status"] = adminreview.ReviewStatusDeleted, "hidden"
		set["reviewed_by"], set["reviewed_at"] = command.ActorID, time.Now().UTC()
	case adminreview.ReviewActionPrompt:
		if command.Prompt != nil {
			set["prompt"] = *command.Prompt
		}
		if command.NegativePrompt != nil {
			set["negative_prompt"] = *command.NegativePrompt
		}
	default:
		return adminreview.ReviewItem{}, false, adminreview.ErrReviewInvalidCommand
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	filter := bson.M{"_id": command.ItemID, "version": expected}
	if command.MediaType != "" {
		filter["media_type"] = command.MediaType
	}
	var updated model.AdminReviewItemDocument
	err := r.data.database.Collection(schema.CollectionAdminReviewItems).FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return adminreview.ReviewItem{}, false, nil
	}
	if err != nil {
		return adminreview.ReviewItem{}, false, fmt.Errorf("compare-and-swap admin review item: %w", err)
	}
	return reviewItemFromDocument(updated), true, nil
}

func (r *mongoAdminReviewRepository) findAudit(ctx context.Context, key string, batch bool) (adminreview.ReviewAudit, bool, error) {
	var raw struct {
		Action         string    `bson:"review_action"`
		ActorID        string    `bson:"actor_id"`
		ItemID         string    `bson:"target_id"`
		Version        int64     `bson:"version"`
		IdempotencyKey string    `bson:"idempotency_key"`
		ItemIDs        []string  `bson:"item_ids"`
		CreatedAt      time.Time `bson:"created_at"`
	}
	id := reviewAuditID(key, batch)
	err := r.data.database.Collection(schema.CollectionAdminAudit).FindOne(ctx, bson.M{"_id": id, "action": bson.M{"$in": bson.A{"admin_review", "admin_review_batch"}}}).Decode(&raw)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return adminreview.ReviewAudit{}, false, nil
	}
	if err != nil {
		return adminreview.ReviewAudit{}, false, fmt.Errorf("find admin review audit: %w", err)
	}
	return adminreview.ReviewAudit{IdempotencyKey: raw.IdempotencyKey, ItemID: raw.ItemID, Action: adminreview.ReviewAction(raw.Action), ActorID: raw.ActorID, Version: raw.Version, At: raw.CreatedAt, ItemIDs: append([]string(nil), raw.ItemIDs...)}, true, nil
}

func (r *mongoAdminReviewRepository) insertAudit(ctx context.Context, audit adminreview.ReviewAudit, batch bool, itemIDs []string) error {
	document := bson.M{"_id": reviewAuditID(audit.IdempotencyKey, batch), "action": map[bool]string{true: "admin_review_batch", false: "admin_review"}[batch], "actor_id": audit.ActorID, "target_id": audit.ItemID, "idempotency_key": audit.IdempotencyKey, "review_action": string(audit.Action), "version": audit.Version, "expected_version": audit.ExpectedVersion, "reason": audit.Reason, "created_at": audit.At}
	if len(itemIDs) > 0 {
		document["item_ids"] = append([]string(nil), itemIDs...)
	}
	if _, err := r.data.database.Collection(schema.CollectionAdminAudit).InsertOne(ctx, document); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return adminreview.ErrReviewIdempotency
		}
		return fmt.Errorf("write admin review audit: %w", err)
	}
	return nil
}

func sameReviewIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (r *mongoAdminReviewRepository) insertOutbox(ctx context.Context, command adminreview.ReviewCommand, item adminreview.ReviewItem, now time.Time) error {
	payload, err := json.Marshal(map[string]any{"itemId": item.ID, "mediaType": item.MediaType, "action": string(command.Action), "reviewStatus": item.ReviewStatus, "visibilityStatus": item.VisibilityStatus, "actorId": command.ActorID, "idempotencyKey": command.IdempotencyKey})
	if err != nil {
		return fmt.Errorf("encode admin review outbox payload: %w", err)
	}
	eventID := "admin.review:" + hashKey(command.IdempotencyKey+":"+item.ID)
	_, err = r.data.database.Collection(schema.CollectionOutboxEvents).InsertOne(ctx, model.OutboxEventDocument{ID: eventID, AggregateID: item.ID, EventType: "admin.review.updated", Payload: payload, DeliveryStatus: string(outbox.DeliveryStatusPending), NextAttemptAt: now, CreatedAt: now, UpdatedAt: now})
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("write admin review outbox: %w", err)
	}
	return nil
}

func reviewAuditID(key string, batch bool) string {
	prefix := "admin-review:"
	if batch {
		prefix = "admin-review-batch:"
	}
	return prefix + hashKey(key)
}

func hashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func reviewMessage(action adminreview.ReviewAction) string {
	switch action {
	case adminreview.ReviewActionApprove:
		return "Review approved"
	case adminreview.ReviewActionReject:
		return "Review rejected"
	case adminreview.ReviewActionPrompt:
		return "Prompt updated successfully"
	case adminreview.ReviewActionDelete:
		return "Review item deleted"
	default:
		return "Review updated"
	}
}

// These methods provide the narrow fallback contract used by the biz tests.
// Production Mongo calls ApplyReview/ApplyBatchReview above to keep all facts
// in one transaction.
func (r *mongoAdminReviewRepository) FindReviewItem(ctx context.Context, id string) (adminreview.ReviewItem, error) {
	if err := r.ready(ctx); err != nil {
		return adminreview.ReviewItem{}, err
	}
	return r.findItem(ctx, id, "")
}

func (r *mongoAdminReviewRepository) CompareAndSwapReview(ctx context.Context, mutation adminreview.ReviewMutation) (adminreview.ReviewItem, bool, error) {
	return r.cas(ctx, adminreview.ReviewCommand{ItemID: mutation.ItemID, MediaType: mutation.MediaType, Action: mutation.Action, ActorID: mutation.ActorID, Reason: mutation.Reason, Prompt: mutation.Prompt, NegativePrompt: mutation.NegativePrompt}, mutation.ExpectedVersion)
}

func (r *mongoAdminReviewRepository) WriteReviewAudit(ctx context.Context, audit adminreview.ReviewAudit) error {
	return r.insertAudit(ctx, audit, false, nil)
}

func (r *mongoAdminReviewRepository) FindReviewAudit(ctx context.Context, key string) (adminreview.ReviewAudit, bool, error) {
	return r.findAudit(ctx, key, false)
}

var _ adminreview.AtomicReviewWriter = (*mongoAdminReviewRepository)(nil)
var _ adminreview.ReviewWriteRepository = (*mongoAdminReviewRepository)(nil)
