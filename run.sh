#!/usr/bin/env bash
# 本地/本机运行入口：加载密钥环境变量后启动服务。
#
# 密钥来自 ./.env.sh（与本文件同目录），该文件不入库。
set -euo pipefail

cd "$(dirname "$0")"

# shellcheck source=./.env.sh
. ./.env.sh

exec go run ./cmd/imgbed -config ./config.yaml
