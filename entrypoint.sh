#!/bin/bash
set -e

# 檢查環境變數
if [ -z "$MONGO_URI" ]; then
  echo "⚠️ 警告: MONGO_URI 未設置，同步功能將降級為 no-op"
fi

if [ -z "$REDIS_URI" ]; then
  echo "⚠️ 警告: REDIS_URI 未設置"
else
  # 嘗試檢查 Redis 連通性
  REDIS_HOST=$(echo "$REDIS_URI" | sed -E 's|redis://([^:]+).*|\1|')
  REDIS_PORT=$(echo "$REDIS_URI" | sed -E 's|.*:([0-9]+).*|\1|')
  if [ -n "$REDIS_HOST" ] && [ -n "$REDIS_PORT" ]; then
    timeout 10s bash -c "until nc -z $REDIS_HOST $REDIS_PORT; do echo '等待 Redis...'; sleep 2; done" || echo "⚠️ Redis 未就緒，繼續啟動..."
  fi
fi

# 確保 Vault 根目錄存在
VAULT_ROOT="${VAULT_ROOT:-/vaults}"
mkdir -p "$VAULT_ROOT"

# 打印環境配置（不包含敏感數據）
echo "啟動配置:"
echo "- 端口: ${PORT:-8080}"
echo "- Vault 根目錄: $VAULT_ROOT"
echo "- MongoDB: $([ -n "$MONGO_URI" ] && echo "已配置" || echo "未配置")"
echo "- Redis: $([ -n "$REDIS_URI" ] && echo "已配置" || echo "未配置")"
echo "- 最大並發任務: ${MAX_CONCURRENT_TASKS:-3}"
echo "- Claude CLI: $(command -v claude &>/dev/null && echo "已安裝" || echo "未安裝")"

# 啟動主程序
echo "啟動 vault-mirror-service..."
exec /app/vault-mirror-service
