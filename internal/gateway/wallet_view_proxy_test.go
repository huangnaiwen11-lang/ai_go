package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 钱包读取只有在 handler 已装配且精确开关开启时才能接管；未开启及相近路径必须继续代理 Node。
func TestWalletView只接管启用的精确路径(t *testing.T) {
	var nodeHits, localHits int
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		nodeHits++
		_, _ = io.WriteString(writer, "node")
	}))
	defer node.Close()
	local := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		localHits++
		_, _ = io.WriteString(writer, "go-wallet")
	})

	disabledGateway := New(Config{DefaultUpstream: mustURL(t, node.URL), WalletViewHandler: local})
	disabledRecorder := httptest.NewRecorder()
	disabledGateway.Handler().ServeHTTP(disabledRecorder, httptest.NewRequest(http.MethodGet, "/api/wallet/summary", nil))
	if disabledRecorder.Body.String() != "node" || localHits != 0 || nodeHits != 1 {
		t.Fatalf("未启用 route-switch 时 response=%q local=%d node=%d", disabledRecorder.Body.String(), localHits, nodeHits)
	}

	enabledGateway := New(Config{
		DefaultUpstream:   mustURL(t, node.URL),
		RouteSwitch:       enabledRouteSwitch{enabled: exactRouteKey{method: http.MethodGet, path: "/api/wallet/ledger"}},
		WalletViewHandler: local,
	})
	ledgerRecorder := httptest.NewRecorder()
	enabledGateway.Handler().ServeHTTP(ledgerRecorder, httptest.NewRequest(http.MethodGet, "/api/wallet/ledger?limit=20&skip=0", nil))
	if ledgerRecorder.Body.String() != "go-wallet" || localHits != 1 || nodeHits != 1 {
		t.Fatalf("启用 ledger 后 response=%q local=%d node=%d", ledgerRecorder.Body.String(), localHits, nodeHits)
	}

	for _, target := range []string{"/api/wallet/summary", "/api/v1/wallet/ledger", "/api/wallet%2Fledger"} {
		recorder := httptest.NewRecorder()
		enabledGateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Body.String() != "node" {
			t.Fatalf("target=%q body=%q，期望继续代理 Node", target, recorder.Body.String())
		}
	}
	if localHits != 1 || nodeHits != 4 {
		t.Fatalf("最终 local=%d node=%d", localHits, nodeHits)
	}
}
