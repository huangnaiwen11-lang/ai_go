package executionv2

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCompile校验共享快照合同(t *testing.T) {
	validEdit := []byte(`{"prompt":"编辑人物","assets":[{"role":"source_image","url":"https://assets.example.test/source.png"}],"parameters":{"width":1024}}`)
	if _, err := Compile(CapabilityImageEdit, "ps-edit-v1", validEdit); err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	cases := []struct {
		name       string
		capability Capability
		sku        string
		input      string
		want       error
	}{
		{"旧编辑 SKU", CapabilityImageEdit, "ps-image-edit-v1", string(validEdit), ErrInvalidSnapshot},
		{"图像编辑缺素材", CapabilityImageEdit, "ps-edit-v1", `{"prompt":"编辑","assets":[],"parameters":{}}`, ErrInvalidSnapshot},
		{"文生图素材", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","assets":[{"role":"source_image","url":"https://assets.example.test/a.png"}],"parameters":{}}`, ErrInvalidSnapshot},
		{"图生视频缺首图", CapabilityImageToVideo, "ps-auto", `{"prompt":"动起来","assets":[{"role":"reference_image","url":"https://assets.example.test/a.png"}],"parameters":{}}`, ErrInvalidSnapshot},
		{"HTTP 素材", CapabilityImageEdit, "ps-edit-v1", `{"prompt":"编辑","assets":[{"role":"source_image","url":"http://assets.example.test/a.png"}],"parameters":{}}`, ErrInvalidSnapshot},
		{"空提示词", CapabilityTextToImage, "ps-image-v1", `{"prompt":"  ","assets":[],"parameters":{}}`, ErrInvalidSnapshot},
		{"未知参数", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","assets":[],"parameters":{"unknown":1}}`, ErrInvalidParameters},
		{"分隔符伪造顶层参数", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","assets":[],"parameters":{"w_i_d_t_h":1024}}`, ErrInvalidParameters},
		{"分隔符伪造嵌套参数", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","assets":[],"parameters":{"render":{"w.i.d.t.h":1024}}}`, ErrInvalidParameters},
		{"商业参数", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","assets":[],"parameters":{"price_cents":1}}`, ErrInvalidParameters},
		{"账本参数", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","assets":[],"parameters":{"diamond_balance":1}}`, ErrInvalidParameters},
		{"支付参数", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","assets":[],"parameters":{"payment_id":"x"}}`, ErrInvalidParameters},
		{"风控参数", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","assets":[],"parameters":{"risk_level":"high"}}`, ErrInvalidParameters},
		{"重复键", CapabilityTextToImage, "ps-image-v1", `{"prompt":"图","prompt":"另一个","assets":[],"parameters":{}}`, ErrInvalidSnapshot},
		{"素材业务字段", CapabilityImageEdit, "ps-edit-v1", `{"prompt":"编辑","assets":[{"role":"source_image","url":"https://assets.example.test/a.png","payment_id":"secret"}],"parameters":{}}`, ErrInvalidSnapshot},
		{"空提示词空值", CapabilityTextToImage, "ps-image-v1", `{"prompt":null,"assets":[],"parameters":{}}`, ErrInvalidSnapshot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.capability, tc.sku, []byte(tc.input))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Compile() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCompile拒绝超限深度和节点(t *testing.T) {
	tooLarge := []byte(`{"prompt":"` + strings.Repeat("x", maxRawInputBytes) + `","assets":[],"parameters":{}}`)
	if _, err := Compile(CapabilityTextToImage, "ps-image-v1", tooLarge); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("Compile(too large) error = %v", err)
	}

	deep := `"x"`
	for range maxJSONDepth + 1 {
		deep = `{"render":` + deep + `}`
	}
	deepInput := []byte(`{"prompt":"图","assets":[],"parameters":` + deep + `}`)
	if _, err := Compile(CapabilityTextToImage, "ps-image-v1", deepInput); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("Compile(deep) error = %v", err)
	}

	items := strings.Repeat(`{"width":1},`, maxJSONNodes)
	nodesInput := []byte(`{"prompt":"图","assets":[],"parameters":{"render":[` + items + `{"width":1}]}}`)
	if _, err := Compile(CapabilityTextToImage, "ps-image-v1", nodesInput); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("Compile(nodes) error = %v", err)
	}
}

func TestValidateJSON拒绝超过一MiB的合法JSON(t *testing.T) {
	raw := []byte(`{"padding":"` + strings.Repeat("x", 1<<20) + `"}`)
	if len(raw) <= 1<<20 {
		t.Fatalf("JSON length = %d, want > 1MiB", len(raw))
	}
	if err := ValidateJSON(raw); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("ValidateJSON() error = %v, want ErrInvalidSnapshot", err)
	}
}

func TestSnapshot冻结输入并生成独立投稿载荷(t *testing.T) {
	raw := []byte(`{"prompt":"图","assets":[],"parameters":{}}`)
	snapshot, err := Compile(CapabilityTextToImage, "ps-image-v1", raw)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	raw[2] = 'X'
	payload, err := snapshot.MarshalSubmissionPayload()
	if err != nil {
		t.Fatalf("MarshalSubmissionPayload() error = %v", err)
	}
	want := []byte(`{"capability":"text_to_image","model_sku":"ps-image-v1","input":{"prompt":"图","assets":[],"parameters":{}}}`)
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload = %s, want %s", payload, want)
	}
	payload[2] = 'X'
	again, err := snapshot.MarshalSubmissionPayload()
	if err != nil || !bytes.Equal(again, want) {
		t.Fatalf("second payload = %s, error = %v", again, err)
	}
	if snapshot.Digest == "" || strings.Contains(snapshot.Digest, "图") {
		t.Fatalf("Digest = %q, want stable opaque digest", snapshot.Digest)
	}
}

func TestCompile接受正式CamelCase技术参数(t *testing.T) {
	_, err := Compile(CapabilityTextToImage, "ps-image-v1", []byte(`{"prompt":"图","assets":[],"parameters":{"cfgScale":7,"aspectRatio":"1:1","render":{"frameCount":16},"durationSeconds":5}}`))
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
}
