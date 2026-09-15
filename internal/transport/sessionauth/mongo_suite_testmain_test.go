package sessionauth

import (
	"os"
	"testing"

	"ai-business-service/internal/testsupport"
)

// TestMain 保护共享 cling_main 的会话集成测试。
func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMongoIntegrationSuite(m))
}
