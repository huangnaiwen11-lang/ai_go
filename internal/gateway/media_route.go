package gateway

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const (
	mediaImagesPath       = "/api/media/images"
	mediaImagesPathPrefix = mediaImagesPath + "/"
)

// mediaRoute 是本地素材域允许的最小公开路由集合，禁止使用前缀开关。
type mediaRoute uint8

const (
	mediaRouteNone mediaRoute = iota
	mediaRouteUpload
	mediaRouteRead
)

func matchLocalMediaRoute(request *http.Request) mediaRoute {
	if request == nil || request.URL == nil || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Path != request.URL.EscapedPath() {
		return mediaRouteNone
	}
	if request.Method == http.MethodPost && request.URL.Path == mediaImagesPath {
		return mediaRouteUpload
	}
	if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, mediaImagesPathPrefix) {
		identifier := strings.TrimPrefix(request.URL.Path, mediaImagesPathPrefix)
		parsed, err := uuid.Parse(identifier)
		if err == nil && parsed.String() == identifier {
			return mediaRouteRead
		}
	}
	return mediaRouteNone
}

func (route mediaRoute) routeKey() exactRouteKey {
	switch route {
	case mediaRouteUpload:
		return exactRouteKey{method: http.MethodPost, path: mediaImagesPath}
	case mediaRouteRead:
		return exactRouteKey{method: http.MethodGet, path: mediaImagesPathPrefix + ":id"}
	default:
		return exactRouteKey{}
	}
}
