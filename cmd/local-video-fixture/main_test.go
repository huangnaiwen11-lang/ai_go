package main

import (
	"testing"
	"time"
)

func TestNewFixtureSet创建两套隔离视频联调身份(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	fixture := newFixtureSet(now)

	if fixture.Prepaid.User.ID == "" || fixture.VIP.User.ID == "" || fixture.Prepaid.User.ID == fixture.VIP.User.ID {
		t.Fatalf("测试用户标识无效：%q / %q", fixture.Prepaid.User.ID, fixture.VIP.User.ID)
	}
	if fixture.Prepaid.User.BindingState != "bound" || fixture.VIP.User.BindingState != "bound" {
		t.Fatalf("测试用户必须是已绑定状态：%q / %q", fixture.Prepaid.User.BindingState, fixture.VIP.User.BindingState)
	}
	if fixture.Prepaid.Account.DiamondBalance != 50 {
		t.Fatalf("预扣用户初始余额 = %d，want 50", fixture.Prepaid.Account.DiamondBalance)
	}
	if fixture.VIP.Account.DiamondBalance != 0 || fixture.VIP.Subscription == nil {
		t.Fatalf("VIP 用户余额/订阅不符合日免验证前提：%d / %#v", fixture.VIP.Account.DiamondBalance, fixture.VIP.Subscription)
	}
	if fixture.VIPVideoQuota.UserID != fixture.VIP.User.ID || fixture.VIPVideoQuota.QuotaKind != "vip_daily_video" || fixture.VIPVideoQuota.Limit != 3 || fixture.VIPVideoQuota.UsedCount != 0 {
		t.Fatalf("VIP 视频日免额度不符合预期：%#v", fixture.VIPVideoQuota)
	}
	if fixture.Template.ContentSurface != "sfw" || fixture.Template.TemplateID == "" {
		t.Fatalf("视频模板不是有效 SFW 模板：%#v", fixture.Template)
	}
	if !fixture.Prepaid.Session.ExpiresAt.After(now) || !fixture.VIP.Session.ExpiresAt.After(now) {
		t.Fatal("测试会话必须在初始化时仍有效")
	}
}

func TestLocalMongoConfig拒绝非本地连接(t *testing.T) {
	if _, err := localMongoConfig("mongodb://example.com:27017/?replicaSet=rs0"); err == nil {
		t.Fatal("远程 MongoDB 连接不应被本地联调命令接受")
	}
}
