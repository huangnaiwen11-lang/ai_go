package worker

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"ai-business-service/internal/integrations/polarstarb2b"
)

// 这些数值来自旧 Node 后端 chat-job-runner.service.js 的 computeBackoffMs，
// 同时满足 PolarStar 参考文档 §7「429/503 遵循 Retry-After、502 指数退避」。
func TestSubmissionBackoff优先遵循RetryAfter(t *testing.T) {
	cases := map[string]struct {
		retryAfter time.Duration
		attempt    int32
		want       time.Duration
	}{
		"平台给出的等待时间原样采用":         {90 * time.Second, 3, 90 * time.Second},
		"存在RetryAfter时不再叠加指数退避": {90 * time.Second, 9, 90 * time.Second},
		"低于下限时抬到1秒":             {200 * time.Millisecond, 3, time.Second},
		"零值不作为等待时间":             {0, 1, submissionRetryBase},
		"负值不作为等待时间":             {-time.Minute, 1, submissionRetryBase},
		"超过上限时压到5分钟":            {30 * time.Minute, 3, submissionRetryCap},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			cause := &polarstarb2b.ClientError{HTTPStatus: 429, Code: "RATE_LIMITED", RetryAfter: testCase.retryAfter}
			if got := submissionBackoff(testCase.attempt, cause); got != testCase.want {
				t.Fatalf("submissionBackoff(%d) = %s, want %s", testCase.attempt, got, testCase.want)
			}
		})
	}
}

func TestSubmissionBackoff无RetryAfter时指数增长并封顶(t *testing.T) {
	cause := errors.New("transport failure")
	cases := map[int32]time.Duration{
		0: 10 * time.Second,
		1: 10 * time.Second,
		2: 20 * time.Second,
		3: 40 * time.Second,
		4: 80 * time.Second,
		5: 160 * time.Second,
		6: submissionRetryCap,
		// 极大尝试次数不得因移位溢出变成负值或回绕成小间隔。
		1 << 20: submissionRetryCap,
		-5:      10 * time.Second,
	}
	for attempt, want := range cases {
		if got := submissionBackoff(attempt, cause); got != want {
			t.Fatalf("submissionBackoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestSubmissionBackoff只认可识别的ClientError(t *testing.T) {
	// 被包裹的 ClientError 仍然生效：调用链上普遍用 %w 包装。
	wrapped := fmt.Errorf("submit b2b: %w", &polarstarb2b.ClientError{HTTPStatus: 503, Code: "SERVICE_UNAVAILABLE", RetryAfter: 45 * time.Second})
	if got := submissionBackoff(3, wrapped); got != 45*time.Second {
		t.Fatalf("submissionBackoff() = %s, want 45s", got)
	}
	// 无法识别的错误必须回退到指数退避，而不是被当成「平台要求立即重试」。
	if got := submissionBackoff(3, errors.New("boom")); got != 40*time.Second {
		t.Fatalf("submissionBackoff() = %s, want 40s", got)
	}
	// 已证明请求未发出的错误没有等待要求，同样走指数退避。
	if got := submissionBackoff(1, polarstarb2b.ErrNotSent); got != submissionRetryBase {
		t.Fatalf("submissionBackoff() = %s, want %s", got, submissionRetryBase)
	}
}

func TestNextSubmissionAttempt从当前时刻起算(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	if got, want := nextSubmissionAttempt(now, 3, nil), now.Add(40*time.Second); !got.Equal(want) {
		t.Fatalf("nextSubmissionAttempt() = %s, want %s", got, want)
	}
	// 无论何时计算，结果都必须晚于当前时刻：等于当前时刻等于忙轮询。
	if got := nextSubmissionAttempt(now, 0, nil); !got.After(now) {
		t.Fatalf("nextSubmissionAttempt() = %s, want after %s", got, now)
	}
}

// 素材搬运的重排节奏来自 docs/对象存储_配置基线.md §6.2（15s × 2^(attempt-1)），
// 与提交链路的 10s 基数刻意不同。这里同时钉住基数，防止有人「顺手统一」成
// submissionRetryBase，那会直接偏离冻结基线。
func TestMaterialUploadBackoff用基线冻结的15秒基数(t *testing.T) {
	cases := map[int32]time.Duration{
		0: 15 * time.Second,
		1: 15 * time.Second,
		2: 30 * time.Second,
		3: time.Minute,
		4: 2 * time.Minute,
		5: 4 * time.Minute,
		// 极大尝试次数不得因移位溢出变成负值或回绕成小间隔。
		1 << 20: materialUploadRetryCap,
		-5:      15 * time.Second,
	}
	for attempt, want := range cases {
		if got := materialUploadBackoff(attempt); got != want {
			t.Fatalf("materialUploadBackoff(%d) = %s, want %s", attempt, got, want)
		}
	}
	if materialUploadRetryBase == submissionRetryBase {
		t.Fatal("素材退避基数不得与提交链路相同：两者是不同冻结口径")
	}
}

func TestNextMaterialUploadAttempt不使用提交链路的RetryAfter(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	if got, want := nextMaterialUploadAttempt(now, 2), now.Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("nextMaterialUploadAttempt() = %s, want %s", got, want)
	}
	if got := nextMaterialUploadAttempt(now, 0); !got.After(now) {
		t.Fatalf("nextMaterialUploadAttempt() = %s, want after %s", got, now)
	}
}
