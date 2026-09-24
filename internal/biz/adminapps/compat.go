package adminapps

import (
	"fmt"
	"strings"
	"time"
)

var legalPageTypes = []string{"privacy", "terms", "support", "deletion", "about"}
var platformNames = []string{"web", "ios", "android", "mini_program", "default"}

func IsLegalPageType(value string) bool { return containsString(legalPageTypes, value) }
func IsPlatformName(value string) bool  { return containsString(platformNames, value) }

func BuildNativeConfigTemplate(app Document) Document {
	apiURL := ResolveAPIURL(app)
	native := FieldObject(app, "nativeConfig")
	legal := FieldObject(native, "legal")
	if legal == nil {
		legal = map[string]any{}
	}
	domain := FieldObject(native, "domain")
	h5URL := firstNonEmpty(FieldString(legal, "h5Url"), apiURL+"/video?platform=ios")
	return Document{
		"title":       firstNonEmpty(PathString(app, "nativeBuild", "displayName"), FieldString(app, "name")),
		"endpointMap": Document{"config": "/native/app-config", "iapOrder": "", "iapVerify": "/api/v1/wallet/verify-purchase", "iapPending": ""},
		"crypto":      Document{"mode": "aes-ecb-base64"},
		"domain":      Document{"apiUrl": apiURL, "originIp": FieldString(domain, "originIp")},
		"legal":       Document{"h5Url": h5URL, "privacyUrl": firstNonEmpty(FieldString(legal, "privacyUrl"), apiURL+"/privacypolicy.html"), "termsUrl": firstNonEmpty(FieldString(legal, "termsUrl"), apiURL+"/TermsofService.html"), "supportUrl": FieldString(legal, "supportUrl"), "deletionUrl": FieldString(legal, "deletionUrl"), "aboutUrl": FieldString(legal, "aboutUrl")},
		"stateRules":  Document{"defaultS": 1, "rules": []any{}},
	}
}

func NativeRuntimeTarget(app Document) (platform, clientID, identifier string, ok bool) {
	platform = FieldString(app, "platform")
	switch platform {
	case "android":
		identifier = firstNonEmpty(PathString(app, "nativeBuild", "android", "applicationId"), FieldString(app, "packageName"), FieldString(app, "bundleId"))
	case "ios":
		identifier = firstNonEmpty(PathString(app, "nativeBuild", "ios", "bundleId"), FieldString(app, "bundleId"))
	default:
		return "", "", "", false
	}
	if identifier == "" {
		return "", "", "", false
	}
	return platform, firstNonEmpty(FieldString(app, "clientId"), identifier), identifier, true
}

func NormalizeReviewMode(input map[string]any) (Document, error) {
	raw, _ := input["mode"].(string)
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		if enabled, _ := input["enabled"].(bool); enabled {
			raw = "strict"
		} else {
			raw = "off"
		}
	}
	if raw != "strict" && raw != "hybrid" && raw != "off" {
		return nil, fmt.Errorf("%w: invalid review mode", ErrInvalid)
	}
	result := Document{"enabled": raw != "off", "mode": raw, "cutoffDate": nil, "versionScope": "all", "versions": []any{}}
	if raw == "hybrid" && input["cutoffDate"] != nil {
		text, ok := input["cutoffDate"].(string)
		if !ok {
			return nil, fmt.Errorf("%w: invalid review-mode cutoffDate", ErrInvalid)
		}
		value, err := time.Parse(time.RFC3339, strings.TrimSpace(text))
		if err != nil {
			return nil, fmt.Errorf("%w: invalid review-mode cutoffDate", ErrInvalid)
		}
		result["cutoffDate"] = value.UTC()
	}
	version, _ := input["versionName"].(string)
	build, _ := input["buildNumber"].(string)
	note, _ := input["note"].(string)
	version, build, note = strings.TrimSpace(version), strings.TrimSpace(build), strings.TrimSpace(note)
	if version != "" || build != "" {
		entry := Document{"versionName": version, "buildNumber": build, "enabled": true}
		if note != "" {
			entry["note"] = note
		}
		result["versions"] = []any{entry}
		result["versionScope"] = "exact"
	}
	return result, nil
}

func SafeContentPolicy() Document {
	return Document{"contentFetchMode": "sfw", "maxContentRating": "sfw", "allowNSFW": false, "allowViolence": false, "minAge": 0, "moderationLevel": "strict", "blockedTags": []any{}, "blockedKeywords": []any{}}
}

func DomainCandidateSeed(app Document, input map[string]any) string {
	if seed, _ := input["seed"].(string); strings.TrimSpace(seed) != "" {
		return slug(strings.TrimSpace(seed))
	}
	return slug(firstNonEmpty(FieldString(app, "appKey"), PathString(app, "nativeBuild", "appKey"), FieldString(app, "clientId"), FieldString(app, "domain"), FieldString(app, "name")))
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	dash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func NormalizedDomain(value string) (string, error) {
	domain := NormalizeWebDomain(value)
	if domain == "" || !strings.Contains(domain, ".") {
		return "", fmt.Errorf("%w: domain must be a valid hostname", ErrInvalid)
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("%w: domain must contain valid hostname labels", ErrInvalid)
		}
	}
	return domain, nil
}
