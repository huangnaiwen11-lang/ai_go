package main

import "testing"

// 钱包读取必须是独立第二道开关；仅启用 Go 会话不能改变现有 Node 钱包读取路径。
func TestLocalWalletView开关仅接受显式True(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"1", false}, {"true", true}, {"TRUE", true},
	} {
		t.Setenv("GATEWAY_LOCAL_WALLET_VIEW_ENABLED", testCase.value)
		if got := localWalletViewEnabled(); got != testCase.want {
			t.Fatalf("localWalletViewEnabled(%q)=%t，期望 %t", testCase.value, got, testCase.want)
		}
	}
}

// 开关关闭时不得读取配置或连接 Mongo，避免本地新增读取域影响 Node 钱包。
func TestNewOptionalWalletView关闭时不读取配置(t *testing.T) {
	handler, cleanup, err := newOptionalWalletViewHandler(false, "/private/tmp/missing-gateway-config.yaml", nil)
	if err != nil || handler != nil || cleanup != nil {
		t.Fatalf("关闭时 handlerPresent=%t cleanupPresent=%t error=%v", handler != nil, cleanup != nil, err)
	}
}
