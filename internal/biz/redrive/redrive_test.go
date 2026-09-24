package redrive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
)

var redriveNow = time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)

// materializePayload 复用既有测试已经验证过的形状，避免自造一个恰好不合法的 payload。
func materializePayload(t *testing.T) []byte {
	t.Helper()
	resultRef := "https://results.example.test/job-result.png"
	raw, err := generation.MarshalProviderResultMaterializeEventPayload(generation.ProviderResultMaterializeEventPayload{
		CreationID: "creation-result-1", StepID: "step-result-1", Provider: creations.PolarStarB2BProvider,
		AccountRef: "account-main", JobID: "job-result-1", Capability: "text_to_image", ResultRef: resultRef,
		TerminalVersion: 1, TerminalDigest: generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalCompleted, resultRef),
	})
	if err != nil {
		t.Fatalf("构造物化 payload: %v", err)
	}
	return raw
}

func settlementPayload(t *testing.T) []byte {
	t.Helper()
	raw, err := generation.MarshalProviderTerminalSettlementEventPayload(generation.ProviderTerminalSettlementEventPayload{
		CreationID: "creation-settlement-1", StepID: "step-settlement-1", Provider: creations.PolarStarB2BProvider,
		AccountRef: "account-main", JobID: "job-settlement-1", Capability: "text_to_image",
		Status: generation.ProviderTerminalFailed, TerminalVersion: 1,
		TerminalDigest: generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalFailed, ""),
	})
	if err != nil {
		t.Fatalf("构造结算 payload: %v", err)
	}
	return raw
}

func attentionEvent(t *testing.T, eventType, payload string) *outbox.Event {
	t.Helper()
	return &outbox.Event{
		ID: "event-" + eventType, AggregateID: "creation-1", EventType: outbox.EventType(eventType),
		Payload: []byte(payload), DeliveryStatus: outbox.DeliveryStatusNeedsAttention,
		AttemptCount: outbox.MaterialUploadRetryBudget + 1, CreatedAt: redriveNow.Add(-72 * time.Hour),
	}
}

func validCommand(eventID string) outbox.RedriveCommand {
	return outbox.RedriveCommand{
		EventID: eventID, ExpectedRedriveCount: 0,
		ActorID: "admin-1", Reason: "provider 已恢复", Key: "ticket-42", At: redriveNow,
	}
}

type fakeStore struct {
	event       *outbox.Event
	findErr     error
	redriveErr  error
	redriveCall int
	lastCommand outbox.RedriveCommand
}

func (store *fakeStore) FindForRedrive(context.Context, string) (*outbox.Event, error) {
	return store.event, store.findErr
}

func (store *fakeStore) RedriveAttention(_ context.Context, command outbox.RedriveCommand) (*outbox.Event, error) {
	store.redriveCall++
	store.lastCommand = command
	if store.redriveErr != nil {
		return nil, store.redriveErr
	}
	// 模拟存储层：只推进窗口与次数，不改写 payload / attempt_count / created_at。
	updated := *store.event
	updated.DeliveryStatus = outbox.DeliveryStatusPending
	updated.RedriveStartedAt = command.At
	updated.RedriveAttemptBase = store.event.AttemptCount
	updated.RedriveCount = store.event.RedriveCount + 1
	updated.AttentionReason = ""
	store.event = &updated
	return &updated, nil
}

type fakeGates struct {
	materializeErr error
	settlementErr  error
	materializeHit int
	settlementHit  int
}

func (gates *fakeGates) LoadProviderResultMaterialization(context.Context, generation.ProviderResultMaterializeEventPayload) (generation.ProviderResultMaterializationTarget, error) {
	gates.materializeHit++
	return generation.ProviderResultMaterializationTarget{}, gates.materializeErr
}

func (gates *fakeGates) LoadProviderTerminalSettlement(context.Context, generation.ProviderTerminalSettlementEventPayload) (generation.ProviderTerminalSettlementTarget, error) {
	gates.settlementHit++
	return generation.ProviderTerminalSettlementTarget{}, gates.settlementErr
}

type fakeLedger struct {
	reservation *ledger.Reservation
	err         error
	calls       int
}

func (port *fakeLedger) FindReservation(context.Context, string) (*ledger.Reservation, error) {
	port.calls++
	return port.reservation, port.err
}

type fakeAudits struct {
	store  map[string]Audit
	werr   error
	ferr   error
	writes int
}

func newFakeAudits() *fakeAudits { return &fakeAudits{store: map[string]Audit{}} }

func (audits *fakeAudits) FindAudit(_ context.Context, key AuditKey) (Audit, bool, error) {
	if audits.ferr != nil {
		return Audit{}, false, audits.ferr
	}
	found, ok := audits.store[AuditID(key)]
	return found, ok, nil
}

func (audits *fakeAudits) WriteAudit(_ context.Context, audit Audit) error {
	if audits.werr != nil {
		return audits.werr
	}
	audits.writes++
	audits.store[AuditID(audit.Key)] = audit
	return nil
}

type immediateTx struct{ calls int }

func (tx *immediateTx) WithinTx(ctx context.Context, run func(context.Context) error) error {
	tx.calls++
	return run(ctx)
}

func newUsecase(store *fakeStore, gates *fakeGates, port *fakeLedger, audits *fakeAudits) (*Usecase, *immediateTx) {
	tx := &immediateTx{}
	return NewUsecaseWithClock(store, gates, port, audits, tx, func() time.Time { return redriveNow }), tx
}

func TestRedrive物化事件重驱成功并写审计(t *testing.T) {
	event := attentionEvent(t, generation.ProviderResultMaterializeEventType, string(materializePayload(t)))
	store := &fakeStore{event: event}
	gates := &fakeGates{}
	port := &fakeLedger{}
	audits := newFakeAudits()
	usecase, tx := newUsecase(store, gates, port, audits)

	command := validCommand(event.ID)
	result, err := usecase.Redrive(context.Background(), command)
	if err != nil {
		t.Fatalf("Redrive() error = %v", err)
	}
	if tx.calls != 1 {
		t.Fatalf("WithinTx 调用次数 = %d, want 1（审计与 CAS 必须同一事务）", tx.calls)
	}
	if result.Replayed || result.RedriveCount != 1 || result.EventID != event.ID {
		t.Fatalf("result = %#v", result)
	}
	if !result.WindowStartedAt.Equal(redriveNow) {
		t.Fatalf("窗口起点 = %v, want %v", result.WindowStartedAt, redriveNow)
	}
	if len(audits.store) != 1 || audits.writes != 1 {
		t.Fatalf("审计写入 = %d 条 / %d 次", len(audits.store), audits.writes)
	}
	// 账本资格只对结算类事件判断：物化事件不应触碰账本。
	if port.calls != 0 || gates.materializeHit != 1 || gates.settlementHit != 0 {
		t.Fatalf("门禁/账本命中 = %d/%d/%d", gates.materializeHit, gates.settlementHit, port.calls)
	}
	if store.lastCommand != command {
		t.Fatalf("传给存储层的命令 = %#v", store.lastCommand)
	}
}

func TestRedrive结算事件只允许reserved(t *testing.T) {
	cases := map[string]struct {
		reservation *ledger.Reservation
		wantErr     error
	}{
		"reserved 允许": {reservation: &ledger.Reservation{Status: ledger.ReservationStatusReserved}},
		"reversed 拒绝": {reservation: &ledger.Reservation{Status: ledger.ReservationStatusReversed}, wantErr: outbox.ErrRedriveNotEligible},
		"confiscated 拒绝": {
			reservation: &ledger.Reservation{Status: ledger.ReservationStatusConfiscated}, wantErr: outbox.ErrRedriveNotEligible,
		},
		"预扣不存在": {reservation: nil, wantErr: outbox.ErrRedriveConflict},
	}
	for name, testCase := range cases {
		event := attentionEvent(t, generation.ProviderTerminalSettlementEventType, string(settlementPayload(t)))
		store := &fakeStore{event: event}
		gates := &fakeGates{}
		port := &fakeLedger{reservation: testCase.reservation}
		audits := newFakeAudits()
		usecase, _ := newUsecase(store, gates, port, audits)

		_, err := usecase.Redrive(context.Background(), validCommand(event.ID))
		if !errors.Is(err, testCase.wantErr) {
			t.Fatalf("%s: error = %v, want %v", name, err, testCase.wantErr)
		}
		if testCase.wantErr != nil {
			if store.redriveCall != 0 || len(audits.store) != 0 {
				t.Fatalf("%s: 被拒的重驱不得改动事件或写审计", name)
			}
		} else if store.redriveCall != 1 || len(audits.store) != 1 {
			t.Fatalf("%s: 允许的重驱应当完成 CAS 与审计", name)
		}
		if gates.settlementHit != 1 {
			t.Fatalf("%s: 终态门禁必须被重跑", name)
		}
	}
}

func TestRedrive重放返回原结果且不产生第二次重驱(t *testing.T) {
	event := attentionEvent(t, generation.ProviderResultMaterializeEventType, string(materializePayload(t)))
	store := &fakeStore{event: event}
	gates := &fakeGates{}
	audits := newFakeAudits()
	usecase, _ := newUsecase(store, gates, &fakeLedger{}, audits)
	command := validCommand(event.ID)

	first, err := usecase.Redrive(context.Background(), command)
	if err != nil {
		t.Fatalf("首次 Redrive() error = %v", err)
	}
	second, err := usecase.Redrive(context.Background(), command)
	if err != nil {
		t.Fatalf("重放 Redrive() error = %v", err)
	}
	if store.redriveCall != 1 {
		t.Fatalf("重放不应产生第二次重驱: RedriveAttention 调用 %d 次", store.redriveCall)
	}
	if audits.writes != 1 {
		t.Fatalf("重放不应新增审计: 写入 %d 次", audits.writes)
	}
	if !second.Replayed {
		t.Fatal("重放结果必须标记 Replayed")
	}
	if second.EventID != first.EventID || second.RedriveCount != first.RedriveCount ||
		second.WindowAttemptBase != first.WindowAttemptBase || !second.RedrivenAt.Equal(first.RedrivenAt) {
		t.Fatalf("重放结果必须与首次一致:\n first=%#v\n second=%#v", first, second)
	}
}

func TestRedrive同一幂等键复用不同请求是冲突(t *testing.T) {
	event := attentionEvent(t, generation.ProviderResultMaterializeEventType, string(materializePayload(t)))
	store := &fakeStore{event: event}
	audits := newFakeAudits()
	usecase, _ := newUsecase(store, &fakeGates{}, &fakeLedger{}, audits)

	if _, err := usecase.Redrive(context.Background(), validCommand(event.ID)); err != nil {
		t.Fatalf("首次 Redrive() error = %v", err)
	}
	reused := validCommand(event.ID)
	reused.Reason = "换了理由但复用同一个幂等键"
	if _, err := usecase.Redrive(context.Background(), reused); !errors.Is(err, ErrRedriveAuditConflict) {
		t.Fatalf("幂等键复用 error = %v, want ErrRedriveAuditConflict", err)
	}
	if store.redriveCall != 1 {
		t.Fatal("冲突请求不得产生第二次重驱")
	}
}

func TestRedrive资格与令牌的拒绝语义(t *testing.T) {
	payload := string(materializePayload(t))
	cases := map[string]struct {
		mutate  func(*outbox.Event, *outbox.RedriveCommand)
		wantErr error
	}{
		"令牌过期": {
			mutate: func(event *outbox.Event, command *outbox.RedriveCommand) {
				event.RedriveCount = 1 // 库里已推进，操作者看到的还是 0
			},
			wantErr: outbox.ErrRedriveConflict,
		},
		"已不是人工关注": {
			mutate: func(event *outbox.Event, _ *outbox.RedriveCommand) {
				event.DeliveryStatus = outbox.DeliveryStatusDelivered
			},
			wantErr: outbox.ErrRedriveNotEligible,
		},
		"已达两次上限": {
			mutate: func(event *outbox.Event, command *outbox.RedriveCommand) {
				event.RedriveCount = outbox.MaxRedriveCount
				command.ExpectedRedriveCount = outbox.MaxRedriveCount
			},
			wantErr: outbox.ErrRedriveNotEligible,
		},
		"不支持的事件类型": {
			mutate: func(event *outbox.Event, _ *outbox.RedriveCommand) {
				event.EventType = outbox.EventType("generation.submission")
			},
			wantErr: ErrUnsupportedRedriveEventType,
		},
		"非法命令": {
			mutate:  func(_ *outbox.Event, command *outbox.RedriveCommand) { command.ActorID = "" },
			wantErr: outbox.ErrInvalidRedriveCommand,
		},
	}
	for name, testCase := range cases {
		event := attentionEvent(t, generation.ProviderResultMaterializeEventType, payload)
		command := validCommand(event.ID)
		testCase.mutate(event, &command)
		store := &fakeStore{event: event}
		audits := newFakeAudits()
		usecase, tx := newUsecase(store, &fakeGates{}, &fakeLedger{}, audits)

		if _, err := usecase.Redrive(context.Background(), command); !errors.Is(err, testCase.wantErr) {
			t.Fatalf("%s: error = %v, want %v", name, err, testCase.wantErr)
		}
		if store.redriveCall != 0 || len(audits.store) != 0 {
			t.Fatalf("%s: 被拒的请求不得改动事件或写审计", name)
		}
		if name == "非法命令" && tx.calls != 0 {
			t.Fatal("非法命令必须在开启事务之前被拒")
		}
	}
}

func TestRedrive门禁不通过时拒绝且不写审计(t *testing.T) {
	event := attentionEvent(t, generation.ProviderResultMaterializeEventType, string(materializePayload(t)))
	store := &fakeStore{event: event}
	gates := &fakeGates{materializeErr: generation.ErrProviderResultPublicationConflict}
	audits := newFakeAudits()
	usecase, _ := newUsecase(store, gates, &fakeLedger{}, audits)

	if _, err := usecase.Redrive(context.Background(), validCommand(event.ID)); !errors.Is(err, generation.ErrProviderResultPublicationConflict) {
		t.Fatalf("门禁失败 error = %v, want 透传门禁错误", err)
	}
	// 门禁在 CAS 之前：公开的失败必须完全不改动事件、也不留下审计。
	if store.redriveCall != 0 || len(audits.store) != 0 {
		t.Fatalf("门禁失败仍改动了状态: redrive=%d audits=%d", store.redriveCall, len(audits.store))
	}
}

func TestRedrive审计写入失败时不返回结果(t *testing.T) {
	event := attentionEvent(t, generation.ProviderResultMaterializeEventType, string(materializePayload(t)))
	store := &fakeStore{event: event}
	audits := newFakeAudits()
	audits.werr = errors.New("audit collection unavailable")
	usecase, _ := newUsecase(store, &fakeGates{}, &fakeLedger{}, audits)

	result, err := usecase.Redrive(context.Background(), validCommand(event.ID))
	if err == nil {
		t.Fatal("审计写入失败必须让整体失败")
	}
	if result != (Result{}) {
		t.Fatalf("失败时不得返回结果: %#v", result)
	}
	if len(audits.store) != 0 {
		t.Fatal("审计写入失败不得留下记录")
	}
}

func TestRedrive存储层失败时不写审计(t *testing.T) {
	event := attentionEvent(t, generation.ProviderResultMaterializeEventType, string(materializePayload(t)))
	store := &fakeStore{event: event, redriveErr: outbox.ErrRedriveConflict}
	audits := newFakeAudits()
	usecase, _ := newUsecase(store, &fakeGates{}, &fakeLedger{}, audits)

	if _, err := usecase.Redrive(context.Background(), validCommand(event.ID)); !errors.Is(err, outbox.ErrRedriveConflict) {
		t.Fatalf("CAS 失败 error = %v", err)
	}
	if audits.writes != 0 || len(audits.store) != 0 {
		t.Fatal("CAS 未成功不得写审计")
	}
}

func TestRedrive缺依赖时拒绝(t *testing.T) {
	event := attentionEvent(t, generation.ProviderResultMaterializeEventType, string(materializePayload(t)))
	command := validCommand(event.ID)
	full := func() (*Usecase, *fakeStore, *fakeAudits) {
		store := &fakeStore{event: event}
		audits := newFakeAudits()
		usecase, _ := newUsecase(store, &fakeGates{}, &fakeLedger{}, audits)
		return usecase, store, audits
	}
	cases := map[string]func(*Usecase){
		"缺存储": func(usecase *Usecase) { usecase.store = nil },
		"缺门禁": func(usecase *Usecase) { usecase.gates = nil },
		"缺账本": func(usecase *Usecase) { usecase.ledger = nil },
		"缺审计": func(usecase *Usecase) { usecase.audits = nil },
		"缺事务": func(usecase *Usecase) { usecase.tx = nil },
	}
	for name, strip := range cases {
		usecase, store, audits := full()
		strip(usecase)
		if _, err := usecase.Redrive(context.Background(), command); !errors.Is(err, ErrRedriveDependenciesUnavailable) {
			t.Fatalf("%s: error = %v, want ErrRedriveDependenciesUnavailable", name, err)
		}
		if store.redriveCall != 0 || len(audits.store) != 0 {
			t.Fatalf("%s: 缺依赖时不得产生副作用", name)
		}
	}
	if _, err := (*Usecase)(nil).Redrive(context.Background(), command); !errors.Is(err, ErrRedriveDependenciesUnavailable) {
		t.Fatalf("nil 用例 error = %v", err)
	}
}

func TestRedrive审计记录不含payload(t *testing.T) {
	// 事件载荷是未受信的 provider 数据，审计表没有理由持有它。
	event := attentionEvent(t, generation.ProviderResultMaterializeEventType, string(materializePayload(t)))
	store := &fakeStore{event: event}
	audits := newFakeAudits()
	usecase, _ := newUsecase(store, &fakeGates{}, &fakeLedger{}, audits)

	if _, err := usecase.Redrive(context.Background(), validCommand(event.ID)); err != nil {
		t.Fatalf("Redrive() error = %v", err)
	}
	for _, audit := range audits.store {
		rendered := strings.Join([]string{
			audit.EventID, audit.AggregateID, audit.EventType, audit.Reason, audit.Key.ActorID, audit.Key.EventKey,
		}, "\x00")
		for _, marker := range []string{"results.example.test", "job-result-1", "creation-result-1", "{", "}"} {
			if strings.Contains(rendered, marker) {
				t.Fatalf("审计记录出现载荷痕迹 %q: %s", marker, rendered)
			}
		}
	}
}

func TestRedriveAuditID由幂等键唯一决定(t *testing.T) {
	base := AuditKey{ActorID: "admin-1", EventKey: "ticket-42", ExpectedRedriveCount: 0}
	if AuditID(base) != AuditID(base) {
		t.Fatal("同一幂等键必须派生同一个 _id")
	}
	// 次数必须在键里：否则同一操作者的第二次重驱会被误判为旧请求重放。
	second := base
	second.ExpectedRedriveCount = 1
	if AuditID(base) == AuditID(second) {
		t.Fatal("不同次数必须派生不同的 _id")
	}
	// 不同操作者的同一张工单不得互相顶掉。
	otherActor := base
	otherActor.ActorID = "admin-2"
	if AuditID(base) == AuditID(otherActor) {
		t.Fatal("不同操作者必须派生不同的 _id")
	}
	otherKey := base
	otherKey.EventKey = "ticket-43"
	if AuditID(base) == AuditID(otherKey) {
		t.Fatal("不同幂等键必须派生不同的 _id")
	}
}
