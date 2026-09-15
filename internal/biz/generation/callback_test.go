package generation

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/executionv2"
)

func TestHandleCallback完成在事务中只确认受控终态(t *testing.T) {
	fixture := newCallbackFixture(t)
	fixture.addSubmittedStep("creation-1", "step-image", "job-image", executionv2.CapabilityTextToImage)
	event := completedCallback("step-image", "job-image", executionv2.CapabilityTextToImage, "image", "nonce-image")

	if err := fixture.usecase.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	fixture.assertCompleted(t, "creation-1", "step-image")
	fixture.assertCompleteInTransaction(t, 1)
	if fixture.reverser.calls != 0 {
		t.Fatalf("ReverseInTx calls = %d, want 0", fixture.reverser.calls)
	}

	if err := fixture.usecase.Handle(context.Background(), event); err != nil {
		t.Fatalf("replayed Handle() error = %v", err)
	}
	if fixture.store.completeWrites != 1 || fixture.store.assets["step-image"] != 1 {
		t.Fatalf("complete writes/assets = %d/%d, want 1/1", fixture.store.completeWrites, fixture.store.assets["step-image"])
	}
	fixture.assertCompleteInTransaction(t, 2)
}

func TestHandleCallback完成I2V收敛创作成功(t *testing.T) {
	fixture := newCallbackFixture(t)
	fixture.addSubmittedStep("creation-1", "step-video", "job-video", executionv2.CapabilityImageToVideo)

	err := fixture.usecase.Handle(context.Background(), completedCallback("step-video", "job-video", executionv2.CapabilityImageToVideo, "video", "nonce-video"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	fixture.assertCompleted(t, "creation-1", "step-video")
}

func TestHandleCallback首帧完成原子激活第二步骤且创作未成功(t *testing.T) {
	fixture := newCallbackFixture(t)
	fixture.addSubmittedStep("creation-video", "step-frame", "job-frame", executionv2.CapabilityTextToImage)
	fixture.addBlockedSecondStep("creation-video", "step-i2v")
	event := completedCallback("step-frame", "job-frame", executionv2.CapabilityTextToImage, "image", "nonce-frame")

	if err := fixture.usecase.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	fixture.assertSecondStepActivatedOnce(t, "creation-video", "step-i2v")
	if fixture.store.creations["creation-video"] == creations.CreationStatusSucceeded {
		t.Fatal("首帧完成不能使两步骤创作提前成功")
	}
	if err := fixture.usecase.Handle(context.Background(), event); err != nil {
		t.Fatalf("replayed Handle() error = %v", err)
	}
	if fixture.store.assets["step-frame"] != 1 || fixture.store.outbox["step-i2v"] != 1 {
		t.Fatalf("frame assets/outbox = %d/%d, want 1/1", fixture.store.assets["step-frame"], fixture.store.outbox["step-i2v"])
	}
}

func TestHandleCallback技术失败或取消在同一事务只冲正一次并收敛创作(t *testing.T) {
	for _, terminal := range []CallbackTerminal{CallbackTerminalFailed, CallbackTerminalCancelled} {
		t.Run(string(terminal), func(t *testing.T) {
			fixture := newCallbackFixture(t)
			fixture.addSubmittedStep("creation-1", "step-1", "job-1", executionv2.CapabilityTextToImage)

			err := fixture.usecase.Handle(context.Background(), failedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, terminal, "nonce-1"))
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			fixture.assertFailedAndReversed(t, "creation-1", "step-1")
			if fixture.tx.calls != 1 || fixture.store.failTxCalls != 1 {
				t.Fatalf("transactions/fail tx contexts = %d/%d, want 1/1", fixture.tx.calls, fixture.store.failTxCalls)
			}
		})
	}
}

func TestHandleCallback二步骤失败或取消在同一事务只冲正一次(t *testing.T) {
	for _, terminal := range []CallbackTerminal{CallbackTerminalFailed, CallbackTerminalCancelled} {
		t.Run(string(terminal), func(t *testing.T) {
			fixture := newCallbackFixture(t)
			fixture.addSubmittedStep("creation-video", "step-i2v", "job-i2v", executionv2.CapabilityImageToVideo)

			err := fixture.usecase.Handle(context.Background(), failedCallback("step-i2v", "job-i2v", executionv2.CapabilityImageToVideo, terminal, "nonce-i2v"))
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			fixture.assertFailedAndReversed(t, "creation-video", "step-i2v")
		})
	}
}

func TestHandleCallback同Nonce重放不重复终态或冲正(t *testing.T) {
	fixture := newCallbackFixture(t)
	fixture.addSubmittedStep("creation-1", "step-1", "job-1", executionv2.CapabilityTextToImage)
	event := failedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, CallbackTerminalFailed, "nonce-1")

	if err := fixture.usecase.Handle(context.Background(), event); err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if err := fixture.usecase.Handle(context.Background(), event); err != nil {
		t.Fatalf("replayed Handle() error = %v", err)
	}
	fixture.assertFailedAndReversed(t, "creation-1", "step-1")
	if fixture.store.failWrites != 1 || fixture.reverser.calls != 1 || fixture.reverser.refunds != 1 {
		t.Fatalf("fail writes/reversals/refunds = %d/%d/%d, want 1/1/1", fixture.store.failWrites, fixture.reverser.calls, fixture.reverser.refunds)
	}
}

func TestHandleCallback同Nonce受控终态事实不同必须拒绝(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*VerifiedCallbackEvent)
	}{
		{"任务", func(event *VerifiedCallbackEvent) { event.JobID = "job-other" }},
		{"能力", func(event *VerifiedCallbackEvent) { event.Capability = executionv2.CapabilityImageEdit }},
		{"摘要", func(event *VerifiedCallbackEvent) { event.PayloadDigest = "payload-other" }},
		{"结果地址", func(event *VerifiedCallbackEvent) { event.ResultURL = "https://assets.example.test/other" }},
		{"媒体类型", func(event *VerifiedCallbackEvent) { event.MediaType = "video" }},
		{"终态", func(event *VerifiedCallbackEvent) {
			event.Terminal = CallbackTerminalFailed
			event.MediaType = ""
			event.ResultURL = ""
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCallbackFixture(t)
			fixture.addSubmittedStep("creation-1", "step-1", "job-1", executionv2.CapabilityTextToImage)
			event := completedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, "image", "nonce-1")
			if err := fixture.usecase.Handle(context.Background(), event); err != nil {
				t.Fatalf("first Handle() error = %v", err)
			}
			testCase.mutate(&event)

			err := fixture.usecase.Handle(context.Background(), event)
			if !errors.Is(err, ErrInvalidCallbackEvent) || errors.Is(err, ErrCallbackAlreadyConfirmed) {
				t.Fatalf("conflicting Handle() error = %v, want ErrInvalidCallbackEvent", err)
			}
			if fixture.store.completeWrites != 1 || fixture.store.assets["step-1"] != 1 || fixture.reverser.calls != 0 {
				t.Fatalf("writes/assets/reversals = %d/%d/%d, want 1/1/0", fixture.store.completeWrites, fixture.store.assets["step-1"], fixture.reverser.calls)
			}
		})
	}
}

func TestHandleCallback不同Nonce同一完成终态只确认既有事实(t *testing.T) {
	fixture := newCallbackFixture(t)
	fixture.addSubmittedStep("creation-1", "step-1", "job-1", executionv2.CapabilityTextToImage)
	first := completedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, "image", "nonce-1")
	second := first
	second.NonceHash = "nonce-2"

	if err := fixture.usecase.Handle(context.Background(), first); err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if err := fixture.usecase.Handle(context.Background(), second); err != nil {
		t.Fatalf("second Handle() error = %v", err)
	}
	if fixture.store.completeWrites != 1 || fixture.store.assets["step-1"] != 1 || fixture.store.outbox["step-1"] != 0 || fixture.reverser.calls != 0 {
		t.Fatalf("writes/assets/outbox/reversals = %d/%d/%d/%d, want 1/1/0/0", fixture.store.completeWrites, fixture.store.assets["step-1"], fixture.store.outbox["step-1"], fixture.reverser.calls)
	}
}

func TestHandleCallback重放比较忽略不同冻结时间(t *testing.T) {
	for _, replay := range []struct {
		name        string
		changeNonce bool
	}{
		{"同 nonce", false},
		{"不同 nonce", true},
	} {
		t.Run(replay.name, func(t *testing.T) {
			times := []time.Time{
				time.Date(2026, time.September, 7, 4, 0, 0, 0, time.UTC),
				time.Date(2026, time.September, 7, 4, 1, 0, 0, time.UTC),
			}
			index := 0
			fixture := newCallbackFixtureWithClock(t, func() time.Time {
				current := times[index]
				index++
				return current
			})
			fixture.addSubmittedStep("creation-1", "step-1", "job-1", executionv2.CapabilityTextToImage)
			first := completedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, "image", "nonce-1")
			second := first
			if replay.changeNonce {
				second.NonceHash = "nonce-2"
			}

			if err := fixture.usecase.Handle(context.Background(), first); err != nil {
				t.Fatalf("first Handle() error = %v", err)
			}
			if err := fixture.usecase.Handle(context.Background(), second); err != nil {
				t.Fatalf("replayed Handle() error = %v", err)
			}
			if fixture.store.completeWrites != 1 || fixture.store.assets["step-1"] != 1 || fixture.store.outbox["step-1"] != 0 || fixture.reverser.calls != 0 {
				t.Fatalf("writes/assets/outbox/reversals = %d/%d/%d/%d, want 1/1/0/0", fixture.store.completeWrites, fixture.store.assets["step-1"], fixture.store.outbox["step-1"], fixture.reverser.calls)
			}
			if receipt := fixture.store.receipts["nonce-1"]; !receipt.at.Equal(times[0]) {
				t.Fatalf("first receipt At = %s, want %s", receipt.at, times[0])
			}
		})
	}
}

func TestHandleCallback不同Nonce乱序终态不吞没冲突(t *testing.T) {
	fixture := newCallbackFixture(t)
	fixture.addSubmittedStep("creation-1", "step-1", "job-1", executionv2.CapabilityTextToImage)

	if err := fixture.usecase.Handle(context.Background(), completedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, "image", "nonce-completed")); err != nil {
		t.Fatalf("completed Handle() error = %v", err)
	}
	err := fixture.usecase.Handle(context.Background(), failedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, CallbackTerminalFailed, "nonce-failed"))
	if !errors.Is(err, ErrInvalidCallbackEvent) {
		t.Fatalf("out-of-order Handle() error = %v, want ErrInvalidCallbackEvent", err)
	}
	fixture.assertCompleted(t, "creation-1", "step-1")
	if fixture.reverser.calls != 0 {
		t.Fatalf("ReverseInTx calls = %d, want 0", fixture.reverser.calls)
	}
}

func TestHandleCallback已冲正或没收预留不退款(t *testing.T) {
	for _, status := range []ledger.ReservationStatus{ledger.ReservationStatusReversed, ledger.ReservationStatusConfiscated} {
		t.Run(string(status), func(t *testing.T) {
			fixture := newCallbackFixture(t)
			fixture.addSubmittedStep("creation-1", "step-1", "job-1", executionv2.CapabilityTextToImage)
			fixture.reverser.statuses["creation-1"] = status

			err := fixture.usecase.Handle(context.Background(), failedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, CallbackTerminalFailed, "nonce-1"))
			if status == ledger.ReservationStatusConfiscated && !errors.Is(err, ledger.ErrReservationStateConflict) {
				t.Fatalf("Handle() error = %v, want ErrReservationStateConflict", err)
			}
			if status == ledger.ReservationStatusReversed && err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if fixture.reverser.calls != 1 || fixture.reverser.refunds != 0 {
				t.Fatalf("reversals/refunds = %d/%d, want 1/0", fixture.reverser.calls, fixture.reverser.refunds)
			}
		})
	}
}

func TestHandleCallback拒绝不匹配回调且不写入(t *testing.T) {
	cases := []struct {
		name  string
		event VerifiedCallbackEvent
	}{
		{"任务不匹配", completedCallback("step-1", "job-other", executionv2.CapabilityTextToImage, "image", "nonce-job")},
		{"能力不匹配", completedCallback("step-1", "job-1", executionv2.CapabilityImageEdit, "image", "nonce-capability")},
		{"媒体类型不匹配", completedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, "video", "nonce-media")},
		{"外部引用不匹配", VerifiedCallbackEvent{StepID: "step-1", ExternalRef: "step-other", JobID: "job-1", Capability: executionv2.CapabilityTextToImage, MediaType: "image", ResultURL: "https://assets.example.test/result", NonceHash: "nonce-ref", PayloadDigest: "payload-nonce-ref", CallbackVersion: "2"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCallbackFixture(t)
			fixture.addSubmittedStep("creation-1", "step-1", "job-1", executionv2.CapabilityTextToImage)

			err := fixture.usecase.Handle(context.Background(), testCase.event)
			if !errors.Is(err, ErrInvalidCallbackEvent) {
				t.Fatalf("Handle() error = %v, want ErrInvalidCallbackEvent", err)
			}
			if fixture.store.completeWrites != 0 || fixture.store.failWrites != 0 || fixture.reverser.calls != 0 {
				t.Fatalf("writes/reversals = %d/%d/%d, want 0/0/0", fixture.store.completeWrites, fixture.store.failWrites, fixture.reverser.calls)
			}
		})
	}
}

func TestHandleCallback只接受关联器提供的可信创作关联(t *testing.T) {
	fixture := newCallbackFixture(t)
	fixture.addSubmittedStep("creation-trusted", "step-1", "job-1", executionv2.CapabilityTextToImage)
	fixture.linker.creationByStep["step-1"] = "creation-other"

	err := fixture.usecase.Handle(context.Background(), completedCallback("step-1", "job-1", executionv2.CapabilityTextToImage, "image", "nonce-1"))
	if !errors.Is(err, ErrInvalidCallbackEvent) {
		t.Fatalf("Handle() error = %v, want ErrInvalidCallbackEvent", err)
	}
	if fixture.store.completeWrites != 0 {
		t.Fatalf("complete writes = %d, want 0", fixture.store.completeWrites)
	}
}

type callbackFixture struct {
	usecase  *CallbackUsecase
	store    *memoryCallbackStore
	linker   *memoryCallbackLinker
	reverser *memoryCallbackReverser
	tx       *memoryCallbackTxRunner
}

func newCallbackFixture(t *testing.T) *callbackFixture {
	return newCallbackFixtureWithClock(t, func() time.Time {
		return time.Date(2026, time.September, 7, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	})
}

func newCallbackFixtureWithClock(t *testing.T, clock func() time.Time) *callbackFixture {
	t.Helper()
	store := newMemoryCallbackStore()
	linker := &memoryCallbackLinker{creationByStep: make(map[string]string)}
	reverser := &memoryCallbackReverser{statuses: make(map[string]ledger.ReservationStatus)}
	tx := &memoryCallbackTxRunner{}
	return &callbackFixture{
		usecase: NewCallbackUsecaseWithClock(store, linker, reverser, tx, clock),
		store:   store, linker: linker, reverser: reverser, tx: tx,
	}
}

func (fixture *callbackFixture) addSubmittedStep(creationID, stepID, jobID string, capability executionv2.Capability) {
	fixture.store.steps[stepID] = &memoryCallbackStep{creationID: creationID, jobID: jobID, capability: capability, status: creations.StepSubmitStatusSubmitted}
	fixture.store.creations[creationID] = creations.CreationStatusPendingSubmission
	fixture.linker.creationByStep[stepID] = creationID
}

func (fixture *callbackFixture) addBlockedSecondStep(creationID, stepID string) {
	fixture.store.steps[stepID] = &memoryCallbackStep{creationID: creationID, capability: executionv2.CapabilityImageToVideo, status: creations.StepSubmitStatusBlocked}
}

func (fixture *callbackFixture) assertCompleted(t *testing.T, creationID, stepID string) {
	t.Helper()
	step := fixture.store.steps[stepID]
	if step == nil || step.status != creations.StepSubmitStatusSucceeded || fixture.store.creations[creationID] != creations.CreationStatusSucceeded {
		t.Fatalf("creation/step state = %q/%#v, want succeeded/succeeded", fixture.store.creations[creationID], step)
	}
}

func (fixture *callbackFixture) assertSecondStepActivatedOnce(t *testing.T, creationID, stepID string) {
	t.Helper()
	step := fixture.store.steps[stepID]
	if step == nil || step.status != creations.StepSubmitStatusReady || fixture.store.outbox[stepID] != 1 || fixture.store.creations[creationID] == creations.CreationStatusSucceeded {
		t.Fatalf("creation/second step/outbox = %q/%#v/%d, want non-succeeded/ready/1", fixture.store.creations[creationID], step, fixture.store.outbox[stepID])
	}
}

func (fixture *callbackFixture) assertFailedAndReversed(t *testing.T, creationID, stepID string) {
	t.Helper()
	step := fixture.store.steps[stepID]
	if step == nil || step.status != creations.StepSubmitStatusGenerationFailed || fixture.store.creations[creationID] != creations.CreationStatusGenerationFailed {
		t.Fatalf("creation/step state = %q/%#v, want generation_failed/generation_failed", fixture.store.creations[creationID], step)
	}
	if fixture.reverser.statuses[creationID] != ledger.ReservationStatusReversed {
		t.Fatalf("reservation status = %q, want reversed", fixture.reverser.statuses[creationID])
	}
}

func (fixture *callbackFixture) assertCompleteInTransaction(t *testing.T, wantCalls int) {
	t.Helper()
	if fixture.tx.calls != wantCalls || fixture.store.completeTxCalls != wantCalls {
		t.Fatalf("transactions/complete tx contexts = %d/%d, want %d/%d", fixture.tx.calls, fixture.store.completeTxCalls, wantCalls, wantCalls)
	}
}

func completedCallback(stepID, jobID string, capability executionv2.Capability, mediaType, nonceHash string) VerifiedCallbackEvent {
	return VerifiedCallbackEvent{StepID: stepID, ExternalRef: stepID, JobID: jobID, Capability: capability, MediaType: mediaType, ResultURL: "https://assets.example.test/result", NonceHash: nonceHash, PayloadDigest: "payload-" + nonceHash, CallbackVersion: "2"}
}

func failedCallback(stepID, jobID string, capability executionv2.Capability, terminal CallbackTerminal, nonceHash string) VerifiedCallbackEvent {
	return VerifiedCallbackEvent{StepID: stepID, ExternalRef: stepID, JobID: jobID, Capability: capability, Terminal: terminal, NonceHash: nonceHash, PayloadDigest: "payload-" + nonceHash, CallbackVersion: "2"}
}

type callbackTxKey struct{}

type memoryCallbackTxRunner struct{ calls int }

func (runner *memoryCallbackTxRunner) WithinTx(ctx context.Context, callback func(context.Context) error) error {
	runner.calls++
	return callback(context.WithValue(ctx, callbackTxKey{}, true))
}

type memoryCallbackLinker struct{ creationByStep map[string]string }

func (linker *memoryCallbackLinker) Link(_ context.Context, event VerifiedCallbackEvent) (LinkedCallback, error) {
	creationID, found := linker.creationByStep[event.StepID]
	if !found {
		return LinkedCallback{}, ErrInvalidCallbackEvent
	}
	return NewLinkedCallback(creationID, event)
}

type memoryCallbackReverser struct {
	calls    int
	refunds  int
	statuses map[string]ledger.ReservationStatus
}

func (reverser *memoryCallbackReverser) ReverseInTx(_ context.Context, creationID string, _ ledger.ReversalReason, _ time.Time) (*ledger.Reservation, error) {
	reverser.calls++
	if reverser.statuses[creationID] == ledger.ReservationStatusReversed {
		return &ledger.Reservation{CreationID: creationID, Status: ledger.ReservationStatusReversed}, nil
	}
	if reverser.statuses[creationID] == ledger.ReservationStatusConfiscated {
		return nil, ledger.ErrReservationStateConflict
	}
	reverser.refunds++
	reverser.statuses[creationID] = ledger.ReservationStatusReversed
	return &ledger.Reservation{CreationID: creationID, Status: ledger.ReservationStatusReversed}, nil
}

type memoryCallbackStore struct {
	steps           map[string]*memoryCallbackStep
	creations       map[string]creations.CreationStatus
	receipts        map[string]memoryCallbackReceipt
	terminals       map[string]memoryCallbackReceipt
	assets          map[string]int
	outbox          map[string]int
	completeWrites  int
	failWrites      int
	completeTxCalls int
	failTxCalls     int
}

type memoryCallbackStep struct {
	creationID string
	jobID      string
	capability executionv2.Capability
	status     creations.StepSubmitStatus
}

type memoryCallbackReceipt struct {
	creationID      string
	stepID          string
	jobID           string
	externalRef     string
	capability      executionv2.Capability
	mediaType       string
	resultURL       string
	terminal        CallbackTerminal
	payloadDigest   string
	callbackVersion string
	at              time.Time
}

func newMemoryCallbackStore() *memoryCallbackStore {
	return &memoryCallbackStore{steps: make(map[string]*memoryCallbackStep), creations: make(map[string]creations.CreationStatus), receipts: make(map[string]memoryCallbackReceipt), terminals: make(map[string]memoryCallbackReceipt), assets: make(map[string]int), outbox: make(map[string]int)}
}

func (store *memoryCallbackStore) Complete(ctx context.Context, command CompletedCallback) error {
	if ctx.Value(callbackTxKey{}) == true {
		store.completeTxCalls++
	}
	receipt := completedReceipt(command)
	if err := store.checkReceipt(command.NonceHash, receipt); err != nil {
		return err
	}
	step := store.steps[command.StepID]
	if step == nil || step.creationID != command.CreationID || step.jobID != command.JobID || step.capability != command.Capability || command.ExternalRef != command.StepID || !compatibleMedia(command.Capability, command.MediaType) {
		return ErrInvalidCallbackEvent
	}
	if step.status != creations.StepSubmitStatusSubmitted {
		if existing, found := store.terminals[command.StepID]; found && sameTerminalFact(existing, receipt) {
			store.receipts[command.NonceHash] = existing
			return ErrCallbackAlreadyConfirmed
		}
		return ErrInvalidCallbackEvent
	}
	store.receipts[command.NonceHash] = receipt
	store.terminals[command.StepID] = receipt
	store.assets[command.StepID]++
	step.status = creations.StepSubmitStatusSucceeded
	store.completeWrites++
	activated := false
	for stepID, candidate := range store.steps {
		if candidate.creationID == command.CreationID && candidate.status == creations.StepSubmitStatusBlocked && candidate.capability == executionv2.CapabilityImageToVideo {
			candidate.status = creations.StepSubmitStatusReady
			store.outbox[stepID]++
			activated = true
		}
	}
	if !activated {
		store.creations[command.CreationID] = creations.CreationStatusSucceeded
	}
	return nil
}

func (store *memoryCallbackStore) Fail(ctx context.Context, command FailedCallback) error {
	if ctx.Value(callbackTxKey{}) == true {
		store.failTxCalls++
	}
	receipt := failedReceipt(command)
	if err := store.checkReceipt(command.NonceHash, receipt); err != nil {
		return err
	}
	step := store.steps[command.StepID]
	if step == nil || step.creationID != command.CreationID || step.jobID != command.JobID || step.capability != command.Capability || command.ExternalRef != command.StepID {
		return ErrInvalidCallbackEvent
	}
	if step.status != creations.StepSubmitStatusSubmitted {
		if existing, found := store.terminals[command.StepID]; found && sameTerminalFact(existing, receipt) {
			store.receipts[command.NonceHash] = existing
			return ErrCallbackAlreadyConfirmed
		}
		return ErrInvalidCallbackEvent
	}
	store.receipts[command.NonceHash] = receipt
	store.terminals[command.StepID] = receipt
	step.status = creations.StepSubmitStatusGenerationFailed
	store.creations[command.CreationID] = creations.CreationStatusGenerationFailed
	store.failWrites++
	return nil
}

func (store *memoryCallbackStore) checkReceipt(nonceHash string, candidate memoryCallbackReceipt) error {
	existing, found := store.receipts[nonceHash]
	if !found {
		return nil
	}
	if sameTerminalFact(existing, candidate) {
		return ErrCallbackAlreadyConfirmed
	}
	return ErrInvalidCallbackEvent
}

// sameTerminalFact 比较用于回执幂等与终态 CAS 的受控终态事实。
// At 是首次确认的持久化时间，不属于后续投递的等价条件。
func sameTerminalFact(first, second memoryCallbackReceipt) bool {
	first.at = time.Time{}
	second.at = time.Time{}
	return first == second
}

func completedReceipt(command CompletedCallback) memoryCallbackReceipt {
	return memoryCallbackReceipt{
		creationID: command.CreationID, stepID: command.StepID, jobID: command.JobID, externalRef: command.ExternalRef,
		capability: command.Capability, mediaType: command.MediaType, resultURL: command.ResultURL,
		payloadDigest: command.PayloadDigest, callbackVersion: command.CallbackVersion, at: command.At,
	}
}

func failedReceipt(command FailedCallback) memoryCallbackReceipt {
	return memoryCallbackReceipt{
		creationID: command.CreationID, stepID: command.StepID, jobID: command.JobID, externalRef: command.ExternalRef,
		capability: command.Capability, terminal: command.Terminal, payloadDigest: command.PayloadDigest,
		callbackVersion: command.CallbackVersion, at: command.At,
	}
}

func compatibleMedia(capability executionv2.Capability, mediaType string) bool {
	if capability == executionv2.CapabilityImageToVideo {
		return mediaType == "video"
	}
	return mediaType == "image"
}
