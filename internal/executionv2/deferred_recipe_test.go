package executionv2

import (
	"errors"
	"testing"
)

func TestDeferredImageToVideoRecipe只能绑定一次HTTPS首帧(t *testing.T) {
	recipe, err := CompileDeferredImageToVideo("ps-auto", []byte(`{"prompt":"动起来","parameters":{"durationSeconds":5}}`))
	if err != nil {
		t.Fatalf("CompileDeferredImageToVideo() error = %v", err)
	}
	if recipe.Digest == "" {
		t.Fatal("recipe digest must be populated")
	}

	snapshot, err := recipe.BindOpeningFrame("https://assets.example.test/frame.png")
	if err != nil {
		t.Fatalf("BindOpeningFrame() error = %v", err)
	}
	if snapshot.Capability != CapabilityImageToVideo || len(snapshot.Input.Assets) != 1 || snapshot.Input.Assets[0].Role != "opening_frame" {
		t.Fatal("绑定后的快照能力或首帧角色不符合预期")
	}
	if _, err := recipe.BindOpeningFrame("https://assets.example.test/another-frame.png"); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("second BindOpeningFrame() error = %v, want ErrInvalidSnapshot", err)
	}
}

func TestDeferredImageToVideoRecipe拒绝素材商业字段和不安全首帧(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "含素材", raw: `{"prompt":"动起来","assets":[],"parameters":{}}`},
		{name: "含商业参数", raw: `{"prompt":"动起来","parameters":{"payment_id":"payment-secret"}}`},
		{name: "含模板字段", raw: `{"prompt":"动起来","templateId":"template-secret","parameters":{}}`},
		{name: "缺少参数", raw: `{"prompt":"动起来"}`},
	}
	for _, testCase := range cases {
		if _, err := CompileDeferredImageToVideo("ps-auto", []byte(testCase.raw)); err == nil {
			t.Fatalf("%s 输入未被拒绝", testCase.name)
		}
	}
	recipe, err := CompileDeferredImageToVideo("ps-auto", []byte(`{"prompt":"动起来","parameters":{}}`))
	if err != nil {
		t.Fatalf("CompileDeferredImageToVideo() error = %v", err)
	}
	if _, err := recipe.BindOpeningFrame("http://assets.example.test/frame.png"); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("BindOpeningFrame(http) error = %v, want ErrInvalidSnapshot", err)
	}
}
