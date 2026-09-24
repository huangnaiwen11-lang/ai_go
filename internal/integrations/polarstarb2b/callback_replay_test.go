package polarstarb2b

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func replayDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func TestParseVerifiedCallbackBody按摘要重放已持久化字节(t *testing.T) {
	body := callbackTestBody("completed", map[string]any{
		"output": map[string]any{"resultUrl": "https://cdn.polarstar.work/result/step-b2b-1.png"},
	})
	event, err := ParseVerifiedCallbackBody(callbackTestTenant, callbackTestAccount, callbackTestJobID, callbackTestDeliver, 2, replayDigest(body), []byte(body))
	if err != nil {
		t.Fatalf("ParseVerifiedCallbackBody() error = %v", err)
	}
	if event.Status != CallbackStatusCompleted || event.ResultURL != "https://cdn.polarstar.work/result/step-b2b-1.png" ||
		event.StepID != callbackTestStepID || event.JobID != callbackTestJobID || event.Capability != "text_to_image" || event.Attempt != 2 {
		t.Fatalf("重放结果 = %#v", event)
	}
}

// 重放路径不做验签，摘要因此是唯一的字节来源证明。缺失、不符、大写、长度不足
// 与字节被改写必须全部拒绝——放宽任何一条都等于接受一份来路不明的报文。
func TestParseVerifiedCallbackBody拒绝摘要不符的字节(t *testing.T) {
	body := callbackTestBody("failed", nil)
	digest := replayDigest(body)
	rewritten := strings.Replace(body, "failed", "cancelled", 1)
	cases := map[string]struct {
		digest string
		body   []byte
	}{
		"摘要缺失":   {"", []byte(body)},
		"摘要为空字节": {digest, nil},
		"摘要不符":   {strings.Repeat("a", 64), []byte(body)},
		"摘要大写":   {strings.ToUpper(digest), []byte(body)},
		"摘要长度不足": {digest[:63], []byte(body)},
		"字节被改写":  {digest, []byte(rewritten)},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseVerifiedCallbackBody(callbackTestTenant, callbackTestAccount, callbackTestJobID, callbackTestDeliver, 1, testCase.digest, testCase.body); !errors.Is(err, ErrInvalidCallback) {
				t.Fatalf("错误 = %v，期望 ErrInvalidCallback", err)
			}
		})
	}
}

// 摘要对得上只证明字节没被改过，不证明字节满足合同：结构校验必须照常执行。
func TestParseVerifiedCallbackBody摘要正确但结构不符时仍拒绝(t *testing.T) {
	body := `{"contractVersion":"b2b.callback.v2"}`
	if _, err := ParseVerifiedCallbackBody(callbackTestTenant, callbackTestAccount, callbackTestJobID, callbackTestDeliver, 1, replayDigest(body), []byte(body)); !errors.Is(err, ErrInvalidCallback) {
		t.Fatalf("错误 = %v，期望 ErrInvalidCallback", err)
	}
}

// 头部身份在重放时由持久化事实提供；报文自报的身份必须与它一致。
func TestParseVerifiedCallbackBody拒绝身份不符的报文(t *testing.T) {
	body := callbackTestBody("failed", nil)
	digest := replayDigest(body)
	for name, args := range map[string]struct{ tenant, account, jobID, deliveryID string }{
		"租户不符":  {"tenant-other", callbackTestAccount, callbackTestJobID, callbackTestDeliver},
		"任务号不符": {callbackTestTenant, callbackTestAccount, "job-other", callbackTestDeliver},
		"投递号不符": {callbackTestTenant, callbackTestAccount, callbackTestJobID, "delivery-other"},
		"租户为空":  {"", callbackTestAccount, callbackTestJobID, callbackTestDeliver},
		"账号为空":  {callbackTestTenant, "", callbackTestJobID, callbackTestDeliver},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseVerifiedCallbackBody(args.tenant, args.account, args.jobID, args.deliveryID, 1, digest, []byte(body)); !errors.Is(err, ErrInvalidCallback) {
				t.Fatalf("错误 = %v，期望 ErrInvalidCallback", err)
			}
		})
	}
}
