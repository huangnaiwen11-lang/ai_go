// media-ticket-check 校验直传视频票据的离线合同。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"ai-business-service/internal/mediacontract"
)

const usage = "用法：media-ticket-check --manifest <脱敏票据文件>\n"

// manifest 只保留离线核对所需的公开票据字段。即使 UploadURL 带签名查询参数，
// 命令也不会发起请求，更不会把该字段写入标准输出或错误输出。
type manifest struct {
	Request presignRequest `json:"request"`
	Ticket  presignTicket  `json:"ticket"`
}

type presignRequest struct {
	Kind          string `json:"kind"`
	ContentType   string `json:"contentType"`
	FileSizeBytes int64  `json:"fileSizeBytes"`
}

type presignTicket struct {
	UploadURL   string      `json:"uploadURL"`
	PublicURL   string      `json:"publicURL"`
	Key         string      `json:"key"`
	Filename    string      `json:"filename"`
	ContentType string      `json:"contentType"`
	MaxBytes    int64       `json:"maxBytes"`
	Headers     http.Header `json:"headers"`
}

type readFileFunc func(string) ([]byte, error)

func main() {
	os.Exit(run(os.Args[1:], os.ReadFile, os.Stdout, os.Stderr))
}

// run 对所有无效票据使用统一错误，防止签名 URL、对象 key 或本地读取细节泄露。
func run(args []string, readFile readFileFunc, stdout, stderr io.Writer) int {
	manifestPath, ok := manifestPathFromArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return 2
	}

	content, err := readFile(manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, "无法读取媒体票据。")
		return 2
	}

	parsed, err := decodeManifest(content)
	if err != nil || mediacontract.ValidateDirectUploadTicket(parsed.request(), parsed.ticket()) != nil {
		fmt.Fprintln(stderr, "媒体票据无效。")
		return 1
	}

	fmt.Fprintln(stdout, "媒体票据门禁通过：仅表示本地票据字段完整，不代表上传或迁移许可。")
	return 0
}

func manifestPathFromArgs(args []string) (string, bool) {
	if len(args) != 2 || args[0] != "--manifest" {
		return "", false
	}

	manifestPath := strings.TrimSpace(args[1])
	return manifestPath, manifestPath != ""
}

// decodeManifest 拒绝未知字段和多段 JSON，避免字段拼写错误或拼接内容被静默忽略。
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

func (value manifest) request() mediacontract.PresignRequest {
	return mediacontract.PresignRequest{
		Kind:          value.Request.Kind,
		ContentType:   value.Request.ContentType,
		FileSizeBytes: value.Request.FileSizeBytes,
	}
}

func (value manifest) ticket() mediacontract.PresignTicket {
	return mediacontract.PresignTicket{
		UploadURL:   value.Ticket.UploadURL,
		PublicURL:   value.Ticket.PublicURL,
		Key:         value.Ticket.Key,
		Filename:    value.Ticket.Filename,
		ContentType: value.Ticket.ContentType,
		MaxBytes:    value.Ticket.MaxBytes,
		Headers:     value.Ticket.Headers,
	}
}
