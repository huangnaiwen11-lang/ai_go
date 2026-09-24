package generation

import (
	"errors"
	"testing"
	"time"

	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const goldenMappingVersion = "polarstar.image.v1"

func goldenCatalog() bizgeneration.PublishedMappingCatalog {
	return bizgeneration.PublishedMappingCatalog{
		Version:       goldenMappingVersion,
		SourceVersion: "catalog-2026-09-18",
		PublishedAt:   time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
		Entries: []bizgeneration.ModelMappingEntry{
			{
				Capability: "text_to_image", ProductKey: "image", PublicModel: "ps-image-v1",
				AllowedInputs: []string{"prompt", "aspectRatio", "seed"},
				Sizes:         []bizgeneration.ModelMappingSize{{Width: 1024, Height: 1024}},
				AspectRatios:  []string{"1:1", "16:9"},
				Enabled:       true,
			},
			{
				Capability: "image_edit", ProductKey: "edit", PublicModel: "ps-edit-v1",
				AllowedInputs: []string{"prompt", "imageUrl"},
				Sizes:         []bizgeneration.ModelMappingSize{{Width: 1024, Height: 1024}},
				Templates:     []bizgeneration.ModelMappingTemplate{{Key: "portrait", ID: "tpl-portrait-1"}},
				Enabled:       true,
			},
		},
	}
}

func TestMappingSnapshotFromCatalogProducesGoldenRequest(t *testing.T) {
	snapshot, err := MappingSnapshotFromCatalog(goldenCatalog(), goldenMappingVersion)
	if err != nil {
		t.Fatalf("MappingSnapshotFromCatalog() error = %v", err)
	}
	if snapshot.Version != goldenMappingVersion || len(snapshot.Models) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, ok := snapshot.Models["edit"].Templates["portrait"]; !ok {
		t.Fatalf("模板键未投影: %#v", snapshot.Models["edit"].Templates)
	}

	route := polarstarb2b.Route{StepID: "step-1", Provider: "polarstar_b2b_v2", AccountRef: "account-a", ContractVersion: "b2b.job.v2", MappingVersion: goldenMappingVersion}
	product := polarstarb2b.ProductInput{
		Capability: "text_to_image", ModelKey: "image",
		Input: polarstarb2b.Input{Prompt: "海边的灯塔", AspectRatio: "1:1"},
	}
	request, err := polarstarb2b.MapRequest(snapshot, route, product, polarstarb2b.CallbackConfig{Mode: "lookup_only"})
	if err != nil {
		t.Fatalf("MapRequest() error = %v", err)
	}

	want := `{"externalId":"step-1","idempotencyKey":"cling-step:step-1","capability":"text_to_image","model":"ps-image-v1","input":{"prompt":"海边的灯塔","aspectRatio":"1:1"},"callbackUrl":null,"callbackPolicy":"disabled","resultUrlPolicy":"permanent"}`
	if got := string(request.Payload()); got != want {
		t.Fatalf("payload = %s\nwant %s", got, want)
	}
	// 投影出来的请求必须能通过适配器自己的恢复校验：否则 Worker 侧无法用同一份
	// 冻结字节重放，admission 与提交就会看到两份不同的请求。
	if _, err := polarstarb2b.RestoreRequest(route, request.Payload(), request.Digest()); err != nil {
		t.Fatalf("RestoreRequest() error = %v", err)
	}
}

func TestMappingSnapshotFromCatalogRejectsFrozenVersionMismatch(t *testing.T) {
	for name, version := range map[string]string{"空版本": "", "版本不一致": "polarstar.image.v2"} {
		version := version
		t.Run(name, func(t *testing.T) {
			if _, err := MappingSnapshotFromCatalog(goldenCatalog(), version); !errors.Is(err, ErrProviderRouteMismatch) {
				t.Fatalf("error = %v, want ErrProviderRouteMismatch", err)
			}
		})
	}
}

func TestMappingSnapshotFromCatalogFailsClosedWithoutEnabledEntries(t *testing.T) {
	catalog := goldenCatalog()
	for index := range catalog.Entries {
		catalog.Entries[index].Enabled = false
	}
	if _, err := MappingSnapshotFromCatalog(catalog, goldenMappingVersion); !errors.Is(err, ErrProviderMappingSnapshot) {
		t.Fatalf("error = %v, want ErrProviderMappingSnapshot", err)
	}

	broken := goldenCatalog()
	broken.Entries[0].PublicModel = ""
	if _, err := MappingSnapshotFromCatalog(broken, goldenMappingVersion); !errors.Is(err, ErrProviderMappingSnapshot) {
		t.Fatalf("error = %v, want ErrProviderMappingSnapshot", err)
	}
}
