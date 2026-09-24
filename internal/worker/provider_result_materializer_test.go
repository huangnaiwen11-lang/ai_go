package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
	"ai-business-service/internal/integrations/r2"
	"ai-business-service/internal/integrations/safefetch"
)

var materializerNow = time.Date(2026, time.September, 19, 16, 0, 0, 0, time.UTC)

func TestProviderResultMaterializer将受限响应写入不可变R2再原子发布(t *testing.T) {
	target := materializerTarget(t)
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderResultMaterializeEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderResultMaterializeEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, NextAttemptAt: materializerNow, CreatedAt: materializerNow, UpdatedAt: materializerNow,
	}
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: target}
	fetcher := &materializerFetcher{response: materializerResponse("image/png", "trusted image bytes")}
	writer := &materializerWriter{}
	worker, err := NewProviderResultMaterializerWorker(queue, store, immediateTx{}, materializerRouter{}, fetcher, writer, materializerR2Config(t), "result-worker", func() time.Time { return materializerNow })
	if err != nil {
		t.Fatalf("NewProviderResultMaterializerWorker() error = %v", err)
	}

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	content := []byte("trusted image bytes")
	contentHash := sha256.Sum256(content)
	contentSHA256 := hex.EncodeToString(contentHash[:])
	tenantHash := sha256.Sum256([]byte("tenant-main"))
	tenantSHA256 := hex.EncodeToString(tenantHash[:])
	wantKey := "gen/text_to_image/" + tenantSHA256 + "/" + target.Payload.StepID + ".png"
	if fetcher.rawURL != target.Payload.ResultRef || writer.input.Key != wantKey || writer.input.ContentType != "image/png" || writer.input.MaxBytes != 1024 || writer.input.TenantSHA256 != tenantSHA256 || string(writer.body) != string(content) {
		t.Fatalf("fetch/write = url=%q input=%#v body=%q", fetcher.rawURL, writer.input, writer.body)
	}
	if len(store.publications) != 1 {
		t.Fatalf("publication count = %d, want 1", len(store.publications))
	}
	publication := store.publications[0]
	if publication.EventID != event.ID || publication.LeaseToken != event.LeaseToken || publication.Target != target || publication.StorageURL != "https://media.example.test/"+wantKey || publication.ContentSHA256 != contentSHA256 || publication.ContentType != "image/png" || publication.ContentLength != int64(len(content)) {
		t.Fatalf("publication = %#v", publication)
	}
	if len(queue.delivered) != 0 || len(queue.failed) != 0 || len(queue.requeued) != 0 {
		t.Fatalf("publication store owns atomic event settlement, outbox=%#v", queue)
	}
}

func TestProviderResultMaterializer失租时取消流式IO且不发布(t *testing.T) {
	target, event := materializerEvent(t, 1, materializerNow)
	writer := &leaseBlockingMaterializerWriter{started: make(chan struct{}), canceled: make(chan error, 1)}
	queue := &leaseLossMaterializerOutbox{
		materializerOutbox: materializerOutbox{event: event},
		writerStarted:      writer.started,
		renewalStarted:     make(chan struct{}, 1),
	}
	store := &materializerStore{target: *target}
	worker, err := NewProviderResultMaterializerWorker(queue, store, immediateTx{}, materializerRouter{},
		&materializerFetcher{response: materializerResponse("image/png", "bytes")}, writer,
		materializerR2Config(t), "result-worker", func() time.Time { return materializerNow })
	if err != nil {
		t.Fatalf("NewProviderResultMaterializerWorker() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.DeliverOnce(ctx, event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}

	select {
	case <-queue.renewalStarted:
		// The renewal deliberately waits until the write is in progress, then
		// reports lease loss. This proves the cancellation is not merely an
		// early preflight failure.
	default:
		t.Fatal("long-running materialization never attempted to renew its lease")
	}
	select {
	case err := <-writer.canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream context error = %v, want lease-driven context.Canceled", err)
		}
	default:
		t.Fatal("lease loss did not cancel the in-flight stream")
	}
	if len(store.publications) != 0 || len(queue.requeued) != 0 || len(queue.failed) != 0 || len(queue.attended) != 0 {
		t.Fatalf("lost lease must not publish or settle stale work: publications=%d requeued=%v failed=%v attended=%v", len(store.publications), queue.requeued, queue.failed, queue.attended)
	}
}

func TestProviderResultMaterializer拒绝结果域策略错误且不写R2(t *testing.T) {
	target := materializerTarget(t)
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderResultMaterializeEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderResultMaterializeEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, NextAttemptAt: materializerNow, CreatedAt: materializerNow, UpdatedAt: materializerNow,
	}
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: target}
	fetcher := &materializerFetcher{err: safefetch.ErrHostNotAllowed}
	writer := &materializerWriter{}
	worker, err := NewProviderResultMaterializerWorker(queue, store, immediateTx{}, materializerRouter{}, fetcher, writer, materializerR2Config(t), "result-worker", func() time.Time { return materializerNow })
	if err != nil {
		t.Fatalf("NewProviderResultMaterializerWorker() error = %v", err)
	}

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(writer.body) != 0 || len(store.publications) != 0 || len(queue.failed) != 1 || len(queue.requeued) != 0 {
		t.Fatalf("policy failure wrote or retried materialization: writer=%q publications=%d failed=%v requeued=%v", writer.body, len(store.publications), queue.failed, queue.requeued)
	}
}

func TestProviderResultMaterializer两步B2B仅用已写入R2的首帧激活第二步(t *testing.T) {
	target := materializerTarget(t)
	deferred, err := creations.CompileDeferredB2BImageToVideo(creations.PublishedB2BProductRecipe{
		TemplateID: "template-video-1", TemplateVersion: 1, Atom: creations.AtomImageToVideo,
		ProductKey: "video-standard", Input: []byte(`{"prompt":"camera move","durationSeconds":5}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	target.NextB2B = &generation.ProviderB2BSecondStepTarget{
		StepID:   "step-video-2",
		Route:    creations.ExecutionRoute{Provider: creations.PolarStarB2BProvider, AccountRef: "account-main", ContractVersion: creations.B2BContractVersion, MappingVersion: "video-mapping-v1"},
		Deferred: *deferred,
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("two-step target = %v", err)
	}
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{ID: generation.ProviderResultMaterializeEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderResultMaterializeEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, NextAttemptAt: materializerNow, CreatedAt: materializerNow, UpdatedAt: materializerNow,
	}
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: target}
	fetcher := &materializerFetcher{response: materializerResponse("image/png", "trusted first frame")}
	writer := &materializerWriter{}
	router := materializerB2BRouter{catalog: videoMaterializerCatalog()}
	storage := materializerR2Config(t)
	// The production adapter rejects reserved .test/.invalid hosts for a
	// provider-consumed opening frame; use a syntactically public R2 domain in
	// this two-step contract test while keeping all network I/O faked.
	storage.PublicURL = "https://media.example.com"
	worker, err := NewProviderResultMaterializerWorker(queue, store, immediateTx{}, router, fetcher, writer, storage, "result-worker", func() time.Time { return materializerNow })
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(store.publications) != 1 || store.publications[0].NextB2B == nil {
		t.Fatalf("publication = %#v, failed=%#v requeued=%#v writer=%#v", store.publications, queue.failed, queue.requeued, writer.input)
	}
	activation := store.publications[0].NextB2B
	if activation.StepID != target.NextB2B.StepID || activation.Route != target.NextB2B.Route || len(activation.Recipe.Assets) != 1 || activation.Recipe.Assets[0].Role != "opening_frame" || activation.Recipe.Assets[0].URL != store.publications[0].StorageURL {
		t.Fatalf("second activation = %#v", activation)
	}
	if _, err := mapB2BRecipe(context.Background(), router.config(), activation.Route, activation.StepID, activation.Recipe); err != nil {
		t.Fatalf("activation recipe no longer maps through frozen catalog: %v", err)
	}
}

func TestProviderResultMaterializer素材失败在预算内按15秒基数重排(t *testing.T) {
	for _, tc := range []struct {
		name    string
		attempt int32
		want    time.Duration
	}{
		{name: "首次失败", attempt: 1, want: 15 * time.Second},
		{name: "第二次失败", attempt: 2, want: 30 * time.Second},
		{name: "预算前一次", attempt: outbox.MaterialUploadRetryBudget - 1, want: 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, event := materializerEvent(t, tc.attempt, materializerNow)
			queue := &materializerOutbox{event: event}
			store := &materializerStore{target: *target}
			writer := &materializerWriter{err: errors.New("r2 temporarily unavailable")}
			worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, writer)

			if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
				t.Fatalf("DeliverOnce() error = %v", err)
			}
			if len(queue.requeued) != 1 || !queue.requeued[0].Equal(materializerNow.Add(tc.want)) {
				t.Fatalf("requeue = %v, want %v (素材必须用 15s 基数，不是提交链路的 10s)", queue.requeued, materializerNow.Add(tc.want))
			}
			if len(queue.failed) != 0 || len(queue.attended) != 0 || len(store.pendings) != 0 {
				t.Fatalf("预算内失败不该终结: failed=%v attended=%v pendings=%d", queue.failed, queue.attended, len(store.pendings))
			}
		})
	}
}

func TestProviderResultMaterializer素材预算耗尽转入人工关注并记录待补传(t *testing.T) {
	target, event := materializerEvent(t, outbox.MaterialUploadRetryBudget, materializerNow)
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: *target}
	writer := &materializerWriter{err: errors.New("r2 temporarily unavailable")}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, writer)

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.attended) != 1 || queue.attended[0] != outbox.AttentionReasonMaterialUploadBudget {
		t.Fatalf("attended = %v, want 素材预算耗尽", queue.attended)
	}
	if len(queue.failed) != 0 {
		t.Fatalf("needs_attention 不得被实现为 failed: %v", queue.failed)
	}
	if len(queue.requeued) != 0 {
		t.Fatalf("预算耗尽后不得继续重排: %v", queue.requeued)
	}
	if len(store.pendings) != 1 {
		t.Fatalf("pending 标记数 = %d, want 1", len(store.pendings))
	}
	pending := store.pendings[0]
	if pending.Payload != target.Payload || pending.Reason != generation.ProviderResultUploadPendingBudgetExhausted || !pending.At.Equal(materializerNow) {
		t.Fatalf("pending = %#v", pending)
	}
	// 不得把 provider 地址当成果发布：素材失败时没有任何 publication。
	if len(store.publications) != 0 {
		t.Fatalf("素材失败却发生了发布: %#v", store.publications)
	}
}

func TestProviderResultMaterializer依赖失败不消耗素材预算(t *testing.T) {
	target, event := materializerEvent(t, outbox.MaterialUploadRetryBudget, materializerNow)
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: *target, publishErr: errors.New("mongo unavailable")}
	writer := &materializerWriter{}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, writer)

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	// 发布事务失败时素材可能根本没被尝试过，因此既不能按素材预算终结，
	// 也不能标记待补传——那会把一次依赖故障误报成补传积压。
	if len(queue.attended) != 0 || len(queue.failed) != 0 || len(queue.requeued) != 1 {
		t.Fatalf("依赖失败处理错误: attended=%v failed=%v requeued=%v", queue.attended, queue.failed, queue.requeued)
	}
	if len(store.pendings) != 0 {
		t.Fatalf("依赖失败不该标记待补传: %#v", store.pendings)
	}
}

// R2 owns the immutable bytes before Mongo publishes the user asset.  A
// transient publication-transaction error must therefore leave the event
// retryable; the next delivery may write the same immutable key again, but it
// must produce only one successful user publication.
func TestProviderResultMaterializer发布事务失败后事件可重试且不重复发布(t *testing.T) {
	target, event := materializerEvent(t, 1, materializerNow)
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: *target, publishFailures: 1}
	writer := &materializerWriter{}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "immutable bytes")}, writer)

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("first DeliverOnce() error = %v", err)
	}
	if writer.calls != 1 || len(store.publications) != 0 || len(queue.requeued) != 1 || len(queue.failed) != 0 || len(queue.attended) != 0 {
		t.Fatalf("failed publication was not retryable: writes=%d publications=%d requeued=%v failed=%v attended=%v", writer.calls, len(store.publications), queue.requeued, queue.failed, queue.attended)
	}

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("retry DeliverOnce() error = %v", err)
	}
	if writer.calls != 2 || writer.physicalWrites != 1 {
		t.Fatalf("retry should stream the immutable result again, writes=%d", writer.calls)
	}
	if len(store.publications) != 1 {
		t.Fatalf("retry duplicated user publication: %#v", store.publications)
	}
	if len(queue.failed) != 0 || len(queue.attended) != 0 {
		t.Fatalf("successful retry was terminalized incorrectly: failed=%v attended=%v", queue.failed, queue.attended)
	}
}

func TestProviderResultMaterializer依赖失败超过最大事件年龄转入人工关注(t *testing.T) {
	target, event := materializerEvent(t, 1, materializerNow.Add(-outbox.MaxEventAge))
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: *target, publishErr: errors.New("mongo unavailable")}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, &materializerWriter{})

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.attended) != 1 || queue.attended[0] != outbox.AttentionReasonEventAge {
		t.Fatalf("attended = %v, want 事件超龄", queue.attended)
	}
	if len(queue.requeued) != 0 || len(queue.failed) != 0 {
		t.Fatalf("超龄后不得继续重排或判失败: requeued=%v failed=%v", queue.requeued, queue.failed)
	}
	// 超龄的依赖失败不代表成果缺失，不得标记待补传。
	if len(store.pendings) != 0 {
		t.Fatalf("依赖类超龄不该标记待补传: %#v", store.pendings)
	}
}

func TestProviderResultMaterializer待补传标记失败时不终结自动领取(t *testing.T) {
	target, event := materializerEvent(t, outbox.MaterialUploadRetryBudget, materializerNow)
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: *target, pendingErr: errors.New("mongo unavailable")}
	writer := &materializerWriter{err: errors.New("r2 temporarily unavailable")}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, writer)

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	// 顺序与失败处理：先把事实落库，再放弃租约。标记失败时宁可继续重试，
	// 也不能留下一个不可领取、却查不到原因的事件。
	if len(queue.attended) != 0 {
		t.Fatalf("标记失败却终结了自动领取: %v", queue.attended)
	}
	if len(queue.requeued) != 1 {
		t.Fatalf("标记失败后应继续重排: %v", queue.requeued)
	}
}

func TestProviderResultMaterializer永久素材错误不进入预算也不标记待补传(t *testing.T) {
	target, event := materializerEvent(t, outbox.MaterialUploadRetryBudget, materializerNow)
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: *target}
	writer := &materializerWriter{err: r2.ErrInvalidObjectRequest}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, writer)

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.failed) != 1 || len(queue.requeued) != 0 || len(queue.attended) != 0 {
		t.Fatalf("确定性素材错误应直接失败: failed=%v requeued=%v attended=%v", queue.failed, queue.requeued, queue.attended)
	}
	if len(store.pendings) != 0 {
		t.Fatalf("确定性错误不是补传积压，不该标记待补传: %#v", store.pendings)
	}
}

func TestProviderResultMaterializer超过冻结上限直接失败且不标记待补传(t *testing.T) {
	target, event := materializerEvent(t, outbox.MaterialUploadRetryBudget-1, materializerNow)
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: *target}
	// The streamed ceiling is a property of the provider response, so the frozen
	// route limit must fail closed instead of consuming the material-upload
	// budget and eventually opening an operator ticket.
	writer := &materializerWriter{err: r2.ErrImmutableStreamTooLarge}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, writer)

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.failed) != 1 || len(queue.requeued) != 0 || len(queue.attended) != 0 {
		t.Fatalf("超过上限应直接失败: failed=%v requeued=%v attended=%v", queue.failed, queue.requeued, queue.attended)
	}
	if len(store.publications) != 0 || len(store.pendings) != 0 {
		t.Fatalf("未发布也不该标记待补传: publications=%#v pendings=%#v", store.publications, store.pendings)
	}
}

func TestProviderResultMaterializer转入attention后发出可采集告警(t *testing.T) {
	target, event := materializerEvent(t, outbox.MaterialUploadRetryBudget, materializerNow)
	queue := &materializerOutbox{event: event}
	store := &materializerStore{target: *target}
	alerter := &recordingAttentionAlerter{}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, &materializerWriter{err: errors.New("r2 temporarily unavailable")})
	worker.SetAttentionAlerter(alerter)

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.attended) != 1 || queue.attended[0] != outbox.AttentionReasonMaterialUploadBudget {
		t.Fatalf("attended = %v", queue.attended)
	}
	if len(alerter.events) != 1 {
		t.Fatalf("alert count = %d, want exactly one signal per transition", len(alerter.events))
	}
	got := alerter.events[0]
	if got.EventID != event.ID || got.AggregateID != event.AggregateID || got.EventType != string(event.EventType) ||
		got.Reason != outbox.AttentionReasonMaterialUploadBudget || got.Attempt != event.AttemptCount || !got.At.Equal(materializerNow) {
		t.Fatalf("attention = %#v", got)
	}
}

func TestProviderResultMaterializer标记未落库时不发告警(t *testing.T) {
	target, event := materializerEvent(t, outbox.MaterialUploadRetryBudget, materializerNow)
	queue := &materializerOutbox{event: event, attendErr: errors.New("mongo unavailable")}
	store := &materializerStore{target: *target}
	alerter := &recordingAttentionAlerter{}
	worker := materializerWorkerForFailure(t, queue, store, &materializerFetcher{response: materializerResponse("image/png", "bytes")}, &materializerWriter{err: errors.New("r2 temporarily unavailable")})
	worker.SetAttentionAlerter(alerter)

	if err := worker.DeliverOnce(context.Background(), event.ID); err == nil {
		t.Fatal("DeliverOnce() error = nil, want the marker failure")
	}
	// 事件仍在重试路径上；此时告警会把运维引向一个并不在 needs_attention 的事件。
	if len(alerter.events) != 0 {
		t.Fatalf("alert fired without a durable transition: %#v", alerter.events)
	}
}

func TestProviderResultMaterializer重驱后重新获得完整预算与年龄(t *testing.T) {
	// 原始预算（AttemptCount 已超）与原始年龄（CreatedAt 已超 72h）都已耗尽。
	createdAt := materializerNow.Add(-72 * time.Hour)
	exhausted := func(event *outbox.Event) {
		event.AttemptCount = outbox.MaterialUploadRetryBudget + 3
	}

	// 对照：没有重驱窗口时，这个事件必须立刻再次进入人工关注 —— 这正是为什么
	// 「把 needs_attention 改回 pending」的朴素重驱等于空操作。
	target, plain := materializerEvent(t, 1, createdAt)
	exhausted(plain)
	plainQueue := &materializerOutbox{event: plain}
	plainWorker := materializerWorkerForFailure(t, plainQueue, &materializerStore{target: *target},
		&materializerFetcher{response: materializerResponse("image/png", "bytes")},
		&materializerWriter{err: errors.New("r2 temporarily unavailable")})
	if err := plainWorker.DeliverOnce(context.Background(), plain.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(plainQueue.attended) != 1 {
		t.Fatalf("未重驱的耗尽事件应当立刻再次终态: attended=%v", plainQueue.attended)
	}

	// 被人工重驱过的事件必须拿到完整的新窗口：继续重排，而不是立刻终态。
	target, redriven := materializerEvent(t, 1, createdAt)
	exhausted(redriven)
	redriven.RedriveCount = 1
	redriven.RedriveStartedAt = materializerNow.Add(-time.Minute)
	redriven.RedriveAttemptBase = redriven.AttemptCount
	queue := &materializerOutbox{event: redriven}
	worker := materializerWorkerForFailure(t, queue, &materializerStore{target: *target},
		&materializerFetcher{response: materializerResponse("image/png", "bytes")},
		&materializerWriter{err: errors.New("r2 temporarily unavailable")})
	if err := worker.DeliverOnce(context.Background(), redriven.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.attended) != 0 {
		t.Fatalf("重驱后不应立刻再次终态: attended=%v", queue.attended)
	}
	if len(queue.requeued) != 1 || len(queue.failed) != 0 {
		t.Fatalf("重驱后应重新排入素材重试: requeued=%v failed=%v", queue.requeued, queue.failed)
	}
}

func materializerEvent(t *testing.T, attempt int32, createdAt time.Time) (*generation.ProviderResultMaterializationTarget, *outbox.Event) {
	t.Helper()
	target := materializerTarget(t)
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderResultMaterializeEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderResultMaterializeEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, AttemptCount: attempt,
		NextAttemptAt: createdAt, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	return &target, event
}

func materializerWorkerForFailure(t *testing.T, queue *materializerOutbox, store *materializerStore, fetcher *materializerFetcher, writer *materializerWriter) *ProviderResultMaterializerWorker {
	t.Helper()
	worker, err := NewProviderResultMaterializerWorker(queue, store, immediateTx{}, materializerRouter{}, fetcher, writer, materializerR2Config(t), "result-worker", func() time.Time { return materializerNow })
	if err != nil {
		t.Fatalf("NewProviderResultMaterializerWorker() error = %v", err)
	}
	return worker
}

func materializerTarget(t *testing.T) generation.ProviderResultMaterializationTarget {
	t.Helper()
	resultRef := "https://results.example.test/job-result.png"
	payload := generation.ProviderResultMaterializeEventPayload{
		CreationID: "creation-result-1", StepID: "step-result-1", Provider: creations.PolarStarB2BProvider, AccountRef: "account-main",
		JobID: "job-result-1", Capability: "text_to_image", ResultRef: resultRef, TerminalVersion: 1,
		TerminalDigest: generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalCompleted, resultRef),
	}
	target := generation.ProviderResultMaterializationTarget{Payload: payload, Sequence: 1}
	if err := target.Validate(); err != nil {
		t.Fatalf("invalid target fixture: %v", err)
	}
	return target
}

func materializerResponse(contentType, body string) *safefetch.Response {
	return &safefetch.Response{Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func materializerR2Config(t *testing.T) r2.Config {
	t.Helper()
	config := r2.Config{AccountID: "account1", AccessKeyID: "access1", SecretAccessKey: "secret1", Region: "auto", PublicBucket: "cling-ai", PublicURL: "https://media.example.test"}
	if err := config.Validate(); err != nil {
		t.Fatalf("invalid R2 config fixture: %v", err)
	}
	return config
}

type immediateTx struct{}

func (immediateTx) WithinTx(ctx context.Context, callback func(context.Context) error) error {
	return callback(ctx)
}

var _ shared.TxRunner = immediateTx{}

type materializerOutbox struct {
	event     *outbox.Event
	delivered []string
	failed    []string
	requeued  []time.Time
	attended  []outbox.AttentionReason
	attendErr error
}

func (*materializerOutbox) Enqueue(context.Context, *outbox.Event) error {
	return outbox.ErrInvalidEvent
}
func (queue *materializerOutbox) Claim(context.Context, string, time.Time, time.Time) (*outbox.Event, error) {
	return queue.event, nil
}
func (queue *materializerOutbox) ClaimByType(_ context.Context, _ string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	if queue.event != nil {
		queue.event.DeliveryStatus, queue.event.LeaseToken, queue.event.LeaseOwner, queue.event.LeaseUntil = outbox.DeliveryStatusDispatching, "lease-result", "result-worker", leaseUntil
	}
	return queue.event, nil
}
func (queue *materializerOutbox) ClaimByIDAndType(_ context.Context, _ string, _ string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	return queue.ClaimByType(context.Background(), "", eventType, now, leaseUntil)
}
func (queue *materializerOutbox) RenewLease(_ context.Context, eventID, leaseToken string, now, leaseUntil time.Time) error {
	if queue.event == nil || queue.event.ID != eventID || queue.event.LeaseToken != leaseToken || !queue.event.LeaseUntil.After(now) || !leaseUntil.After(now) {
		return outbox.ErrLeaseConflict
	}
	queue.event.LeaseUntil = leaseUntil
	return nil
}
func (queue *materializerOutbox) Requeue(_ context.Context, eventID, _ string, next time.Time) error {
	queue.requeued = append(queue.requeued, next)
	return nil
}
func (queue *materializerOutbox) MarkDelivered(_ context.Context, eventID, _ string, _ time.Time) error {
	queue.delivered = append(queue.delivered, eventID)
	return nil
}
func (queue *materializerOutbox) MarkFailed(_ context.Context, eventID, _ string, _ time.Time) error {
	queue.failed = append(queue.failed, eventID)
	return nil
}
func (queue *materializerOutbox) MarkNeedsAttention(_ context.Context, eventID, _ string, reason outbox.AttentionReason, _ time.Time) error {
	if queue.attendErr != nil {
		return queue.attendErr
	}
	queue.attended = append(queue.attended, reason)
	return nil
}

// RedriveAttention 只是接口占位：重驱语义由 internal/data 的 Mongo 实现与
// outbox 的重驱用例测试覆盖，本包的用例不驱动人工重驱。
func (queue *materializerOutbox) RedriveAttention(context.Context, outbox.RedriveCommand) (*outbox.Event, error) {
	return nil, outbox.ErrRedriveNotEligible
}

var _ outbox.Repository = (*materializerOutbox)(nil)

type materializerStore struct {
	target          generation.ProviderResultMaterializationTarget
	loadErr         error
	publishErr      error
	publishFailures int
	pendingErr      error
	publications    []generation.ProviderResultPublication
	pendings        []generation.ProviderResultUploadPending
}

func (store *materializerStore) LoadProviderResultMaterialization(_ context.Context, payload generation.ProviderResultMaterializeEventPayload) (generation.ProviderResultMaterializationTarget, error) {
	if store.loadErr != nil {
		return generation.ProviderResultMaterializationTarget{}, store.loadErr
	}
	if payload != store.target.Payload {
		return generation.ProviderResultMaterializationTarget{}, generation.ErrProviderResultPublicationConflict
	}
	return store.target, nil
}
func (store *materializerStore) PublishProviderResultMaterialization(_ context.Context, publication generation.ProviderResultPublication) error {
	if store.publishFailures > 0 {
		store.publishFailures--
		return errors.New("simulated Mongo publication transaction failure")
	}
	if store.publishErr != nil {
		return store.publishErr
	}
	store.publications = append(store.publications, publication)
	return nil
}
func (store *materializerStore) MarkProviderResultUploadPending(_ context.Context, pending generation.ProviderResultUploadPending) error {
	if store.pendingErr != nil {
		return store.pendingErr
	}
	store.pendings = append(store.pendings, pending)
	return nil
}

var _ generation.ProviderResultMaterializationStore = (*materializerStore)(nil)

type materializerRouter struct{}

func (materializerRouter) ProviderForRoute(provider, accountRef string) (platform.ProviderHandle, error) {
	if provider != creations.PolarStarB2BProvider || accountRef != "account-main" {
		return platform.ProviderHandle{}, errors.New("unexpected route")
	}
	client, err := polarstarb2b.NewClient(polarstarb2b.ClientOptions{BaseURL: "https://api.example.test", APIKey: "test-key", AccountRef: accountRef, ExpectedTenantID: "tenant-main"})
	if err != nil {
		return platform.ProviderHandle{}, err
	}
	return platform.ProviderHandle{ID: creations.PolarStarB2BProvider, AccountRef: accountRef, TenantID: "tenant-main", MaxResultBytes: 1024, ResultHostAllowlist: []string{"results.example.test"}, B2B: client}, nil
}

type materializerB2BRouter struct {
	catalog bizgeneration.PublishedMappingCatalog
}

func (router materializerB2BRouter) ProviderForRoute(provider, accountRef string) (platform.ProviderHandle, error) {
	return materializerRouter{}.ProviderForRoute(provider, accountRef)
}

func (router materializerB2BRouter) B2BSubmission() (platform.B2BSubmissionConfig, bool) {
	return router.config(), true
}

func (router materializerB2BRouter) config() platform.B2BSubmissionConfig {
	return platform.B2BSubmissionConfig{Catalogs: materializerCatalogStore{catalog: router.catalog}, Callback: polarstarb2b.CallbackConfig{Mode: "lookup_only"}}
}

type materializerCatalogStore struct {
	catalog bizgeneration.PublishedMappingCatalog
}

func (store materializerCatalogStore) PublishedCatalog(_ context.Context, version string) (*bizgeneration.PublishedMappingCatalog, error) {
	if version != store.catalog.Version {
		return nil, bizgeneration.ErrMappingCatalogUnavailable
	}
	catalog := store.catalog
	return &catalog, nil
}

func videoMaterializerCatalog() bizgeneration.PublishedMappingCatalog {
	return bizgeneration.PublishedMappingCatalog{
		Version: "video-mapping-v1", SourceVersion: "source-video-v1", PublishedAt: materializerNow,
		Entries: []bizgeneration.ModelMappingEntry{{
			Capability: "image_to_video", ProductKey: "video-standard", PublicModel: "ps-video-v1",
			AllowedInputs: []string{"prompt", "durationSeconds", "imageUrl"}, Durations: []int{5}, Enabled: true,
		}},
	}
}

type materializerFetcher struct {
	response *safefetch.Response
	err      error
	rawURL   string
}

func (fetcher *materializerFetcher) Fetch(_ context.Context, _ platform.ProviderHandle, rawURL string) (*safefetch.Response, error) {
	fetcher.rawURL = rawURL
	if fetcher.err != nil {
		return nil, fetcher.err
	}
	return fetcher.response, nil
}

type materializerWriter struct {
	input          r2.ImmutableStreamInput
	body           []byte
	calls          int
	physicalWrites int
	objects        map[string]r2.ImmutableStreamedObject
	err            error
}

// StreamImmutable mirrors the client contract the worker relies on: the body is
// consumed exactly once and the content identity is reported back to the caller
// instead of being baked into the key.
func (writer *materializerWriter) StreamImmutable(_ context.Context, input r2.ImmutableStreamInput, source io.Reader) (r2.ImmutableStreamedObject, error) {
	writer.calls++
	writer.input = input
	writer.body, _ = io.ReadAll(source)
	if writer.err != nil {
		return r2.ImmutableStreamedObject{}, writer.err
	}
	digest := sha256.Sum256(writer.body)
	committed := r2.ImmutableStreamedObject{
		Key: input.Key, ContentSHA256: hex.EncodeToString(digest[:]), ContentLength: int64(len(writer.body)),
	}
	if writer.objects == nil {
		writer.objects = make(map[string]r2.ImmutableStreamedObject)
	}
	if existing, ok := writer.objects[input.Key]; ok {
		return existing, nil
	}
	writer.physicalWrites++
	writer.objects[input.Key] = committed
	return committed, nil
}

// leaseLossMaterializerOutbox makes the renewal fail only after the stream has
// started. A synchronous preflight renewal could not satisfy this test: the
// materializer needs a watchdog that owns cancellation while I/O is active.
type leaseLossMaterializerOutbox struct {
	materializerOutbox
	writerStarted  <-chan struct{}
	renewalStarted chan struct{}
}

func (queue *leaseLossMaterializerOutbox) RenewLease(ctx context.Context, _, _ string, _, _ time.Time) error {
	select {
	case queue.renewalStarted <- struct{}{}:
	default:
	}
	select {
	case <-queue.writerStarted:
		return outbox.ErrLeaseConflict
	case <-ctx.Done():
		return ctx.Err()
	}
}

// leaseBlockingMaterializerWriter represents the R2 streaming leg. It returns
// only when the worker-owned context is cancelled, so a test cannot pass by
// merely abandoning the later Mongo publication transaction.
type leaseBlockingMaterializerWriter struct {
	started  chan struct{}
	canceled chan error
}

func (writer *leaseBlockingMaterializerWriter) StreamImmutable(ctx context.Context, _ r2.ImmutableStreamInput, _ io.Reader) (r2.ImmutableStreamedObject, error) {
	close(writer.started)
	<-ctx.Done()
	err := ctx.Err()
	writer.canceled <- err
	return r2.ImmutableStreamedObject{}, err
}
