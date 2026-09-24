#!/usr/bin/env bash
# 本地四链路验收：只使用 local_execution_v2 合同模拟器、测试 fixture 和隔离 Mongo。
# 不读取生产配置，不发真实 PolarStar 请求，也不依赖 R2。
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "${script_dir}/.."

command -v go >/dev/null || { echo '缺少 go' >&2; exit 127; }

go_cache_dir="${GOCACHE:-/tmp/ai-business-go-cache}"

echo '[1/3] fixture 结构与本地 execution.v2/mock 合同'
GOCACHE="${go_cache_dir}" go test -count=1 \
  ./cmd/local-image-fixture \
  ./cmd/local-video-fixture
GOCACHE="${go_cache_dir}" go test -count=1 \
  ./internal/integrations/generation -run '^TestLocalPlatformHandler'

echo '[2/3] 文生图、图片编辑、图生视频、两步文生视频 Mongo E2E（含失败冲正/审核幂等）'
# test-isolated-mongo.sh 只启动一次性 mongo:7 rs0；若 Docker 不可用，脚本应
# 原样失败并由调用方区分为环境阻断，而不是改用开发者已有数据库。
bash "${script_dir}/test-isolated-mongo.sh" \
  ./internal/transport/t2i \
  ./internal/transport/video \
  -run 'TestHandler(本地端到端创建预扣投递回调和状态读取|模板图编辑本地端到端闭环|视频模板单图I2V本地端到端闭环|视频模板文本首帧到I2V本地端到端闭环|文生图失败回调自动冲正|模板图编辑明确拒绝自动冲正|模板图编辑审核没收不退款|视频模板I2V失败回调自动冲正|视频模板I2V审核没收不退款)$' \
  -count=1

echo '本地四链路验收通过（仅 local_execution_v2/mock；未访问 PolarStar/R2）'
