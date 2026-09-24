package polarstarb2b

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

var publicModelID = regexp.MustCompile(`^ps-[a-z0-9]+(-[a-z0-9]+)*$`)
var stableID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

type requestWire struct {
	ExternalID      string  `json:"externalId"`
	IdempotencyKey  string  `json:"idempotencyKey"`
	Capability      string  `json:"capability"`
	Model           string  `json:"model"`
	TemplateID      string  `json:"templateId,omitempty"`
	Input           Input   `json:"input"`
	CallbackURL     *string `json:"callbackUrl"`
	CallbackPolicy  string  `json:"callbackPolicy"`
	ResultURLPolicy string  `json:"resultUrlPolicy"`
}

// MapRequest compiles a published product mapping into exact frozen public wire bytes.
// Callers own authorization, input URL publication, and persistence before any HTTP call.
// No source from execution.v2 is passed through: its private controls are a different contract.
func MapRequest(snapshot MappingSnapshot, route Route, product ProductInput, callback CallbackConfig) (Request, error) {
	if !validRoute(route) || route.MappingVersion != snapshot.Version || !supportedCapability(product.Capability) {
		return Request{}, ErrInvalidRequest
	}
	resolved, err := resolveProduct(snapshot, product, false)
	if err != nil {
		return Request{}, ErrInvalidRequest
	}
	var callbackURL *string
	policy := "disabled"
	switch callback.Mode {
	case "lookup_only":
		if callback.URL != "" {
			return Request{}, ErrInvalidRequest
		}
	case "webhook":
		if _, ok := publicHTTPSURL(callback.URL); !ok {
			return Request{}, ErrInvalidRequest
		}
		callbackURL = &callback.URL
		policy = "bounded"
	default:
		return Request{}, ErrInvalidRequest
	}
	id := "cling-step:" + route.StepID
	payload, err := json.Marshal(requestWire{ExternalID: route.StepID, IdempotencyKey: id, Capability: product.Capability, Model: resolved.mapping.Model, TemplateID: resolved.templateID, Input: resolved.input, CallbackURL: callbackURL, CallbackPolicy: policy, ResultURLPolicy: "permanent"})
	if err != nil || len(payload) > 128<<10 {
		return Request{}, ErrInvalidRequest
	}
	sum := sha256.Sum256(payload)
	return Request{payload: payload, digest: hex.EncodeToString(sum[:]), externalID: route.StepID, capability: product.Capability, route: route}, nil
}

// ValidateProduct applies the exact same public-product and input rules as
// MapRequest, without requiring a step identity or callback configuration.
// Admission uses it before any reservation is created, so an invalid published
// recipe cannot charge the user only to fail in the submission worker later.
func ValidateProduct(snapshot MappingSnapshot, product ProductInput) error {
	_, err := resolveProduct(snapshot, product, false)
	return err
}

// ValidateDeferredImageToVideoProduct validates the immutable base of a
// two-step text-to-video product before its first frame exists. It is not a
// weaker general validator: it accepts only image_to_video, requires the
// normal public input rules, and rejects every pre-bound primary image. The
// normal ValidateProduct/MapRequest path must run again after the owned R2
// opening frame has been injected.
func ValidateDeferredImageToVideoProduct(snapshot MappingSnapshot, product ProductInput) error {
	if product.Capability != "image_to_video" || product.Input.ImageURL != "" {
		return ErrInvalidRequest
	}
	for _, asset := range product.Assets {
		if asset.Role == "source_image" || asset.Role == "opening_frame" {
			return ErrInvalidRequest
		}
	}
	_, err := resolveProduct(snapshot, product, true)
	return err
}

type resolvedProduct struct {
	mapping    ModelMapping
	templateID string
	input      Input
}

func resolveProduct(snapshot MappingSnapshot, product ProductInput, allowUnboundOpeningFrame bool) (resolvedProduct, error) {
	if snapshot.Version == "" || !supportedCapability(product.Capability) {
		return resolvedProduct{}, ErrInvalidRequest
	}
	mapping, ok := snapshot.Models[product.ModelKey]
	if !ok || mapping.Capability != product.Capability || !publicModelID.MatchString(mapping.Model) || len(mapping.Model) > 100 {
		return resolvedProduct{}, ErrInvalidRequest
	}
	templateID := ""
	if product.TemplateKey != "" {
		var ok bool
		templateID, ok = mapping.Templates[product.TemplateKey]
		if !ok || !validID(templateID, 200) {
			return resolvedProduct{}, ErrInvalidRequest
		}
	}
	input, ok := mapAssets(product.Input, product.Assets, product.Capability)
	if !ok || !validateInput(input, mapping, templateID, allowUnboundOpeningFrame) {
		return resolvedProduct{}, ErrInvalidRequest
	}
	return resolvedProduct{mapping: mapping, templateID: templateID, input: input}, nil
}

func mapAssets(input Input, assets []Asset, capability string) (Input, bool) {
	if len(assets) > 7 {
		return Input{}, false
	}
	// Allocate fresh slices before appending; input remains owned by the caller.
	input.AdditionalImageURLs = slices.Clone(input.AdditionalImageURLs)
	input.ReferenceImageURLs = slices.Clone(input.ReferenceImageURLs)
	referenceInput := len(input.ReferenceImageURLs) > 0
	additionalInput := len(input.AdditionalImageURLs) > 0
	for _, asset := range assets {
		if asset.URL == "" {
			return Input{}, false
		}
		var target *string
		switch asset.Role {
		case "source_image":
			target = &input.ImageURL
		case "opening_frame":
			if capability != "image_to_video" {
				return Input{}, false
			}
			target = &input.ImageURL
		case "face_image":
			target = &input.FaceImageURL
		case "garment_image":
			target = &input.GarmentImageURL
		case "guide_image":
			target = &input.GuideImageURL
		case "additional_image":
			if additionalInput {
				return Input{}, false
			}
			input.AdditionalImageURLs = append(input.AdditionalImageURLs, asset.URL)
		case "reference_image":
			if referenceInput {
				return Input{}, false
			}
			input.ReferenceImageURLs = append(input.ReferenceImageURLs, asset.URL)
		default:
			return Input{}, false
		}
		if target != nil {
			if *target != "" {
				return Input{}, false
			}
			*target = asset.URL
		}
	}
	return input, true
}

func validRoute(r Route) bool {
	return validID(r.StepID, 189) && r.Provider == "polarstar_b2b_v2" && validID(r.AccountRef, 200) && r.ContractVersion == "b2b.job.v2" && validID(r.MappingVersion, 200)
}

func validID(s string, max int) bool { return len(s) > 0 && len(s) <= max && stableID.MatchString(s) }
func supportedCapability(c string) bool {
	return c == "text_to_image" || c == "image_edit" || c == "image_to_video"
}

func validateInput(in Input, m ModelMapping, templateID string, allowUnboundOpeningFrame bool) bool {
	for _, s := range []string{in.Prompt, in.NegativePrompt, in.AudioPrompt} {
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 8000 {
			return false
		}
	}
	if strings.TrimSpace(in.Prompt) == "" && (templateID == "" || m.Capability == "text_to_image") {
		return false
	}
	if in.Seed != nil && (*in.Seed < 0 || *in.Seed > 9007199254740991) {
		return false
	}
	if in.Width != 0 || in.Height != 0 {
		if in.Width <= 0 || in.Height <= 0 || !slices.Contains(m.Sizes, ImageSize{in.Width, in.Height}) {
			return false
		}
	}
	if in.AspectRatio != "" && !slices.Contains(m.AspectRatios, in.AspectRatio) {
		return false
	}
	if in.DurationSeconds != 0 && (!slices.Contains([]int{5, 10, 15}, in.DurationSeconds) || !slices.Contains(m.Durations, in.DurationSeconds)) {
		return false
	}
	if len(in.AdditionalImageURLs) > 1 || len(in.ReferenceImageURLs) > 2 {
		return false
	}
	urls := []string{in.ImageURL, in.FaceImageURL, in.GarmentImageURL, in.GuideImageURL}
	for _, s := range urls {
		if s != "" {
			if _, ok := publicHTTPSURL(s); !ok {
				return false
			}
		}
	}
	for _, list := range [][]string{in.AdditionalImageURLs, in.ReferenceImageURLs} {
		seen := map[string]bool{}
		for _, s := range list {
			canonical, ok := publicHTTPSURL(s)
			if !ok || seen[canonical] {
				return false
			}
			seen[canonical] = true
		}
	}
	if !allowUnboundOpeningFrame && m.Capability != "text_to_image" && m.Model != "ps-reference-v1" && in.ImageURL == "" {
		return false
	}
	if m.Capability == "image_to_video" && in.DurationSeconds == 0 {
		return false
	}
	if m.Model == "ps-reference-v1" {
		if m.Capability != "image_to_video" || len(in.ReferenceImageURLs) < 1 || templateID != "" || in.AspectRatio != "" || len(in.AdditionalImageURLs) != 0 || in.DurationSeconds != 10 || in.EnableAudio == nil || !*in.EnableAudio {
			return false
		}
	} else if len(in.ReferenceImageURLs) != 0 {
		return false
	}
	// Validate even unused allowlist entries; misspelled/private fields cannot broaden the contract.
	for _, field := range m.AllowedInputs {
		if !knownInput(field) {
			return false
		}
	}
	encoded, err := json.Marshal(in)
	if err != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(encoded, &fields) != nil {
		return false
	}
	for field := range fields {
		if !slices.Contains(m.AllowedInputs, field) || !capabilityInput(m.Capability, field) {
			return false
		}
	}
	return true
}

func knownInput(field string) bool {
	return capabilityInput("text_to_image", field) || capabilityInput("image_edit", field) || capabilityInput("image_to_video", field)
}

func capabilityInput(capability, field string) bool {
	switch field {
	case "prompt", "negativePrompt", "aspectRatio", "seed":
		return true
	case "width", "height":
		return capability != "image_to_video"
	case "imageUrl":
		return capability != "text_to_image"
	case "faceImageUrl", "garmentImageUrl", "guideImageUrl", "additionalImageUrls":
		return capability == "image_edit"
	case "referenceImageUrls", "durationSeconds", "enableAudio", "audioPrompt":
		return capability == "image_to_video"
	default:
		return false
	}
}

// ValidCallbackURL 报告一个回调地址能否被冻结进公开请求。
//
// 它与 MapRequest 内部的校验共用同一份实现。构造期若另写一份规则，就会造出
// 「配置通过校验、提交永远 ErrInvalidRequest」的地址——那种配置在启动时看不
// 出问题，只有等第一笔任务进来才暴露。
func ValidCallbackURL(raw string) bool {
	_, ok := publicHTTPSURL(raw)
	return ok
}

// publicHTTPSURL is a syntax/address check with no DNS or network I/O. The publisher
// must additionally prove authorization, public reachability and URL lifetime; this
// cannot establish DNS or redirect safety for a later fetch performed by the supplier.
func publicHTTPSURL(raw string) (string, bool) {
	if raw == "" || len(raw) > 8192 || strings.TrimSpace(raw) != raw || strings.Contains(raw, "#") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasSuffix(host, ".") || strings.Contains(host, "%") {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || blockedAddress(ip) {
			return "", false
		}
	} else {
		if strings.Contains(host, ":") || !strings.Contains(host, ".") {
			return "", false
		}
		for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home", ".test", ".invalid"} {
			if strings.HasSuffix(host, suffix) {
				return "", false
			}
		}
		parts := strings.Split(host, ".")
		for _, part := range parts {
			if len(part) == 0 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
				return "", false
			}
			for _, ch := range part {
				if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
					return "", false
				}
			}
		}
		// Reject legacy numeric/hex address notation rather than depend on URL parser behavior.
		tld := parts[len(parts)-1]
		if tld[0] < 'a' || tld[0] > 'z' || strings.HasPrefix(tld, "0x") {
			return "", false
		}
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	return u.String(), true
}

var nonPublicRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2002::/16"),
}

func blockedAddress(ip netip.Addr) bool {
	for _, prefix := range nonPublicRanges {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
