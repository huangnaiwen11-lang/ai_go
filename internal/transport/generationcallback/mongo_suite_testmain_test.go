package generationcallback

import (
	"os"
	"testing"

	"ai-business-service/internal/testsupport"
)

// TestMain 防止回调测试的 Outbox 事件被并发测试包的通用领取用例占用。
func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMongoIntegrationSuite(m))
}
