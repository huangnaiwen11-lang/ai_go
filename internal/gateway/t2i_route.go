package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const (
	t2iCreatePath                 = "/api/chat/image/async"
	t2iStatusesPath               = "/api/images/statuses"
	t2iDetailPrefix               = "/api/images/"
	maxT2IClassificationBytes int = 1 << 20
)

// t2iRoute 表示已审核的公开 T2I 路由种类，不允许使用前缀匹配替代。
type t2iRoute uint8

const (
	t2iRouteNone t2iRoute = iota
	t2iRouteCreate
	t2iRouteStatuses
	t2iRouteDetail
)

// matchT2ILocalRoute 只接受无 query、无路径编码且路径完全规范的公开 T2I 路由。
func matchT2ILocalRoute(request *http.Request) t2iRoute {
	if request == nil || request.URL == nil || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Path != request.URL.EscapedPath() {
		return t2iRouteNone
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == t2iCreatePath:
		return t2iRouteCreate
	case request.Method == http.MethodPost && request.URL.Path == t2iStatusesPath:
		return t2iRouteStatuses
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, t2iDetailPrefix):
		identifier := strings.TrimPrefix(request.URL.Path, t2iDetailPrefix)
		parsed, err := uuid.Parse(identifier)
		if err == nil && parsed.String() == identifier {
			return t2iRouteDetail
		}
	}
	return t2iRouteNone
}

// routeKey 返回路由开关使用的稳定字面量；详情 ID 不属于开关文件内容。
func (route t2iRoute) routeKey() exactRouteKey {
	switch route {
	case t2iRouteCreate:
		return exactRouteKey{method: http.MethodPost, path: t2iCreatePath}
	case t2iRouteStatuses:
		return exactRouteKey{method: http.MethodPost, path: t2iStatusesPath}
	case t2iRouteDetail:
		return exactRouteKey{method: http.MethodGet, path: t2iDetailPrefix + ":id"}
	default:
		return exactRouteKey{}
	}
}

// isT2ILocalCandidate 在不读取登录态或业务数据的前提下判断请求能否交给 Go。
// 分类失败必须回退 Node，避免静默丢弃 I2I、聊天或模板语义。
func isT2ILocalCandidate(route t2iRoute, request *http.Request) bool {
	switch route {
	case t2iRouteCreate:
		return isT2ICreateCandidate(request) || isImageEditCreateCandidate(request)
	case t2iRouteStatuses:
		return isT2IStatusesCandidate(request)
	case t2iRouteDetail:
		return true
	default:
		return false
	}
}

// isImageEditCreateCandidate 只接管已选模板的一张用户图片编辑请求。
// 其他图生图、历史 operation 或技术字段必须继续由 Node 保持原语义。
func isImageEditCreateCandidate(request *http.Request) bool {
	fields, ok := readTopLevelJSONObjectPreservingBody(request)
	if !ok || !isRequiredJSONString(fields, "templateId") || !isSingleHTTPSImageURL(fields["inputImages"]) {
		return false
	}
	templateID, _ := jsonString(fields["templateId"])
	for field, raw := range fields {
		switch field {
		case "templateId", "inputImages":
		case "prompt", "negativePrompt":
			if !isOptionalJSONString(fields, field) {
				return false
			}
		case "operation":
			if !isJSONStringValue(raw, "generate") {
				return false
			}
		case "optimizePrompt":
			if !isJSONBoolValue(raw, false) {
				return false
			}
		case "aspectRatio":
			value, valid := jsonString(raw)
			if !valid || !isSupportedAspectRatio(value) {
				return false
			}
		case "presetTags":
			if !isTemplatePresetTag(raw, templateID) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

var aspectRatioPattern = regexp.MustCompile(`^\d+:\d+$`)

// isSupportedAspectRatio 与现网 HTTP 合同保持同一格式边界；具体如何写入技术快照只由服务端决定。
func isSupportedAspectRatio(value string) bool {
	return aspectRatioPattern.MatchString(value)
}

func jsonString(raw json.RawMessage) (string, bool) {
	var value string
	return value, json.Unmarshal(raw, &value) == nil
}

// isTemplatePresetTag 只接受前端为展示和历史兼容携带的同名模板标签，不能借标签切换模板或技术配方。
func isTemplatePresetTag(raw json.RawMessage, templateID string) bool {
	var tags []string
	return json.Unmarshal(raw, &tags) == nil && len(tags) == 1 && tags[0] == templateID
}

func isSingleHTTPSImageURL(raw json.RawMessage) bool {
	var values []string
	if json.Unmarshal(raw, &values) != nil || len(values) != 1 || strings.TrimSpace(values[0]) != values[0] || !strings.HasPrefix(values[0], "https://") {
		return false
	}
	return true
}

// isT2ICreateCandidate 只接受自由文生图的最小产品输入。
func isT2ICreateCandidate(request *http.Request) bool {
	fields, ok := readTopLevelJSONObjectPreservingBody(request)
	if !ok || containsForbiddenT2IField(fields) {
		return false
	}
	if _, exists := fields["inputImages"]; exists {
		return false
	}
	if !isRequiredJSONString(fields, "prompt") || !isOptionalJSONString(fields, "negativePrompt") || !isOptionalJSONString(fields, "aspectRatio") {
		return false
	}
	if operation, exists := fields["operation"]; exists && !isJSONStringValue(operation, "generate") {
		return false
	}
	return true
}

// isT2IStatusesCandidate 仅在整批图片 ID 都是规范 Go UUID 时允许本地读取。
func isT2IStatusesCandidate(request *http.Request) bool {
	fields, ok := readTopLevelJSONObjectPreservingBody(request)
	if !ok || len(fields) != 1 {
		return false
	}
	rawIDs, exists := fields["imageIds"]
	if !exists {
		return false
	}
	var identifiers []string
	if json.Unmarshal(rawIDs, &identifiers) != nil || len(identifiers) == 0 || len(identifiers) > 20 {
		return false
	}
	seen := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		parsed, err := uuid.Parse(identifier)
		if err != nil || parsed.String() != identifier {
			return false
		}
		if _, duplicate := seen[identifier]; duplicate {
			return false
		}
		seen[identifier] = struct{}{}
	}
	return true
}

func containsForbiddenT2IField(fields map[string]json.RawMessage) bool {
	for field, raw := range fields {
		switch field {
		case "prompt", "negativePrompt", "aspectRatio", "operation":
		case "presetTags":
			if !isEmptyJSONArray(raw) {
				return true
			}
		case "optimizePrompt":
			if !isJSONBoolValue(raw, false) {
				return true
			}
		case "inputImages", "provider", "templateId", "templateTitle", "messageId", "agentId":
			return true
		default:
			return true
		}
	}
	return false
}

func isEmptyJSONArray(raw json.RawMessage) bool {
	var values []json.RawMessage
	return json.Unmarshal(raw, &values) == nil && values != nil && len(values) == 0
}

func isJSONBoolValue(raw json.RawMessage, expected bool) bool {
	var value bool
	return json.Unmarshal(raw, &value) == nil && value == expected
}

func isRequiredJSONString(fields map[string]json.RawMessage, name string) bool {
	raw, exists := fields[name]
	if !exists {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
}

func isOptionalJSONString(fields map[string]json.RawMessage, name string) bool {
	raw, exists := fields[name]
	if !exists {
		return true
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func isJSONStringValue(raw json.RawMessage, expected string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value == expected
}

func isJSONNumber(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || ensureEOF(decoder) != nil {
		return false
	}
	_, ok := value.(json.Number)
	return ok
}

// readTopLevelJSONObjectPreservingBody 拒绝重复根字段，并在所有路径恢复原始 Body。
func readTopLevelJSONObjectPreservingBody(request *http.Request) (map[string]json.RawMessage, bool) {
	if request == nil || request.Body == nil {
		return nil, false
	}
	prefix, err := io.ReadAll(io.LimitReader(request.Body, int64(maxT2IClassificationBytes+1)))
	if err != nil {
		return nil, false
	}
	request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), request.Body))
	if len(prefix) > maxT2IClassificationBytes {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(prefix))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, keyOK := keyToken.(string)
		if err != nil || !keyOK {
			return nil, false
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, false
		}
		fields[key] = raw
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || ensureEOF(decoder) != nil {
		return nil, false
	}
	return fields, true
}
