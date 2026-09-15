// static-contract-check 校验 Identity、Wallet/Billing 与 Admin 的本地静态合同清单。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"ai-business-service/internal/admincontract"
	"ai-business-service/internal/identitycontract"
	"ai-business-service/internal/walletcontract"
)

const usage = "用法：static-contract-check --stage <identity|wallet|admin> --manifest <清单文件>\n"

type readFileFunc func(string) ([]byte, error)

// routeWire 是 JSON 清单的传输结构。它只包含公开 method/path 与别名，不接收 token、
// 请求正文、支付回执或账户资料。
type routeWire struct {
	Method  string   `json:"method"`
	Path    string   `json:"path"`
	Aliases []string `json:"aliases"`
}

type logoutWire struct {
	ClearsLocalToken bool `json:"clearsLocalToken"`
	CallsServer      bool `json:"callsServer"`
}

type identityWire struct {
	Classification      string      `json:"classification"`
	MigrationPermission string      `json:"migrationPermission"`
	SourceScope         string      `json:"sourceScope"`
	GoRouteEnabled      bool        `json:"goRouteEnabled"`
	Routes              []routeWire `json:"routes"`
	Logout              logoutWire  `json:"logout"`
}

type balanceWire struct {
	Route             routeWire `json:"route"`
	MayCreateWallet   bool      `json:"mayCreateWallet"`
	CacheControl      string    `json:"cacheControl"`
	VaryAuthorization bool      `json:"varyAuthorization"`
	CacheStatus       bool      `json:"cacheStatus"`
	MayExposeStale    bool      `json:"mayExposeStale"`
}

type ledgerWire struct {
	Route              routeWire `json:"route"`
	DefaultLimit       int       `json:"defaultLimit"`
	MinimumLimit       int       `json:"minimumLimit"`
	MaximumSkip        int       `json:"maximumSkip"`
	CursorPrecedesSkip bool      `json:"cursorPrecedesSkip"`
}

type mutationWire struct {
	EntryPoint         string `json:"entryPoint"`
	ProductionTxn      bool   `json:"productionTxn"`
	PreventsNegative   bool   `json:"preventsNegative"`
	IdempotentReplay   bool   `json:"idempotentReplay"`
	PublishAfterCommit bool   `json:"publishAfterCommit"`
}

type paymentWire struct {
	Owner                      string      `json:"owner"`
	WebhookOwner               string      `json:"webhookOwner"`
	WaitForBackendReady        bool        `json:"waitForBackendReady"`
	ProxyRoutes                []routeWire `json:"proxyRoutes"`
	Callback                   routeWire   `json:"callback"`
	CallbackRequiresV2HMAC     bool        `json:"callbackRequiresV2HMAC"`
	CallbackRejectsNonceReplay bool        `json:"callbackRejectsNonceReplay"`
}

type walletWire struct {
	Classification      string       `json:"classification"`
	MigrationPermission string       `json:"migrationPermission"`
	SourceScope         string       `json:"sourceScope"`
	GoRouteEnabled      bool         `json:"goRouteEnabled"`
	Balance             balanceWire  `json:"balance"`
	Ledger              ledgerWire   `json:"ledger"`
	Mutation            mutationWire `json:"mutation"`
	Payment             paymentWire  `json:"payment"`
}

type accessWire struct {
	SuperAdminAllPaths               bool `json:"superAdminAllPaths"`
	LegacyAdminWithoutScopesAllPaths bool `json:"legacyAdminWithoutScopesAllPaths"`
	ScopedAdminRequiresPathScope     bool `json:"scopedAdminRequiresPathScope"`
}

type adminWire struct {
	Classification                        string     `json:"classification"`
	MigrationPermission                   string     `json:"migrationPermission"`
	SourceScope                           string     `json:"sourceScope"`
	VueShellEnabled                       bool       `json:"vueShellEnabled"`
	GoRouteEnabled                        bool       `json:"goRouteEnabled"`
	BrowserEntry                          string     `json:"browserEntry"`
	APIEntry                              string     `json:"apiEntry"`
	Access                                accessWire `json:"access"`
	AuditWriteFailureDoesNotBlockBusiness bool       `json:"auditWriteFailureDoesNotBlockBusiness"`
}

func main() {
	os.Exit(run(os.Args[1:], os.ReadFile, os.Stdout, os.Stderr))
}

// run 对外只输出固定成功或失败文本，避免把清单路径、未知字段及其可能包含的私有值
// 写入终端。该命令不读取其他文件，也不会触发 HTTP、数据库或生产环境访问。
func run(args []string, readFile readFileFunc, stdout, stderr io.Writer) int {
	stage, manifestPath, ok := parseArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return 2
	}

	content, err := readFile(manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, "无法读取静态合同清单。")
		return 2
	}
	if validateStageManifest(stage, content) != nil {
		fmt.Fprintln(stderr, "静态合同清单无效。")
		return 1
	}

	fmt.Fprintln(stdout, "静态合同门禁通过：仅表示本地源码基线一致，不代表迁移或切流许可。")
	return 0
}

func parseArgs(args []string) (stage string, manifestPath string, ok bool) {
	if len(args) != 4 {
		return "", "", false
	}

	values := make(map[string]string, 2)
	for index := 0; index < len(args); index += 2 {
		name := args[index]
		value := strings.TrimSpace(args[index+1])
		if (name != "--stage" && name != "--manifest") || value == "" {
			return "", "", false
		}
		if _, duplicated := values[name]; duplicated {
			return "", "", false
		}
		values[name] = value
	}

	stage = values["--stage"]
	manifestPath = values["--manifest"]
	if manifestPath == "" || (stage != "identity" && stage != "wallet" && stage != "admin") {
		return "", "", false
	}
	return stage, manifestPath, true
}

func validateStageManifest(stage string, content []byte) error {
	switch stage {
	case "identity":
		var wire identityWire
		if err := decodeOneDocument(content, &wire); err != nil {
			return err
		}
		return identitycontract.ValidateManifest(identityManifestFromWire(wire))
	case "wallet":
		var wire walletWire
		if err := decodeOneDocument(content, &wire); err != nil {
			return err
		}
		return walletcontract.ValidateManifest(walletManifestFromWire(wire))
	case "admin":
		var wire adminWire
		if err := decodeOneDocument(content, &wire); err != nil {
			return err
		}
		return admincontract.ValidateManifest(adminManifestFromWire(wire))
	default:
		return errors.New("unsupported stage")
	}
}

// decodeOneDocument 使用严格 JSON 解析，拒绝缺失字段、重复字段、未知字段与拼接
// 文档。特别是 false 这类安全开关必须在清单中显式出现，不能因为 Go 零值而被静默
// 当作已关闭。
func decodeOneDocument(content []byte, target any) error {
	if err := validateUniqueJSONDocument(content); err != nil {
		return err
	}

	var raw any
	if err := json.Unmarshal(content, &raw); err != nil {
		return err
	}
	if err := requireWireFields(raw, reflect.TypeOf(target)); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// validateUniqueJSONDocument 在绑定 wire struct 前逐 token 扫描 JSON，避免
// encoding/json 对同名字段“以后者覆盖前者”的默认行为使审核结论被悄然改写。
func validateUniqueJSONDocument(content []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// consumeJSONValue 递归消费一个 JSON 值，并在每个对象层级拒绝重复键。错误保持
// 通用，调用层不会把它直接输出，避免不可信清单内容进入终端。
func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}

	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			fieldToken, err := decoder.Token()
			if err != nil {
				return err
			}
			fieldName, ok := fieldToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicated := seen[fieldName]; duplicated {
				return errors.New("duplicate JSON object key")
			}
			seen[fieldName] = struct{}{}

			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		endToken, err := decoder.Token()
		if err != nil {
			return err
		}
		if endToken != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		endToken, err := decoder.Token()
		if err != nil {
			return err
		}
		if endToken != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
		return nil
	default:
		return errors.New("invalid JSON delimiter")
	}
}

// requireWireFields 根据 wire struct 的 JSON 标签逐层检查字段存在性。当前所有静态
// 合同字段都属于冻结基线，因此不允许 optional 或 omitempty：缺失与显式 false、0、
// 空数组有不同审计含义，必须在反序列化前区分。
func requireWireFields(value any, targetType reflect.Type) error {
	for targetType.Kind() == reflect.Pointer {
		targetType = targetType.Elem()
	}

	switch targetType.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return errors.New("invalid JSON object")
		}

		for index := 0; index < targetType.NumField(); index++ {
			field := targetType.Field(index)
			if field.PkgPath != "" {
				continue
			}

			fieldName, required := jsonFieldName(field)
			if !required {
				continue
			}
			fieldValue, exists := object[fieldName]
			if !exists || fieldValue == nil {
				return errors.New("required JSON field is missing")
			}
			if err := requireWireFields(fieldValue, field.Type); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		array, ok := value.([]any)
		if !ok {
			return errors.New("invalid JSON array")
		}
		for _, item := range array {
			if item == nil {
				return errors.New("invalid JSON array item")
			}
			if err := requireWireFields(item, targetType.Elem()); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

// jsonFieldName 只接受显式 JSON 标签，防止未来新增 wire 字段因默认命名规则而绕过
// 字段存在性校验。当前静态合同 wire 均为公开的传输结构，必须逐项声明标签。
func jsonFieldName(field reflect.StructField) (string, bool) {
	name := strings.Split(field.Tag.Get("json"), ",")[0]
	if name == "" || name == "-" {
		return "", false
	}
	return name, true
}

func identityManifestFromWire(wire identityWire) identitycontract.Manifest {
	routes := make([]identitycontract.Route, 0, len(wire.Routes))
	for _, route := range wire.Routes {
		routes = append(routes, identitycontract.Route{
			Method:  route.Method,
			Path:    route.Path,
			Aliases: append([]string(nil), route.Aliases...),
		})
	}
	return identitycontract.Manifest{
		Classification:      wire.Classification,
		MigrationPermission: wire.MigrationPermission,
		SourceScope:         wire.SourceScope,
		GoRouteEnabled:      wire.GoRouteEnabled,
		Routes:              routes,
		Logout: identitycontract.LogoutBaseline{
			ClearsLocalToken: wire.Logout.ClearsLocalToken,
			CallsServer:      wire.Logout.CallsServer,
		},
	}
}

func walletManifestFromWire(wire walletWire) walletcontract.Manifest {
	return walletcontract.Manifest{
		Classification:      wire.Classification,
		MigrationPermission: wire.MigrationPermission,
		SourceScope:         wire.SourceScope,
		GoRouteEnabled:      wire.GoRouteEnabled,
		Balance: walletcontract.BalanceBaseline{
			Route:             walletRouteFromWire(wire.Balance.Route),
			MayCreateWallet:   wire.Balance.MayCreateWallet,
			CacheControl:      wire.Balance.CacheControl,
			VaryAuthorization: wire.Balance.VaryAuthorization,
			CacheStatus:       wire.Balance.CacheStatus,
			MayExposeStale:    wire.Balance.MayExposeStale,
		},
		Ledger: walletcontract.LedgerBaseline{
			Route:              walletRouteFromWire(wire.Ledger.Route),
			DefaultLimit:       wire.Ledger.DefaultLimit,
			MinimumLimit:       wire.Ledger.MinimumLimit,
			MaximumSkip:        wire.Ledger.MaximumSkip,
			CursorPrecedesSkip: wire.Ledger.CursorPrecedesSkip,
		},
		Mutation: walletcontract.MutationBaseline{
			EntryPoint:         wire.Mutation.EntryPoint,
			ProductionTxn:      wire.Mutation.ProductionTxn,
			PreventsNegative:   wire.Mutation.PreventsNegative,
			IdempotentReplay:   wire.Mutation.IdempotentReplay,
			PublishAfterCommit: wire.Mutation.PublishAfterCommit,
		},
		Payment: walletcontract.PaymentBoundary{
			Owner:                      wire.Payment.Owner,
			WebhookOwner:               wire.Payment.WebhookOwner,
			WaitForBackendReady:        wire.Payment.WaitForBackendReady,
			ProxyRoutes:                walletRoutesFromWire(wire.Payment.ProxyRoutes),
			Callback:                   walletRouteFromWire(wire.Payment.Callback),
			CallbackRequiresV2HMAC:     wire.Payment.CallbackRequiresV2HMAC,
			CallbackRejectsNonceReplay: wire.Payment.CallbackRejectsNonceReplay,
		},
	}
}

func walletRouteFromWire(wire routeWire) walletcontract.Route {
	return walletcontract.Route{
		Method:  wire.Method,
		Path:    wire.Path,
		Aliases: append([]string(nil), wire.Aliases...),
	}
}

func walletRoutesFromWire(wires []routeWire) []walletcontract.Route {
	routes := make([]walletcontract.Route, 0, len(wires))
	for _, wire := range wires {
		routes = append(routes, walletRouteFromWire(wire))
	}
	return routes
}

func adminManifestFromWire(wire adminWire) admincontract.Manifest {
	return admincontract.Manifest{
		Classification:      wire.Classification,
		MigrationPermission: wire.MigrationPermission,
		SourceScope:         wire.SourceScope,
		VueShellEnabled:     wire.VueShellEnabled,
		GoRouteEnabled:      wire.GoRouteEnabled,
		BrowserEntry:        wire.BrowserEntry,
		APIEntry:            wire.APIEntry,
		Access: admincontract.AccessBaseline{
			SuperAdminAllPaths:               wire.Access.SuperAdminAllPaths,
			LegacyAdminWithoutScopesAllPaths: wire.Access.LegacyAdminWithoutScopesAllPaths,
			ScopedAdminRequiresPathScope:     wire.Access.ScopedAdminRequiresPathScope,
		},
		AuditWriteFailureDoesNotBlockBusiness: wire.AuditWriteFailureDoesNotBlockBusiness,
	}
}
