package generation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const (
	observationTestTenant   = "tenant-cling"
	observationTestAccount  = "account-main"
	observationTestJobID    = "job-b2b-1"
	observationTestStepID   = "step-b2b-1"
	observationTestDelivery = "delivery-b2b-1"
)

var observationTestAt = time.Date(2026, time.September, 19, 12, 0, 0, 0, time.UTC)

func observationTestBody(status, capability string, extra map[string]any) []byte {
	body := map[string]any{
		"contractVersion": polarstarb2b.CallbackContractVersion,
		"tenantId":        observationTestTenant,
		"jobId":           observationTestJobID,
		"externalId":      observationTestStepID,
		"capability":      capability,
		"status":          status,
		"deliveryId":      observationTestDelivery,
	}
	for key, value := range extra {
		body[key] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return payload
}

func observationTestRecord(t *testing.T, payload []byte) *bizgeneration.ProviderInboxRecord {
	t.Helper()
	record, err := bizgeneration.NewProviderInboxRecord(
		observationTestAccount, observationTestDelivery, observationTestStepID, observationTestJobID, "", payload, 1, observationTestAt,
	)
	if err != nil {
		t.Fatalf("构造投递事实: %v", err)
	}
	return &record
}

func observationTestIntent(t *testing.T) *bizgeneration.ProviderSubmissionIntent {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"externalId":     observationTestStepID,
		"idempotencyKey": "cling-step:" + observationTestStepID,
		"capability":     "text_to_image",
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	request := bizgeneration.FrozenProviderRequest{
		Route: creations.ExecutionRoute{
			Provider: creations.PolarStarB2BProvider, AccountRef: observationTestAccount,
			ContractVersion: "b2b.job.v2", MappingVersion: "mapping-1",
		},
		StepID: observationTestStepID, Capability: "text_to_image",
		IdempotencyKey: "cling-step:" + observationTestStepID, Digest: hex.EncodeToString(sum[:]), Payload: payload,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("冻结请求不合法: %v", err)
	}
	return &bizgeneration.ProviderSubmissionIntent{Request: request, PreparedAt: observationTestAt, Fence: 1, ExternalExecutionID: observationTestJobID}
}

func observationTestParser(t *testing.T) *B2BObservationParser {
	t.Helper()
	parser, err := NewB2BObservationParser(observationTestTenant, observationTestAccount)
	if err != nil {
		t.Fatalf("构造归一化解析器: %v", err)
	}
	return parser
}

func TestB2BObservationParser把完成态投递归一化为终态(t *testing.T) {
	parser := observationTestParser(t)
	record := observationTestRecord(t, observationTestBody("completed", "image_to_video", map[string]any{
		"output": map[string]any{"resultUrl": "https://cdn.polarstar.work/result/step-b2b-1.mp4"},
	}))
	observation, err := parser.Parse(record)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if observation.Capability != "image_to_video" || observation.Status != bizgeneration.ProviderTerminalCompleted ||
		observation.ResultRef != "https://cdn.polarstar.work/result/step-b2b-1.mp4" {
		t.Fatalf("观察值 = %#v", observation)
	}
}

func TestB2BObservationParser非完成态不带结果地址(t *testing.T) {
	parser := observationTestParser(t)
	for _, status := range []string{"failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			record := observationTestRecord(t, observationTestBody(status, "text_to_image", map[string]any{
				"error": map[string]any{"code": "MODEL_BUSY"},
			}))
			observation, err := parser.Parse(record)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if observation.Status != bizgeneration.ProviderTerminalStatus(status) || observation.ResultRef != "" {
				t.Fatalf("观察值 = %#v", observation)
			}
		})
	}
}

func TestB2BObservationParser拒绝与持久化事实不符的报文(t *testing.T) {
	payload := observationTestBody("completed", "text_to_image", map[string]any{
		"output": map[string]any{"resultUrl": "https://cdn.polarstar.work/r.png"},
	})
	otherAccountParser, err := NewB2BObservationParser(observationTestTenant, "account-other")
	if err != nil {
		t.Fatal(err)
	}
	// 同一份字节被配错了账号的解析器读到时，绝不能当成合法投递：账号是 inbox
	// 键空间的一部分，配错账号意味着这份终态会被写进另一笔业务的槽位。
	if _, err := otherAccountParser.Parse(observationTestRecord(t, payload)); !errors.Is(err, bizgeneration.ErrProviderInboxStepMismatch) {
		t.Fatalf("账号不符错误 = %v，期望 ErrProviderInboxStepMismatch", err)
	}

	// 报文自报的 externalId 与记录里的步骤不一致：同样拒绝，不挑一个信。
	record := observationTestRecord(t, payload)
	record.StepID = "step-other"
	if _, err := observationTestParser(t).Parse(record); !errors.Is(err, bizgeneration.ErrProviderInboxStepMismatch) {
		t.Fatalf("步骤不符错误 = %v，期望 ErrProviderInboxStepMismatch", err)
	}
}

func TestB2BObservationParser拒绝被改写或不完整的字节(t *testing.T) {
	payload := observationTestBody("completed", "text_to_image", map[string]any{
		"output": map[string]any{"resultUrl": "https://cdn.polarstar.work/r.png"},
	})
	parser := observationTestParser(t)

	rewritten := observationTestRecord(t, payload)
	rewritten.Payload = []byte(strings.Replace(string(payload), "completed", "failed", 1))
	if _, err := parser.Parse(rewritten); !errors.Is(err, polarstarb2b.ErrInvalidCallback) {
		t.Fatalf("改写字节错误 = %v，期望 ErrInvalidCallback", err)
	}

	truncated := observationTestRecord(t, payload)
	truncated.Payload = payload[:len(payload)/2]
	if _, err := parser.Parse(truncated); !errors.Is(err, polarstarb2b.ErrInvalidCallback) {
		t.Fatalf("截断字节错误 = %v，期望 ErrInvalidCallback", err)
	}

	// 摘要对得上但能力不在冻结集合：必须在解析层被挡住，不能落到领域再判。
	unknown := observationTestRecord(t, observationTestBody("completed", "text_to_video", map[string]any{
		"output": map[string]any{"resultUrl": "https://cdn.polarstar.work/r.png"},
	}))
	if _, err := parser.Parse(unknown); !errors.Is(err, polarstarb2b.ErrInvalidCallback) {
		t.Fatalf("未知能力错误 = %v，期望 ErrInvalidCallback", err)
	}
}

func TestNewB2BObservationParser拒绝不完整身份(t *testing.T) {
	for name, args := range map[string]struct{ tenant, account string }{
		"租户为空":  {"", observationTestAccount},
		"账号为空":  {observationTestTenant, ""},
		"租户带空白": {observationTestTenant + " ", observationTestAccount},
		"账号超长":  {observationTestTenant, strings.Repeat("a", 201)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewB2BObservationParser(args.tenant, args.account); !errors.Is(err, ErrB2BObservationParserConfig) {
				t.Fatalf("错误 = %v，期望 ErrB2BObservationParserConfig", err)
			}
		})
	}
}

func TestB2BObservationParser缺少事实或解析器时拒绝(t *testing.T) {
	var absent *B2BObservationParser
	if _, err := absent.Parse(observationTestRecord(t, observationTestBody("failed", "text_to_image", nil))); !errors.Is(err, ErrB2BObservationParserConfig) {
		t.Fatalf("空解析器错误 = %v", err)
	}
	if _, err := observationTestParser(t).Parse(nil); !errors.Is(err, ErrB2BObservationParserConfig) {
		t.Fatalf("空事实错误 = %v", err)
	}
	if _, err := observationTestParser(t).ParseLookup(nil, polarstarb2b.Job{}); !errors.Is(err, ErrB2BObservationParserConfig) {
		t.Fatalf("空意图错误 = %v", err)
	}
}

// 摘要口径必须与 data 层持久化的 PayloadDigest 同源：否则重放永远摘要不符。
func TestB2BObservationParser按持久化摘要核对字节(t *testing.T) {
	payload := observationTestBody("failed", "text_to_image", nil)
	record := observationTestRecord(t, payload)
	sum := sha256.Sum256(payload)
	if record.PayloadDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("PayloadDigest = %q", record.PayloadDigest)
	}
	if _, err := observationTestParser(t).Parse(record); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
}

func observationTestJob(status string, mutate func(*polarstarb2b.Job)) polarstarb2b.Job {
	job := polarstarb2b.Job{
		ContractVersion: "b2b.job.v2", JobID: observationTestJobID, TenantID: observationTestTenant,
		ExternalID: observationTestStepID, Capability: "text_to_image", Status: status,
		ResponseDigest: strings.Repeat("a", 64),
	}
	if mutate != nil {
		mutate(&job)
	}
	return job
}

func TestB2BObservationParser把查询终态归一化为同一套结论(t *testing.T) {
	parser := observationTestParser(t)
	completed := observationTestJob("completed", func(job *polarstarb2b.Job) {
		job.ResultURL = "https://cdn.polarstar.work/r.png"
	})
	outcome, err := parser.ParseLookup(observationTestIntent(t), completed)
	if err != nil {
		t.Fatalf("ParseLookup() error = %v", err)
	}
	if !outcome.Terminal || outcome.JobID != observationTestJobID || outcome.ResponseDigest != completed.ResponseDigest {
		t.Fatalf("查询结果 = %#v", outcome)
	}
	if outcome.Observation.Status != bizgeneration.ProviderTerminalCompleted || outcome.Observation.ResultRef != completed.ResultURL {
		t.Fatalf("观察值 = %#v", outcome.Observation)
	}

	for _, status := range []string{"failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			outcome, err := parser.ParseLookup(observationTestIntent(t), observationTestJob(status, nil))
			if err != nil {
				t.Fatalf("ParseLookup() error = %v", err)
			}
			if !outcome.Terminal || outcome.Observation.Status != bizgeneration.ProviderTerminalStatus(status) || outcome.Observation.ResultRef != "" {
				t.Fatalf("观察值 = %#v", outcome.Observation)
			}
		})
	}
}

// 中间态不是失败：把「还在跑」当成终态会把未完成的结论写进账本，而把中间态
// 当成错误会让一次正常等待变成告警。
func TestB2BObservationParser把中间态报告为未终结(t *testing.T) {
	parser := observationTestParser(t)
	for _, status := range []string{"queued", "dispatching", "processing"} {
		t.Run(status, func(t *testing.T) {
			outcome, err := parser.ParseLookup(observationTestIntent(t), observationTestJob(status, nil))
			if err != nil {
				t.Fatalf("ParseLookup() error = %v", err)
			}
			if outcome.Terminal || outcome.Observation != (bizgeneration.ProviderTerminalObservation{}) {
				t.Fatalf("查询结果 = %#v", outcome)
			}
			if outcome.JobID != observationTestJobID || outcome.ResponseDigest == "" {
				t.Fatalf("未终结结果丢失身份: %#v", outcome)
			}
		})
	}
}

func TestB2BObservationParser拒绝与冻结意图不符的查询响应(t *testing.T) {
	parser := observationTestParser(t)
	for name, job := range map[string]polarstarb2b.Job{
		"租户不符":   observationTestJob("failed", func(job *polarstarb2b.Job) { job.TenantID = "tenant-other" }),
		"外部标识不符": observationTestJob("failed", func(job *polarstarb2b.Job) { job.ExternalID = "step-other" }),
		"能力不符":   observationTestJob("failed", func(job *polarstarb2b.Job) { job.Capability = "image_edit" }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parser.ParseLookup(observationTestIntent(t), job); !errors.Is(err, bizgeneration.ErrProviderInboxStepMismatch) {
				t.Fatalf("错误 = %v，期望 ErrProviderInboxStepMismatch", err)
			}
		})
	}
}

// 完成态缺少结果地址在客户端解码阶段就被拒；这里再挡一次，保证归一化入口
// 单独被调用时也不会产出「完成但没有结果」的观察值。
func TestB2BObservationParser拒绝完成态缺少结果地址(t *testing.T) {
	if _, err := observationTestParser(t).ParseLookup(observationTestIntent(t), observationTestJob("completed", nil)); !errors.Is(err, bizgeneration.ErrInvalidProviderTerminalObservation) {
		t.Fatalf("错误 = %v，期望 ErrInvalidProviderTerminalObservation", err)
	}
}
