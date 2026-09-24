package adminapps

import (
	"strings"
	"testing"
)

func TestBuildPackageConfigExportsStoredNativeBuild(t *testing.T) {
	app := Document{
		"name": "Demo", "appKey": "demo", "nativeBuild": map[string]any{
			"displayName": "Demo", "android": map[string]any{"applicationId": "com.example.demo", "signingCertificateSha256": []any{"ab:cd" + strings.Repeat("ef", 30)}},
			"ios":      map[string]any{"bundleId": "com.example.demo.ios"},
			"google":   map[string]any{"iosClientId": "ios", "iosReversedClientId": "reverse", "androidClientId": "android", "serverClientId": "server"},
			"firebase": map[string]any{"androidGoogleServicesJson": "{}"},
		},
	}
	config, err := BuildPackageConfig(app)
	if err != nil {
		t.Fatalf("BuildPackageConfig() error = %v", err)
	}
	if got := FieldString(FieldObject(FieldObject(config, "config"), "android"), "namespace"); got != "com.example.demo" {
		t.Fatalf("namespace=%q", got)
	}
	if got := FieldString(FieldObject(config, "files"), "androidGoogleServicesJson"); got != "{}" {
		t.Fatalf("file=%q", got)
	}
}

func TestBuildNativeConfigTemplateKeepsConfiguredOriginAndLegalURL(t *testing.T) {
	app := Document{"name": "Demo", "apiUrl": "https://api.example.com/path", "nativeConfig": map[string]any{
		"domain": map[string]any{"originIp": "203.0.113.10"}, "legal": map[string]any{"privacyUrl": "https://legal.example/privacy"},
	}}
	template := BuildNativeConfigTemplate(app)
	if got := FieldString(FieldObject(template, "domain"), "originIp"); got != "203.0.113.10" {
		t.Fatalf("originIp=%q", got)
	}
	if got := FieldString(FieldObject(template, "legal"), "privacyUrl"); got != "https://legal.example/privacy" {
		t.Fatalf("privacyUrl=%q", got)
	}
}

func TestNormalizeReviewModeRejectsInvalidCutoff(t *testing.T) {
	if _, err := NormalizeReviewMode(map[string]any{"mode": "hybrid", "cutoffDate": "not-a-time"}); err == nil {
		t.Fatal("invalid cutoff accepted")
	}
	mode, err := NormalizeReviewMode(map[string]any{"mode": "strict", "versionName": "1.0", "buildNumber": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if mode["versionScope"] != "exact" || mode["enabled"] != true {
		t.Fatalf("mode=%#v", mode)
	}
}
