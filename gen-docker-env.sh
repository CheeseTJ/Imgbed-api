#!/usr/bin/env bash
# 从 ./.env.sh（shell 脚本格式，会 source /home/ubuntu/db/.env）生成 Docker 用的
# .env.docker（纯 KEY=VALUE，无引号、无 export）。
#
# 为什么要这一步：
#   Compose 的 env_file 不会执行 shell，`. /home/ubuntu/db/.env` 与 `export` 都
#   解析不了；而且默认的 `KEY=VALUE` 格式会对 `$VAR` 做插值，密码里的 `$` 会被
#   悄悄吃掉（本机实测：`5tgb$RFV` 变成 `5tgb`）。因此 compose 里这份 env_file
#   必须配 `format: raw`。
#
# 用法：改了 .env.sh 或密钥后重新执行本脚本，再 `docker compose up -d`。
set -euo pipefail
cd "$(dirname "$0")"

# shellcheck source=./.env.sh
. ./.env.sh

out=.env.docker
umask 077

KEYS=(
  IMGBED_DB_HOST
  IMGBED_DB_PORT
  IMGBED_DB_NAME
  IMGBED_DB_USER
  IMGBED_DB_PASSWORD
  IMGBED_DB_TLS_MODE
  IMGBED_UCENTER_BASE_URL
  IMGBED_UCENTER_APP
  IMGBED_R2_ACCOUNT_ID
  IMGBED_R2_ACCESS_KEY_ID
  IMGBED_R2_SECRET_ACCESS_KEY
  IMGBED_R2_BUCKET
  IMGBED_R2_PUBLIC_BASE
  IMGBED_R2_CF_API_TOKEN
  IMGBED_R2_QUOTA_BYTES
  IMGBED_ALLOWED_ORIGINS
)

{
  echo "# 由 gen-docker-env.sh 自动生成，请勿手改 / 勿入库"
  for k in "${KEYS[@]}"; do
    v=${!k-}
    [ -n "$v" ] || continue
    printf '%s=%s\n' "$k" "$v"
  done
} > "$out"

chmod 600 "$out"
echo "已生成 $out（$(wc -l < "$out") 行）"
