package main

import (
	"os"
	"strings"
	"testing"
)

// 独立 Worker 只能负责受控轮询，不能悄悄变成第二个 HTTP 网关。
// 这个静态契约防止未来维护时把路由或监听器接入后台命令。
func TestWorker入口只装配轮询并处理退出信号(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("ReadFile(main.go) error = %v", err)
	}
	source := string(raw)
	for _, forbidden := range []string{"internal/server", "http.ListenAndServe", "http.Server"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("Worker 入口不得包含 %q", forbidden)
		}
	}
	if !strings.Contains(source, "signal.NotifyContext") {
		t.Fatal("Worker 入口必须使用 signal.NotifyContext 安全退出")
	}
}
