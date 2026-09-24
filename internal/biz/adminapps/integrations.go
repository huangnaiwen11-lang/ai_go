package adminapps

import (
	"fmt"
	"strings"
)

// healthStatusValues 与 Node `defaultHealth` 允许的取值一致。
var healthStatusValues = []string{"unconfigured", "healthy", "degraded", "error"}

// NormalizeServerIntegrations 复刻 Node `app.serverIntegrations.js` 的
// `normalizeServerIntegrations`。
//
// existing 为 nil 表示创建（Node 传 undefined，靠默认参数兜成 {}）。
//
// 一个容易踩的点：`health` 是**服务端运行态**。Node 的 `defaultHealth` 只从既有值里
// 挑合法状态，客户端提交的 health 一律被忽略 —— 照抄这个行为很重要，
// 否则前端一次保存就能把健康状态刷成 healthy。
func NormalizeServerIntegrations(app Document, input, existing any) (map[string]any, error) {
	payload, ok := input.(map[string]any)
	if !ok {
		// 显式 null 或非对象都按 Node 的 400 处理（Node 只在 undefined 时才用默认空对象）。
		return nil, fmt.Errorf("%w: serverIntegrations must be a JSON object", ErrInvalid)
	}
	previous, _ := existing.(map[string]any)

	googleIdentity, err := normalizeGoogleIdentity(app, payload["googleIdentity"], previous["googleIdentity"])
	if err != nil {
		return nil, err
	}
	firebase, err := normalizeFirebase(payload["firebase"], previous["firebase"])
	if err != nil {
		return nil, err
	}
	googlePlay, err := normalizeGooglePlay(app, payload["googlePlay"], previous["googlePlay"])
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"googleIdentity": googleIdentity,
		"firebase":       firebase,
		"googlePlay":     googlePlay,
	}, nil
}

func normalizeGoogleIdentity(app Document, input, existing any) (map[string]any, error) {
	payload, _ := input.(map[string]any)
	previous, _ := existing.(map[string]any)

	audiences := stringList(payload["allowedAudiences"])
	if _, exists := payload["allowedAudiences"]; !exists {
		audiences = stringList(previous["allowedAudiences"])
		if len(audiences) == 0 {
			audiences = stringList([]any{PathString(app, "nativeBuild", "google", "serverClientId")})
		}
	}
	parties := stringList(payload["allowedAuthorizedParties"])
	if _, exists := payload["allowedAuthorizedParties"]; !exists {
		parties = stringList(previous["allowedAuthorizedParties"])
		if len(parties) == 0 {
			parties = stringList([]any{PathString(app, "nativeBuild", "google", "androidClientId")})
		}
	}

	enabled := boolInput(payload, "enabled", previous)
	if enabled && len(audiences) == 0 {
		return nil, fmt.Errorf("%w: serverIntegrations.googleIdentity requires at least one allowed audience", ErrInvalid)
	}
	if enabled && len(AndroidPackageNames(app)) > 0 && len(parties) == 0 {
		return nil, fmt.Errorf("%w: serverIntegrations.googleIdentity requires at least one allowed authorized party for Android", ErrInvalid)
	}
	return map[string]any{
		"enabled":                  enabled,
		"allowedAudiences":         audiences,
		"allowedAuthorizedParties": parties,
		"health":                   defaultHealth(previous["health"]),
	}, nil
}

func normalizeFirebase(input, existing any) (map[string]any, error) {
	payload, _ := input.(map[string]any)
	previous, _ := existing.(map[string]any)

	enabled := boolInput(payload, "enabled", previous)
	projectID := stringInput(payload, "projectId", previous)
	secretRef, err := optionalSecretRef(
		stringInput(payload, "credentialSecretRef", previous),
		"serverIntegrations.firebase.credentialSecretRef",
	)
	if err != nil {
		return nil, err
	}
	if enabled && (projectID == "" || secretRef == "") {
		return nil, fmt.Errorf("%w: serverIntegrations.firebase requires projectId and credentialSecretRef when enabled", ErrInvalid)
	}
	result := map[string]any{"enabled": enabled, "health": defaultHealth(previous["health"])}
	if projectID != "" {
		result["projectId"] = projectID
	}
	if secretRef != "" {
		result["credentialSecretRef"] = secretRef
	}
	return result, nil
}

func normalizeGooglePlay(app Document, input, existing any) (map[string]any, error) {
	payload, _ := input.(map[string]any)
	previous, _ := existing.(map[string]any)

	enabled := boolInput(payload, "enabled", previous)
	appPackageNames := AndroidPackageNames(app)
	appPackageName := ""
	if len(appPackageNames) > 0 {
		appPackageName = appPackageNames[0]
	}

	// 注意「键存在但值为空」与「键不存在」是两件事：
	// Node 用 `input.packageName !== undefined ? ... : (existing.packageName || appPackageName)`，
	// 显式提交空串会走空串分支（并在 enabled 时被下面的必填校验挡下），不会回落到 App 的包名。
	packageName := ""
	if _, exists := payload["packageName"]; exists {
		packageName = trimmedString(payload["packageName"])
	} else {
		packageName = FieldString(previous, "packageName")
		if packageName == "" {
			packageName = appPackageName
		}
	}
	packageName = strings.ToLower(packageName)

	secretRef, err := optionalSecretRef(
		stringInput(payload, "credentialSecretRef", previous),
		"serverIntegrations.googlePlay.credentialSecretRef",
	)
	if err != nil {
		return nil, err
	}
	if packageName != "" && len(appPackageNames) > 0 && !containsString(appPackageNames, packageName) {
		return nil, fmt.Errorf("%w: serverIntegrations.googlePlay.packageName must match App packageName", ErrInvalid)
	}
	if enabled && (packageName == "" || secretRef == "") {
		return nil, fmt.Errorf("%w: serverIntegrations.googlePlay requires packageName and credentialSecretRef when enabled", ErrInvalid)
	}
	rtdn, err := normalizeRTDN(payload["rtdn"], previous["rtdn"])
	if err != nil {
		return nil, err
	}
	result := map[string]any{"enabled": enabled, "rtdn": rtdn, "health": defaultHealth(previous["health"])}
	if packageName != "" {
		result["packageName"] = packageName
	}
	if secretRef != "" {
		result["credentialSecretRef"] = secretRef
	}
	return result, nil
}

func normalizeRTDN(input, existing any) (map[string]any, error) {
	payload, _ := input.(map[string]any)
	previous, _ := existing.(map[string]any)

	merged := make(map[string]any, len(previous)+len(payload))
	for key, value := range previous {
		merged[key] = value
	}
	for key, value := range payload {
		merged[key] = value
	}

	enabled, _ := merged["enabled"].(bool)
	topic := FieldString(merged, "topic")
	subscription := FieldString(merged, "subscription")
	pushAudience := FieldString(merged, "pushAudience")
	serviceAccountEmail := strings.ToLower(FieldString(merged, "serviceAccountEmail"))
	if enabled && (topic == "" || subscription == "" || pushAudience == "" || serviceAccountEmail == "") {
		return nil, fmt.Errorf("%w: serverIntegrations.googlePlay.rtdn requires topic, subscription, pushAudience, and serviceAccountEmail when enabled", ErrInvalid)
	}
	result := map[string]any{"enabled": enabled}
	if topic != "" {
		result["topic"] = topic
	}
	if subscription != "" {
		result["subscription"] = subscription
	}
	if pushAudience != "" {
		result["pushAudience"] = pushAudience
	}
	if serviceAccountEmail != "" {
		result["serviceAccountEmail"] = serviceAccountEmail
	}
	return result, nil
}

// defaultHealth 复刻 Node `defaultHealth`：只认合法的状态取值，其余兜成 unconfigured。
func defaultHealth(existing any) map[string]any {
	previous, _ := existing.(map[string]any)
	status := FieldString(previous, "status")
	if !containsString(healthStatusValues, status) {
		status = "unconfigured"
	}
	health := map[string]any{"status": status}
	if checkedAt, exists := previous["checkedAt"]; exists && checkedAt != nil {
		health["checkedAt"] = checkedAt
	}
	if code := FieldString(previous, "code"); code != "" {
		health["code"] = code
	}
	if message := FieldString(previous, "message"); message != "" {
		health["message"] = truncateRunes(message, 500)
	}
	return health
}

// optionalSecretRef 校验密钥引用必须是 APP_* 形式的环境变量名。
// 直接落库一个明文密钥是这类字段最常见的误用，所以格式不对就拒绝。
func optionalSecretRef(value, path string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !secretRefPattern.MatchString(value) {
		return "", fmt.Errorf("%w: %s must be an APP_* environment secret reference", ErrInvalid, path)
	}
	return value, nil
}

func stringList(value any) []string {
	var rows []any
	switch typed := value.(type) {
	case []any:
		rows = typed
	case []string:
		rows = make([]any, len(typed))
		for index, item := range typed {
			rows[index] = item
		}
	default:
		return []string{}
	}
	result := make([]string, 0, len(rows))
	seen := map[string]struct{}{}
	for _, row := range rows {
		text := trimmedString(row)
		if text == "" {
			continue
		}
		if _, exists := seen[text]; exists {
			continue
		}
		seen[text] = struct{}{}
		result = append(result, text)
	}
	return result
}

func trimmedString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

// boolInput 复刻 Node 的 `input.x !== undefined ? input.x === true : existing.x === true`：
// 只要键存在就以本次提交为准，且**非布尔值一律算 false**。
func boolInput(payload map[string]any, key string, previous map[string]any) bool {
	if value, exists := payload[key]; exists {
		flag, _ := value.(bool)
		return flag
	}
	flag, _ := previous[key].(bool)
	return flag
}

func stringInput(payload map[string]any, key string, previous map[string]any) string {
	if value, exists := payload[key]; exists {
		return trimmedString(value)
	}
	return FieldString(previous, key)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
