package data

import (
	"os"
	"testing"

	"ai-business-service/internal/testsupport"
)

// TestMain 串行化共享 cling_main 的跨包 Mongo 集成测试，防止 Outbox 租约互相干扰。
func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMongoIntegrationSuite(m))
}
