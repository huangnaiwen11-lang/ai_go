package admincontract

import (
	"errors"
	"testing"
)

func TestValidateManifest接受冻结的管理后台静态合同(t *testing.T) {
	if err := ValidateManifest(validManifest()); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
}

func TestValidateManifest拒绝未授权的Vue或Go迁移(t *testing.T) {
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
			name: "擅自启用 Vue 壳层",
			mutate: func(manifest *Manifest) {
				manifest.VueShellEnabled = true
			},
		},
		{
			name: "擅自启用 Go Admin 路由",
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

func TestValidateManifest冻结三条权限分支与审计语义(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{
			name: "super admin 失去全量权限",
			mutate: func(manifest *Manifest) {
				manifest.Access.SuperAdminAllPaths = false
			},
		},
		{
			name: "legacy admin 误套 scope 限制",
			mutate: func(manifest *Manifest) {
				manifest.Access.LegacyAdminWithoutScopesAllPaths = false
			},
		},
		{
			name: "scoped admin 不再按 scope 限制",
			mutate: func(manifest *Manifest) {
				manifest.Access.ScopedAdminRequiresPathScope = false
			},
		},
		{
			name: "审计写入失败开始中断业务",
			mutate: func(manifest *Manifest) {
				manifest.AuditWriteFailureDoesNotBlockBusiness = false
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

func validManifest() Manifest {
	return Manifest{
		Classification:                        ClassificationPending,
		MigrationPermission:                   MigrationNotGranted,
		SourceScope:                           SourceScopeStaticAudit,
		BrowserEntry:                          "/admin/",
		APIEntry:                              "/api",
		AuditWriteFailureDoesNotBlockBusiness: true,
		Access: AccessBaseline{
			SuperAdminAllPaths:               true,
			LegacyAdminWithoutScopesAllPaths: true,
			ScopedAdminRequiresPathScope:     true,
		},
	}
}
