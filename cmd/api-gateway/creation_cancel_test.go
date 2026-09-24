package main

import "testing"

func TestCreationCancel开关仅接受显式True(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"1", false}, {"true", true}, {"TRUE", true},
	} {
		t.Setenv("GATEWAY_CREATION_CANCEL_ENABLED", testCase.value)
		if got := creationCancelEnabled(); got != testCase.want {
			t.Fatalf("creationCancelEnabled(%q) = %t, want %t", testCase.value, got, testCase.want)
		}
	}
}

func TestNewOptionalCreationCancelHandler关闭时不读取配置(t *testing.T) {
	handler, cleanup, err := newOptionalCreationCancelHandler(false, "/private/tmp/missing-gateway-config.yaml", nil)
	if err != nil || handler != nil || cleanup != nil {
		t.Fatalf("关闭时 handlerPresent=%t cleanupPresent=%t error=%v，期望均为空", handler != nil, cleanup != nil, err)
	}
}
