package creations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/executionv2"
)

// StepAtom 表示生成中台支持的最小技术原子。
// 用户侧只选择模板，不能直接提交该类型的值。
type StepAtom string

const (
	// AtomTextToImage 表示文生图原子。
	AtomTextToImage StepAtom = "text_to_image"
	// AtomImageEdit 表示模板图编辑原子，例如换装或脱衣模板。
	AtomImageEdit StepAtom = "image_edit"
	// AtomImageToVideo 表示图生视频原子。
	AtomImageToVideo StepAtom = "image_to_video"
)

// CreationStatus 表示用户可见创作的生命周期状态。
type CreationStatus string

const (
	// CreationStatusPendingSubmission 表示本地占位和预留已提交，尚未提交生成中台。
	CreationStatusPendingSubmission CreationStatus = "pending_submission"
	// CreationStatusSubmissionFailed 表示生成中台提交已确定失败，等待外层事务完成冲正和发件箱结案。
	CreationStatusSubmissionFailed CreationStatus = "submission_failed"
	// CreationStatusConfiscated 表示审核拒绝后，已预留的权益被没收且不退款。
	CreationStatusConfiscated CreationStatus = "confiscated"
	// CreationStatusSucceeded 表示所有生成步骤已由回调 CAS 确认成功。
	CreationStatusSucceeded CreationStatus = "succeeded"
	// CreationStatusGenerationFailed 表示已提交步骤由回调 CAS 确认技术失败或取消。
	CreationStatusGenerationFailed CreationStatus = "generation_failed"
)

// StepSubmitStatus 表示内部步骤是否能够被后续提交器执行。
type StepSubmitStatus string

const (
	// StepSubmitStatusReady 表示步骤无依赖，可以由后续提交器执行。
	StepSubmitStatusReady StepSubmitStatus = "ready"
	// StepSubmitStatusBlocked 表示步骤依赖前序输出，目前不能提交。
	StepSubmitStatusBlocked StepSubmitStatus = "blocked"
	// StepSubmitStatusReconciling 表示步骤的上一次提交结果未知，等待再次领取后核对。
	StepSubmitStatusReconciling StepSubmitStatus = "reconciling"
	// StepSubmitStatusSubmitted 表示步骤已经获得生成中台的稳定任务标识。
	StepSubmitStatusSubmitted StepSubmitStatus = "submitted"
	// StepSubmitStatusSubmissionFailed 表示步骤提交已确定失败，不能再由提交器执行。
	StepSubmitStatusSubmissionFailed StepSubmitStatus = "submission_failed"
	// StepSubmitStatusConfiscated 表示步骤因审核拒绝被终止，不能再由提交器执行。
	StepSubmitStatusConfiscated StepSubmitStatus = "confiscated"
	// StepSubmitStatusSucceeded 表示步骤只能由回调 CAS 确认成功。
	StepSubmitStatusSucceeded StepSubmitStatus = "succeeded"
	// StepSubmitStatusGenerationFailed 表示步骤只能由回调 CAS 确认技术失败或取消。
	StepSubmitStatusGenerationFailed StepSubmitStatus = "generation_failed"
)

// StepPlan 是服务端模板解析后得到的内部执行计划。
// 它不应由未来 HTTP 或 gRPC 客户端直接传入。
type StepPlan struct {
	Sequence int32
	Atom     StepAtom
}

// InitialSubmission 是服务端模板编译器传入的首步骤技术快照。
// 它只能携带模型 SKU 和技术输入 JSON；步骤原子必须从已编译计划推导，不能由调用方指定。
// 用户、模板、权益、余额、账本、支付和其他产品事实不属于该类型。
type InitialSubmission struct {
	ModelSKU string
	Input    json.RawMessage
}

// DeferredImageToVideo 表示等待首帧成功后才可提交的第二步图生视频技术输入。
// 它只由服务端模板编译器构造，不能携带首帧或任何业务事实。
type DeferredImageToVideo struct {
	ModelSKU      string
	InputTemplate json.RawMessage
}

// DeferredRecipeStatus 表示延迟图生视频配方的业务状态。
type DeferredRecipeStatus string

const (
	// DeferredRecipeStatusPending 表示配方已冻结，等待首帧回调绑定。
	DeferredRecipeStatusPending DeferredRecipeStatus = "pending"
	// DeferredRecipeStatusConsumed 表示首帧已经绑定，不能再次用于生成第二步骤快照。
	DeferredRecipeStatusConsumed DeferredRecipeStatus = "consumed"
)

// DeferredRecipe 是创建事务内冻结的第二步图生视频技术配方。
// 它不保存用户、模板、计费、账本或首帧事实。
type DeferredRecipe struct {
	StepID        string
	CreationID    string
	Atom          StepAtom
	ModelSKU      string
	InputTemplate []byte
	Digest        string
	Status        DeferredRecipeStatus
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CreateReservedRequest 是创建创作占位和账本预留的内部应用命令。
// 它不携带订阅、VIP、余额等权益事实；订阅只能由用例从服务端读模型读取。
// InputDigest 仅保存提示词和素材引用的可信摘要，绝不保存原文或 URL。
type CreateReservedRequest struct {
	UserID          string
	IdempotencyKey  string
	TemplateID      string
	TemplateVersion int64
	Product         entitlement.GenerationRequest
	InputDigest     string
	Plan            []StepPlan
	// InitialSubmission 仅允许服务端模板编译器填写，未来传输层客户端不得直接构造。
	InitialSubmission *InitialSubmission
	// DeferredImageToVideo 仅允许两步骤文生视频的服务端模板编译器填写。
	DeferredImageToVideo *DeferredImageToVideo
}

// Creation 是用户可见的创作占位领域对象。
type Creation struct {
	ID              string
	IdempotencyKey  string
	UserID          string
	TemplateID      string
	TemplateVersion int64
	// Output 记录用户选择模板后得到的产品类型，供状态读取严格隔离图片与视频创作。
	Output entitlement.ProductOutput
	// VideoDurationSeconds 冻结视频产品时长，确保状态读取不受后续模板更新影响。
	// 图片创作固定为 0，不能把图片状态投影为视频任务。
	VideoDurationSeconds int32
	RequestFingerprint   string
	Status               CreationStatus
	Version              int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// CreationStep 是创作内部的单个技术原子执行事实。
type CreationStep struct {
	ID              string
	CreationID      string
	Sequence        int32
	Atom            StepAtom
	SubmitStatus    StepSubmitStatus
	CallbackVersion int64
	CreatedAt       time.Time
}

// CreateReservedResult 是创建成功后返回给应用层的稳定创作与本次结算投影。
// 余额只是在预留成功后的同一事务中读取的快照，不可作为下一次创建的余额门禁。
type CreateReservedResult struct {
	Creation            *Creation
	Steps               []CreationStep
	Reservation         *ledger.Reservation
	DiamondBalanceAfter int64
}

// RequestFingerprint 返回用于幂等冲突判定的 SHA-256 请求指纹。
// 字段按固定顺序做长度前缀编码，避免相邻变长字段拼接后产生边界歧义。
// 非法输入摘要会返回 ErrInvalidCreateCommand，调用方不得把该命令当作可创建请求继续处理。
func RequestFingerprint(request CreateReservedRequest) (string, error) {
	if !isInputDigest(request.InputDigest) {
		return "", ErrInvalidCreateCommand
	}
	snapshot, err := compileInitialSubmission(request)
	if err != nil {
		return "", err
	}
	deferredDigest, err := deferredRecipeDigest(request)
	if err != nil {
		return "", err
	}
	return requestFingerprint(request, snapshot, deferredDigest)
}

func requestFingerprint(request CreateReservedRequest, snapshot *executionv2.Snapshot, deferredDigest string) (string, error) {
	if !isInputDigest(request.InputDigest) {
		return "", ErrInvalidCreateCommand
	}

	fields := make([]string, 0, 12+len(request.Plan)*2)
	fields = append(fields,
		request.UserID,
		request.TemplateID,
		strconv.FormatInt(request.TemplateVersion, 10),
		string(request.Product.Output),
		strconv.FormatInt(int64(request.Product.Video.DurationSeconds), 10),
		strconv.FormatBool(request.Product.Video.EnableAudio),
		strconv.FormatInt(int64(request.Product.Video.ReferenceImageCount), 10),
		request.InputDigest,
		strconv.Itoa(len(request.Plan)),
	)
	for _, step := range request.Plan {
		fields = append(fields, strconv.FormatInt(int64(step.Sequence), 10), string(step.Atom))
	}
	if snapshot != nil {
		fields = append(fields, "initial_submission", string(snapshot.Capability), snapshot.ModelSKU, snapshot.Digest)
	}
	if deferredDigest != "" {
		fields = append(fields, "deferred_image_to_video", deferredDigest)
	}

	encoded := make([]byte, 0, 256)
	for _, field := range fields {
		encoded = appendLengthPrefixed(encoded, field)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func deferredRecipeDigest(request CreateReservedRequest) (string, error) {
	if request.DeferredImageToVideo == nil {
		return "", nil
	}
	compiled, err := executionv2.CompileDeferredImageToVideo(request.DeferredImageToVideo.ModelSKU, request.DeferredImageToVideo.InputTemplate)
	if err != nil {
		return "", ErrInvalidCreateCommand
	}
	return compiled.Digest, nil
}

// ValidatePlan 校验产品输出和内部步骤计划之间的固定映射关系。
// Animate 和未知原子不会匹配任一合法组合，因此会被明确拒绝。
func ValidatePlan(product entitlement.GenerationRequest, plan []StepPlan) error {
	if !hasContinuousSequences(plan) {
		return ErrInvalidStepPlan
	}

	switch product.Output {
	case entitlement.ProductOutputImage:
		if len(plan) == 1 && (plan[0].Atom == AtomTextToImage || plan[0].Atom == AtomImageEdit) {
			return nil
		}
	case entitlement.ProductOutputVideo:
		if len(plan) == 1 && plan[0].Atom == AtomImageToVideo {
			return nil
		}
		if len(plan) == 2 && plan[0].Atom == AtomTextToImage && plan[1].Atom == AtomImageToVideo {
			return nil
		}
	}
	return ErrInvalidStepPlan
}

// appendLengthPrefixed 向缓冲区追加“字节长度:字段内容;”格式。
// 长度基于字节而不是 rune，保证哈希输入与 Go 字符串的实际编码一致。
func appendLengthPrefixed(buffer []byte, field string) []byte {
	buffer = strconv.AppendInt(buffer, int64(len(field)), 10)
	buffer = append(buffer, ':')
	buffer = append(buffer, field...)
	return append(buffer, ';')
}

// isInputDigest 只接受小写的 SHA-256 十六进制摘要。
// 这样不会把提示词、素材 URL 或其他原始敏感载荷写入创作领域对象。
func isInputDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for index := range value {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// hasContinuousSequences 确保步骤编号从 1 开始连续递增。
func hasContinuousSequences(plan []StepPlan) bool {
	if len(plan) == 0 {
		return false
	}
	for index, step := range plan {
		if step.Sequence != int32(index+1) {
			return false
		}
	}
	return true
}

func compileInitialSubmission(request CreateReservedRequest) (*executionv2.Snapshot, error) {
	if request.InitialSubmission == nil {
		return nil, nil
	}
	if len(request.Plan) == 0 {
		return nil, ErrInvalidCreateCommand
	}
	capability, ok := capabilityForAtom(request.Plan[0].Atom)
	if !ok {
		return nil, ErrInvalidCreateCommand
	}
	snapshot, err := executionv2.Compile(capability, request.InitialSubmission.ModelSKU, request.InitialSubmission.Input)
	if err != nil {
		return nil, ErrInvalidCreateCommand
	}
	return snapshot, nil
}

func capabilityForAtom(atom StepAtom) (executionv2.Capability, bool) {
	switch atom {
	case AtomTextToImage:
		return executionv2.CapabilityTextToImage, true
	case AtomImageEdit:
		return executionv2.CapabilityImageEdit, true
	case AtomImageToVideo:
		return executionv2.CapabilityImageToVideo, true
	default:
		return "", false
	}
}
