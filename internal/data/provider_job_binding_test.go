package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestProviderJobBindingCASPersistsIdentityAndWakesRecovery(t *testing.T) {
	f, command := seedProviderIntent(t)
	store := providerIntentStore(t, f)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		return store.PrepareProviderSubmission(ctx, command)
	}); err != nil {
		t.Fatalf("prepare provider submission: %v", err)
	}
	binder, ok := store.(generation.ProviderJobBinder)
	if !ok {
		t.Fatal("submission repository does not implement ProviderJobBinder")
	}
	at := f.now.Add(10 * time.Second)
	binding := generation.ProviderJobBinding{
		EventID: command.EventID, CreationID: command.CreationID, StepID: command.Request.StepID,
		LeaseToken: command.LeaseToken, LeaseOwner: command.LeaseOwner, Fence: command.Fence,
		Route: command.Request.Route, ExternalID: "polar-job-1", Capability: command.Request.Capability, At: at,
	}
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return binder.BindProviderJob(ctx, binding) }); err != nil {
		t.Fatalf("bind provider job: %v", err)
	}
	var step bson.M
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": command.Request.StepID}).Decode(&step); err != nil {
		t.Fatal(err)
	}
	if step["external_execution_id"] != binding.ExternalID || step["submit_status"] != string(creations.StepSubmitStatusSubmitted) {
		t.Fatalf("step after binding = %#v", step)
	}
	var event bson.M
	if err := f.events.FindOne(f.ctx, bson.M{"_id": command.EventID}).Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event["job_id"] != binding.ExternalID || event["delivery_status"] != string(outbox.DeliveryStatusDelivered) || event["lease_token"] != nil {
		t.Fatalf("submission event after binding = %#v", event)
	}
	var recovery bson.M
	if err := f.events.FindOne(f.ctx, bson.M{"_id": "generation.reconcile:" + command.Request.StepID}).Decode(&recovery); err != nil {
		t.Fatal(err)
	}
	if got := valueTime(recovery["next_attempt_at"]); !got.Equal(at) {
		t.Fatalf("recovery next_attempt_at = %#v, want %s", recovery["next_attempt_at"], at)
	}
}

func TestProviderJobBindingRejectsOldLeaseWithoutOverwritingNewLease(t *testing.T) {
	f, command := seedProviderIntent(t)
	store := providerIntentStore(t, f)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return store.PrepareProviderSubmission(ctx, command) }); err != nil {
		t.Fatalf("prepare provider submission: %v", err)
	}
	// Simulate a re-claimed event: the old worker still holds the original
	// token, while the durable event now belongs to a newer lease/fence.
	newToken, newOwner := "lease-new", "worker-new"
	if _, err := f.events.UpdateOne(f.ctx, bson.M{"_id": command.EventID}, bson.M{"$set": bson.M{
		"lease_token": newToken, "lease_owner": newOwner, "attempt_count": command.Fence + 1,
	}}); err != nil {
		t.Fatal(err)
	}
	binder := store.(generation.ProviderJobBinder)
	binding := generation.ProviderJobBinding{
		EventID: command.EventID, CreationID: command.CreationID, StepID: command.Request.StepID,
		LeaseToken: command.LeaseToken, LeaseOwner: command.LeaseOwner, Fence: command.Fence,
		Route: command.Request.Route, ExternalID: "polar-job-old", Capability: command.Request.Capability, At: f.now,
	}
	err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return binder.BindProviderJob(ctx, binding) })
	if !errors.Is(err, generation.ErrSubmissionConflict) && !errors.Is(err, generation.ErrProviderJobBindingConflict) {
		t.Fatalf("old lease binding error = %v", err)
	}
	var step bson.M
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": command.Request.StepID}).Decode(&step); err != nil {
		t.Fatal(err)
	}
	if _, exists := step["external_execution_id"]; exists || step["submit_status"] != "submitting" {
		t.Fatalf("old lease changed step = %#v", step)
	}
}

// Bind 是 recovery event 的第四个生产者：如果它只按 ID/type 唤醒，损坏的
// aggregate 或 payload 会在“绑定成功”后继续被 lookup worker 以错误归属处理。
func TestProviderJobBindingRejectsInconsistentRecoveryIdentity(t *testing.T) {
	for _, field := range []string{"aggregate_id", "payload"} {
		t.Run(field, func(t *testing.T) {
			f, command := seedPreparedProviderIntent(t)
			if _, err := f.events.UpdateOne(f.ctx, bson.M{"_id": generation.ProviderInboxRecoveryEventID(f.stepID)}, bson.M{"$set": bson.M{field: "wrong"}}); err != nil {
				t.Fatal(err)
			}
			binding := generation.ProviderJobBinding{
				EventID: command.EventID, CreationID: command.CreationID, StepID: command.Request.StepID,
				LeaseToken: command.LeaseToken, LeaseOwner: command.LeaseOwner, Fence: command.Fence,
				Route: command.Request.Route, ExternalID: "polar-job-identity", Capability: command.Request.Capability, At: f.now,
			}
			err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
				return providerIntentStore(t, f).(generation.ProviderJobBinder).BindProviderJob(ctx, binding)
			})
			if !errors.Is(err, generation.ErrProviderJobBindingConflict) {
				t.Fatalf("inconsistent recovery %s accepted: %v", field, err)
			}
			var step bson.M
			if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
				t.Fatal(err)
			}
			if step["external_execution_id"] != nil || step["submit_status"] != "submitting" {
				t.Fatalf("failed binding escaped transaction: %#v", step)
			}
		})
	}
}
