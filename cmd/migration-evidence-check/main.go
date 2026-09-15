// migration-evidence-check 校验 Identity、Wallet/Billing 和 Admin 的离线迁移证据清单。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"ai-business-service/internal/migrationevidence"
)

const usage = "用法：migration-evidence-check --manifest <脱敏清单文件>\n"

// manifest 只保存阶段名、行为场景及不可逆材料引用；不允许原始运行数据或凭据字段。
type manifest struct {
	Stage    migrationevidence.Stage `json:"stage"`
	Evidence []evidence              `json:"evidence"`
}

type evidence struct {
	Kind        migrationevidence.EvidenceKind `json:"kind"`
	ArtifactRef string                         `json:"artifactRef"`
}

type readFileFunc func(string) ([]byte, error)

func main() {
	os.Exit(run(os.Args[1:], os.ReadFile, os.Stdout, os.Stderr))
}

// run 统一屏蔽解析失败原因，避免把文件路径、材料引用或敏感字段值带入终端日志。
func run(args []string, readFile readFileFunc, stdout, stderr io.Writer) int {
	manifestPath, ok := manifestPathFromArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return 2
	}

	content, err := readFile(manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, "无法读取迁移证据清单。")
		return 2
	}

	parsed, err := decodeManifest(content)
	if err != nil || migrationevidence.Validate(parsed.capture()) != nil {
		fmt.Fprintln(stderr, "迁移证据清单无效。")
		return 1
	}

	fmt.Fprintln(stdout, "迁移证据门禁通过：仅表示本地清单完整，不代表迁移或切流许可。")
	return 0
}

func manifestPathFromArgs(args []string) (string, bool) {
	if len(args) != 2 || args[0] != "--manifest" {
		return "", false
	}

	path := strings.TrimSpace(args[1])
	return path, path != ""
}

// decodeManifest 拒绝未知字段和多段 JSON，确保敏感字段不会被解析器静默忽略。
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

func (value manifest) capture() migrationevidence.Capture {
	evidenceList := make([]migrationevidence.Evidence, 0, len(value.Evidence))
	for _, item := range value.Evidence {
		evidenceList = append(evidenceList, migrationevidence.Evidence{
			Kind:        item.Kind,
			ArtifactRef: item.ArtifactRef,
		})
	}

	return migrationevidence.Capture{
		Stage:    value.Stage,
		Evidence: evidenceList,
	}
}
