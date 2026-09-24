#!/usr/bin/env bash
set -euo pipefail

# Admin 同源只读探针：默认打管理后台的 8081 入口，绝不打印 Cookie、Authorization
# 或响应体。认证态由调用方通过 ADMIN_COOKIE 提供（例如浏览器导出的 Cookie 头）；
# 未提供时，所有受保护路径预期返回 401。
BASE_URL="${ADMIN_BASE_URL:-http://127.0.0.1:8081/api}"
COOKIE="${ADMIN_COOKIE:-}"

endpoints=(
  "GET /admin/apps"
  "GET /admin/apps/overview"
  "GET /admin/apps/000000000000000000000000/package-config"
  "GET /admin/utm-links"
  "GET /admin/utm-links/sources?onlyEnabled=true"
  "GET /admin/analytics/utm-funnel?days=30"
  "GET /admin/analytics/utm-funnel/trend?days=30"
  "GET /admin/blog"
  "GET /admin/blog/stats"
  "GET /admin/config/pricing?environment=production"
  "GET /admin/wallet/packages"
  "GET /admin/wallet/vip-config"
  "GET /admin/wallet/subscription-strategy"
  "GET /admin/config/system/wallet.coinPackagesOverrides"
  "GET /admin/analytics/subscription/overview?days=30"
  "GET /admin/analytics/subscription/trends?days=30"
  "GET /admin/analytics/subscription/breakdown"
  "GET /admin/analytics/subscription/subscribers?page=1&pageSize=20"
  "GET /admin/image-review"
  "GET /admin/image-review/pending"
  "GET /admin/image-review/stats"
  "GET /admin/image-review/000000000000000000000000/log"
  "GET /admin/video-review"
  "GET /admin/video-review/pending"
  "GET /admin/video-review/stats"
  "GET /admin/video-review/template-options"
  "GET /admin/video-review/gpu-status"
  "GET /admin/video-review/today-by-gpu"
  "GET /admin/video-review/000000000000000000000000/log"
  "GET /admin/images"
  "GET /admin/images/provider-status"
  "GET /admin/images/today-by-gpu"
  "GET /admin/gpu-fleet/nodes"
  "GET /admin/generation/fleet"
  "GET /admin/generation/stats"
  "GET /admin/analytics/ga4/status"
  "GET /admin/analytics/ga4/overview"
  "GET /admin/analytics/ga4/pwa/summary"
)

printf 'method\tpath\thttp\tcode\n'
for spec in "${endpoints[@]}"; do
  method="${spec%% *}"
  path="${spec#* }"
  body_file="$(mktemp)"
  if [[ -n "$COOKIE" ]]; then
    status="$(curl -sS -o "$body_file" -w '%{http_code}' -X "$method" "${BASE_URL}${path}" -H "Cookie: $COOKIE")"
  else
    status="$(curl -sS -o "$body_file" -w '%{http_code}' -X "$method" "${BASE_URL}${path}")"
  fi
  # 只提取稳定错误码；不输出 body，避免意外泄漏响应中的用户信息或凭据。
  code="-"
  if command -v jq >/dev/null 2>&1; then
    code="$(jq -r '.code // "-"' < "$body_file" 2>/dev/null || printf '%s' '-')"
  fi
  printf '%s\t%s\t%s\t%s\n' "$method" "$path" "$status" "$code"
  rm -f "$body_file"
done
