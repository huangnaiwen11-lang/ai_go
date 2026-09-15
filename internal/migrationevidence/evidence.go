package migrationevidence

import (
	"errors"
	"regexp"
)

var (
	ErrUnsupportedStage   = errors.New("migration evidence: unsupported stage")
	ErrInvalidEvidence    = errors.New("migration evidence: invalid evidence set")
	ErrEvidenceIncomplete = errors.New("migration evidence: evidence is incomplete")
	ErrInvalidArtifactRef = errors.New("migration evidence: invalid artifact reference")
)

// Stage 表示仍处于“待核实”状态的迁移阶段。仅允许在此处显式维护的阶段，
// 避免把未审计的功能（例如已明确排除的 Animate）加入准入材料。
type Stage string

const (
	Identity      Stage = "identity"
	WalletBilling Stage = "wallet_billing"
	Admin         Stage = "admin"
)

// EvidenceKind 是一个可审计的行为场景，而非 API 路由或实现方案。
// 场景名称保持稳定，方便后续将真实运行捕获与静态矩阵逐项关联。
type EvidenceKind string

const (
	CredentialCompatibility EvidenceKind = "credential_compatibility"
	AccountScopeRejection   EvidenceKind = "account_scope_rejection"
	OAuthAndMagicLink       EvidenceKind = "oauth_and_magic_link"
	LoginLockAndAudit       EvidenceKind = "login_lock_and_audit"
	UnauthorizedEnvelope    EvidenceKind = "unauthorized_envelope"
	RegistrationIdempotency EvidenceKind = "registration_idempotency"

	WalletCreationSemantics  EvidenceKind = "wallet_creation_semantics"
	LedgerPagination         EvidenceKind = "ledger_pagination"
	BalanceMutationInvariant EvidenceKind = "balance_mutation_invariant"
	MutationIdempotency      EvidenceKind = "mutation_idempotency"
	PaymentCallbackBoundary  EvidenceKind = "payment_callback_boundary"
	EntitlementConvergence   EvidenceKind = "entitlement_convergence"

	AdminRoutePermission    EvidenceKind = "admin_route_permission"
	AdminScopeCompatibility EvidenceKind = "admin_scope_compatibility"
	AdminAPIAuthentication  EvidenceKind = "admin_api_authentication"
	AdminMutationAudit      EvidenceKind = "admin_mutation_audit"
	AdminPageAccess         EvidenceKind = "admin_page_access"
	AdminRollbackReadiness  EvidenceKind = "admin_rollback_readiness"
)

// Evidence 只容纳不可逆的哈希或受控审计记录号，绝不放入请求正文、Cookie、
// Authorization、支付回执、签名、用户标识或回调载荷。
type Evidence struct {
	Kind        EvidenceKind
	ArtifactRef string
}

// Capture 是一次阶段级迁移审计的最小脱敏描述。
// 校验通过仅代表清单字段和场景齐全，不代表该阶段已获准迁移或切流。
type Capture struct {
	Stage    Stage
	Evidence []Evidence
}

var requiredEvidenceByStage = map[Stage][]EvidenceKind{
	Identity: {
		CredentialCompatibility,
		AccountScopeRejection,
		OAuthAndMagicLink,
		LoginLockAndAudit,
		UnauthorizedEnvelope,
		RegistrationIdempotency,
	},
	WalletBilling: {
		WalletCreationSemantics,
		LedgerPagination,
		BalanceMutationInvariant,
		MutationIdempotency,
		PaymentCallbackBoundary,
		EntitlementConvergence,
	},
	Admin: {
		AdminRoutePermission,
		AdminScopeCompatibility,
		AdminAPIAuthentication,
		AdminMutationAudit,
		AdminPageAccess,
		AdminRollbackReadiness,
	},
}

var (
	sha256ArtifactRef = regexp.MustCompile(`\Asha256:[a-f0-9]{64}\z`)
	auditArtifactRef  = regexp.MustCompile(`\Aaudit:[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}\z`)
)

// RequiredEvidenceKinds 返回阶段必须覆盖的运行行为场景副本。
// 返回副本而非内部切片，避免调用方意外修改后影响后续校验。
func RequiredEvidenceKinds(stage Stage) ([]EvidenceKind, bool) {
	required, ok := requiredEvidenceByStage[stage]
	if !ok {
		return nil, false
	}

	return append([]EvidenceKind(nil), required...), true
}

// Validate 只验证脱敏清单的结构、场景完备性与材料引用格式。
// 它不会读取材料、连接生产环境或授权任何服务、路由和 Gateway 分流。
func Validate(capture Capture) error {
	required, supported := requiredEvidenceByStage[capture.Stage]
	if !supported {
		return ErrUnsupportedStage
	}

	known := make(map[EvidenceKind]struct{}, len(required))
	for _, kind := range required {
		known[kind] = struct{}{}
	}

	seen := make(map[EvidenceKind]struct{}, len(capture.Evidence))
	for _, evidence := range capture.Evidence {
		if _, allowed := known[evidence.Kind]; !allowed {
			return ErrInvalidEvidence
		}
		if _, duplicate := seen[evidence.Kind]; duplicate {
			return ErrInvalidEvidence
		}
		if !isOpaqueArtifactRef(evidence.ArtifactRef) {
			return ErrInvalidArtifactRef
		}
		seen[evidence.Kind] = struct{}{}
	}

	for _, kind := range required {
		if _, exists := seen[kind]; !exists {
			return ErrEvidenceIncomplete
		}
	}

	return nil
}

// isOpaqueArtifactRef 通过固定格式消除“看似引用、实为敏感值”的通道。
// 目前仅接受 SHA-256 摘要或 UUID 形式的受控审计记录号。
func isOpaqueArtifactRef(value string) bool {
	return sha256ArtifactRef.MatchString(value) || auditArtifactRef.MatchString(value)
}
