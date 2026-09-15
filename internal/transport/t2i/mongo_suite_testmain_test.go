package t2i

import (
	"os"
	"testing"

	"ai-business-service/internal/testsupport"
)

// TestMain 防止 T2I/I2I 测试的预扣和 Outbox 数据与其他包并发冲突。
func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMongoIntegrationSuite(m))
}
