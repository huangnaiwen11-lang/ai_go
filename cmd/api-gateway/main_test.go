package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/gateway"
)

func TestRunGateway监听失败仍清理回调依赖(t *testing.T) {
	upstream, err := url.Parse("http://node.invalid")
	if err != nil {
		t.Fatal(err)
	}
	cleanupCalls := 0
	listenErr := errors.New("listen failed")
	err = runGateway(
		":8080",
		newGateway(upstream, gateway.NewFileRouteSwitch(""), 0, nil, nil, nil),
		func() { cleanupCalls++ },
		func(address string, handler http.Handler) error {
			if address != ":8080" || handler == nil {
				t.Fatal("监听参数不正确")
			}
			return listenErr
		},
	)
	if !errors.Is(err, listenErr) {
		t.Fatalf("runGateway() error = %v, want listener error", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
}

func TestRunGateway监听正常返回仍清理回调依赖(t *testing.T) {
	upstream, err := url.Parse("http://node.invalid")
	if err != nil {
		t.Fatal(err)
	}
	cleanupCalls := 0
	err = runGateway(
		":8080",
		newGateway(upstream, gateway.NewFileRouteSwitch(""), 0, nil, nil, nil),
		func() { cleanupCalls++ },
		func(string, http.Handler) error { return nil },
	)
	if err != nil {
		t.Fatalf("runGateway() error = %v", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
}

func TestGenerationCallback开关默认关闭且显式开启(t *testing.T) {
	t.Setenv("GATEWAY_GENERATION_CALLBACK_ENABLED", "")
	if generationCallbackEnabled() {
		t.Fatal("未配置开关时不应装配本地生成回调")
	}
	t.Setenv("GATEWAY_GENERATION_CALLBACK_ENABLED", "true")
	if !generationCallbackEnabled() {
		t.Fatal("显式 true 时应装配本地生成回调")
	}
}

// 支付回调有独立开关，避免仅因为生成回调已启用而意外切走 Node 的收款通知。
func TestLocalPaymentCallback开关仅接受显式True(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"1", false}, {"true", true}, {"TRUE", true},
	} {
		t.Setenv("GATEWAY_LOCAL_PAYMENT_CALLBACK_ENABLED", testCase.value)
		if got := localPaymentCallbackEnabled(); got != testCase.want {
			t.Fatalf("localPaymentCallbackEnabled(%q) = %t，期望 %t", testCase.value, got, testCase.want)
		}
	}
}

// 本地支付入口必须与会话和受控 IAP 验证器分开开关，避免意外用测试验证器截获 Node 流量。
func TestLocalPaymentEntry开关仅接受显式True(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"1", false}, {"true", true}, {"TRUE", true},
	} {
		t.Setenv("GATEWAY_LOCAL_PAYMENT_ENTRY_ENABLED", testCase.value)
		if got := localPaymentEntryEnabled(); got != testCase.want {
			t.Fatalf("localPaymentEntryEnabled(%q) = %t，期望 %t", testCase.value, got, testCase.want)
		}
		t.Setenv("GATEWAY_LOCAL_IAP_TEST_VERIFIER_ENABLED", testCase.value)
		if got := localIAPTestVerifierEnabled(); got != testCase.want {
			t.Fatalf("localIAPTestVerifierEnabled(%q) = %t，期望 %t", testCase.value, got, testCase.want)
		}
	}
}

// 入口总开关关闭时不能读取配置或连接 MongoDB；缺少本地配置时仅显式开启才报错。
func TestNewOptionalPaymentEntry默认不读取配置且显式开启才读取(t *testing.T) {
	handler, cleanup, err := newOptionalPaymentEntryHandler(false, "/private/tmp/missing-gateway-config.yaml", nil)
	if err != nil || handler != nil || cleanup != nil {
		t.Fatalf("关闭时 handlerPresent=%t cleanupPresent=%t error=%v，期望均为空", handler != nil, cleanup != nil, err)
	}
	handler, cleanup, err = newOptionalPaymentEntryHandler(true, "/private/tmp/missing-gateway-config.yaml", nil)
	if err == nil || handler != nil || cleanup != nil {
		t.Fatalf("开启且配置缺失时 handlerPresent=%t cleanupPresent=%t error=%v，期望初始化失败", handler != nil, cleanup != nil, err)
	}
}

// 支付回调必须使用独立密钥；缺失密钥时不能构造 Handler，更不能回退复用生成回调密钥。
func TestNewPaymentCallbackHandler只接受独立支付回调密钥(t *testing.T) {
	usecase := &payments.ConfirmedCallbackUsecase{}
	if handler, err := newPaymentCallbackHandler(&conf.Security{}, "/api/v1/internal/payment-confirmed", usecase); err == nil || handler != nil {
		t.Fatal("缺失支付回调密钥时不应构造 Handler")
	}
	handler, err := newPaymentCallbackHandler(&conf.Security{PaycoresCallbackHmacKey: "gateway-payment-callback-key-at-least-thirty-two-characters"}, "/api/v1/internal/payment-confirmed", usecase)
	if err != nil || handler == nil {
		t.Fatalf("独立支付回调密钥未构造 Handler：%v", err)
	}
}

// 关闭支付开关时绝不读取配置或连接本地 MongoDB，避免默认改变 Node 的收款路径。
func TestNewOptionalPaymentCallback默认不读取配置(t *testing.T) {
	handler, cleanup, err := newOptionalPaymentCallback(false, "/private/tmp/missing-gateway-config.yaml")
	if err != nil || handler != nil || cleanup != nil {
		t.Fatal("支付开关关闭时不应读取配置或装配本地 Handler")
	}
}

func TestGoSessionAuth开关默认关闭且显式开启(t *testing.T) {
	testCases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "未设置", raw: "", want: false},
		{name: "显式关闭", raw: "false", want: false},
		{name: "其他值", raw: "enabled", want: false},
		{name: "忽略大小写的真值", raw: " TrUe ", want: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("GATEWAY_GO_SESSION_AUTH_ENABLED", testCase.raw)
			if got := goSessionAuthEnabled(); got != testCase.want {
				t.Fatalf("goSessionAuthEnabled() = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestNewOptionalGenerationCallback默认不读取配置且显式开启才读取(t *testing.T) {
	handler, cleanup, err := newOptionalGenerationCallback(false, "/private/tmp/missing-gateway-config.yaml")
	if err != nil || handler != nil || cleanup != nil {
		t.Fatal("关闭开关时不应读取配置或装配本地回调")
	}
	handler, cleanup, err = newOptionalGenerationCallback(true, "/private/tmp/missing-gateway-config.yaml")
	if err == nil || handler != nil || cleanup != nil {
		t.Fatal("显式开启时必须读取受控 Gateway 配置")
	}
}

func TestNewOptionalSessionAuthenticator默认不读取配置且显式开启才读取(t *testing.T) {
	authenticator, cleanup, err := newOptionalSessionAuthenticator(false, "/private/tmp/missing-gateway-config.yaml")
	if err != nil || authenticator != nil || cleanup != nil {
		t.Fatal("关闭开关时不应读取配置或装配本地会话认证器")
	}
	authenticator, cleanup, err = newOptionalSessionAuthenticator(true, "/private/tmp/missing-gateway-config.yaml")
	if err == nil || authenticator != nil || cleanup != nil {
		t.Fatal("显式开启时必须读取受控 Gateway 配置")
	}
}

func TestParseBackendUpstream错误不泄露原始Userinfo(t *testing.T) {
	_, err := parseBackendUpstream("http://user:secret@%zz")
	if err == nil {
		t.Fatal("非法上游地址应被拒绝")
	}
	if strings.Contains(err.Error(), "secret") || err.Error() != "invalid backend upstream" {
		t.Fatalf("错误信息泄露上游输入: %q", err)
	}
}

func TestLoadGatewayBootstrap读取本地回调基地址(t *testing.T) {
	bootstrap, err := loadGatewayBootstrap("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("loadGatewayBootstrap() error = %v", err)
	}
	if got := bootstrap.GetIntegrations().GetGeneration().GetCallbackBaseUrl(); got != "http://127.0.0.1:18000" {
		t.Fatalf("callback base URL = %q, want local gateway origin", got)
	}
}

func TestLoadOptionalGatewayBootstrap仅在开启时读取有效本地配置(t *testing.T) {
	bootstrap, err := loadOptionalGatewayBootstrap(false, "/private/tmp/missing-gateway-config.yaml")
	if err != nil || bootstrap != nil {
		t.Fatal("关闭开关时不应读取 Gateway 配置")
	}
	bootstrap, err = loadOptionalGatewayBootstrap(true, "../../configs/config.yaml")
	if err != nil || bootstrap == nil || bootstrap.GetSecurity().GetGenerationCallbackHmacKey() == "" {
		t.Fatalf("开启开关时未读取有效本地 Gateway 配置: %v", err)
	}
}

func TestGatewayListenAddr默认匹配本地回调端口且允许显式覆盖(t *testing.T) {
	t.Setenv("GATEWAY_ADDR", "")
	if got := gatewayListenAddr(); got != ":18000" {
		t.Fatalf("gatewayListenAddr() = %q, want :18000", got)
	}
	t.Setenv("GATEWAY_ADDR", "127.0.0.1:19080")
	if got := gatewayListenAddr(); got != "127.0.0.1:19080" {
		t.Fatalf("gatewayListenAddr() = %q, want explicit address", got)
	}
}

// 公开 T2I 只能由独立开关显式开启，默认与非 true 值都必须保持关闭。
func TestPublicT2IEnabled仅接受显式True(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"1", false}, {"true", true}, {"TRUE", true},
	} {
		t.Setenv("GATEWAY_PUBLIC_T2I_ENABLED", testCase.value)
		if got := publicT2IEnabled(); got != testCase.want {
			t.Fatalf("publicT2IEnabled(%q) = %t, want %t", testCase.value, got, testCase.want)
		}
	}
}

// 公开视频开关独立于 T2I；仅会话开关和该开关均显式开启时才允许连接本地 MongoDB。
func TestPublicVideoEnabled仅接受显式True(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"1", false}, {"true", true}, {"TRUE", true},
	} {
		t.Setenv("GATEWAY_PUBLIC_VIDEO_ENABLED", testCase.value)
		if got := publicVideoEnabled(); got != testCase.want {
			t.Fatalf("publicVideoEnabled(%q) = %t, want %t", testCase.value, got, testCase.want)
		}
	}
}

func TestNewOptionalPublicVideoHandler默认不读取配置且显式开启才读取(t *testing.T) {
	handler, cleanup, err := newOptionalPublicVideoHandler(false, "/private/tmp/missing-gateway-config.yaml", nil)
	if err != nil || handler != nil || cleanup != nil {
		t.Fatal("关闭开关时不应读取配置或装配本地视频 Handler")
	}
	handler, cleanup, err = newOptionalPublicVideoHandler(true, "/private/tmp/missing-gateway-config.yaml", nil)
	if err == nil || handler != nil || cleanup != nil {
		t.Fatal("显式开启时必须读取受控 Gateway 配置并校验认证器")
	}
}

// 视频创建与其可信回调必须同期开关；没有本地回调 Handler 时绝不能接管预扣创建。
func TestPublicVideoLocalEnabled要求会话视频和回调同时可用(t *testing.T) {
	callback := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, testCase := range []struct {
		name           string
		sessionEnabled bool
		videoEnabled   bool
		callback       http.Handler
		want           bool
	}{
		{name: "缺会话", videoEnabled: true, callback: callback, want: false},
		{name: "缺视频开关", sessionEnabled: true, callback: callback, want: false},
		{name: "缺本地回调", sessionEnabled: true, videoEnabled: true, want: false},
		{name: "三项齐全", sessionEnabled: true, videoEnabled: true, callback: callback, want: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := publicVideoLocalEnabled(testCase.sessionEnabled, testCase.videoEnabled, testCase.callback); got != testCase.want {
				t.Fatalf("publicVideoLocalEnabled() = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestNewGateway将生成回调处理器写入受控配置(t *testing.T) {
	upstream, err := url.Parse("http://node.invalid")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	callback := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		called = true
		writer.WriteHeader(http.StatusNoContent)
	})

	proxy := newGateway(upstream, gateway.NewFileRouteSwitch(""), time.Second, callback, nil, nil)
	recorder := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/internal/generation-callback", nil))

	if !called || recorder.Code != http.StatusNoContent {
		t.Fatal("生成回调处理器未被注入 Gateway 配置")
	}
}

func TestNewGenerationCallbackHandler只接受独立回调密钥(t *testing.T) {
	usecase := &bizgeneration.CallbackUsecase{}
	if handler, err := newGenerationCallbackHandler(&conf.Security{}, usecase); err == nil || handler != nil {
		t.Fatal("缺失回调密钥时不应构造处理器")
	}

	handler, err := newGenerationCallbackHandler(&conf.Security{GenerationCallbackHmacKey: "gateway-callback-key-at-least-thirty-two-characters"}, usecase)
	if err != nil || handler == nil {
		t.Fatalf("有效独立回调密钥未构造处理器: %v", err)
	}
}

func TestNewConfiguredGenerationCallbackHandler拒绝非本地Mongo配置(t *testing.T) {
	handler, cleanup, err := newConfiguredGenerationCallbackHandler(&conf.Data{}, &conf.Security{GenerationCallbackHmacKey: "gateway-callback-key-at-least-thirty-two-characters"})
	if err == nil || handler != nil || cleanup != nil {
		t.Fatal("非本地 Mongo 配置不应构造回调处理器")
	}
}

func TestNewConfiguredSessionAuthenticator拒绝非本地Mongo配置(t *testing.T) {
	authenticator, cleanup, err := newConfiguredSessionAuthenticator(&conf.Data{})
	if err == nil || authenticator != nil || cleanup != nil {
		t.Fatal("非本地 Mongo 配置不应构造会话认证器")
	}
}

func TestCombineCleanups按逆序各执行一次并忽略空函数(t *testing.T) {
	var calls []string
	cleanup := combineCleanups(
		func() { calls = append(calls, "first") },
		nil,
		func() { calls = append(calls, "last") },
	)

	cleanup()

	if got := strings.Join(calls, ","); got != "last,first" {
		t.Fatalf("cleanup order = %q, want last,first", got)
	}
}

func TestLoadGatewayBootstrap拒绝缺失配置文件(t *testing.T) {
	bootstrap, err := loadGatewayBootstrap("/private/tmp/does-not-exist-gateway-config.yaml")
	if err == nil || bootstrap != nil {
		t.Fatal("缺失配置文件不应构造本地回调配置")
	}
}

func TestRejectsDeprecatedPrefixUpstreams(t *testing.T) {
	if err := rejectDeprecatedPrefixUpstreams("/api/homepage=http://127.0.0.1:8081"); err == nil {
		t.Fatal("rejectDeprecatedPrefixUpstreams accepted deprecated prefix configuration")
	}
	if err := rejectDeprecatedPrefixUpstreams(" \t"); err != nil {
		t.Fatalf("rejectDeprecatedPrefixUpstreams rejected blank configuration: %v", err)
	}
}

func TestParseOptionalAdmissionTimeout(t *testing.T) {
	testCases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset keeps existing behavior", raw: "", want: 0},
		{name: "parses valid duration", raw: "250ms", want: 250 * time.Millisecond},
		{name: "rejects invalid duration", raw: "later", wantErr: true},
		{name: "rejects negative duration", raw: "-1s", wantErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := parseOptionalAdmissionTimeout(testCase.raw)
			if testCase.wantErr {
				if err == nil {
					t.Fatal("parseOptionalAdmissionTimeout() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOptionalAdmissionTimeout() error = %v", err)
			}
			if got != testCase.want {
				t.Fatalf("parseOptionalAdmissionTimeout() = %s, want %s", got, testCase.want)
			}
		})
	}
}
