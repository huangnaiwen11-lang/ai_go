package adminapps

import "strings"

// managedLegalFields 与 Node `app.service.js` 的 MANAGED_LEGAL_FIELDS 一致。
//
// 注意与 legalURLFields 的区别：legalURLFields 是 nativeConfig.legal 里的**全部** URL 字段
// （含 h5Url），这里是**托管发布**能接管的 5 个槽位（不含 h5Url）。
// 两张表不能合并 —— 合并会让 h5Url 被误判成可托管槽位。
var managedLegalFields = []string{"privacyUrl", "termsUrl", "supportUrl", "deletionUrl", "aboutUrl"}

// Merge 把补丁浅合并进既有文档。
//
// 只用于「派生标识 / 归属断言」这类要看 App 全貌的场景，
// 不作为写库的 $set 内容 —— 写库只写补丁里显式出现的字段。
func Merge(existing map[string]any, patch Document) Document {
	merged := make(Document, len(existing)+len(patch))
	for key, value := range existing {
		merged[key] = value
	}
	for key, value := range patch {
		merged[key] = value
	}
	return merged
}

// MergeDeep 复刻 Node `mergePlainObjects`：递归合并普通对象，数组整体替换。
//
// 与 Node 的差异只有一处、且不可达：Node 会跳过值为 `undefined` 的键，
// 而 Go 的 map 里不存在 `undefined` —— 没提交的键根本不在 map 中。
// 显式 `null` 在两边都表示「覆盖成 null」。
func MergeDeep(base, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range override {
		existing, exists := merged[key]
		existingObject, existingIsObject := existing.(map[string]any)
		valueObject, valueIsObject := value.(map[string]any)
		if exists && existingIsObject && valueIsObject {
			merged[key] = MergeDeep(existingObject, valueObject)
			continue
		}
		merged[key] = value
	}
	return merged
}

// WithoutServerOwnedStateRules 去掉 `stateRules`。
//
// stateRules 由审核模式（`PATCH /admin/apps/:id/review-mode`）在服务端生成，
// 客户端提交的这份一律丢弃 —— 让客户端能写它等于让前端覆盖审核策略。
func WithoutServerOwnedStateRules(nativeConfig map[string]any) map[string]any {
	if nativeConfig == nil {
		return nil
	}
	editable := make(map[string]any, len(nativeConfig))
	for key, value := range nativeConfig {
		if key == "stateRules" {
			continue
		}
		editable[key] = value
	}
	return editable
}

// PreserveManagedLegalURLs 复刻 Node `preserveManagedLegalUrls`。
//
// 托管发布的协议页 URL 归服务端管理：只要槽位未禁用、且已有已发布版本，
// 就用槽位里的 publicUrl 覆盖客户端提交的值。否则前端一次「保存」
// 就能把服务端发布的协议页地址改回客户端里的旧值。
func PreserveManagedLegalURLs(existing, incoming, legalPages map[string]any) map[string]any {
	if incoming == nil {
		return nil
	}
	next := MergeDeep(nil, incoming)
	legal, _ := next["legal"].(map[string]any)
	preserved := false
	for _, field := range managedLegalFields {
		slot, ok := legalPages[strings.TrimSuffix(field, "Url")].(map[string]any)
		if !ok {
			continue
		}
		if status, _ := slot["status"].(string); status == "disabled" {
			continue
		}
		publicURL := strings.TrimSpace(FieldString(slot, "publicUrl"))
		versionID := strings.TrimSpace(FieldString(slot, "currentVersionId"))
		if publicURL == "" || versionID == "" {
			continue
		}
		if legal == nil {
			legal = map[string]any{}
		}
		legal[field] = publicURL
		preserved = true
	}
	if !preserved {
		return next
	}
	next["legal"] = legal
	return next
}

// FieldString 读取指定键的字符串值并去掉首尾空白；非字符串一律返回空串。
func FieldString(document map[string]any, key string) string {
	value, _ := document[key].(string)
	return strings.TrimSpace(value)
}

// FieldObject 读取指定键的嵌套对象。
func FieldObject(document map[string]any, key string) map[string]any {
	switch object := document[key].(type) {
	case map[string]any:
		return object
	case Document:
		return map[string]any(object)
	default:
		return nil
	}
}

// FieldArray 读取指定键的数组。
func FieldArray(document map[string]any, key string) []any {
	switch rows := document[key].(type) {
	case []any:
		return rows
	case []string:
		result := make([]any, len(rows))
		for index := range rows {
			result[index] = rows[index]
		}
		return result
	default:
		return nil
	}
}

// PathString 沿多段路径读取字符串值。
//
// 与 `nestedPathString` 的差别：那个从 Document 出发，这个从任意 map 出发，
// 用于「先取子对象再往里看」的场景（例如 nativeBuild.android）。
func PathString(document map[string]any, path ...string) string {
	var current any = document
	for _, key := range path {
		var object map[string]any
		switch typed := current.(type) {
		case map[string]any:
			object = typed
		case Document:
			object = map[string]any(typed)
		default:
			return ""
		}
		next, exists := object[key]
		if !exists {
			return ""
		}
		current = next
	}
	value, _ := current.(string)
	return strings.TrimSpace(value)
}
