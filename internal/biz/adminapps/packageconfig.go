package adminapps

import (
	"fmt"
	"strings"
)

// BuildPackageConfig maps the persisted nativeBuild representation to the
// public build-pipeline contract. File contents are returned only to a
// privileged admin request and are never copied to list/get App projections.
func BuildPackageConfig(app Document) (Document, error) {
	native := FieldObject(app, "nativeBuild")
	if native == nil {
		return nil, fmt.Errorf("%w: Missing native build config", ErrInvalid)
	}
	android := FieldObject(native, "android")
	ios := FieldObject(native, "ios")
	facebook := FieldObject(native, "facebook")
	google := FieldObject(native, "google")
	firebase := FieldObject(native, "firebase")

	appKey := firstNonEmpty(FieldString(app, "appKey"), FieldString(native, "appKey"), appKeyFallback(app))
	displayName := firstNonEmpty(FieldString(native, "displayName"), FieldString(app, "name"))
	androidID := FieldString(android, "applicationId")
	iosID := FieldString(ios, "bundleId")
	if displayName == "" || androidID == "" || iosID == "" || FieldString(google, "androidClientId") == "" || FieldString(google, "serverClientId") == "" || FieldString(google, "iosClientId") == "" || FieldString(google, "iosReversedClientId") == "" {
		return nil, fmt.Errorf("%w: Missing native build config", ErrInvalid)
	}
	namespace := firstNonEmpty(FieldString(android, "namespace"), androidID)
	return Document{
		"appKey": appKey,
		"config": Document{
			"displayName": displayName,
			"android": Document{
				"applicationId": androidID, "namespace": namespace,
				"deepLinkSchemes":          stringSlice(android["deepLinkSchemes"]),
				"signingCertificateSha256": normalizedFingerprints(android["signingCertificateSha256"]),
			},
			"ios":      Document{"bundleId": iosID},
			"facebook": Document{"appId": FieldString(facebook, "appId"), "clientToken": FieldString(facebook, "clientToken")},
			"google": Document{
				"iosClientId": FieldString(google, "iosClientId"), "iosReversedClientId": FieldString(google, "iosReversedClientId"),
				"androidClientId": FieldString(google, "androidClientId"), "serverClientId": FieldString(google, "serverClientId"),
			},
		},
		"files": Document{"androidGoogleServicesJson": nullableString(firebase, "androidGoogleServicesJson"), "iosGoogleServiceInfoPlist": nullableString(firebase, "iosGoogleServiceInfoPlist")},
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func appKeyFallback(app Document) string {
	raw := firstNonEmpty(FieldString(app, "packageName"), FieldString(app, "bundleId"), FieldString(app, "domain"), FieldString(app, "name"))
	if index := strings.LastIndex(raw, "."); index >= 0 && index < len(raw)-1 {
		raw = raw[index+1:]
	}
	raw = strings.ToLower(raw)
	raw = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, raw)
	return strings.Trim(raw, "-")
}

func stringSlice(value any) []string {
	rows := FieldArray(Document{"value": value}, "value")
	result := make([]string, 0, len(rows))
	seen := map[string]struct{}{}
	for _, row := range rows {
		text, _ := row.(string)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		result = append(result, text)
	}
	return result
}

func normalizedFingerprints(value any) []string {
	values := stringSlice(value)
	result := make([]string, 0, len(values))
	for _, value := range values {
		hex := strings.ToUpper(strings.NewReplacer(":", "", " ", "").Replace(value))
		if len(hex) != 64 {
			continue
		}
		parts := make([]string, 0, 32)
		for i := 0; i < len(hex); i += 2 {
			parts = append(parts, hex[i:i+2])
		}
		result = append(result, strings.Join(parts, ":"))
	}
	return result
}

func nullableString(document map[string]any, key string) any {
	if value := FieldString(document, key); value != "" {
		return value
	}
	return nil
}
