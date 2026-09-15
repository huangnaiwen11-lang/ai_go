package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// exactRouteKey 标识唯一可被本地处理的路由形态。查询参数故意不参与匹配：路由契约
// 由精确 HTTP method 与规范化 path 定义。
type exactRouteKey struct {
	method string
	path   string
}

// RouteSwitch 决定已确认的精确路由是否允许在本地执行。开关不可用或无效时必须继续
// 由 Node 处理请求。
type RouteSwitch interface {
	Enabled(exactRouteKey) bool
}

type disabledRouteSwitch struct{}

func (disabledRouteSwitch) Enabled(exactRouteKey) bool { return false }

type fileRouteSwitch struct {
	path string
}

// NewFileRouteSwitch 返回 fail-closed 的文件精确路由开关。每次查询都读取文件，
// 因此原子替换后的无效文件会立即回退 Node，无需重启进程。
func NewFileRouteSwitch(path string) RouteSwitch {
	return fileRouteSwitch{path: strings.TrimSpace(path)}
}

func (switcher fileRouteSwitch) Enabled(route exactRouteKey) bool {
	if switcher.path == "" || !isConfirmedExactRoute(route) {
		return false
	}

	contents, err := os.ReadFile(switcher.path)
	if err != nil {
		return false
	}
	routes, ok := decodeRouteSwitchDocument(contents)
	if !ok {
		return false
	}

	return routes[route]
}

func decodeRouteSwitchDocument(contents []byte) (map[exactRouteKey]bool, bool) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var document map[string]json.RawMessage
	if err := decoder.Decode(&document); err != nil {
		return nil, false
	}
	if err := ensureEOF(decoder); err != nil || len(document) != 1 {
		return nil, false
	}
	rawRoutes, ok := document["routes"]
	if !ok {
		return nil, false
	}

	var configuredRoutes map[string]bool
	if err := json.Unmarshal(rawRoutes, &configuredRoutes); err != nil || configuredRoutes == nil {
		return nil, false
	}

	routes := make(map[exactRouteKey]bool, len(configuredRoutes))
	for rawKey, enabled := range configuredRoutes {
		route, ok := parseConfirmedExactRouteKey(rawKey)
		if !ok {
			return nil, false
		}
		routes[route] = enabled
	}
	return routes, true
}

func ensureEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	return err
}

func parseConfirmedExactRouteKey(raw string) (exactRouteKey, bool) {
	method, path, found := strings.Cut(raw, " ")
	if !found || method == "" || path == "" || strings.Contains(path, " ") {
		return exactRouteKey{}, false
	}
	route := exactRouteKey{method: method, path: path}
	return route, isConfirmedExactRoute(route)
}
