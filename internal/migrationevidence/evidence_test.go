package migrationevidence

import (
	"errors"
	"testing"
)

func TestValidate接受三个后续阶段的完整脱敏证据(t *testing.T) {
	for _, stage := range []Stage{Identity, WalletBilling, Admin} {
		t.Run(string(stage), func(t *testing.T) {
			capture := validCapture(stage)

			if err := Validate(capture); err != nil {
				t.Fatalf("Validate(%s) error = %v", stage, err)
			}
		})
	}
}

func TestValidate接受受控审计记录号(t *testing.T) {
	capture := validCapture(Admin)
	capture.Evidence[0].ArtifactRef = "audit:123e4567-e89b-12d3-a456-426614174000"

	if err := Validate(capture); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidate拒绝敏感材料值且不回显原文(t *testing.T) {
	capture := validCapture(Identity)
	capture.Evidence[0].ArtifactRef = "Bearer private-token"

	err := Validate(capture)
	if !errors.Is(err, ErrInvalidArtifactRef) {
		t.Fatalf("error = %v, want ErrInvalidArtifactRef", err)
	}
	if err != nil && err.Error() == "Bearer private-token" {
		t.Fatalf("error must not reveal sensitive reference: %q", err)
	}
}

func TestValidate拒绝缺失重复或未知的证据场景(t *testing.T) {
	t.Run("缺失", func(t *testing.T) {
		capture := validCapture(WalletBilling)
		capture.Evidence = capture.Evidence[:len(capture.Evidence)-1]

		if err := Validate(capture); !errors.Is(err, ErrEvidenceIncomplete) {
			t.Fatalf("error = %v, want ErrEvidenceIncomplete", err)
		}
	})

	for _, testCase := range []struct {
		name     string
		evidence Evidence
	}{
		{
			name:     "重复",
			evidence: Evidence{Kind: AdminRoutePermission, ArtifactRef: validArtifactRef},
		},
		{
			name:     "未知",
			evidence: Evidence{Kind: EvidenceKind("legacy_animate"), ArtifactRef: validArtifactRef},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			capture := validCapture(Admin)
			capture.Evidence = append(capture.Evidence, testCase.evidence)

			if err := Validate(capture); !errors.Is(err, ErrInvalidEvidence) {
				t.Fatalf("error = %v, want ErrInvalidEvidence", err)
			}
		})
	}
}

const validArtifactRef = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func validCapture(stage Stage) Capture {
	kinds, ok := RequiredEvidenceKinds(stage)
	if !ok {
		panic("test fixture stage is invalid")
	}

	evidence := make([]Evidence, 0, len(kinds))
	for _, kind := range kinds {
		evidence = append(evidence, Evidence{Kind: kind, ArtifactRef: validArtifactRef})
	}

	return Capture{Stage: stage, Evidence: evidence}
}
