package video

import (
	"os"
	"testing"

	"ai-business-service/internal/testsupport"
)

// TestMain 防止 I2V 本地回归与其他包共享 Outbox 时发生租约竞争。
func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMongoIntegrationSuite(m))
}
