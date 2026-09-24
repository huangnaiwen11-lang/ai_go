package adminapps

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// 创建与更新共用的字段上限，取自 Node `app.routes.js` 的 zod 契约。
// 注意 name 的 API 上限是 80，而 Mongoose 模型层没有 maxlength —— 以更严的 API 契约为准。
const (
	maxNameLength        = 80
	maxDescriptionLength = 500
	maxClientIDLength    = 200
	maxBundleIDLength    = 200
	maxPackageNameLength = 200
	maxDomainLength      = 200
	maxAPIURLLength      = 500
	maxAppKeyLength      = 80
)

// nativeBuild 子结构的上限，同样取自 zod。
const (
	maxDisplayNameLength         = 100
	maxDeepLinkSchemeLength      = 100
	maxSigningFingerprints       = 10
	maxFacebookAppIDLength       = 100
	maxFacebookClientTokenLength = 500
	maxOAuthClientIDLength       = 300
	maxNativeBuildFileChars      = 20000
)

// defaultAppAPIURL 与 Node `DEFAULT_APP_API_URL` 一致：apiUrl 与 domain 都没有时的兜底 origin。
//
// 这个常量在 Node 侧被 `app.packageConfig.js`、`app.nativeConfigTemplate.js`
// 和前端 `nativeConfigTemplate.ts` 三处各写了一遍，值都是同一个。
const defaultAppAPIURL = "https://cling-ai.com"

// 创建白名单 = Node `app.routes.js` 里 `createBody`（zod）的键集，一字不多。
//
// 注意 Node 的两条路径**故意不对称**，这里如实保留：
//   - 创建只收 13 个键，`version` / `iconUrl` / `contact` / `packageConfig` 会被 zod 丢掉；
//   - 更新收 `version` / `iconUrl` / `contact`（见 updatableFields）。
//
// 前端 `sanitizeAppPayload` 两个键都拼了，但新建路径上它们恒为 undefined
// （表单没有对应字段，`registrationValues()` 也不设置），经 JSON.stringify 后不会出现在请求体里
// —— 所以严格拒绝未知键不会误伤前端。
//
// Node 用 zod `z.object` 静默丢弃未知键，这里改为显式 400：
// 静默丢字段会让「前端拼错键名」在 Go 与 Node 下表现不同，而 400 至少是响的。
var creatableFields = []string{
	"name", "platform", "description", "appKey", "clientId", "bundleId",
	"packageName", "domain", "apiUrl", "status", "serverIntegrations",
	"nativeConfig", "nativeBuild",
}

// 更新白名单 = Node `updateApp` 的 allowedFields + serverIntegrations。
//
// `serverIntegrations` 不在 allowedFields 里，但 updateApp 在循环之外单独处理了它
// （`data.serverIntegrations !== undefined`），所以它确实可写。
// `appKey` 与 `platform` 两个路径都不可改 —— 与 Node 一致。
var updatableFields = []string{
	"name", "description", "clientId", "bundleId", "packageName", "domain",
	"apiUrl", "version", "status", "iconUrl", "contact", "nativeBuild",
	"nativeConfig", "serverIntegrations",
}

var (
	platformValues = []string{"ios", "android", "web"}
	statusValues   = []string{"active", "suspended", "deprecated"}

	// secretRefPattern 与 Node `app.serverIntegrations.js` 的 SECRET_REF_RE 一致。
	secretRefPattern = regexp.MustCompile(`^APP_[A-Z0-9_]{3,120}$`)

	// fingerprintPattern 与 Node 的 sha256CertificateFingerprint 一致：
	// 64 位裸 hex，或 32 组冒号分隔的 hex。
	fingerprintPattern = regexp.MustCompile(`^(?:[A-Fa-f0-9]{64}|(?:[A-Fa-f0-9]{2}:){31}[A-Fa-f0-9]{2})$`)

	// objectIDPattern 与 Node `app.routes.js` 的 idParams 一致。
	objectIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{24}$`)
)

// ValidateCreate 校验创建请求体，返回待写入的字段集合。
func ValidateCreate(raw map[string]any) (Document, error) {
	document, err := validateFields(raw, creatableFields)
	if err != nil {
		return nil, err
	}

	name, err := requiredString(document, "name", maxNameLength)
	if err != nil {
		return nil, err
	}
	document["name"] = name

	platform, err := requiredEnum(document, "platform", platformValues)
	if err != nil {
		return nil, err
	}
	document["platform"] = platform

	if _, exists := document["status"]; exists {
		status, err := enumValue(document["status"], "status", statusValues)
		if err != nil {
			return nil, err
		}
		document["status"] = status
	} else {
		// 与 Node 模型默认值一致。
		document["status"] = "active"
	}

	// appKey 可来自顶层，也可来自 nativeBuild.appKey
	// （Node 的 `data.appKey || data.nativeBuild?.appKey`）。
	if _, exists := document["appKey"]; !exists {
		if appKey := PathString(document, "nativeBuild", "appKey"); appKey != "" {
			document["appKey"] = appKey
		}
	}

	if err := normalizeCommonFields(document, document); err != nil {
		return nil, err
	}
	if err := normalizeWritePayload(document, document, nil); err != nil {
		return nil, err
	}
	return document, nil
}

// ValidateUpdate 校验更新请求体（部分字段）。
//
// existing 是库里的当前文档：Node 的 `updateApp` 用 `app.platform` 决定 domain 要不要
// 走 web 归一化，用合并后的 app 派生原生标识。只看补丁本身会漏掉这些规则。
//
// 与 Node 的一处差异（有意收紧）：Node 的 PATCH 路由**没有** body 校验，
// 靠 Mongoose 兜底（非法 enum 会变成 500）。这里把 zod 契约前置到 PATCH，
// 非法值直接 400 —— 对一个写接口来说，前置校验比「先落库再报错」更好排障。
func ValidateUpdate(raw map[string]any, existing map[string]any) (Document, error) {
	document, err := validateFields(raw, updatableFields)
	if err != nil {
		return nil, err
	}
	if len(document) == 0 {
		return nil, fmt.Errorf("%w: empty update", ErrInvalid)
	}
	if _, exists := document["name"]; exists {
		name, err := requiredString(document, "name", maxNameLength)
		if err != nil {
			return nil, err
		}
		document["name"] = name
	}
	if _, exists := document["status"]; exists {
		status, err := enumValue(document["status"], "status", statusValues)
		if err != nil {
			return nil, err
		}
		document["status"] = status
	}

	if err := normalizeCommonFields(document, Merge(existing, document)); err != nil {
		return nil, err
	}
	// 归一化会改写 document 里的值（apiUrl / domain），合并视图必须重算 ——
	// nativeConfig 的归属断言比的正是归一化之后的 origin。
	if err := normalizeWritePayload(document, Merge(existing, document), existing); err != nil {
		return nil, err
	}
	return document, nil
}

// IsObjectID 判定字符串是否为 Node `idParams` 认可的 24 位 hex。
func IsObjectID(value string) bool {
	return objectIDPattern.MatchString(value)
}

// validateFields 检查未知键并把已知键原样收集起来。
func validateFields(raw map[string]any, allowed []string) (Document, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: missing body", ErrInvalid)
	}
	permitted := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		permitted[field] = struct{}{}
	}
	document := make(Document, len(raw))
	for key, value := range raw {
		if _, ok := permitted[key]; !ok {
			return nil, fmt.Errorf("%w: unknown field %q", ErrInvalid, key)
		}
		document[key] = value
	}
	return document, nil
}

// normalizeCommonFields 处理两个入口共用的字段归一化。
//
// effective 是「补丁 + 既有值」的合并视图，用于那些要看 App 全貌才能判定的规则
// （web 的 domain 归一化）。创建时它就是 document 本身。
func normalizeCommonFields(document, effective Document) error {
	stringLimits := map[string]int{
		"description": maxDescriptionLength,
		"clientId":    maxClientIDLength,
		"bundleId":    maxBundleIDLength,
		"packageName": maxPackageNameLength,
		"domain":      maxDomainLength,
		"apiUrl":      maxAPIURLLength,
		"iconUrl":     maxAPIURLLength,
		"appKey":      maxAppKeyLength,
	}
	keys := make([]string, 0, len(stringLimits))
	for key := range stringLimits {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, exists := document[key]; !exists {
			continue
		}
		value, err := optionalString(document[key], key, stringLimits[key])
		if err != nil {
			return err
		}
		if value == "" {
			// 空字符串与「未提交」在 Node 里都落成 undefined，这里显式清空字段。
			document[key] = nil
			continue
		}
		document[key] = value
	}

	if _, exists := document["domain"]; exists {
		if platform, _ := effective["platform"].(string); platform == "web" {
			if domain, _ := document["domain"].(string); domain != "" {
				document["domain"] = NormalizeWebDomain(domain)
			}
		}
	}
	if raw, ok := document["apiUrl"].(string); ok && raw != "" {
		normalized, err := NormalizeAPIURL(raw)
		if err != nil {
			return err
		}
		document["apiUrl"] = normalized
	}

	// version 与 contact 在 Node 模型里是子文档（`{current, minSupported}` / `{email, name}`），
	// iconUrl 是字符串。三者在 createBody 里都不存在，只在 PATCH 可写。
	for _, key := range []string{"version", "contact"} {
		if _, exists := document[key]; !exists {
			continue
		}
		if _, err := optionalObject(document[key], key); err != nil {
			return err
		}
	}

	if _, exists := document["nativeBuild"]; exists {
		nativeBuild, err := normalizeNativeBuild(document["nativeBuild"])
		if err != nil {
			return err
		}
		document["nativeBuild"] = nativeBuild
	}
	return nil
}

// normalizeWritePayload 处理两个入口共用的子对象载荷：nativeConfig 与 serverIntegrations。
//
// existing 为 nil 表示创建（Node 在创建时把 existing 当 undefined 传给
// normalizeServerIntegrations）。
func normalizeWritePayload(document, effective, existing Document) error {
	if raw, exists := document["nativeConfig"]; exists {
		if existing == nil {
			nativeConfig, err := ValidateNativeConfig(raw)
			if err != nil {
				return err
			}
			editable := WithoutServerOwnedStateRules(nativeConfig)
			// Node 只在「显式配置去掉 stateRules 后非空」时才写 nativeConfig，
			// 空对象会被当成「没给」并落到服务端默认模板。Go 不做默认模板，
			// 于是同样把空对象当成「没给」—— 保持两边「写不写这个字段」一致。
			if len(editable) == 0 {
				delete(document, "nativeConfig")
				return nil
			}
			if err := AssertNativeConfigOwnership(effective, editable); err != nil {
				return err
			}
			document["nativeConfig"] = editable
		} else {
			merged, err := ApplyNativeConfigUpdate(effective, existing, raw)
			if err != nil {
				return err
			}
			// Node 只在原生平台上对更新结果再断言一次归属（web 不走这条）。
			if platform, _ := effective["platform"].(string); platform == "ios" || platform == "android" {
				if err := AssertNativeConfigOwnership(effective, merged); err != nil {
					return err
				}
			}
			document["nativeConfig"] = merged
		}
	}

	if raw, exists := document["serverIntegrations"]; exists {
		integrations, err := NormalizeServerIntegrations(effective, raw, existing["serverIntegrations"])
		if err != nil {
			return err
		}
		document["serverIntegrations"] = integrations
	}
	return nil
}

// normalizeNativeBuild 复刻 zod `nativeBuild` 的校验：结构、长度上限与指纹格式。
func normalizeNativeBuild(value any) (map[string]any, error) {
	nativeBuild, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: nativeBuild must be a JSON object", ErrInvalid)
	}
	if _, err := optionalString(nativeBuild["appKey"], "nativeBuild.appKey", maxAppKeyLength); err != nil {
		return nil, err
	}
	if _, err := optionalString(nativeBuild["displayName"], "nativeBuild.displayName", maxDisplayNameLength); err != nil {
		return nil, err
	}

	android, err := optionalObjectField(nativeBuild, "android")
	if err != nil {
		return nil, err
	}
	if android != nil {
		for _, field := range []struct {
			key   string
			limit int
		}{{"applicationId", maxPackageNameLength}, {"namespace", maxPackageNameLength}} {
			if _, err := optionalString(android[field.key], "nativeBuild.android."+field.key, field.limit); err != nil {
				return nil, err
			}
		}
		if _, err := stringArray(android["deepLinkSchemes"], "nativeBuild.android.deepLinkSchemes", maxDeepLinkSchemeLength, 0); err != nil {
			return nil, err
		}
		fingerprints, err := stringArray(android["signingCertificateSha256"], "nativeBuild.android.signingCertificateSha256", 0, maxSigningFingerprints)
		if err != nil {
			return nil, err
		}
		for _, fingerprint := range fingerprints {
			if !fingerprintPattern.MatchString(fingerprint) {
				return nil, fmt.Errorf("%w: Invalid SHA-256 certificate fingerprint", ErrInvalid)
			}
		}
	}

	ios, err := optionalObjectField(nativeBuild, "ios")
	if err != nil {
		return nil, err
	}
	if ios != nil {
		if _, err := optionalString(ios["bundleId"], "nativeBuild.ios.bundleId", maxBundleIDLength); err != nil {
			return nil, err
		}
	}

	facebook, err := optionalObjectField(nativeBuild, "facebook")
	if err != nil {
		return nil, err
	}
	if facebook != nil {
		if _, err := optionalString(facebook["appId"], "nativeBuild.facebook.appId", maxFacebookAppIDLength); err != nil {
			return nil, err
		}
		if _, err := optionalString(facebook["clientToken"], "nativeBuild.facebook.clientToken", maxFacebookClientTokenLength); err != nil {
			return nil, err
		}
	}

	google, err := optionalObjectField(nativeBuild, "google")
	if err != nil {
		return nil, err
	}
	if google != nil {
		for _, key := range []string{"iosClientId", "iosReversedClientId", "androidClientId", "serverClientId"} {
			if _, err := optionalString(google[key], "nativeBuild.google."+key, maxOAuthClientIDLength); err != nil {
				return nil, err
			}
		}
	}

	firebase, err := optionalObjectField(nativeBuild, "firebase")
	if err != nil {
		return nil, err
	}
	if firebase != nil {
		for _, key := range []string{"androidGoogleServicesJson", "iosGoogleServiceInfoPlist"} {
			if _, err := optionalString(firebase[key], "nativeBuild.firebase."+key, maxNativeBuildFileChars); err != nil {
				return nil, err
			}
		}
	}
	return nativeBuild, nil
}

// optionalObjectField 读取可选子对象；键不存在返回 nil，类型不符返回 400。
func optionalObjectField(container map[string]any, key string) (map[string]any, error) {
	value, exists := container[key]
	if !exists {
		return nil, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a JSON object", ErrInvalid, key)
	}
	return object, nil
}

// stringArray 校验可选字符串数组，逐项做长度上限与数组长度上限检查。
func stringArray(value any, path string, itemLimit, maxItems int) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	rows, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be an array", ErrInvalid, path)
	}
	if maxItems > 0 && len(rows) > maxItems {
		return nil, fmt.Errorf("%w: %s exceeds %d items", ErrInvalid, path, maxItems)
	}
	result := make([]string, 0, len(rows))
	for index, row := range rows {
		text, ok := row.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s[%d] must be a string", ErrInvalid, path, index)
		}
		text = strings.TrimSpace(text)
		if itemLimit > 0 && len([]rune(text)) > itemLimit {
			return nil, fmt.Errorf("%w: %s[%d] exceeds %d characters", ErrInvalid, path, index, itemLimit)
		}
		result = append(result, text)
	}
	return result, nil
}

func requiredString(document Document, key string, maxLength int) (string, error) {
	value, err := optionalString(document[key], key, maxLength)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalid, key)
	}
	return value, nil
}

func optionalString(value any, key string, maxLength int) (string, error) {
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s must be a string", ErrInvalid, key)
	}
	text = strings.TrimSpace(text)
	if len([]rune(text)) > maxLength {
		return "", fmt.Errorf("%w: %s exceeds %d characters", ErrInvalid, key, maxLength)
	}
	return text, nil
}

func optionalObject(value any, key string) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be an object", ErrInvalid, key)
	}
	return object, nil
}

func requiredEnum(document Document, key string, allowed []string) (string, error) {
	if _, exists := document[key]; !exists {
		return "", fmt.Errorf("%w: %s is required", ErrInvalid, key)
	}
	return enumValue(document[key], key, allowed)
}

func enumValue(value any, key string, allowed []string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s must be a string", ErrInvalid, key)
	}
	text = strings.TrimSpace(text)
	for _, candidate := range allowed {
		if text == candidate {
			return text, nil
		}
	}
	return "", fmt.Errorf("%w: %s must be one of %s", ErrInvalid, key, strings.Join(allowed, ", "))
}

// NormalizeWebDomain 复刻 Node `normalizeWebDomain`：只保留主机名，去协议、路径与端口。
//
// 端口必须去掉：Node 成功分支走 `url.hostname`（不含端口），
// 失败分支也显式 `.split(':')[0]`。留着端口会让 `example.com:8080` 与 `example.com`
// 被当成两个不同的 domain。
func NormalizeWebDomain(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	input := raw
	if !strings.Contains(input, "://") {
		input = "https://" + input
	}
	if parsed, err := url.Parse(input); err == nil && parsed.Host != "" {
		return strings.ToLower(parsed.Hostname())
	}
	fallback := raw
	if index := strings.Index(fallback, "://"); index >= 0 {
		fallback = fallback[index+3:]
	}
	if index := strings.IndexAny(fallback, "/?#"); index >= 0 {
		fallback = fallback[:index]
	}
	if index := strings.Index(fallback, ":"); index >= 0 {
		fallback = fallback[:index]
	}
	return strings.ToLower(strings.TrimSpace(fallback))
}

// NormalizeAPIURL 复刻 Node `normalizeApiUrl`：归一化成 `scheme://host[:port]` 形式的 origin。
//
// 非法输入返回 ErrInvalid 而不是空串：Node 在这里是抛 400 的，
// 静默清空会让「填了个错的 API 地址」变成「保存成功但地址没了」。
func NormalizeAPIURL(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", nil
	}
	input := raw
	if !strings.Contains(input, "://") {
		input = "https://" + input
	}
	parsed, err := url.Parse(input)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%w: apiUrl must be a valid http(s) URL", ErrInvalid)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: apiUrl must be a valid http(s) URL", ErrInvalid)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: apiUrl must be a valid http(s) URL", ErrInvalid)
	}
	if port := parsed.Port(); port != "" && port != defaultPortFor(scheme) {
		return scheme + "://" + host + ":" + port, nil
	}
	return scheme + "://" + host, nil
}

func defaultPortFor(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// ResolveAPIURL 复刻 Node `resolveAppApiUrl`：apiUrl 优先，其次 domain，最后兜底。
//
// 与 NormalizeAPIURL 的差别是「不报错」：这里读的是已落库的值，
// 遇到脏数据时按兜底链继续走，而不是让整个读接口 500。
func ResolveAPIURL(document map[string]any) string {
	if raw := FieldString(document, "apiUrl"); raw != "" {
		if normalized, err := NormalizeAPIURL(raw); err == nil && normalized != "" {
			return normalized
		}
	}
	if domain := NormalizeWebDomain(FieldString(document, "domain")); domain != "" {
		return "https://" + domain
	}
	return defaultAppAPIURL
}

// NativeIdentifiers 复刻 Node 模型 pre('validate') 的派生规则：
// [platform==android ? clientId : 空, packageName, nativeBuild.android.applicationId]
// 全部小写去重。空结果返回 nil（Node 用 undefined 表示「没有标识」）。
func NativeIdentifiers(document map[string]any) []string {
	candidates := make([]string, 0, 3)
	if platform := FieldString(document, "platform"); platform == "android" {
		candidates = append(candidates, FieldString(document, "clientId"))
	}
	candidates = append(candidates, FieldString(document, "packageName"))
	candidates = append(candidates, PathString(document, "nativeBuild", "android", "applicationId"))

	seen := map[string]struct{}{}
	identifiers := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		normalized := strings.ToLower(strings.TrimSpace(candidate))
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		identifiers = append(identifiers, normalized)
	}
	if len(identifiers) == 0 {
		return nil
	}
	return identifiers
}

// IsAndroidApp 判定目标 App 是否走 Google Play 体系。
func IsAndroidApp(document map[string]any) bool {
	return FieldString(document, "platform") == "android"
}

// AndroidPackageNames 复刻 Node `androidPackageNamesForApp` 的取值来源。
func AndroidPackageNames(document map[string]any) []string {
	names := make([]string, 0, 3)
	for _, candidate := range []string{
		FieldString(document, "packageName"),
		PathString(document, "nativeBuild", "android", "applicationId"),
		FieldString(document, "clientId"),
	} {
		normalized := strings.ToLower(strings.TrimSpace(candidate))
		if normalized != "" {
			names = append(names, normalized)
		}
	}
	return names
}

// ErrConflict 表示与既有 App 的标识冲突（HTTP 409）。
var ErrConflict = errors.New("admin apps: conflict")

// ConflictError 在 ErrConflict 之上携带对外说明文案。
//
// Node 的四次唯一性查询各自有不同文案（'clientId already registered' /
// 'Bundle ID already registered' / …），前端直接把 message 显示给管理员。
// 统一成一句「冲突」会让管理员无法判断是哪个标识撞了。
type ConflictError struct{ Message string }

func (e *ConflictError) Error() string { return "admin apps: conflict: " + e.Message }

// Is 让 errors.Is(err, ErrConflict) 对 ConflictError 成立，
// 这样调用方既能用哨兵值分流，也能取回具体文案。
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }
