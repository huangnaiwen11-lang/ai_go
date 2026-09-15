package main

import "testing"

func TestLocalWorksView开关仅接受显式True(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"1", false}, {"true", true}, {"TRUE", true},
	} {
		t.Setenv("GATEWAY_LOCAL_WORKS_VIEW_ENABLED", testCase.value)
		if got := localWorksViewEnabled(); got != testCase.want {
			t.Fatalf("localWorksViewEnabled(%q)=%t，期望 %t", testCase.value, got, testCase.want)
		}
	}
}

func TestNewOptionalWorksView关闭时不读取配置(t *testing.T) {
	handler, cleanup, err := newOptionalWorksViewHandler(false, "/private/tmp/missing-gateway-config.yaml", nil)
	if err != nil || handler != nil || cleanup != nil {
		t.Fatalf("关闭时 handlerPresent=%t cleanupPresent=%t error=%v", handler != nil, cleanup != nil, err)
	}
}
