package walletcontract

import (
	"errors"
	"reflect"
	"testing"
)

func TestValidateManifest接受冻结的钱包静态合同(t *testing.T) {
	if err := ValidateManifest(validManifest()); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
}

func TestValidateManifest拒绝迁移许可或Go分流(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{
			name: "擅自授予迁移许可",
			mutate: func(manifest *Manifest) {
				manifest.MigrationPermission = "已授予"
			},
		},
		{
			name: "擅自登记 Go 分流",
			mutate: func(manifest *Manifest) {
				manifest.GoRouteEnabled = true
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := validManifest()
			testCase.mutate(&manifest)

			err := ValidateManifest(manifest)
			if !errors.Is(err, ErrMigrationNotAllowed) {
				t.Fatalf("ValidateManifest() error = %v, want ErrMigrationNotAllowed", err)
			}
		})
	}
}

func TestValidateManifest冻结余额账本与支付边界(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{
			name: "余额读取误标为无副作用",
			mutate: func(manifest *Manifest) {
				manifest.Balance.MayCreateWallet = false
			},
		},
		{
			name: "账本忽略 cursor 优先级",
			mutate: func(manifest *Manifest) {
				manifest.Ledger.CursorPrecedesSkip = false
			},
		},
		{
			name: "账务在提交前发布事件",
			mutate: func(manifest *Manifest) {
				manifest.Mutation.PublishAfterCommit = false
			},
		},
		{
			name: "主站错误接管支付 Webhook",
			mutate: func(manifest *Manifest) {
				manifest.Payment.WebhookOwner = "主站"
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := validManifest()
			testCase.mutate(&manifest)

			err := ValidateManifest(manifest)
			if !errors.Is(err, ErrInvalidStaticContract) {
				t.Fatalf("ValidateManifest() error = %v, want ErrInvalidStaticContract", err)
			}
		})
	}
}

func TestValidateManifest冻结PayCores代理入口与V2回调边界(t *testing.T) {
	manifest := validManifest()
	wantProxyRoutes := []Route{
		{Method: "GET", Path: "/api/wallet/external-payment-methods", Aliases: []string{"/api/v1/wallet/external-payment-methods"}},
		{Method: "POST", Path: "/api/wallet/create-external-checkout", Aliases: []string{"/api/v1/wallet/create-external-checkout"}},
		{Method: "POST", Path: "/api/wallet/external-checkout-billing-prefill", Aliases: []string{"/api/v1/wallet/external-checkout-billing-prefill"}},
		{Method: "POST", Path: "/api/wallet/crypto/create-payment", Aliases: []string{"/api/v1/wallet/crypto/create-payment"}},
		{Method: "POST", Path: "/api/wallet/crypto/refresh-payment", Aliases: []string{"/api/v1/wallet/crypto/refresh-payment"}},
		{Method: "GET", Path: "/api/wallet/crypto/status/:orderId", Aliases: []string{"/api/v1/wallet/crypto/status/:orderId"}},
	}
	if !reflect.DeepEqual(manifest.Payment.ProxyRoutes, wantProxyRoutes) {
		t.Fatalf("PayCores proxy routes = %#v, want %#v", manifest.Payment.ProxyRoutes, wantProxyRoutes)
	}
	if !reflect.DeepEqual(
		manifest.Payment.Callback,
		Route{Method: "POST", Path: "/api/internal/payment-confirmed", Aliases: []string{"/api/v1/internal/payment-confirmed"}},
	) {
		t.Fatalf("PayCores callback = %#v, want frozen V2 payment-confirmed callback", manifest.Payment.Callback)
	}
	if !manifest.Payment.CallbackRequiresV2HMAC || !manifest.Payment.CallbackRejectsNonceReplay {
		t.Fatal("PayCores callback must freeze V2 HMAC and nonce replay rejection")
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{
			name: "遗漏 PayCores 代理入口",
			mutate: func(manifest *Manifest) {
				manifest.Payment.ProxyRoutes = manifest.Payment.ProxyRoutes[:len(manifest.Payment.ProxyRoutes)-1]
			},
		},
		{
			name: "回调不再要求 V2 HMAC",
			mutate: func(manifest *Manifest) {
				manifest.Payment.CallbackRequiresV2HMAC = false
			},
		},
		{
			name: "回调不再拒绝 nonce 重放",
			mutate: func(manifest *Manifest) {
				manifest.Payment.CallbackRejectsNonceReplay = false
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			invalid := validManifest()
			testCase.mutate(&invalid)

			if err := ValidateManifest(invalid); !errors.Is(err, ErrInvalidStaticContract) {
				t.Fatalf("ValidateManifest() error = %v, want ErrInvalidStaticContract", err)
			}
		})
	}
}

func validManifest() Manifest {
	return Manifest{
		Classification:      ClassificationPending,
		MigrationPermission: MigrationNotGranted,
		SourceScope:         SourceScopeStaticAudit,
		Balance: BalanceBaseline{
			Route:             Route{Method: "GET", Path: "/api/wallet/balance", Aliases: []string{"/api/v1/wallet/balance"}},
			MayCreateWallet:   true,
			CacheControl:      "private, no-store, max-age=0, must-revalidate",
			VaryAuthorization: true,
			CacheStatus:       true,
			MayExposeStale:    true,
		},
		Ledger: LedgerBaseline{
			Route:              Route{Method: "GET", Path: "/api/wallet/ledger", Aliases: []string{"/api/v1/wallet/ledger"}},
			DefaultLimit:       50,
			MinimumLimit:       1,
			MaximumSkip:        100000,
			CursorPrecedesSkip: true,
		},
		Mutation: MutationBaseline{
			EntryPoint:         "applyMutation",
			ProductionTxn:      true,
			PreventsNegative:   true,
			IdempotentReplay:   true,
			PublishAfterCommit: true,
		},
		Payment: PaymentBoundary{
			Owner:                      "PayCores",
			WebhookOwner:               "PayCores",
			WaitForBackendReady:        true,
			ProxyRoutes:                expectedPaymentProxyRoutes(),
			Callback:                   expectedPaymentCallback(),
			CallbackRequiresV2HMAC:     true,
			CallbackRejectsNonceReplay: true,
		},
	}
}
