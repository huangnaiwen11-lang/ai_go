package generationcontract

import "errors"

var (
	ErrUnsupportedMode       = errors.New("generation contract: unsupported public mode")
	ErrInvalidPublicContract = errors.New("generation contract: invalid public route or branch")
	ErrInvalidEvidence       = errors.New("generation contract: invalid evidence set")
	ErrEvidenceIncomplete    = errors.New("generation contract: evidence is incomplete")
)

// Mode 是面向用户的产品模式，不等同于 generation-service 的底层 capability。
// 这个边界可防止历史专项功能因为底层执行原子相近而被误放入公开 Go 迁移范围。
type Mode string

const (
	TextToImage  Mode = "text_to_image"
	ImageToImage Mode = "image_to_image"
	ImageToVideo Mode = "image_to_video"
)

// EvidenceKind 表示迁移前必须由受控、脱敏运行捕获证明的行为面。
type EvidenceKind string

const (
	RequestValidation    EvidenceKind = "request_validation"
	Authentication       EvidenceKind = "authentication"
	AccountEligibility   EvidenceKind = "account_eligibility"
	SensitiveRateLimit   EvidenceKind = "sensitive_rate_limit"
	ContentPolicy        EvidenceKind = "content_policy"
	BillingSettlement    EvidenceKind = "billing_settlement"
	TaskPersistence      EvidenceKind = "task_persistence"
	IdempotentSubmission EvidenceKind = "idempotent_submission"
	CallbackConvergence  EvidenceKind = "callback_convergence"
	RefundConvergence    EvidenceKind = "refund_convergence"
)

var requiredEvidenceKinds = [...]EvidenceKind{
	RequestValidation,
	Authentication,
	AccountEligibility,
	SensitiveRateLimit,
	ContentPolicy,
	BillingSettlement,
	TaskPersistence,
	IdempotentSubmission,
	CallbackConvergence,
	RefundConvergence,
}

var knownEvidenceKinds = func() map[EvidenceKind]struct{} {
	kinds := make(map[EvidenceKind]struct{}, len(requiredEvidenceKinds))
	for _, kind := range requiredEvidenceKinds {
		kinds[kind] = struct{}{}
	}
	return kinds
}()

// Evidence 只保存脱敏材料引用，不保存 Authorization、Cookie、请求正文、任务 ID 或
// 其他可识别数据。ArtifactRef 可指向受控捕获的哈希或审计记录号。
type Evidence struct {
	Kind        EvidenceKind
	ArtifactRef string
}

// Capture 是一次模式级迁移审计的最小公开描述。Scope 精确区分同一 Node 路由下的
// T2I/I2I 与 I2V/prompt-only 分支，避免“同一路径”被误解成整体可迁移。
type Capture struct {
	Mode     Mode
	Route    string
	Scope    string
	Evidence []Evidence
}

type publicContract struct {
	route string
	scope string
}

var publicContracts = map[Mode]publicContract{
	TextToImage: {
		route: "POST /api/chat/image/async",
		scope: "inputImages=absent",
	},
	ImageToImage: {
		route: "POST /api/chat/image/async",
		scope: "inputImages=1-2",
	},
	ImageToVideo: {
		route: "POST /api/chat/video",
		scope: "imageUrl=present",
	},
}

// Validate 仅验证迁移准入材料是否完整，不解析或相信材料内容，更不代表允许将
// 任何流量切到 Go。错误保持通用，避免审计日志或调用方引用被返回给外部。
func Validate(capture Capture) error {
	contract, supported := publicContracts[capture.Mode]
	if !supported {
		return ErrUnsupportedMode
	}
	if capture.Route != contract.route || capture.Scope != contract.scope {
		return ErrInvalidPublicContract
	}

	seen := make(map[EvidenceKind]bool, len(capture.Evidence))
	for _, evidence := range capture.Evidence {
		if _, known := knownEvidenceKinds[evidence.Kind]; !known || seen[evidence.Kind] {
			return ErrInvalidEvidence
		}
		if evidence.ArtifactRef == "" {
			return ErrEvidenceIncomplete
		}
		seen[evidence.Kind] = true
	}
	for _, required := range requiredEvidenceKinds {
		if !seen[required] {
			return ErrEvidenceIncomplete
		}
	}

	return nil
}
