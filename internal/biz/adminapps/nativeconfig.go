package adminapps

import (
	"fmt"
	"net/url"
	"strings"
)

// legalURLFields 与 Node `app.nativeConfigValidation.js` 的 LEGAL_URL_FIELDS 一致。
//
// 比 managedLegalFields 多一个 h5Url —— h5Url 属于 nativeConfig 内部字段，
// 不是托管发布的槽位。两张表不能合并。
var legalURLFields = []string{"h5Url", "privacyUrl", "termsUrl", "supportUrl", "deletionUrl", "aboutUrl"}

// iapCompositeKeys 与 Node `updateApp` 里判断「是否局部扩展补丁」用的键集一致。
var iapCompositeKeys = []string{"endpointMap", "crypto", "iap", "v2", "stateRules"}

// ValidateNativeConfig 复刻 Node `normalizeNativeConfig`：**只校验，不归一化**。
//
// Node 的实现也是原地校验后原样返回，所以这里不做任何值改写 ——
// 唯一例外是调用方随后会调 WithoutServerOwnedStateRules 去掉 stateRules。
//
// 显式 `null` 在 Node 里等价于 `{}`，这里也返回空 map。
func ValidateNativeConfig(value any) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	config, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: nativeConfig must be a JSON object", ErrInvalid)
	}
	if err := validateNativeConfigShape(config); err != nil {
		return nil, err
	}
	return config, nil
}

func validateNativeConfigShape(config map[string]any) error {
	if value, exists := config["v2"]; exists {
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%w: nativeConfig.v2 must be an array", ErrInvalid)
		}
	}
	for _, field := range []string{"endpointMap", "crypto", "stateRules"} {
		value, exists := config[field]
		if !exists {
			continue
		}
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%w: nativeConfig.%s must be a JSON object", ErrInvalid, field)
		}
	}
	if err := validateLegalConfig(config); err != nil {
		return err
	}
	if err := validateIAPConfig(config); err != nil {
		return err
	}
	return validateAdjustConfig(config)
}

func validateLegalConfig(config map[string]any) error {
	value, exists := config["legal"]
	if !exists {
		return nil
	}
	legal, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: nativeConfig.legal must be a JSON object", ErrInvalid)
	}
	for _, alias := range []string{"private", "users"} {
		if _, exists := legal[alias]; exists {
			return fmt.Errorf("%w: nativeConfig.legal must use privacyUrl/termsUrl, not private/users aliases", ErrInvalid)
		}
	}
	for _, field := range legalURLFields {
		if err := validateLegalURL(legal[field], field); err != nil {
			return err
		}
	}
	return nil
}

// validateLegalURL 只接受绝对 http(s) URL。
//
// 非字符串也要报错：Node 对非字符串走 `asTrimmedString` 得到空串再 `new URL(”)`，
// 抛出的同样是 400。放行会让 `legal.privacyUrl = 123` 静默落库。
func validateLegalURL(value any, field string) error {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("%w: nativeConfig.legal.%s must be a valid http(s) URL", ErrInvalid, field)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	parsed, err := url.Parse(text)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%w: nativeConfig.legal.%s must be a valid http(s) URL", ErrInvalid, field)
	}
	return nil
}

func validateIAPConfig(config map[string]any) error {
	value, exists := config["iap"]
	if !exists {
		return nil
	}
	iap, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: nativeConfig.iap must be a JSON object", ErrInvalid)
	}
	products, exists := iap["products"]
	if !exists {
		return nil
	}
	rows, ok := products.([]any)
	if !ok {
		return fmt.Errorf("%w: nativeConfig.iap.products must be an array", ErrInvalid)
	}
	for index, row := range rows {
		if _, ok := row.(map[string]any); !ok {
			return fmt.Errorf("%w: nativeConfig.iap.products[%d] must be a JSON object", ErrInvalid, index)
		}
	}
	return nil
}

func validateAdjustConfig(config map[string]any) error {
	value, exists := config["adjust"]
	if !exists {
		return nil
	}
	adjust, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: nativeConfig.adjust must be a JSON object", ErrInvalid)
	}
	if events, exists := adjust["events"]; exists {
		if _, ok := events.(map[string]any); !ok {
			return fmt.Errorf("%w: nativeConfig.adjust.events must be a JSON object", ErrInvalid)
		}
	}
	return nil
}

// AssertNativeConfigOwnership 复刻 Node `assertNativeConfigOwnership` 里**能就地判定**的部分。
//
// 有意收窄（fail-closed）：Node 先把显式配置合并到 `buildDefaultNativeConfig(app)` 之上再断言，
// 因此「客户端没给的字段」在 Node 那里一定有值。Go 侧不做服务端模板生成
// （那会拖入 pricing / nativeShell 两个配置域），于是只断言客户端**确实给了**的字段：
//
//   - `domain.apiUrl` 给了就必须等于 App 自身的 origin；
//   - legal URL 与 `v2` 里的 http(s) URL 必须落在合法主机白名单内；
//   - `iap.products` 给了就按平台校验 provider、必填项与标识唯一性。
//
// 两处刻意**不**复刻：
//   - `products_missing`（要求 iap.products 非空）：Node 那条规则靠默认模板补齐商品，
//     在合并后的配置上永远不可能触发，是死规则。这里跟着合并语义一起省掉；
//   - 合法主机白名单里由默认模板贡献的那几个 host：默认 legal URL 本来就派生自 App 自身
//     origin，所以省略后得到的是**更小**的白名单 —— 更严，不是更松。
func AssertNativeConfigOwnership(app Document, nativeConfig map[string]any) error {
	if len(nativeConfig) == 0 {
		return nil
	}
	expectedAPIURL := ResolveAPIURL(app)
	if domain, ok := nativeConfig["domain"].(map[string]any); ok {
		if raw, exists := domain["apiUrl"]; exists {
			configured, err := NormalizeAPIURL(stringValue(raw))
			if err != nil {
				return err
			}
			if configured != expectedAPIURL {
				return fmt.Errorf("%w: nativeConfig.domain.apiUrl does not match the target App", ErrInvalid)
			}
		}
	}

	hosts := allowedLegalHosts(app)
	if legal, ok := nativeConfig["legal"].(map[string]any); ok {
		for _, field := range legalURLFields {
			if err := assertAllowedLegalHost(legal[field], "nativeConfig.legal."+field, hosts); err != nil {
				return err
			}
		}
	}
	if rows, ok := nativeConfig["v2"].([]any); ok {
		for rowIndex, row := range rows {
			cells, ok := row.([]any)
			if !ok {
				continue
			}
			for columnIndex, cell := range cells {
				path := fmt.Sprintf("nativeConfig.v2[%d][%d]", rowIndex, columnIndex)
				text := strings.TrimSpace(stringValue(cell))
				if strings.HasPrefix(text, "//") {
					return fmt.Errorf("%w: %s must use an explicit http(s) scheme", ErrInvalid, path)
				}
				if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
					if err := assertAllowedLegalHost(cell, path, hosts); err != nil {
						return err
					}
				}
			}
		}
	}
	return assertIAPOwnership(app, nativeConfig)
}

// allowedLegalHosts 复刻 Node `allowedLegalHosts`，但**不含**默认模板贡献的 host（见函数上方说明）。
func allowedLegalHosts(app Document) map[string]struct{} {
	hosts := map[string]struct{}{}
	addHost(hosts, NormalizeWebDomain(FieldString(app, "domain")))
	addHost(hosts, hostOf(ResolveAPIURL(app)))
	for _, slot := range FieldObject(app, "legalPages") {
		entry, ok := slot.(map[string]any)
		if !ok {
			continue
		}
		addHost(hosts, hostOf(FieldString(entry, "publicUrl")))
	}
	return hosts
}

func addHost(hosts map[string]struct{}, host string) {
	if host != "" {
		hosts[host] = struct{}{}
	}
}

func hostOf(rawURL string) string {
	if strings.TrimSpace(rawURL) == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// assertAllowedLegalHost 允许精确命中或子域命中，与 Node 的
// `host === candidate || host.endsWith('.' + candidate)` 一致。
func assertAllowedLegalHost(value any, path string, hosts map[string]struct{}) error {
	host := hostOf(stringValue(value))
	if host == "" {
		return nil
	}
	for candidate := range hosts {
		if host == candidate || strings.HasSuffix(host, "."+candidate) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s points at a legal URL for another App", ErrInvalid, path)
}

func assertIAPOwnership(app Document, nativeConfig map[string]any) error {
	iap, ok := nativeConfig["iap"].(map[string]any)
	if !ok {
		return nil
	}
	rows, ok := iap["products"].([]any)
	if !ok || len(rows) == 0 {
		// 见 AssertNativeConfigOwnership 的说明：Node 的 products_missing 依赖默认模板，这里不复刻。
		return nil
	}
	expectedProvider := "apple"
	if IsAndroidApp(app) {
		expectedProvider = "google"
	}
	if strings.ToLower(FieldString(iap, "provider")) != expectedProvider {
		return fmt.Errorf("%w: nativeConfig.iap.provider does not match the target App", ErrInvalid)
	}

	productIDs := map[string]int{}
	storeProductIDs := map[string]int{}
	for index, row := range rows {
		product, ok := row.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: nativeConfig.iap.products[%d] must be a JSON object", ErrInvalid, index)
		}
		storeProductID := FieldString(product, "storeProductId")
		if IsAndroidApp(app) {
			if strings.ToLower(FieldString(product, "provider")) != "google" || productIdentifier(product) == "" || storeProductID == "" {
				return fmt.Errorf("%w: nativeConfig contains an invalid Google Play product", ErrInvalid)
			}
		} else if storeProductID == "" {
			return fmt.Errorf("%w: nativeConfig contains an invalid Apple product", ErrInvalid)
		}
		if err := claimUnique(productIDs, productIdentifier(product), index, "productId"); err != nil {
			return err
		}
		if err := claimUnique(storeProductIDs, storeProductID, index, "storeProductId"); err != nil {
			return err
		}
	}
	return nil
}

func productIdentifier(product map[string]any) string {
	if productID := FieldString(product, "productId"); productID != "" {
		return productID
	}
	return FieldString(product, "code")
}

func claimUnique(seen map[string]int, value string, index int, field string) error {
	if value == "" {
		return nil
	}
	if _, exists := seen[value]; exists {
		return fmt.Errorf("%w: nativeConfig IAP %s must be unique", ErrInvalid, field)
	}
	seen[value] = index
	return nil
}

// ApplyNativeConfigUpdate 复刻 Node `updateApp` 里的 `mergeNativeConfigUpdate`。
//
// 与 Node 的唯一差异：`isPartialExtensionPatch` 分支里 Node 的合并基底是
// `buildDefaultNativeConfig(app)`，这里退化成既有值 —— 同样是不做服务端模板生成的后果。
// 表现差异是「客户端提交局部补丁时，缺省字段不再被服务端模板补齐」，
// 而不是「客户端提交的值被改写」。
func ApplyNativeConfigUpdate(app, existing Document, raw any) (map[string]any, error) {
	incoming, err := ValidateNativeConfig(raw)
	if err != nil {
		return nil, err
	}
	existingNativeConfig := FieldObject(existing, "nativeConfig")
	includesStateRules := false
	if _, exists := incoming["stateRules"]; exists {
		includesStateRules = true
	}
	editable := WithoutServerOwnedStateRules(incoming)

	if len(editable) == 0 {
		// 只提交了 stateRules：保留既有配置不动（stateRules 由服务端管理）。
		if includesStateRules {
			return orEmptyObject(existingNativeConfig), nil
		}
		// 提交了空对象：既有配置非空时这是调用方写错了，不是「清空」。
		if len(existingNativeConfig) > 0 {
			return nil, fmt.Errorf("%w: nativeConfig must not be empty; omit it or use the generated template", ErrInvalid)
		}
		return orEmptyObject(existingNativeConfig), nil
	}

	merged := editable
	if isPartialExtensionPatch(incoming) {
		merged = MergeDeep(existingNativeConfig, editable)
	}
	preserved := PreserveManagedLegalURLs(existingNativeConfig, merged, FieldObject(existing, "legalPages"))
	if stateRules, exists := existingNativeConfig["stateRules"]; exists {
		preserved["stateRules"] = stateRules
	}
	return preserved, nil
}

// isPartialExtensionPatch 判断补丁是否只碰了「扩展字段」。
// 碰到 endpointMap / crypto / iap / v2 / stateRules 中任何一个都算整块替换。
func isPartialExtensionPatch(incoming map[string]any) bool {
	for key := range incoming {
		for _, composite := range iapCompositeKeys {
			if key == composite {
				return false
			}
		}
	}
	return true
}

func orEmptyObject(object map[string]any) map[string]any {
	if object == nil {
		return map[string]any{}
	}
	return object
}
