package polarstarb2b

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func mapperFixture() (MappingSnapshot, Route, ProductInput, CallbackConfig) {
	fields := []string{"prompt", "negativePrompt", "imageUrl", "faceImageUrl", "garmentImageUrl", "guideImageUrl", "additionalImageUrls", "referenceImageUrls", "durationSeconds", "aspectRatio", "enableAudio", "audioPrompt", "seed", "width", "height"}
	snapshot := MappingSnapshot{Version: "published-v1", Models: map[string]ModelMapping{
		"image":     {Capability: "text_to_image", Model: "ps-auto", AllowedInputs: fields, AspectRatios: []string{"1:1", "9:16"}, Sizes: []ImageSize{{1024, 1024}}},
		"edit":      {Capability: "image_edit", Model: "ps-auto", AllowedInputs: fields, AspectRatios: []string{"1:1"}, Templates: map[string]string{"portrait": "public-portrait"}},
		"video":     {Capability: "image_to_video", Model: "ps-rush-v1", AllowedInputs: fields, AspectRatios: []string{"9:16"}, Durations: []int{5, 10, 15}},
		"reference": {Capability: "image_to_video", Model: "ps-reference-v1", AllowedInputs: fields, Durations: []int{10}},
	}}
	route := Route{StepID: "step-1", Provider: "polarstar_b2b_v2", AccountRef: "account-a", ContractVersion: "b2b.job.v2", MappingVersion: snapshot.Version}
	return snapshot, route, ProductInput{Capability: "text_to_image", ModelKey: "image", Input: Input{Prompt: "海边的灯塔", AspectRatio: "1:1"}}, CallbackConfig{Mode: "lookup_only"}
}

func TestMapRequestGolden(t *testing.T) {
	for _, name := range []string{"text_to_image", "image_edit", "image_to_video", "reference"} {
		t.Run(name, func(t *testing.T) {
			s, r, p, cb := mapperFixture()
			switch name {
			case "image_edit":
				p = ProductInput{Capability: name, ModelKey: "edit", TemplateKey: "portrait", Input: Input{ImageURL: "https://assets.example.com/source.png"}}
			case "image_to_video":
				p = ProductInput{Capability: name, ModelKey: "video", Input: Input{Prompt: "浪花轻轻拍岸", ImageURL: "https://assets.example.com/frame.png", DurationSeconds: 5, AspectRatio: "9:16"}}
				cb = CallbackConfig{Mode: "webhook", URL: "https://callbacks.example.com/polarstar"}
			case "reference":
				on := true
				p = ProductInput{Capability: "image_to_video", ModelKey: "reference", Input: Input{Prompt: "走向灯塔", ImageURL: "https://assets.example.com/frame.png", ReferenceImageURLs: []string{"https://assets.example.com/b.png", "https://assets.example.com/a.png"}, DurationSeconds: 10, EnableAudio: &on}}
			}
			req, err := MapRequest(s, r, p, cb)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile("testdata/request_" + name + ".golden.json")
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, want); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(req.Payload(), compact.Bytes()) {
				t.Fatalf("payload mismatch:\n%s\nwant:\n%s", req.Payload(), compact.Bytes())
			}
			if req.ExternalID() != "step-1" || req.IdempotencyKey() != "cling-step:step-1" || req.Capability() != p.Capability {
				t.Fatal("identity mismatch")
			}
		})
	}
}

func TestRequestBusinessExternalID(t *testing.T) {
	s, r, p, c := mapperFixture()
	req, err := MapRequest(s, r, p, c)
	if err != nil {
		t.Fatal(err)
	}
	if req.ExternalID() != r.StepID {
		t.Fatalf("externalId must be stepID: got %q", req.ExternalID())
	}
	if req.IdempotencyKey() != "cling-step:"+r.StepID {
		t.Fatal("idempotencyKey missing stable prefix")
	}
}

func TestReferenceWithoutPrimaryImage(t *testing.T) {
	s, r, p, c := mapperFixture()
	on := true
	p = ProductInput{Capability: "image_to_video", ModelKey: "reference", Input: Input{Prompt: "walk", ReferenceImageURLs: []string{"https://assets.example.com/a.png"}, DurationSeconds: 10, EnableAudio: &on}}
	if _, err := MapRequest(s, r, p, c); err != nil {
		t.Fatal("public reference input does not require a separate imageUrl", err)
	}
}

func TestValidateDeferredImageToVideoProduct只放行未绑定首帧的第二阶段基础配方(t *testing.T) {
	snapshot, _, _, _ := mapperFixture()
	base := ProductInput{
		Capability: "image_to_video", ModelKey: "video",
		Input: Input{Prompt: "浪花轻轻拍岸", DurationSeconds: 10, AspectRatio: "9:16"},
	}
	if err := ValidateDeferredImageToVideoProduct(snapshot, base); err != nil {
		t.Fatalf("unbound deferred product rejected: %v", err)
	}
	if err := ValidateProduct(snapshot, base); err == nil {
		t.Fatal("regular product validation accepted an image-less video")
	}

	for name, mutate := range map[string]func(*ProductInput){
		"wrong capability": func(product *ProductInput) { product.Capability = "text_to_image" },
		"input image url":  func(product *ProductInput) { product.Input.ImageURL = "https://assets.example.com/frame.png" },
		"source asset": func(product *ProductInput) {
			product.Assets = []Asset{{Role: "source_image", URL: "https://assets.example.com/frame.png"}}
		},
		"opening asset": func(product *ProductInput) {
			product.Assets = []Asset{{Role: "opening_frame", URL: "https://assets.example.com/frame.png"}}
		},
		"missing duration": func(product *ProductInput) { product.Input.DurationSeconds = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Input = base.Input
			candidate.Assets = append([]Asset(nil), base.Assets...)
			mutate(&candidate)
			if err := ValidateDeferredImageToVideoProduct(snapshot, candidate); err == nil {
				t.Fatal("invalid deferred product accepted")
			}
		})
	}
}

func TestMapRequestInvalidInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MappingSnapshot, *Route, *ProductInput, *CallbackConfig)
	}{
		{"empty step", func(_ *MappingSnapshot, r *Route, _ *ProductInput, _ *CallbackConfig) { r.StepID = "" }},
		{"long key", func(_ *MappingSnapshot, r *Route, _ *ProductInput, _ *CallbackConfig) {
			r.StepID = strings.Repeat("x", 190)
		}},
		{"missing account", func(_ *MappingSnapshot, r *Route, _ *ProductInput, _ *CallbackConfig) { r.AccountRef = "" }},
		{"wrong provider", func(_ *MappingSnapshot, r *Route, _ *ProductInput, _ *CallbackConfig) {
			r.Provider = "local_execution_v2"
		}},
		{"mapping version mismatch", func(_ *MappingSnapshot, r *Route, _ *ProductInput, _ *CallbackConfig) {
			r.MappingVersion = "unpublished"
		}},
		{"arbitrary sku", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			p.ModelKey = "ps-private-model"
		}},
		{"capability mismatch", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) { p.Capability = "image_edit" }},
		{"composite video", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) { p.Capability = "text_to_video" }},
		{"empty prompt", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) { p.Input.Prompt = "  " }},
		{"prompt too long", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			p.Input.Prompt = strings.Repeat("中", 8001)
		}},
		{"negative too long", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			p.Input.NegativePrompt = strings.Repeat("中", 8001)
		}},
		{"invalid utf8", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			p.Input.Prompt = string([]byte{0xff})
		}},
		{"negative seed", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			v := int64(-1)
			p.Input.Seed = &v
		}},
		{"unsafe seed", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			v := int64(9007199254740992)
			p.Input.Seed = &v
		}},
		{"size mismatch", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			p.Input.Width = 1024
			p.Input.Height = 512
		}},
		{"missing height", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) { p.Input.Width = 1024 }},
		{"unsupported ratio", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) { p.Input.AspectRatio = "3:7" }},
		{"video option on image", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) { p.Input.DurationSeconds = 5 }},
		{"source on text", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			p.Input.ImageURL = "https://assets.example.com/x"
		}},
		{"unknown template", func(_ *MappingSnapshot, _ *Route, p *ProductInput, _ *CallbackConfig) {
			p.TemplateKey = "private-workflow"
		}},
		{"field outside model allowlist", func(s *MappingSnapshot, _ *Route, _ *ProductInput, _ *CallbackConfig) {
			m := s.Models["image"]
			m.AllowedInputs = []string{"prompt"}
			s.Models["image"] = m
		}},
		{"private model in mapping", func(s *MappingSnapshot, _ *Route, _ *ProductInput, _ *CallbackConfig) {
			m := s.Models["image"]
			m.Model = "gpu-model"
			s.Models["image"] = m
		}},
		{"private field in mapping", func(s *MappingSnapshot, _ *Route, _ *ProductInput, _ *CallbackConfig) {
			m := s.Models["image"]
			m.AllowedInputs = append(m.AllowedInputs, "workflow")
			s.Models["image"] = m
		}},
		{"callback disabled with url", func(_ *MappingSnapshot, _ *Route, _ *ProductInput, c *CallbackConfig) {
			c.URL = "https://callbacks.example.com/x"
		}},
		{"webhook missing url", func(_ *MappingSnapshot, _ *Route, _ *ProductInput, c *CallbackConfig) { c.Mode = "webhook" }},
		{"unknown callback mode", func(_ *MappingSnapshot, _ *Route, _ *ProductInput, c *CallbackConfig) { c.Mode = "auto" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, r, p, c := mapperFixture()
			tc.mutate(&s, &r, &p, &c)
			if _, err := MapRequest(s, r, p, c); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestMapRequestImageURLValidation(t *testing.T) {
	bad := []string{"http://assets.example.com/a", "https://u:p@assets.example.com/a", "https://localhost/a", "https://127.0.0.1/a", "https://10.0.0.1/a", "https://172.16.0.1/a", "https://192.168.0.1/a", "https://169.254.169.254/a", "https://[::1]/a", "https://[::ffff:127.0.0.1]/a", "https://[fc00::1]/a", "https://service.internal/a", "https://localhost./a", "https://2130706433/a", "https://127.1/a", "https://assets.example.com/a#fragment", " https://assets.example.com/a"}
	for _, u := range bad {
		t.Run(u, func(t *testing.T) {
			s, r, p, c := mapperFixture()
			p.Capability = "image_edit"
			p.ModelKey = "edit"
			p.Input = Input{Prompt: "edit", ImageURL: u}
			if _, err := MapRequest(s, r, p, c); err == nil {
				t.Fatal("unsafe URL accepted")
			}
		})
	}
}

func TestReferenceRestrictions(t *testing.T) {
	for _, name := range []string{"missing references", "three references", "duplicate references", "equivalent duplicate references", "template", "aspect ratio", "additional image", "duration", "audio missing", "audio disabled", "other model"} {
		t.Run(name, func(t *testing.T) {
			s, r, p, c := mapperFixture()
			on := true
			p = ProductInput{Capability: "image_to_video", ModelKey: "reference", Input: Input{Prompt: "walk", ImageURL: "https://assets.example.com/frame.png", ReferenceImageURLs: []string{"https://assets.example.com/a.png"}, DurationSeconds: 10, EnableAudio: &on}}
			switch name {
			case "missing references":
				p.Input.ReferenceImageURLs = nil
			case "three references":
				p.Input.ReferenceImageURLs = []string{"https://assets.example.com/a", "https://assets.example.com/b", "https://assets.example.com/c"}
			case "duplicate references":
				p.Input.ReferenceImageURLs = append(p.Input.ReferenceImageURLs, p.Input.ReferenceImageURLs[0])
			case "equivalent duplicate references":
				p.Input.ReferenceImageURLs = append(p.Input.ReferenceImageURLs, "https://ASSETS.example.com:443/a.png")
			case "template":
				m := s.Models["reference"]
				m.Templates = map[string]string{"x": "public-x"}
				s.Models["reference"] = m
				p.TemplateKey = "x"
			case "aspect ratio":
				p.Input.AspectRatio = "9:16"
			case "additional image":
				p.Input.AdditionalImageURLs = []string{"https://assets.example.com/x"}
			case "duration":
				p.Input.DurationSeconds = 5
			case "audio missing":
				p.Input.EnableAudio = nil
			case "audio disabled":
				on = false
			case "other model":
				p.ModelKey = "video"
			}
			if _, err := MapRequest(s, r, p, c); err == nil {
				t.Fatal("invalid reference request accepted")
			}
		})
	}
}

func TestFrozenRequestOwnershipAndPromptBoundary(t *testing.T) {
	s, r, p, c := mapperFixture()
	p.Input.Prompt = strings.Repeat("中", 8000)
	seed := int64(9007199254740991)
	p.Input.Seed = &seed
	req, err := MapRequest(s, r, p, c)
	if err != nil {
		t.Fatal(err)
	}
	before := req.Payload()
	digest := req.Digest()
	returned := req.Payload()
	returned[0] = '!'
	seed = 1
	p.Input.Prompt = "changed"
	delete(s.Models, "image")
	if !bytes.Equal(before, req.Payload()) || req.Digest() != digest || req.Validate() != nil {
		t.Fatal("frozen request mutated")
	}
	sum := sha256.Sum256(before)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("digest does not match exact wire bytes")
	}
	if (Request{}).Validate() == nil {
		t.Fatal("empty request valid")
	}
}

func TestMapAssetRoles(t *testing.T) {
	s, r, p, c := mapperFixture()
	p = ProductInput{Capability: "image_edit", ModelKey: "edit", Input: Input{Prompt: "edit"}, Assets: []Asset{{Role: "source_image", URL: "https://assets.example.com/source.png"}, {Role: "face_image", URL: "https://assets.example.com/face.png"}, {Role: "garment_image", URL: "https://assets.example.com/garment.png"}, {Role: "guide_image", URL: "https://assets.example.com/guide.png"}, {Role: "additional_image", URL: "https://assets.example.com/extra.png"}}}
	req, err := MapRequest(s, r, p, c)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Input Input }
	if err := json.Unmarshal(req.Payload(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Input.ImageURL != p.Assets[0].URL || body.Input.FaceImageURL != p.Assets[1].URL || body.Input.GarmentImageURL != p.Assets[2].URL || body.Input.GuideImageURL != p.Assets[3].URL || len(body.Input.AdditionalImageURLs) != 1 || body.Input.AdditionalImageURLs[0] != p.Assets[4].URL {
		t.Fatal("asset role mismatch")
	}
	for _, name := range []string{"private role", "duplicate primary", "conflicting input url", "invalid role url", "edit opening frame"} {
		t.Run(name, func(t *testing.T) {
			q := p
			q.Assets = append([]Asset(nil), p.Assets...)
			switch name {
			case "private role":
				q.Assets = append(q.Assets, Asset{Role: "workflow", URL: "https://assets.example.com/x"})
			case "duplicate primary":
				q.Assets = append(q.Assets, q.Assets[0])
			case "conflicting input url":
				q.Input.ImageURL = "https://assets.example.com/x"
			case "invalid role url":
				q.Assets[0].URL = "https://127.0.0.1/x"
			case "edit opening frame":
				q.Assets[0].Role = "opening_frame"
			}
			if _, err := MapRequest(s, r, q, c); err == nil {
				t.Fatal("invalid asset role accepted")
			}
		})
	}
	// Go's two-step video may use its owned opening frame. Reference order is retained.
	on := true
	p = ProductInput{Capability: "image_to_video", ModelKey: "reference", Input: Input{Prompt: "walk", DurationSeconds: 10, EnableAudio: &on}, Assets: []Asset{{Role: "opening_frame", URL: "https://assets.example.com/frame.png"}, {Role: "reference_image", URL: "https://assets.example.com/b.png"}, {Role: "reference_image", URL: "https://assets.example.com/a.png"}}}
	req, err = MapRequest(s, r, p, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(req.Payload(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input.ReferenceImageURLs) != 2 || body.Input.ReferenceImageURLs[0] != p.Assets[1].URL || body.Input.ReferenceImageURLs[1] != p.Assets[2].URL {
		t.Fatal("ordered references changed")
	}
}

func BenchmarkMapRequest(b *testing.B) {
	s, r, p, c := mapperFixture()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := MapRequest(s, r, p, c); err != nil {
			b.Fatal(err)
		}
	}
}
