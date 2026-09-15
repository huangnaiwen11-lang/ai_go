#!/usr/bin/env bash
set -euo pipefail

# 统一本地门禁：所有命令都只使用当前源码和本地测试夹具，不读取生产配置。
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FRONTEND_DIR="${FRONTEND_DIR:-${ROOT_DIR}/../ai-frontend-service}"
GO_CACHE_DIR="${GOCACHE:-/tmp/ai-business-go-cache}"

cd "${ROOT_DIR}"
echo "[1/4] Go 全量编译"
GOCACHE="${GO_CACHE_DIR}" go test -run '^$' ./...

echo "[2/4] Go 本地合同测试"
# 集合和索引清单完整性不依赖 MongoDB，不能随集成测试一起跳过。
GOCACHE="${GO_CACHE_DIR}" go test ./internal/data/migrate -run '^TestFrozenSchemaCoversAllDeclarations$' -count=1
GOCACHE="${GO_CACHE_DIR}" go test ./internal/realtime -run '^Test' -count=1
GOCACHE="${GO_CACHE_DIR}" go test ./internal/integrations/generation -run '^TestLocalPlatformHandler' -count=1
GOCACHE="${GO_CACHE_DIR}" go test ./internal/integrations/appstore -run '^Test(IssueLocalReceipt|LocalVerifier)' -count=1
GOCACHE="${GO_CACHE_DIR}" go test ./internal/transport/authentry -run '^Test(LocalBindingVerifier|IssueLocalBindingCredential)' -count=1

echo "[3/4] 阶段 0 审计与迁移矩阵"
node --test scripts/phase0-audit.test.mjs
node scripts/phase0-audit.mjs --validate-matrix

if [[ -d "${FRONTEND_DIR}" ]]; then
  echo "[4/4] 新前端 Go-only 审计"
  # 根 tsconfig 只声明 references；直接 --noEmit 不会检查其中的应用源码。
  (cd "${FRONTEND_DIR}" && npm run verify:go-only && npx tsc -p tsconfig.app.json --noEmit && npx tsc -p tsconfig.node.json --noEmit)
else
  echo "[4/4] 未找到新前端目录，跳过前端检查：${FRONTEND_DIR}"
fi

echo "本地验收门禁通过"
