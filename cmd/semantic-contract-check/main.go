// Command semantic-contract-check 校验主站重构前冻结的业务语义案例。
//
// 案例只保存脱敏后的输入、结果和状态摘要，避免把真实用户数据、凭证或签名带入
// 重构仓库。后续的 Go API 与集成测试必须引用这些案例，而不是自行解释业务规则。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

var sensitiveFieldFragments = []string{
	"authorization",
	"cookie",
	"password",
	"receipt",
	"secret",
	"signature",
	"token",
}

// SemanticCase 记录一条可回放的、面向用户可观察的业务事实。
// DatabaseBefore 和 DatabaseAfter 只允许保存计数、状态和值摘要，不能保存原始业务数据。
type SemanticCase struct {
	CaseID   string          `json:"case_id"`
	Consumer Consumer        `json:"consumer"`
	Actor    Actor           `json:"actor"`
	Template Template        `json:"template"`
	Fixture  json.RawMessage `json:"fixture"`
	Expected Expected        `json:"expected"`
	Source   Source          `json:"source"`
}

// Consumer 标识某个真实客户端会使用的精确 HTTP 入口。
type Consumer struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Actor 仅表达影响业务决策的用户状态，不承载用户身份信息。
type Actor struct {
	AccountStatus string `json:"account_status"`
	BindingState  string `json:"binding_state"`
	Reviewed      bool   `json:"reviewed"`
	UserTimezone  string `json:"user_timezone"`
}

// Template 固定任务或目录读取时使用的模板版本。
// 支付、会话等不关联模板的案例统一使用 not_applicable 哨兵，避免字段被随意遗漏。
type Template struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	ContentSurface string `json:"content_surface"`
	ProductMode    string `json:"product_mode"`
}

// Expected 同时固定 HTTP、持久化状态和外部副作用，防止重构只比较响应字段。
type Expected struct {
	HTTP           HTTPExpectation `json:"http"`
	DatabaseBefore map[string]any  `json:"database_before"`
	DatabaseAfter  map[string]any  `json:"database_after"`
	SideEffects    []string        `json:"side_effects"`
	TechnicalCalls []TechnicalCall `json:"technical_calls"`
}

// HTTPExpectation 描述客户端可见的状态与业务错误码。
type HTTPExpectation struct {
	Status    int    `json:"status"`
	ErrorCode string `json:"error_code"`
}

// TechnicalCall 是主站到生成中台的脱敏调用摘要。
// 它只记录可验证的协议事实，绝不记录素材、提示词、签名和任何财务字段。
type TechnicalCall struct {
	Atom            string `json:"atom"`
	CallbackPurpose string `json:"callback_purpose"`
	ProtocolVersion string `json:"protocol_version"`
	IdempotencyKey  string `json:"idempotency_key"`
}

// Source 说明案例来自已经确认的业务基线，还是隔离的本地 Node 观察。
type Source struct {
	Kind string `json:"kind"`
}

func main() {
	casesPath := flag.String("cases", "", "业务语义案例 JSON 文件路径")
	flag.Parse()

	if strings.TrimSpace(*casesPath) == "" {
		fail(errors.New("missing --cases"))
	}

	raw, err := os.ReadFile(*casesPath)
	if err != nil {
		fail(fmt.Errorf("read cases: %w", err))
	}

	count, err := ValidateCases(raw)
	if err != nil {
		fail(err)
	}

	fmt.Printf("semantic cases valid: %d\n", count)
}

// ValidateCases 校验案例集合并返回有效案例数量。
func ValidateCases(raw []byte) (int, error) {
	var cases []json.RawMessage
	if err := decodeStrict(raw, &cases); err != nil {
		return 0, fmt.Errorf("decode cases: %w", err)
	}
	if len(cases) == 0 {
		return 0, errors.New("cases must not be empty")
	}

	seen := make(map[string]struct{}, len(cases))
	for index, item := range cases {
		if err := ValidateCase(item); err != nil {
			return 0, fmt.Errorf("case %d: %w", index, err)
		}

		var semanticCase SemanticCase
		if err := decodeStrict(item, &semanticCase); err != nil {
			return 0, fmt.Errorf("case %d: decode validated case: %w", index, err)
		}
		if _, exists := seen[semanticCase.CaseID]; exists {
			return 0, fmt.Errorf("case %d: duplicate case_id %q", index, semanticCase.CaseID)
		}
		seen[semanticCase.CaseID] = struct{}{}
	}

	return len(cases), nil
}

// ValidateCase 校验单条案例是否足以作为重构语义门禁。
func ValidateCase(raw []byte) error {
	var semanticCase SemanticCase
	if err := decodeStrict(raw, &semanticCase); err != nil {
		return fmt.Errorf("decode case: %w", err)
	}
	if err := rejectSensitiveFields(raw); err != nil {
		return err
	}

	if strings.TrimSpace(semanticCase.CaseID) == "" {
		return errors.New("missing case_id")
	}
	if strings.TrimSpace(semanticCase.Consumer.Method) == "" || strings.TrimSpace(semanticCase.Consumer.Path) == "" {
		return errors.New("missing consumer method or path")
	}
	if !strings.HasPrefix(semanticCase.Consumer.Path, "/") {
		return errors.New("consumer path must start with /")
	}
	if strings.TrimSpace(semanticCase.Actor.AccountStatus) == "" || strings.TrimSpace(semanticCase.Actor.BindingState) == "" {
		return errors.New("missing actor account_status or binding_state")
	}
	if semanticCase.Expected.HTTP.Status < 100 || semanticCase.Expected.HTTP.Status > 599 {
		return errors.New("expected http status must be between 100 and 599")
	}
	if semanticCase.Expected.DatabaseAfter == nil || len(semanticCase.Expected.DatabaseAfter) == 0 {
		return errors.New("missing database_after")
	}
	if semanticCase.Expected.DatabaseBefore == nil || len(semanticCase.Expected.DatabaseBefore) == 0 {
		return errors.New("missing database_before")
	}
	if semanticCase.Expected.SideEffects == nil {
		return errors.New("missing side_effects")
	}
	if !validSourceKind(semanticCase.Source.Kind) {
		return fmt.Errorf("invalid source kind %q", semanticCase.Source.Kind)
	}
	if err := validateTemplate(semanticCase.Template); err != nil {
		return err
	}
	if err := validateTechnicalCalls(semanticCase.Expected.TechnicalCalls); err != nil {
		return err
	}

	return nil
}

func validateTemplate(template Template) error {
	if strings.TrimSpace(template.ID) == "" || strings.TrimSpace(template.Version) == "" {
		return errors.New("missing template id or version")
	}
	if strings.TrimSpace(template.ContentSurface) == "" || strings.TrimSpace(template.ProductMode) == "" {
		return errors.New("missing template content_surface or product_mode")
	}
	return nil
}

func validateTechnicalCalls(calls []TechnicalCall) error {
	if calls == nil {
		return errors.New("missing technical_calls")
	}
	for index, call := range calls {
		if !validAtom(call.Atom) {
			return fmt.Errorf("technical_calls[%d]: invalid atom %q", index, call.Atom)
		}
		if strings.TrimSpace(call.CallbackPurpose) == "" || strings.TrimSpace(call.ProtocolVersion) == "" || strings.TrimSpace(call.IdempotencyKey) == "" {
			return fmt.Errorf("technical_calls[%d]: missing callback or idempotency facts", index)
		}
	}
	return nil
}

func validAtom(atom string) bool {
	return atom == "text_to_image" || atom == "image_edit" || atom == "image_to_video"
}

func decodeStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("multiple JSON documents are not allowed")
	}
	return nil
}

func validSourceKind(kind string) bool {
	return kind == "confirmed_business_baseline" || kind == "isolated_local_node_observation"
}

func rejectSensitiveFields(raw []byte) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("decode sensitive-field scan: %w", err)
	}

	paths := make([]string, 0, 1)
	collectSensitiveFieldPaths(value, "", &paths)
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	return fmt.Errorf("sensitive field is not allowed: %s", strings.Join(paths, ", "))
}

func collectSensitiveFieldPaths(value any, parent string, paths *[]string) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			path := key
			if parent != "" {
				path = parent + "." + key
			}
			if isSensitiveField(key) {
				*paths = append(*paths, path)
				continue
			}
			collectSensitiveFieldPaths(child, path, paths)
		}
	case []any:
		for index, child := range current {
			collectSensitiveFieldPaths(child, fmt.Sprintf("%s[%d]", parent, index), paths)
		}
	}
}

func isSensitiveField(field string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(field, "_", ""))
	for _, fragment := range sensitiveFieldFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err.Error())
	os.Exit(1)
}
