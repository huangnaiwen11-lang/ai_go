package identitycontract

import (
	"errors"
	"testing"
)

func TestValidateManifest接受冻结的静态身份合同(t *testing.T) {
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

func TestValidateManifest要求双API前缀与本地登出基线(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{
			name: "缺少版本前缀别名",
			mutate: func(manifest *Manifest) {
				manifest.Routes[0].Aliases = nil
			},
		},
		{
			name: "登出误改为服务端请求",
			mutate: func(manifest *Manifest) {
				manifest.Logout = LogoutBaseline{ClearsLocalToken: true, CallsServer: true}
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

func TestValidateManifest拒绝遗漏已登记的OAuth与账户维护路由(t *testing.T) {
	manifest := validManifest()
	manifest.Routes = manifest.Routes[:len(manifest.Routes)-1]

	err := ValidateManifest(manifest)
	if !errors.Is(err, ErrInvalidStaticContract) {
		t.Fatalf("ValidateManifest() error = %v, want ErrInvalidStaticContract", err)
	}
}

func validManifest() Manifest {
	return Manifest{
		Classification:      ClassificationPending,
		MigrationPermission: MigrationNotGranted,
		SourceScope:         SourceScopeStaticAudit,
		Routes: []Route{
			{Method: "POST", Path: "/api/auth/register", Aliases: []string{"/api/v1/auth/register"}},
			{Method: "POST", Path: "/api/auth/login", Aliases: []string{"/api/v1/auth/login"}},
			{Method: "GET", Path: "/api/auth/me", Aliases: []string{"/api/v1/auth/me"}},
			{Method: "POST", Path: "/api/auth/guest", Aliases: []string{"/api/v1/auth/guest"}},
			{Method: "PUT", Path: "/api/auth/me/push-token", Aliases: []string{"/api/v1/auth/me/push-token"}},
			{Method: "DELETE", Path: "/api/auth/me/push-token", Aliases: []string{"/api/v1/auth/me/push-token"}},
			{Method: "POST", Path: "/api/auth/me/signup-bonus-repair", Aliases: []string{"/api/v1/auth/me/signup-bonus-repair"}},
			{Method: "POST", Path: "/api/auth/email/send-magic-link", Aliases: []string{"/api/v1/auth/email/send-magic-link"}},
			{Method: "GET", Path: "/api/auth/email/verify", Aliases: []string{"/api/v1/auth/email/verify"}},
			{Method: "GET", Path: "/api/auth/google", Aliases: []string{"/api/v1/auth/google"}},
			{Method: "GET", Path: "/api/auth/apple", Aliases: []string{"/api/v1/auth/apple"}},
			{Method: "POST", Path: "/api/auth/apple/callback", Aliases: []string{"/api/v1/auth/apple/callback"}},
			{Method: "GET", Path: "/api/auth/facebook", Aliases: []string{"/api/v1/auth/facebook"}},
			{Method: "GET", Path: "/api/auth/facebook/upgrade", Aliases: []string{"/api/v1/auth/facebook/upgrade"}},
			{Method: "GET", Path: "/api/auth/facebook/callback", Aliases: []string{"/api/v1/auth/facebook/callback"}},
			{Method: "GET", Path: "/api/auth/twitter", Aliases: []string{"/api/v1/auth/twitter"}},
			{Method: "GET", Path: "/api/auth/twitter/callback", Aliases: []string{"/api/v1/auth/twitter/callback"}},
			{Method: "GET", Path: "/api/auth/google/upgrade", Aliases: []string{"/api/v1/auth/google/upgrade"}},
			{Method: "GET", Path: "/api/auth/google/callback", Aliases: []string{"/api/v1/auth/google/callback"}},
			{Method: "POST", Path: "/api/auth/native/apple", Aliases: []string{"/api/v1/auth/native/apple"}},
			{Method: "POST", Path: "/api/auth/native/google", Aliases: []string{"/api/v1/auth/native/google"}},
		},
		Logout: LogoutBaseline{ClearsLocalToken: true},
	}
}
