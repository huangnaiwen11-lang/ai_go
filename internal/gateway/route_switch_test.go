package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestFileRouteSwitchFailsClosedAndReloads(t *testing.T) {
	t.Parallel()

	switchPath := filepath.Join(t.TempDir(), "routes.json")
	writeRouteSwitchFile(t, switchPath, `{"routes":{"GET /api/growth/ping":true}}`)

	routeSwitch := NewFileRouteSwitch(switchPath)
	growthPingKey := exactRouteKey{method: http.MethodGet, path: growthPingPath}
	if !routeSwitch.Enabled(growthPingKey) {
		t.Fatal("known enabled route should be enabled")
	}

	atomicallyReplaceRouteSwitchFile(t, switchPath, `{"routes":{"GET /api/growth/ping":false}}`)
	if routeSwitch.Enabled(growthPingKey) {
		t.Fatal("atomic replacement should disable the known route without a process restart")
	}

	// 只要出现一个未知键，整个文件都必须失效；否则操作者可能误将已审查和未审查的
	// 路由混合放行。
	atomicallyReplaceRouteSwitchFile(t, switchPath, `{"routes":{"GET /api/growth/ping":true,"GET /api/unregistered":true}}`)
	if routeSwitch.Enabled(growthPingKey) {
		t.Fatal("document containing an unknown route should fail closed for the known route")
	}
	if routeSwitch.Enabled(exactRouteKey{method: http.MethodGet, path: "/api/unregistered"}) {
		t.Fatal("unknown route key should never be enabled")
	}

	if err := os.Remove(switchPath); err != nil {
		t.Fatalf("remove route switch file: %v", err)
	}
	if routeSwitch.Enabled(growthPingKey) {
		t.Fatal("missing route switch file should fail closed")
	}
}

func TestFileRouteSwitchRejectsMalformedDocument(t *testing.T) {
	t.Parallel()

	switchPath := filepath.Join(t.TempDir(), "routes.json")
	writeRouteSwitchFile(t, switchPath, `{`)

	routeSwitch := NewFileRouteSwitch(switchPath)
	if routeSwitch.Enabled(exactRouteKey{method: http.MethodGet, path: growthPingPath}) {
		t.Fatal("malformed route switch document should fail closed")
	}
}

func TestFileRouteSwitchRejectsIncompleteOrUnknownDocuments(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		contents string
	}{
		{name: "missing routes", contents: `{}`},
		{name: "unknown top level key", contents: `{"routes":{"GET /api/growth/ping":true},"unreviewed":true}`},
		{name: "null routes", contents: `{"routes":null}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			switchPath := filepath.Join(t.TempDir(), "routes.json")
			writeRouteSwitchFile(t, switchPath, testCase.contents)

			if NewFileRouteSwitch(switchPath).Enabled(exactRouteKey{method: http.MethodGet, path: growthPingPath}) {
				t.Fatal("incomplete route switch document should fail closed")
			}
		})
	}
}

func TestFileRouteSwitchRejectsDirectory(t *testing.T) {
	t.Parallel()

	if NewFileRouteSwitch(t.TempDir()).Enabled(exactRouteKey{method: http.MethodGet, path: growthPingPath}) {
		t.Fatal("directory path should fail closed")
	}
}

func TestFileRouteSwitchRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	routeSwitch := NewFileRouteSwitch("")
	if routeSwitch.Enabled(exactRouteKey{method: http.MethodGet, path: growthPingPath}) {
		t.Fatal("empty route switch path should fail closed")
	}
}

func writeRouteSwitchFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write route switch file: %v", err)
	}
}

func atomicallyReplaceRouteSwitchFile(t *testing.T, path, contents string) {
	t.Helper()
	nextPath := path + ".next"
	writeRouteSwitchFile(t, nextPath, contents)
	if err := os.Rename(nextPath, path); err != nil {
		t.Fatalf("atomically replace route switch file: %v", err)
	}
}
