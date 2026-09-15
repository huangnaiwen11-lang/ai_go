package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun接受三类冻结静态合同(t *testing.T) {
	for _, testCase := range []struct {
		stage string
		body  []byte
	}{
		{stage: "identity", body: validIdentityManifest(t)},
		{stage: "wallet", body: validWalletManifest()},
		{stage: "admin", body: validAdminManifest()},
	} {
		t.Run(testCase.stage, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run(
				[]string{"--stage", testCase.stage, "--manifest", testCase.stage + ".json"},
				func(string) ([]byte, error) { return testCase.body, nil },
				&stdout,
				&stderr,
			)

			if code != 0 {
				t.Fatalf("run exit code = %d, stderr = %q", code, stderr.String())
			}
			if got, want := stdout.String(), "静态合同门禁通过：仅表示本地源码基线一致，不代表迁移或切流许可。\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRun拒绝未授权字段且不回显内容(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	privateValue := "private-routing-flag"
	code := run(
		[]string{"--stage", "identity", "--manifest", "identity.json"},
		func(string) ([]byte, error) {
			return []byte(`{"classification":"待核实","migrationPermission":"未授予","sourceScope":"仅静态源码审计","goRouteEnabled":true,"private":"` + privateValue + `"}`), nil
		},
		&stdout,
		&stderr,
	)

	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "静态合同清单无效。\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if bytes.Contains(stderr.Bytes(), []byte(privateValue)) {
		t.Fatalf("stderr must not reveal private input: %q", stderr.String())
	}
}

// 静态合同的 false 不是“缺省即可”的默认值，而是必须由审计材料明确声明的安全
// 开关。重复键也必须拒绝，避免 JSON 最后一个同名字段静默覆盖前面的审核结论。
func TestRun拒绝缺失或重复的关键合同字段(t *testing.T) {
	testCases := []struct {
		name  string
		stage string
		body  []byte
	}{
		{
			name:  "Wallet 缺失明确关闭 Go 路由的开关",
			stage: "wallet",
			body: []byte(strings.Replace(
				string(validWalletManifest()),
				`"goRouteEnabled":false,`,
				"",
				1,
			)),
		},
		{
			name:  "Identity 缺失明确禁止服务端登出的字段",
			stage: "identity",
			body: []byte(strings.Replace(
				string(validIdentityManifest(t)),
				",\n    \"callsServer\": false",
				"",
				1,
			)),
		},
		{
			name:  "Admin 缺失明确关闭 Vue 壳层的开关",
			stage: "admin",
			body: []byte(strings.Replace(
				string(validAdminManifest()),
				`"vueShellEnabled":false,`,
				"",
				1,
			)),
		},
		{
			name:  "Wallet 重复 Go 路由开关",
			stage: "wallet",
			body: []byte(strings.Replace(
				string(validWalletManifest()),
				`"goRouteEnabled":false,`,
				`"goRouteEnabled":true,"goRouteEnabled":false,`,
				1,
			)),
		},
		{
			name:  "Admin 重复嵌套已知权限字段",
			stage: "admin",
			body: []byte(strings.Replace(
				string(validAdminManifest()),
				`"scopedAdminRequiresPathScope":true`,
				`"scopedAdminRequiresPathScope":false,"scopedAdminRequiresPathScope":true`,
				1,
			)),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run(
				[]string{"--stage", testCase.stage, "--manifest", testCase.stage + ".json"},
				func(string) ([]byte, error) { return testCase.body, nil },
				&stdout,
				&stderr,
			)

			if code != 1 {
				t.Fatalf("run exit code = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if got, want := stderr.String(), "静态合同清单无效。\n"; got != want {
				t.Fatalf("stderr = %q, want %q", got, want)
			}
		})
	}
}

func TestRun拒绝读取失败(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		[]string{"--stage", "admin", "--manifest", "admin.json"},
		func(string) ([]byte, error) { return nil, errors.New("permission denied") },
		&stdout,
		&stderr,
	)

	if code != 2 {
		t.Fatalf("run exit code = %d, want 2", code)
	}
	if got, want := stderr.String(), "无法读取静态合同清单。\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func Test工程内三份静态合同清单均可通过(t *testing.T) {
	for _, testCase := range []struct {
		stage    string
		filename string
	}{
		{stage: "identity", filename: "identity-static-contract.json"},
		{stage: "wallet", filename: "wallet-static-contract.json"},
		{stage: "admin", filename: "admin-static-contract.json"},
	} {
		t.Run(testCase.stage, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join("..", "..", "docs", "audit", testCase.filename))
			if err != nil {
				t.Fatalf("读取静态合同清单失败：%v", err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run(
				[]string{"--stage", testCase.stage, "--manifest", testCase.filename},
				func(string) ([]byte, error) { return content, nil },
				&stdout,
				&stderr,
			)
			if code != 0 {
				t.Fatalf("示例清单 exit code = %d, stderr = %q", code, stderr.String())
			}
		})
	}
}

func validIdentityManifest(t *testing.T) []byte {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "audit", "identity-static-contract.json"))
	if err != nil {
		t.Fatalf("读取 Identity 静态合同清单失败：%v", err)
	}
	return content
}

func validWalletManifest() []byte {
	return []byte(`{
  "classification":"待核实",
  "migrationPermission":"未授予",
  "sourceScope":"仅静态源码审计",
  "goRouteEnabled":false,
  "balance":{"route":{"method":"GET","path":"/api/wallet/balance","aliases":["/api/v1/wallet/balance"]},"mayCreateWallet":true,"cacheControl":"private, no-store, max-age=0, must-revalidate","varyAuthorization":true,"cacheStatus":true,"mayExposeStale":true},
  "ledger":{"route":{"method":"GET","path":"/api/wallet/ledger","aliases":["/api/v1/wallet/ledger"]},"defaultLimit":50,"minimumLimit":1,"maximumSkip":100000,"cursorPrecedesSkip":true},
  "mutation":{"entryPoint":"applyMutation","productionTxn":true,"preventsNegative":true,"idempotentReplay":true,"publishAfterCommit":true},
  "payment":{"owner":"PayCores","webhookOwner":"PayCores","waitForBackendReady":true,"proxyRoutes":[{"method":"GET","path":"/api/wallet/external-payment-methods","aliases":["/api/v1/wallet/external-payment-methods"]},{"method":"POST","path":"/api/wallet/create-external-checkout","aliases":["/api/v1/wallet/create-external-checkout"]},{"method":"POST","path":"/api/wallet/external-checkout-billing-prefill","aliases":["/api/v1/wallet/external-checkout-billing-prefill"]},{"method":"POST","path":"/api/wallet/crypto/create-payment","aliases":["/api/v1/wallet/crypto/create-payment"]},{"method":"POST","path":"/api/wallet/crypto/refresh-payment","aliases":["/api/v1/wallet/crypto/refresh-payment"]},{"method":"GET","path":"/api/wallet/crypto/status/:orderId","aliases":["/api/v1/wallet/crypto/status/:orderId"]}],"callback":{"method":"POST","path":"/api/internal/payment-confirmed","aliases":["/api/v1/internal/payment-confirmed"]},"callbackRequiresV2HMAC":true,"callbackRejectsNonceReplay":true}
}`)
}

func validAdminManifest() []byte {
	return []byte(`{
  "classification":"待核实",
  "migrationPermission":"未授予",
  "sourceScope":"仅静态源码审计",
  "vueShellEnabled":false,
  "goRouteEnabled":false,
  "browserEntry":"/admin/",
  "apiEntry":"/api",
  "access":{"superAdminAllPaths":true,"legacyAdminWithoutScopesAllPaths":true,"scopedAdminRequiresPathScope":true},
  "auditWriteFailureDoesNotBlockBusiness":true
}`)
}
