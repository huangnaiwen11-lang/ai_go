// generation-evidence-check 校验三项基础创作能力的离线证据清单。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"ai-business-service/internal/generationcontract"
)

const usage = "用法：generation-evidence-check --manifest <脱敏清单文件>\n"

// manifest 只保留可公开审核的模式、路由、分支和脱敏材料引用。
// 命令不读取原始请求、账号、任务或任何鉴权数据。
type manifest struct {
	Mode     generationcontract.Mode `json:"mode"`
	Route    string                  `json:"route"`
	Scope    string                  `json:"scope"`
	Evidence []evidence              `json:"evidence"`
}

type evidence struct {
	Kind        generationcontract.EvidenceKind `json:"kind"`
	ArtifactRef string                          `json:"artifactRef"`
}

type readFileFunc func(string) ([]byte, error)

func main() {
	os.Exit(run(os.Args[1:], os.ReadFile, os.Stdout, os.Stderr))
}

// run 使用统一错误文案，避免把材料引用、文件系统细节或解析结果写入终端日志。
func run(args []string, readFile readFileFunc, stdout, stderr io.Writer) int {
	manifestPath, ok := manifestPathFromArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return 2
	}

	content, err := readFile(manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, "无法读取证据清单。")
		return 2
	}

	parsed, err := decodeManifest(content)
	if err != nil || generationcontract.Validate(parsed.capture()) != nil {
		fmt.Fprintln(stderr, "证据清单无效。")
		return 1
	}

	fmt.Fprintln(stdout, "证据门禁通过：仅表示本地清单完整，不代表迁移或切流许可。")
	return 0
}

func manifestPathFromArgs(args []string) (string, bool) {
	if len(args) != 2 || args[0] != "--manifest" {
		return "", false
	}

	manifestPath := strings.TrimSpace(args[1])
	return manifestPath, manifestPath != ""
}

// decodeManifest 禁止未知字段和多段 JSON，避免拼写错误或拼接内容被静默忽略。
func decodeManifest(content []byte) (manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()

	var parsed manifest
	if err := decoder.Decode(&parsed); err != nil {
		return manifest{}, err
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return manifest{}, errors.New("multiple JSON values")
		}
		return manifest{}, err
	}

	return parsed, nil
}

func (value manifest) capture() generationcontract.Capture {
	evidenceList := make([]generationcontract.Evidence, 0, len(value.Evidence))
	for _, item := range value.Evidence {
		evidenceList = append(evidenceList, generationcontract.Evidence{
			Kind:        item.Kind,
			ArtifactRef: item.ArtifactRef,
		})
	}

	return generationcontract.Capture{
		Mode:     value.Mode,
		Route:    value.Route,
		Scope:    value.Scope,
		Evidence: evidenceList,
	}
}
