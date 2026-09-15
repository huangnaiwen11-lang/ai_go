package migrate

import (
	"os"
	"testing"

	"ai-business-service/internal/testsupport"
)

// TestMain 让 schema 集成测试与其他共享 cling_main 的测试包互斥执行。
func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMongoIntegrationSuite(m))
}
