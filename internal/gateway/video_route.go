package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

const (
	videoCreatePath         = "/api/chat/video"
	videoStatusesPath       = "/api/chat/videos/status"
	videoDetailPrefix       = "/api/chat/video/"
	maxVideoStatusBatchSize = 20
)

// videoRoute 表示已审核的公开视频路由种类；仅这三条精确路由可以由 Go 接管。
type videoRoute uint8

const (
	videoRouteNone videoRoute = iota
	videoRouteCreate
	videoRouteStatuses
	videoRouteDetail
)

// matchVideoLocalRoute 拒绝 query、编码路径和相邻视频路由，避免扩大现网切流范围。
func matchVideoLocalRoute(request *http.Request) videoRoute {
	if request == nil || request.URL == nil || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Path != request.URL.EscapedPath() {
		return videoRouteNone
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == videoCreatePath:
		return videoRouteCreate
	case request.Method == http.MethodPost && request.URL.Path == videoStatusesPath:
		return videoRouteStatuses
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, videoDetailPrefix):
		identifier := strings.TrimPrefix(request.URL.Path, videoDetailPrefix)
		parsed, err := uuid.Parse(identifier)
		if err == nil && parsed.String() == identifier {
			return videoRouteDetail
		}
	}
	return videoRouteNone
}

// routeKey 返回受控开关使用的稳定路径字面量；详情中的 ID 不参与开关文件。
func (route videoRoute) routeKey() exactRouteKey {
	switch route {
	case videoRouteCreate:
		return exactRouteKey{method: http.MethodPost, path: videoCreatePath}
	case videoRouteStatuses:
		return exactRouteKey{method: http.MethodPost, path: videoStatusesPath}
	case videoRouteDetail:
		return exactRouteKey{method: http.MethodGet, path: videoDetailPrefix + ":id"}
	default:
		return exactRouteKey{}
	}
}

// isVideoLocalCandidate 只允许不改变用户语义的模板视频请求进入 Go。
// 尚未在 execution.v2 技术合同中实现的音频、客户端参数覆盖等请求必须继续代理 Node。
func isVideoLocalCandidate(route videoRoute, request *http.Request) bool {
	switch route {
	case videoRouteCreate:
		return isVideoCreateCandidate(request)
	case videoRouteStatuses:
		return isVideoStatusesCandidate(request)
	case videoRouteDetail:
		return true
	default:
		return false
	}
}

func isVideoCreateCandidate(request *http.Request) bool {
	fields, ok := readTopLevelJSONObjectPreservingBody(request)
	if !ok || !isRequiredTrimmedJSONString(fields, "templateId") {
		return false
	}
	for field, raw := range fields {
		switch field {
		case "templateId":
		case "imageUrl":
			if !isHTTPSImageURL(raw) {
				return false
			}
		case "prompt":
			if !isRequiredJSONString(fields, "prompt") {
				return false
			}
		case "negativePrompt", "aspectRatio":
			// 当前模板配方未声明客户端覆盖能力；非空覆盖继续由 Node 保持既有语义。
			if !isEmptyJSONString(raw) {
				return false
			}
		case "durationSeconds":
			if !isAllowedVideoDuration(raw) {
				return false
			}
		case "enableAudio":
			// execution.v2 当前未支持音频参数，必须回退 Node 而非静默丢弃用户选择。
			if !isJSONBoolValue(raw, false) {
				return false
			}
		default:
			return false
		}
	}
	_, hasImage := fields["imageUrl"]
	_, hasPrompt := fields["prompt"]
	return hasImage != hasPrompt
}

// isVideoStatusesCandidate 仅当全部状态 ID 都是规范 UUID 且批量大小受限时本地读取。
func isVideoStatusesCandidate(request *http.Request) bool {
	fields, ok := readTopLevelJSONObjectPreservingBody(request)
	if !ok || len(fields) != 1 {
		return false
	}
	rawIDs, exists := fields["taskIds"]
	if !exists {
		return false
	}
	var identifiers []string
	if json.Unmarshal(rawIDs, &identifiers) != nil || len(identifiers) == 0 || len(identifiers) > maxVideoStatusBatchSize {
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

func isRequiredTrimmedJSONString(fields map[string]json.RawMessage, name string) bool {
	raw, exists := fields[name]
	if !exists {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" && strings.TrimSpace(value) == value
}

func isHTTPSImageURL(raw json.RawMessage) bool {
	var value string
	if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) != value {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func isEmptyJSONString(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value == ""
}

func isAllowedVideoDuration(raw json.RawMessage) bool {
	var duration int32
	return json.Unmarshal(raw, &duration) == nil && (duration == 5 || duration == 10 || duration == 15)
}
