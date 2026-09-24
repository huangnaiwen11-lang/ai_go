package generation

import (
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
)

func mappingCatalogFixture() PublishedMappingCatalog {
	return PublishedMappingCatalog{
		Version:       "polarstar.image.v1",
		SourceVersion: "catalog-2026-09-18",
		PublishedAt:   time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
		Entries: []ModelMappingEntry{
			{
				Capability: string(creations.AtomTextToImage), ProductKey: "image", PublicModel: "ps-image-v1",
				AllowedInputs: []string{"prompt", "aspectRatio", "seed"},
				Sizes:         []ModelMappingSize{{Width: 1024, Height: 1024}},
				AspectRatios:  []string{"1:1", "16:9"},
				Enabled:       true,
			},
			{
				Capability: string(creations.AtomImageEdit), ProductKey: "edit", PublicModel: "ps-edit-v1",
				AllowedInputs: []string{"prompt", "imageUrl", "faceImageUrl"},
				Sizes:         []ModelMappingSize{{Width: 1024, Height: 1024}},
				Templates:     []ModelMappingTemplate{{Key: "portrait", ID: "tpl-portrait-1"}},
				Enabled:       true,
			},
		},
	}
}

func TestPublishedMappingCatalogNormalizeAcceptsPublishedVersion(t *testing.T) {
	catalog := mappingCatalogFixture()
	// 非 UTC 时刻必须被归一，否则同一版本在不同写入路径下会得到不同的 published_at。
	catalog.PublishedAt = time.Date(2026, 9, 18, 16, 0, 0, 0, time.FixedZone("CST", 8*3600))

	got, err := catalog.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got.Version != catalog.Version || got.SourceVersion != catalog.SourceVersion || len(got.Entries) != 2 {
		t.Fatalf("Normalize() = %#v", got)
	}
	if !got.PublishedAt.Equal(time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)) || got.PublishedAt.Location() != time.UTC {
		t.Fatalf("Normalize() published_at = %v", got.PublishedAt)
	}
}

func TestPublishedMappingCatalogNormalizeRejectsIncompleteVersions(t *testing.T) {
	cases := map[string]func(*PublishedMappingCatalog){
		"缺少版本号":  func(c *PublishedMappingCatalog) { c.Version = "" },
		"版本号含空白": func(c *PublishedMappingCatalog) { c.Version = " polarstar.image.v1" },
		"缺少来源版本": func(c *PublishedMappingCatalog) { c.SourceVersion = "" },
		"缺少发布时刻": func(c *PublishedMappingCatalog) { c.PublishedAt = time.Time{} },
		"没有条目":   func(c *PublishedMappingCatalog) { c.Entries = nil },
		"重复产品键": func(c *PublishedMappingCatalog) {
			c.Entries[1].ProductKey = c.Entries[0].ProductKey
		},
	}
	for name, mutate := range cases {
		mutate := mutate
		t.Run(name, func(t *testing.T) {
			catalog := mappingCatalogFixture()
			mutate(&catalog)
			if _, err := catalog.Normalize(); !errors.Is(err, ErrInvalidMappingCatalog) {
				t.Fatalf("Normalize() error = %v, want ErrInvalidMappingCatalog", err)
			}
		})
	}
}

func TestModelMappingEntryNormalizeRejectsUnsafeEntries(t *testing.T) {
	cases := map[string]func(*ModelMappingEntry){
		"未知能力":    func(e *ModelMappingEntry) { e.Capability = "text_to_video" },
		"空产品键":    func(e *ModelMappingEntry) { e.ProductKey = "" },
		"公开模型为空":  func(e *ModelMappingEntry) { e.PublicModel = "" },
		"公开模型含空白": func(e *ModelMappingEntry) { e.PublicModel = "ps-image-v1 " },
		"重复输入字段":  func(e *ModelMappingEntry) { e.AllowedInputs = []string{"prompt", "prompt"} },
		"输入字段含空白": func(e *ModelMappingEntry) { e.AllowedInputs = []string{"prompt detail"} },
		"重复比例":    func(e *ModelMappingEntry) { e.AspectRatios = []string{"1:1", "1:1"} },
		"非正尺寸":    func(e *ModelMappingEntry) { e.Sizes = []ModelMappingSize{{Width: 0, Height: 1024}} },
		"非正时长":    func(e *ModelMappingEntry) { e.Durations = []int{0} },
		"重复模板键": func(e *ModelMappingEntry) {
			e.Templates = []ModelMappingTemplate{{Key: "a", ID: "1"}, {Key: "a", ID: "2"}}
		},
		"模板 ID 为空": func(e *ModelMappingEntry) { e.Templates = []ModelMappingTemplate{{Key: "a", ID: ""}} },
	}
	for name, mutate := range cases {
		mutate := mutate
		t.Run(name, func(t *testing.T) {
			catalog := mappingCatalogFixture()
			mutate(&catalog.Entries[0])
			if _, err := catalog.Normalize(); !errors.Is(err, ErrInvalidMappingCatalog) {
				t.Fatalf("Normalize() error = %v, want ErrInvalidMappingCatalog", err)
			}
		})
	}
}

func TestPublishedMappingCatalogEntryLookup(t *testing.T) {
	catalog := mappingCatalogFixture()
	normalized, err := catalog.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	entry, ok := normalized.Entry("edit")
	if !ok || entry.PublicModel != "ps-edit-v1" || entry.Capability != string(creations.AtomImageEdit) {
		t.Fatalf("Entry(edit) = %#v, %t", entry, ok)
	}
	if _, ok := normalized.Entry("missing"); ok {
		t.Fatal("Entry(missing) 必须返回未找到")
	}
}

func TestPublishedMappingCatalogNormalizeDoesNotMutateInput(t *testing.T) {
	catalog := mappingCatalogFixture()
	inputs := append([]string(nil), catalog.Entries[0].AllowedInputs...)

	normalized, err := catalog.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	normalized.Entries[0].AllowedInputs[0] = "changed"
	normalized.Entries[0].Sizes[0].Width = 1

	if catalog.Entries[0].AllowedInputs[0] != inputs[0] {
		t.Fatalf("Normalize() 修改了入参 AllowedInputs: %#v", catalog.Entries[0].AllowedInputs)
	}
	if catalog.Entries[0].Sizes[0].Width != 1024 {
		t.Fatalf("Normalize() 修改了入参 Sizes: %#v", catalog.Entries[0].Sizes)
	}
}
