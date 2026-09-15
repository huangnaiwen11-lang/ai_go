// Package catalogcontract 比较 Catalog HTTP 响应中对外可见的部分。
package catalogcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"strings"
)

// Response 是离线契约回放使用的已捕获 HTTP 响应数据。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Compare 报告 baseline 与 candidate 的首个公开契约差异。条件缓存的 304 只暴露
// ETag 及其必须为空的响应正文。
func Compare(baseline, candidate Response) error {
	return compareWithCacheContract(baseline, candidate, CacheContract{})
}

func compareWithCacheContract(baseline, candidate Response, contract CacheContract) error {
	if baseline.StatusCode != candidate.StatusCode {
		return fmt.Errorf("HTTP status differs")
	}
	if baseline.StatusCode == http.StatusNotModified {
		if err := compareNotModified(baseline, candidate); err != nil {
			return err
		}
		return compareCacheContract(baseline.Header, candidate.Header, contract)
	}
	if err := compareJSONBody(baseline.Body, candidate.Body); err != nil {
		return err
	}
	if !reflect.DeepEqual(headerValues(baseline.Header, "Content-Type"), headerValues(candidate.Header, "Content-Type")) {
		return fmt.Errorf("Content-Type differs")
	}
	if err := compareLocation(baseline.Header, candidate.Header); err != nil {
		return err
	}
	if !reflect.DeepEqual(headerValues(baseline.Header, "Cache-Control"), headerValues(candidate.Header, "Cache-Control")) {
		return fmt.Errorf("Cache-Control differs")
	}
	if !reflect.DeepEqual(varyTokens(baseline.Header), varyTokens(candidate.Header)) {
		return fmt.Errorf("Vary differs")
	}
	if err := compareRequestID(baseline.Header, candidate.Header); err != nil {
		return err
	}
	return compareCacheContract(baseline.Header, candidate.Header, contract)
}

func compareCacheContract(baseline, candidate http.Header, contract CacheContract) error {
	if err := compareDataStale(baseline, candidate); err != nil {
		return err
	}
	if err := compareCacheStatus(baseline, candidate, contract.RequireCacheStatus); err != nil {
		return err
	}
	return compareETagPolicy(baseline, candidate, contract.ETagPolicy)
}

// compareLocation 校验首个重定向目标。回放器不会自动跟随重定向，因此 Location
// 直接决定调用方后续行为：两侧都不存在时保持等价；存在时必须各自恰好一个且完全一致。
// 错误只报告字段类别，避免将目标地址写入审计输出。
func compareLocation(baseline, candidate http.Header) error {
	baselineValues := headerValues(baseline, "Location")
	candidateValues := headerValues(candidate, "Location")
	if len(baselineValues) == 0 && len(candidateValues) == 0 {
		return nil
	}
	if len(baselineValues) != 1 || len(candidateValues) != 1 {
		return fmt.Errorf("Location is missing or invalid")
	}
	if baselineValues[0] != candidateValues[0] {
		return fmt.Errorf("Location differs")
	}

	return nil
}

func compareDataStale(baseline, candidate http.Header) error {
	baselineValues := headerValues(baseline, "X-Data-Stale")
	candidateValues := headerValues(candidate, "X-Data-Stale")
	if (len(baselineValues) > 0) != (len(candidateValues) > 0) {
		return fmt.Errorf("X-Data-Stale presence differs")
	}
	if len(baselineValues) == 0 {
		return nil
	}
	if len(baselineValues) != 1 || baselineValues[0] != "1" || len(candidateValues) != 1 || candidateValues[0] != "1" {
		return fmt.Errorf("X-Data-Stale is missing or invalid")
	}

	return nil
}

func compareCacheStatus(baseline, candidate http.Header, required bool) error {
	baselineValues := headerValues(baseline, "X-Cache-Status")
	candidateValues := headerValues(candidate, "X-Cache-Status")
	if (len(baselineValues) > 0) != (len(candidateValues) > 0) {
		if required {
			return fmt.Errorf("X-Cache-Status is missing or invalid")
		}
		return fmt.Errorf("X-Cache-Status presence differs")
	}
	if required && (len(baselineValues) != 1 || baselineValues[0] == "" || len(candidateValues) != 1 || candidateValues[0] == "") {
		return fmt.Errorf("X-Cache-Status is missing or invalid")
	}

	return nil
}

func compareETagPolicy(baseline, candidate http.Header, policy ETagPolicy) error {
	switch policy {
	case ETagForbidden:
		if len(headerValues(baseline, "ETag")) != 0 || len(headerValues(candidate, "ETag")) != 0 {
			return fmt.Errorf("ETag must be absent")
		}
	case ETagRequired:
		baselineETag, baselineOK := singleHeaderValue(baseline, "ETag")
		candidateETag, candidateOK := singleHeaderValue(candidate, "ETag")
		if !baselineOK || baselineETag == "" || !candidateOK || candidateETag == "" {
			return fmt.Errorf("ETag is missing or invalid")
		}
	}

	return nil
}

func compareNotModified(baseline, candidate Response) error {
	if len(baseline.Body) != 0 || len(candidate.Body) != 0 {
		return fmt.Errorf("304 response body differs")
	}
	baselineETag, baselineOK := singleHeaderValue(baseline.Header, "ETag")
	candidateETag, candidateOK := singleHeaderValue(candidate.Header, "ETag")
	if !baselineOK || !candidateOK {
		return fmt.Errorf("304 ETag must appear exactly once")
	}
	if baselineETag != candidateETag {
		return fmt.Errorf("304 ETag differs")
	}

	return nil
}

func singleHeaderValue(header http.Header, name string) (string, bool) {
	values := headerValues(header, name)
	if len(values) != 1 {
		return "", false
	}

	return values[0], true
}

func compareJSONBody(baseline, candidate []byte) error {
	baselineValue, ok := decodeJSON(baseline)
	if !ok {
		return fmt.Errorf("baseline JSON body is invalid")
	}
	candidateValue, ok := decodeJSON(candidate)
	if !ok {
		return fmt.Errorf("candidate JSON body is invalid")
	}
	if !jsonValuesEqual(baselineValue, candidateValue) {
		return fmt.Errorf("JSON body differs")
	}

	return nil
}

func decodeJSON(body []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailingValue any
	if err := decoder.Decode(&trailingValue); err != io.EOF {
		return nil, false
	}

	return value, true
}

func jsonValuesEqual(baseline, candidate any) bool {
	switch baselineValue := baseline.(type) {
	case nil:
		return candidate == nil
	case bool:
		candidateValue, ok := candidate.(bool)
		return ok && baselineValue == candidateValue
	case string:
		candidateValue, ok := candidate.(string)
		return ok && baselineValue == candidateValue
	case json.Number:
		candidateValue, ok := candidate.(json.Number)
		if !ok {
			return false
		}
		baselineNumber, baselineOK := new(big.Rat).SetString(baselineValue.String())
		candidateNumber, candidateOK := new(big.Rat).SetString(candidateValue.String())
		return baselineOK && candidateOK && baselineNumber.Cmp(candidateNumber) == 0
	case []any:
		candidateValue, ok := candidate.([]any)
		if !ok || len(baselineValue) != len(candidateValue) {
			return false
		}
		for index := range baselineValue {
			if !jsonValuesEqual(baselineValue[index], candidateValue[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		candidateValue, ok := candidate.(map[string]any)
		if !ok || len(baselineValue) != len(candidateValue) {
			return false
		}
		for key, baselineItem := range baselineValue {
			candidateItem, ok := candidateValue[key]
			if !ok || !jsonValuesEqual(baselineItem, candidateItem) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func compareRequestID(baseline, candidate http.Header) error {
	baselineValues := headerValues(baseline, "X-Request-Id")
	if len(baselineValues) != 1 {
		return fmt.Errorf("baseline X-Request-Id must appear exactly once")
	}
	candidateValues := headerValues(candidate, "X-Request-Id")
	if len(candidateValues) != 1 {
		return fmt.Errorf("candidate X-Request-Id must appear exactly once")
	}
	if baselineValues[0] != candidateValues[0] {
		return fmt.Errorf("X-Request-Id differs")
	}

	return nil
}

func headerValues(header http.Header, name string) []string {
	var values []string
	for key, headerValues := range header {
		if strings.EqualFold(key, name) {
			values = append(values, headerValues...)
		}
	}
	return values
}

func varyTokens(header http.Header) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, value := range headerValues(header, "Vary") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				tokens[strings.ToLower(token)] = struct{}{}
			}
		}
	}

	return tokens
}
