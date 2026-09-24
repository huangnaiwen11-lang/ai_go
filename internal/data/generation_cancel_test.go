package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	generationcancel "ai-business-service/internal/biz/generationcancel"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoGenerationCancel已绑定B2B任务创建唯一取消事件并标记创作(t *testing.T) {
	fixture, prepared := seedProviderIntent(t)
	repository, ok := NewGenerationSubmissionRepository(fixture.data).(generation.ProviderSubmissionStore)
	if !ok {
		t.Fatal("generation submission repository lacks ProviderSubmissionStore")
	}
	if err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
		return repository.PrepareProviderSubmission(ctx, prepared)
	}); err != nil {
		t.Fatalf("PrepareProviderSubmission() error = %v", err)
	}
	binding := generation.ProviderJobBinding{
		EventID: fixture.eventID, CreationID: fixture.creationID, StepID: fixture.stepID,
		LeaseToken: fixture.leaseToken, LeaseOwner: "worker-1", Fence: 1,
		Route: prepared.Request.Route, ExternalID: "job-cancel-1", Capability: "text_to_image", At: fixture.now,
	}
	if err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
		binder, ok := repository.(generation.ProviderJobBinder)
		if !ok {
			return errors.New("generation submission repository lacks ProviderJobBinder")
		}
		return binder.BindProviderJob(ctx, binding)
	}); err != nil {
		t.Fatalf("BindProviderJob() error = %v", err)
	}

	usecase := generationcancel.NewUsecaseWithClock(NewGenerationCancelStore(fixture.data), fixture.runner, func() time.Time { return fixture.now })
	command := generationcancel.Command{CreationID: fixture.creationID, UserID: fixture.userID}
	result, err := usecase.Cancel(fixture.ctx, command)
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if !result.Accepted || result.Replayed || result.CancelEventCount != 1 || result.CreationID != fixture.creationID {
		t.Fatalf("Cancel() result = %#v", result)
	}

	var creation model.CreationDocument
	if err := fixture.creations.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.creationID}}).Decode(&creation); err != nil {
		t.Fatalf("read creation: %v", err)
	}
	if creation.Status != string(creations.CreationStatusCancelling) {
		t.Fatalf("creation status = %q, want cancelling", creation.Status)
	}
	var event model.OutboxEventDocument
	cancelEventID := generation.ProviderCancelEventID(fixture.stepID)
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: cancelEventID}}).Decode(&event); err != nil {
		t.Fatalf("read cancel event: %v", err)
	}
	payload, err := generation.ParseProviderCancelEventPayload(event.Payload)
	if err != nil {
		t.Fatalf("parse frozen cancel payload: %v", err)
	}
	if event.AggregateID != fixture.creationID || event.EventType != generation.ProviderCancelEventType || event.DeliveryStatus != string(outbox.DeliveryStatusPending) ||
		payload.JobID != binding.ExternalID || payload.AccountRef != prepared.Request.Route.AccountRef || payload.Capability != binding.Capability {
		t.Fatalf("cancel event = %#v / %#v", event, payload)
	}

	replay, err := usecase.Cancel(fixture.ctx, command)
	if err != nil {
		t.Fatalf("replay Cancel() error = %v", err)
	}
	if !replay.Accepted || !replay.Replayed || replay.CancelEventCount != 1 {
		t.Fatalf("replay result = %#v", replay)
	}
	count, err := fixture.events.CountDocuments(fixture.ctx, bson.D{{Key: "_id", Value: cancelEventID}})
	if err != nil || count != 1 {
		t.Fatalf("cancel event count = %d / %v, want 1", count, err)
	}
}

func TestMongoGenerationCancel无意图或任务时拒绝且零副作用(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	fixture.seedDispatching()
	usecase := generationcancel.NewUsecaseWithClock(NewGenerationCancelStore(fixture.data), fixture.runner, func() time.Time { return fixture.now })

	_, err := usecase.Cancel(fixture.ctx, generationcancel.Command{CreationID: fixture.creationID, UserID: fixture.userID})
	if !errors.Is(err, generationcancel.ErrNotReady) {
		t.Fatalf("Cancel() error = %v, want ErrNotReady", err)
	}
	var creation model.CreationDocument
	if err := fixture.creations.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.creationID}}).Decode(&creation); err != nil {
		t.Fatalf("read creation: %v", err)
	}
	if creation.Status != string(creations.CreationStatusPendingSubmission) {
		t.Fatalf("not-ready cancel changed creation status = %q", creation.Status)
	}
	if count, err := fixture.events.CountDocuments(fixture.ctx, bson.D{{Key: "aggregate_id", Value: fixture.creationID}, {Key: "event_type", Value: generation.ProviderCancelEventType}}); err != nil || count != 0 {
		t.Fatalf("not-ready cancel created events = %d / %v", count, err)
	}
}

func TestMongoGenerationCancel已准备未绑定任务会在绑定后补建取消事件(t *testing.T) {
	fixture, prepared := seedProviderIntent(t)
	repository, ok := NewGenerationSubmissionRepository(fixture.data).(generation.ProviderSubmissionStore)
	if !ok {
		t.Fatal("generation submission repository lacks ProviderSubmissionStore")
	}
	if err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
		return repository.PrepareProviderSubmission(ctx, prepared)
	}); err != nil {
		t.Fatalf("PrepareProviderSubmission() error = %v", err)
	}
	usecase := generationcancel.NewUsecaseWithClock(NewGenerationCancelStore(fixture.data), fixture.runner, func() time.Time { return fixture.now })
	result, err := usecase.Cancel(fixture.ctx, generationcancel.Command{CreationID: fixture.creationID, UserID: fixture.userID})
	if err != nil {
		t.Fatalf("prepared Cancel() error = %v", err)
	}
	if !result.Accepted || result.CancelEventCount != 0 || result.Replayed {
		t.Fatalf("prepared cancel result = %#v, want accepted receipt without job event", result)
	}

	binder, ok := repository.(generation.ProviderJobBinder)
	if !ok {
		t.Fatal("generation submission repository lacks ProviderJobBinder")
	}
	binding := generation.ProviderJobBinding{
		EventID: fixture.eventID, CreationID: fixture.creationID, StepID: fixture.stepID,
		LeaseToken: fixture.leaseToken, LeaseOwner: "worker-1", Fence: 1,
		Route: prepared.Request.Route, ExternalID: "job-bound-after-cancel", Capability: "text_to_image", At: fixture.now,
	}
	if err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
		return binder.BindProviderJob(ctx, binding)
	}); err != nil {
		t.Fatalf("BindProviderJob() error = %v", err)
	}

	var event model.OutboxEventDocument
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: generation.ProviderCancelEventID(fixture.stepID)}}).Decode(&event); err != nil {
		t.Fatalf("missing cancel event after binding: %v", err)
	}
	payload, err := generation.ParseProviderCancelEventPayload(event.Payload)
	if err != nil || payload.JobID != binding.ExternalID {
		t.Fatalf("cancel payload after binding = %#v / %v", payload, err)
	}
}

func TestMongoGenerationCancel非Owner一律NotFound(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	fixture.seedDispatching()
	usecase := generationcancel.NewUsecaseWithClock(NewGenerationCancelStore(fixture.data), fixture.runner, func() time.Time { return fixture.now })

	_, err := usecase.Cancel(fixture.ctx, generationcancel.Command{CreationID: fixture.creationID, UserID: "other-user"})
	if !errors.Is(err, generationcancel.ErrNotFound) {
		t.Fatalf("Cancel() error = %v, want ErrNotFound", err)
	}
}
