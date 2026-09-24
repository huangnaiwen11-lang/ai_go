package main

import (
	"bytes"
	"testing"

	"ai-business-service/internal/biz/adminpricing"
	"ai-business-service/internal/biz/adminreview"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// seedUserIDs 固定三个 id，避免测试依赖库里的真实用户。
func seedUserIDs() []string {
	return []string{"seed-user-1", "seed-user-2", "seed-user-3"}
}

func TestRunRejectsInvalidOptionsBeforeConnecting(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "未知 samples 取值", args: []string{"--samples=whatever"}},
		{name: "legacy database 等于 target", args: []string{"--legacy-database=cling_main"}},
		{name: "legacy database 为空", args: []string{"--legacy-database="}},
		{name: "非本机 mongo uri", args: []string{"--mongo-uri=mongodb://example.com:27017/?replicaSet=rs0"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(testCase.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code = %d, want 2（stderr=%q）", code, stderr.String())
			}
		})
	}
}

// TestCleanLegacySeedsAreImportable 把种子文档喂给**生产导入器**的分类函数，
// 断言干净种子的每一条都会被判为 ready —— 否则导入会被 orphan/conflict 挡在门外。
func TestCleanLegacySeedsAreImportable(t *testing.T) {
	documents := legacyReviewDocuments(seedUserIDs(), false)
	total := 0
	for source, docs := range documents {
		if len(docs) == 0 {
			t.Fatalf("source %s has no seed documents", source)
		}
		for _, document := range docs {
			outcome := adminreview.MapLegacyDocument(source, document)
			if outcome.Classification != adminreview.ImportReady {
				t.Fatalf("%s %v classified as %s（%s）", source, document["_id"], outcome.Classification, outcome.Reason)
			}
			if outcome.Record.LegacySourceID == "" || outcome.Record.OutputRef == "" || outcome.Record.UserID == "" {
				t.Fatalf("%s %v produced an incomplete record: %+v", source, document["_id"], outcome.Record)
			}
			total++
		}
	}
	if total != 11 {
		t.Fatalf("clean seed total = %d, want 11", total)
	}
}

// TestSampleSeedsExposeOrphanAndConflict 断言 with-samples 追加的三条畸形文档
// 确实被归类为 2 orphan + 1 conflict —— 这是 dry-run 对账要演示的东西。
func TestSampleSeedsExposeOrphanAndConflict(t *testing.T) {
	documents := legacyReviewDocuments(seedUserIDs(), true)
	counts := map[adminreview.ImportClassification]int{}
	for source, docs := range documents {
		for _, document := range docs {
			counts[adminreview.MapLegacyDocument(source, document).Classification]++
		}
	}
	if counts[adminreview.ImportReady] != 11 {
		t.Fatalf("ready = %d, want 11", counts[adminreview.ImportReady])
	}
	if counts[adminreview.ImportOrphan] != 2 {
		t.Fatalf("orphan = %d, want 2", counts[adminreview.ImportOrphan])
	}
	if counts[adminreview.ImportConflict] != 1 {
		t.Fatalf("conflict = %d, want 1", counts[adminreview.ImportConflict])
	}
}

// TestCleanModeRemovesExactlyTheNonReadyFixtureSamples keeps a clean rerun
// usable after somebody has deliberately run with-samples.  The removal list
// must stay limited to fixture-owned document IDs: no collection-wide cleanup
// is permitted here.
func TestCleanModeRemovesExactlyTheNonReadyFixtureSamples(t *testing.T) {
	documents := legacyReviewDocuments(seedUserIDs(), true)
	want := map[string]map[bson.ObjectID]struct{}{}
	for source, rows := range documents {
		for _, document := range rows {
			if adminreview.MapLegacyDocument(source, document).Classification == adminreview.ImportReady {
				continue
			}
			id, ok := document["_id"].(bson.ObjectID)
			if !ok {
				t.Fatalf("non-ready fixture %s has _id %T, want bson.ObjectID", source, document["_id"])
			}
			if want[source] == nil {
				want[source] = map[bson.ObjectID]struct{}{}
			}
			want[source][id] = struct{}{}
		}
	}

	got := fixtureSampleCleanupIDs()
	if len(got) != len(want) {
		t.Fatalf("cleanup source count = %d, want %d; got=%v", len(got), len(want), got)
	}
	var count int
	for source, ids := range got {
		if len(ids) == 0 {
			t.Fatalf("cleanup source %s has no IDs", source)
		}
		for _, id := range ids {
			count++
			if _, ok := want[source][id]; !ok {
				t.Fatalf("cleanup unexpectedly targets %s/%s", source, id.Hex())
			}
			delete(want[source], id)
		}
	}
	if count != 3 {
		t.Fatalf("cleanup target count = %d, want 3", count)
	}
	for source, ids := range want {
		if len(ids) != 0 {
			t.Fatalf("cleanup misses non-ready fixture IDs in %s: %v", source, ids)
		}
	}
}

// TestEverySeedDocumentHasID 保证每条种子都能被幂等 upsert 命中。
func TestEverySeedDocumentHasID(t *testing.T) {
	batches := map[string][]bson.M{
		"admin_pricing_configs": pricingConfigDocuments(),
		"apps":                  appDocuments(),
		"utmlinks":              utmLinkDocuments(),
	}
	for source, documents := range legacyReviewDocuments(seedUserIDs(), true) {
		batches[source] = documents
	}
	for name, documents := range batches {
		if len(documents) == 0 {
			t.Fatalf("%s has no seed documents", name)
		}
		seen := map[any]struct{}{}
		for _, document := range documents {
			id, ok := document["_id"]
			if !ok || id == nil || id == "" {
				t.Fatalf("%s seed document without usable _id: %v", name, document)
			}
			if _, duplicated := seen[id]; duplicated {
				t.Fatalf("%s has duplicated _id %v", name, id)
			}
			seen[id] = struct{}{}
		}
	}
}

// TestUTMSeedIDsAreObjectIDs UTM 的更新/删除会把 id 按 hex 解析，
// 用字符串 _id 会让列表能读但写不了。
func TestUTMSeedIDsAreObjectIDs(t *testing.T) {
	for _, document := range utmLinkDocuments() {
		objectID, ok := document["_id"].(bson.ObjectID)
		if !ok {
			t.Fatalf("utm seed _id is %T, want bson.ObjectID", document["_id"])
		}
		if objectID.IsZero() {
			t.Fatal("utm seed _id must not be the nil ObjectID")
		}
	}
}

// TestPricingSeedOverridesDriveEffectiveCatalog 用生产定价逻辑回放种子覆盖值，
// 断言「改价 / 停用 / 自定义套餐」三件事都真的生效 —— 页面读到的将是这份目录。
func TestPricingSeedOverridesDriveEffectiveCatalog(t *testing.T) {
	var coinDocument bson.M
	for _, document := range pricingConfigDocuments() {
		if document["key"] == adminpricing.CoinPackagesKey {
			coinDocument = document
		}
	}
	if coinDocument == nil {
		t.Fatal("seed is missing the coin packages override document")
	}
	overrides, ok := coinDocument["value"].(map[string]any)
	if !ok {
		t.Fatalf("coin overrides value is %T, want bson.M", coinDocument["value"])
	}
	packages, err := adminpricing.EffectiveCoinPackages(overrides)
	if err != nil {
		t.Fatalf("EffectiveCoinPackages: %v", err)
	}
	byID := map[string]adminpricing.Package{}
	for _, item := range packages {
		byID[item.ID] = item
	}
	if pkg := byID["coins_200"]; pkg.Enabled {
		t.Fatalf("coins_200 must be disabled by the seed override: %+v", pkg)
	}
	if pkg := byID["coins_1000"]; pkg.PriceInCents != 899 || pkg.BonusCoins != 350 {
		t.Fatalf("coins_1000 override not applied: %+v", pkg)
	}
	custom, exists := byID["coins_seed_777"]
	if !exists {
		t.Fatal("custom seed package coins_seed_777 is missing from the effective catalog")
	}
	if !custom.Custom || custom.Builtin || custom.Coins != 7777 || custom.BonusCoins != 777 {
		t.Fatalf("custom seed package is not effective: %+v", custom)
	}
}

// TestAppSeedFieldsMatchAdminContract 断言 apps 种子带齐列表/概览需要的字段。
func TestAppSeedFieldsMatchAdminContract(t *testing.T) {
	statuses := map[string]int{}
	for _, document := range appDocuments() {
		for _, key := range []string{"name", "platform", "status", "stats", "createdAt", "updatedAt"} {
			if _, ok := document[key]; !ok {
				t.Fatalf("app seed %v is missing %q", document["_id"], key)
			}
		}
		statuses[document["status"].(string)]++
	}
	if statuses["active"] != 2 || statuses["suspended"] != 1 {
		t.Fatalf("app seed statuses = %v, want 2 active + 1 suspended", statuses)
	}
}
