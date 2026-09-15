package main

import (
	"os"
	"testing"

	"ai-business-service/internal/testsupport"
)

// TestMain 保护 Gateway 账号、支付等本地 Mongo 端到端测试的隔离性。
func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMongoIntegrationSuite(m))
}
