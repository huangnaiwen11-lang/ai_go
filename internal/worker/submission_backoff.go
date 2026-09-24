package worker

import (
	"errors"
	"time"

	"ai-business-service/internal/integrations/polarstarb2b"
)

// 重排节奏与旧 Node 后端保持一致（backend/src/infrastructure/jobQueue/
// chat-job-runner.service.js 的 computeBackoffMs），并满足 PolarStar 参考文档
// §7 的要求：429/503 遵循 Retry-After、502 指数退避、504 复用同一幂等键。
//
// 旧实现：
//
//	retryAfterSeconds > 0 → clamp(1000ms, 5min, retryAfterSeconds*1000)
//	否则                  → min(5min, 10s * 2^retryCount)
const (
	// submissionRetryBase 是首次重排的间隔；之后每次翻倍。
	submissionRetryBase = 10 * time.Second
	// submissionRetryCap 同时约束指数退避与平台给出的 Retry-After。
	submissionRetryCap = 5 * time.Minute
	// retryAfterFloor 防止平台给出 0 或亚秒值时形成忙轮询。
	retryAfterFloor = time.Second
	// maxRetryBackoffExponent 只用于避免移位溢出；实际上限由 submissionRetryCap 决定。
	maxRetryBackoffExponent = 16
)

// nextSubmissionAttempt 计算下一次尝试的最早时刻。
//
// attempt 取自已领取事件的尝试计数（`SubmissionRecord.Fence`），因此它同时是
// 「已经尝试过几次」的唯一事实来源；不要另建计数器，否则崩溃重启后会从头开始。
func nextSubmissionAttempt(now time.Time, attempt int32, cause error) time.Time {
	return now.Add(submissionBackoff(attempt, cause))
}

// submissionBackoff 返回本次重排应等待的时长。
// Retry-After 优先于指数退避：平台明确要求的等待时间比我们的估算更权威。
func submissionBackoff(attempt int32, cause error) time.Duration {
	if delay := retryAfterDelay(cause); delay > 0 {
		return delay
	}
	return exponentialBackoff(attempt)
}

// retryAfterDelay 提取平台要求的等待时长；没有则返回 0。
//
// 只认适配器的 ClientError：它是唯一经过解析与上限校验的来源。
// 上限在适配器里已经夹过一次，这里再夹一次，避免调用方绕过适配器构造错误。
func retryAfterDelay(cause error) time.Duration {
	var clientErr *polarstarb2b.ClientError
	if !errors.As(cause, &clientErr) || clientErr.RetryAfter <= 0 {
		return 0
	}
	delay := clientErr.RetryAfter
	if delay < retryAfterFloor {
		delay = retryAfterFloor
	}
	if delay > submissionRetryCap {
		delay = submissionRetryCap
	}
	return delay
}

// exponentialBackoff 返回 10s × 2^(attempt-1)，上限 5 分钟。
//
// 指数取 attempt-1 而不是 attempt：尝试计数在领取时已经自增，第一次失败
// （attempt == 1）应当退避 10s，与旧实现的 retryCount == 0 对齐。
func exponentialBackoff(attempt int32) time.Duration {
	exponent := attempt - 1
	if exponent < 0 {
		exponent = 0
	}
	if exponent > maxRetryBackoffExponent {
		exponent = maxRetryBackoffExponent
	}
	delay := submissionRetryBase << uint(exponent)
	if delay <= 0 || delay > submissionRetryCap {
		return submissionRetryCap
	}
	return delay
}

// 素材搬运的重排节奏与提交链路刻意分开。
//
// 口径来自 docs/对象存储_配置基线.md §6.2 冻结的「重试 5 次，退避 15000ms × 2^(attempt-1)」。
// 不复用 submissionRetryBase/RetryAfter 的原因有两条：
//  1. 基数不同（15s vs 10s），照抄提交链路会直接偏离基线；
//  2. 提交链路的 Retry-After 来自 provider POST 响应，素材搬运是下载与对象写入，
//     平台给出的等待时间对它没有权威性，混用会让素材重试跟着平台漂移。
const (
	// materialUploadRetryBase 是素材搬运首次重排的间隔；之后每次翻倍。
	materialUploadRetryBase = 15 * time.Second
	// materialUploadRetryCap 防止指数退避超过运营可接受的等待时间。
	// 预算只有 5 次，实际上限为 15s × 2^4 = 4 分钟，cap 只是防御性上界。
	materialUploadRetryCap = 5 * time.Minute
)

// nextMaterialUploadAttempt 计算素材搬运下一次尝试的最早时刻。
//
// attempt 取自已领取事件的 AttemptCount，与提交链路共用同一个权威计数：
// 它在领取时自增、重新入队时不重置。
func nextMaterialUploadAttempt(now time.Time, attempt int32) time.Time {
	return now.Add(materialUploadBackoff(attempt))
}

func materialUploadBackoff(attempt int32) time.Duration {
	exponent := attempt - 1
	if exponent < 0 {
		exponent = 0
	}
	if exponent > maxRetryBackoffExponent {
		exponent = maxRetryBackoffExponent
	}
	delay := materialUploadRetryBase << uint(exponent)
	if delay <= 0 || delay > materialUploadRetryCap {
		return materialUploadRetryCap
	}
	return delay
}
