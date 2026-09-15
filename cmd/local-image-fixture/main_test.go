package main

import (
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/data/model"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestNewImageFixtureSet创建隔离的图片联调数据(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	fixture := newImageFixtureSet(now)

	if fixture.Prepaid.User.ID == "" || fixture.VIP.User.ID == "" || fixture.Prepaid.User.ID == fixture.VIP.User.ID {
		t.Fatalf("测试用户标识无效：%q / %q", fixture.Prepaid.User.ID, fixture.VIP.User.ID)
	}
	if fixture.Prepaid.User.BindingState != "bound" || fixture.VIP.User.BindingState != "bound" {
		t.Fatalf("测试用户必须是已绑定状态：%q / %q", fixture.Prepaid.User.BindingState, fixture.VIP.User.BindingState)
	}
	if fixture.Prepaid.Account.DiamondBalance != 20 {
		t.Fatalf("预扣用户初始余额 = %d，want 20", fixture.Prepaid.Account.DiamondBalance)
	}
	if fixture.VIP.Account.DiamondBalance != 0 || fixture.VIP.Subscription == nil {
		t.Fatalf("VIP 用户余额/订阅不符合日免验证前提：%d / %#v", fixture.VIP.Account.DiamondBalance, fixture.VIP.Subscription)
	}
	if fixture.VIPImageQuota.UserID != fixture.VIP.User.ID || fixture.VIPImageQuota.QuotaKind != "vip_daily_image" || fixture.VIPImageQuota.Limit != 10 || fixture.VIPImageQuota.UsedCount != 0 {
		t.Fatalf("VIP 图片日免额度不符合预期：%#v", fixture.VIPImageQuota)
	}
	if fixture.Freeform.TemplateID != "t2i-freeform" || !fixture.Freeform.Enabled || fixture.Freeform.ContentSurface != "sfw" {
		t.Fatalf("自由文生图配方不是有效 SFW 模板：%#v", fixture.Freeform)
	}
	if fixture.ImageEdit.TemplateID != "local-image-edit-dress-up" || !fixture.ImageEdit.Enabled || fixture.ImageEdit.ContentSurface != "sfw" {
		t.Fatalf("图编辑配方不是有效 SFW 模板：%#v", fixture.ImageEdit)
	}
	var editParameters bson.M
	if err := bson.Unmarshal(fixture.ImageEdit.Parameters, &editParameters); err != nil || editParameters["kind"] != "image_edit" {
		t.Fatalf("图编辑模板缺少服务端技术配方：%v / %#v", err, editParameters)
	}
	if !fixture.Prepaid.Session.ExpiresAt.After(now) || !fixture.VIP.Session.ExpiresAt.After(now) {
		t.Fatal("测试会话必须在初始化时仍有效")
	}
}

func TestLocalImageMongoConfig拒绝非本地连接(t *testing.T) {
	if _, err := localImageMongoConfig("mongodb://example.com:27017/?replicaSet=rs0"); err == nil {
		t.Fatal("远程 MongoDB 连接不应被本地图片联调命令接受")
	}
}

func TestPlanImageFixtureTemplateInserts复用兼容的固定配方(t *testing.T) {
	now := time.Date(2026, time.September, 9, 10, 0, 0, 0, time.UTC)
	fixture := newImageFixtureSet(now)

	insertions, err := planImageFixtureTemplateInserts(map[string]model.TemplateDocument{
		fixture.Freeform.TemplateID: fixture.Freeform,
	}, fixture)
	if err != nil {
		t.Fatalf("兼容的既有自由文生图配方不应阻断本地 fixture：%v", err)
	}
	if len(insertions) != 1 || insertions[0].TemplateID != fixture.ImageEdit.TemplateID {
		t.Fatalf("待写入模板 = %#v，want 仅包含 I2I 固定模板", insertions)
	}
}

func TestPlanImageFixtureTemplateInserts拒绝不兼容的固定配方(t *testing.T) {
	now := time.Date(2026, time.September, 9, 10, 0, 0, 0, time.UTC)
	fixture := newImageFixtureSet(now)
	conflicting := fixture.Freeform
	conflicting.Enabled = false

	_, err := planImageFixtureTemplateInserts(map[string]model.TemplateDocument{
		fixture.Freeform.TemplateID: conflicting,
	}, fixture)
	if err == nil || !strings.Contains(err.Error(), fixture.Freeform.TemplateID) {
		t.Fatalf("不兼容配方应按模板标识拒绝，got %v", err)
	}
}
