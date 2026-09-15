package generationcontract

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate接受三种基础能力的完整脱敏证据(t *testing.T) {
	for _, testCase := range []Capture{
		validCapture(TextToImage, "POST /api/chat/image/async", "inputImages=absent"),
		validCapture(ImageToImage, "POST /api/chat/image/async", "inputImages=1-2"),
		validCapture(ImageToVideo, "POST /api/chat/video", "imageUrl=present"),
	} {
		if err := Validate(testCase); err != nil {
			t.Fatalf("Validate(%s) error = %v", testCase.Mode, err)
		}
	}
}

func TestValidate拒绝Animate和其他非基础能力(t *testing.T) {
	err := Validate(validCapture(Mode("animate"), "POST /api/animate/start", "legacy"))
	if !errors.Is(err, ErrUnsupportedMode) {
		t.Fatalf("error = %v, want ErrUnsupportedMode", err)
	}
}

func TestValidate拒绝缺少关键证据且不泄露引用内容(t *testing.T) {
	capture := validCapture(TextToImage, "POST /api/chat/image/async", "inputImages=absent")
	capture.Evidence = capture.Evidence[:len(capture.Evidence)-1]
	capture.Evidence[0].ArtifactRef = "private-capture-reference"

	err := Validate(capture)
	if !errors.Is(err, ErrEvidenceIncomplete) {
		t.Fatalf("error = %v, want ErrEvidenceIncomplete", err)
	}
	if strings.Contains(err.Error(), "private-capture-reference") {
		t.Fatalf("error must not reveal evidence reference: %q", err)
	}
}

func TestValidate拒绝不匹配的产品入口与分支(t *testing.T) {
	testCases := []Capture{
		validCapture(TextToImage, "POST /api/chat/video", "inputImages=absent"),
		validCapture(ImageToImage, "POST /api/chat/image/async", "inputImages=absent"),
		validCapture(ImageToVideo, "POST /api/chat/video", "prompt-only"),
	}

	for _, capture := range testCases {
		err := Validate(capture)
		if !errors.Is(err, ErrInvalidPublicContract) {
			t.Fatalf("Validate(%s) error = %v, want ErrInvalidPublicContract", capture.Mode, err)
		}
	}
}

func TestValidate拒绝重复或未知证据类型(t *testing.T) {
	testCases := []Evidence{
		{Kind: Authentication, ArtifactRef: "sha256:duplicate"},
		{Kind: EvidenceKind("legacy_animate"), ArtifactRef: "sha256:unknown"},
	}

	for _, evidence := range testCases {
		capture := validCapture(TextToImage, "POST /api/chat/image/async", "inputImages=absent")
		capture.Evidence = append(capture.Evidence, evidence)

		err := Validate(capture)
		if !errors.Is(err, ErrInvalidEvidence) {
			t.Fatalf("Validate(%s) error = %v, want ErrInvalidEvidence", evidence.Kind, err)
		}
	}
}

func validCapture(mode Mode, route, scope string) Capture {
	evidence := make([]Evidence, 0, len(requiredEvidenceKinds))
	for _, kind := range requiredEvidenceKinds {
		evidence = append(evidence, Evidence{
			Kind:        kind,
			ArtifactRef: "sha256:local-contract-fixture",
		})
	}

	return Capture{
		Mode:     mode,
		Route:    route,
		Scope:    scope,
		Evidence: evidence,
	}
}
