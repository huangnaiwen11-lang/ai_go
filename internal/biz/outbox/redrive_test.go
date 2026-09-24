package outbox

import (
	"testing"
	"time"
)

func redriveTestEvent(t *testing.T, createdAt time.Time) *Event {
	t.Helper()
	// 这里的 ID 只用于占位：本文件的用例不落库，因此不需要跨用例唯一。
	// t.Name() 含中文与斜杠，不是合法的稳定步骤 ID。
	event, err := NewPending(SubmissionEventID("step-1"), "creation-1", []byte(`{"prompt":"x"}`), createdAt)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	return event
}

func TestAttentionWindow未重驱时就是建单即窗口(t *testing.T) {
	createdAt := time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC)
	event := redriveTestEvent(t, createdAt)

	startedAt, attemptBase := event.AttentionWindow()
	if !startedAt.Equal(createdAt) || attemptBase != 0 {
		t.Fatalf("AttentionWindow() = %v / %d, want 建单时间 / 0", startedAt, attemptBase)
	}

	// 回归底线：未重驱事件的判定结果必须与引入重驱之前逐位一致。改造前预算用
	// AttemptCount、年龄用 CreatedAt；窗口版本必须给出完全相同的结果，否则
	// 这次改动会悄悄改掉 C5 已经加固过的两条终态口径。
	for _, attempt := range []int32{0, 1, MaterialUploadRetryBudget - 1, MaterialUploadRetryBudget, MaterialUploadRetryBudget + 3} {
		for _, age := range []time.Duration{0, time.Minute, MaxEventAge - time.Second, MaxEventAge, 48 * time.Hour} {
			event.AttemptCount = attempt
			event.CreatedAt = createdAt
			now := createdAt.Add(age)
			start, base := event.AttentionWindow()
			if RetryBudgetExhausted(event.AttemptCount-base, MaterialUploadRetryBudget) != RetryBudgetExhausted(attempt, MaterialUploadRetryBudget) {
				t.Fatalf("预算判定被改变: attempt=%d age=%s", attempt, age)
			}
			if EventAgeExceeded(start, now) != EventAgeExceeded(createdAt, now) {
				t.Fatalf("年龄判定被改变: attempt=%d age=%s", attempt, age)
			}
		}
	}
}

func TestAttentionWindow重驱后开启新窗口(t *testing.T) {
	createdAt := time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC)
	redrivenAt := createdAt.Add(30 * time.Hour)
	event := redriveTestEvent(t, createdAt)
	// 事件在原始预算与原始年龄都已耗尽之后才被人工重驱。
	event.AttemptCount = MaterialUploadRetryBudget + 2
	event.RedriveStartedAt = redrivenAt
	event.RedriveAttemptBase = event.AttemptCount
	event.RedriveCount = 1

	startedAt, attemptBase := event.AttentionWindow()
	if !startedAt.Equal(redrivenAt) || attemptBase != event.AttemptCount {
		t.Fatalf("AttentionWindow() = %v / %d, want 重驱时刻 / %d", startedAt, attemptBase, event.AttemptCount)
	}
	// 这正是重驱的意义：如果不推进窗口，这个事件会在下一次失败时立刻回到
	// needs_attention（预算已耗尽、年龄已超标），重驱等于空操作。
	if RetryBudgetExhausted(event.AttemptCount-attemptBase, MaterialUploadRetryBudget) {
		t.Fatal("重驱后的事件没有拿到新的重试预算")
	}
	if EventAgeExceeded(startedAt, redrivenAt.Add(time.Minute)) {
		t.Fatal("重驱后的事件没有拿到新的年龄窗口")
	}
	if !EventAgeExceeded(event.CreatedAt, redrivenAt.Add(time.Minute)) {
		t.Fatal("原始事实时间应当仍然超标；否则说明年龄判定没有真正改用窗口")
	}
}

func TestAttentionWindow窗口起点不得早于建单时间(t *testing.T) {
	createdAt := time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC)
	event := redriveTestEvent(t, createdAt)
	// 只可能来自时钟回拨：窗口起点早于建单时间。起点退回建单时间，但基数必须
	// 保留，否则已经推进过的事实会被抹掉。
	event.RedriveStartedAt = createdAt.Add(-time.Hour)
	event.RedriveAttemptBase = 4

	startedAt, attemptBase := event.AttentionWindow()
	if !startedAt.Equal(createdAt) || attemptBase != 4 {
		t.Fatalf("AttentionWindow() = %v / %d, want 建单时间 / 4", startedAt, attemptBase)
	}
}

func TestRedriveEligible只在人工关注且未达上限时为真(t *testing.T) {
	event := redriveTestEvent(t, time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC))
	if event.RedriveEligible() {
		t.Fatal("pending 事件不应当可重驱")
	}
	event.DeliveryStatus = DeliveryStatusDelivered
	if event.RedriveEligible() {
		t.Fatal("delivered 事件不应当可重驱")
	}
	event.DeliveryStatus = DeliveryStatusNeedsAttention
	if !event.RedriveEligible() {
		t.Fatal("人工关注且未达上限的事件应当可重驱")
	}
	for count := int32(0); count < MaxRedriveCount; count++ {
		event.RedriveCount = count
		if !event.RedriveEligible() {
			t.Fatalf("redrive_count = %d 时应当仍可重驱", count)
		}
	}
	event.RedriveCount = MaxRedriveCount
	if event.RedriveEligible() {
		t.Fatalf("redrive_count = %d 已达 V1 上限，不应再可重驱", MaxRedriveCount)
	}
	if (*Event)(nil).RedriveEligible() {
		t.Fatal("nil 事件不应当可重驱")
	}
}

func TestRedriveCommandValidate拒绝匿名无原因或负次数(t *testing.T) {
	now := time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC)
	valid := RedriveCommand{
		EventID: "event-1", ExpectedRedriveCount: 0,
		ActorID: "admin-1", Reason: "provider 已恢复", Key: "ticket-42", At: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}

	cases := map[string]func(RedriveCommand) RedriveCommand{
		"缺事件":    func(c RedriveCommand) RedriveCommand { c.EventID = ""; return c },
		"缺操作者":   func(c RedriveCommand) RedriveCommand { c.ActorID = ""; return c },
		"缺原因":    func(c RedriveCommand) RedriveCommand { c.Reason = ""; return c },
		"缺幂等键":   func(c RedriveCommand) RedriveCommand { c.Key = ""; return c },
		"负次数令牌":  func(c RedriveCommand) RedriveCommand { c.ExpectedRedriveCount = -1; return c },
		"次数超出上限": func(c RedriveCommand) RedriveCommand { c.ExpectedRedriveCount = MaxRedriveCount + 1; return c },
		"缺时刻":    func(c RedriveCommand) RedriveCommand { c.At = time.Time{}; return c },
		"首尾空白":   func(c RedriveCommand) RedriveCommand { c.Reason = " 偷懒 "; return c },
	}
	for name, mutate := range cases {
		if err := mutate(valid).Validate(); err == nil {
			t.Fatalf("%s: Validate() error = nil, want 拒绝", name)
		}
	}
	// 次数令牌的边界必须分清「无效请求」与「事件不满足资格」：
	// 观察到 == MaxRedriveCount 是真实可观察值 → 请求合法，由资格判断拒绝；
	// 写成更大的值不可能被观察到 → 请求本身无效。
	atCap := valid
	atCap.ExpectedRedriveCount = MaxRedriveCount
	if err := atCap.Validate(); err != nil {
		t.Fatalf("观察到上限次数是合法请求，应交给资格判断: %v", err)
	}
	beyondCap := valid
	beyondCap.ExpectedRedriveCount = MaxRedriveCount + 1
	if err := beyondCap.Validate(); err != ErrInvalidRedriveCommand {
		t.Fatalf("超出可观察上限的次数令牌应判为无效请求: %v", err)
	}
}
