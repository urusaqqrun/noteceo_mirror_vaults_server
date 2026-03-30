#!/bin/bash
set -e

# 檢查環境變數
if [ -z "$POSTGRES_URI" ]; then
  echo "⚠️ 警告: POSTGRES_URI 未設置，資料庫功能將無法使用"
fi

if [ -z "$REDIS_URI" ]; then
  echo "⚠️ 警告: REDIS_URI 未設置"
else
  # 嘗試檢查 Redis 連通性（支援 redis:// 和 rediss:// 格式）
  REDIS_HOST=$(echo "$REDIS_URI" | sed -E 's|^rediss?://||' | sed -E 's|:[0-9]+.*||')
  REDIS_PORT=$(echo "$REDIS_URI" | sed -E 's|^rediss?://[^:]+:||' | sed -E 's|[^0-9].*||')
  if [ -n "$REDIS_HOST" ] && [ -n "$REDIS_PORT" ]; then
    timeout 10s bash -c "until nc -z $REDIS_HOST $REDIS_PORT; do echo '等待 Redis...'; sleep 2; done" || echo "⚠️ Redis 未就緒，繼續啟動..."
  fi
fi

# 確保 Vault 根目錄存在
VAULT_ROOT="${VAULT_ROOT:-/vaults}"
mkdir -p "$VAULT_ROOT"

# 確保 shared 目錄存在且所有 UID 可讀取
mkdir -p "$VAULT_ROOT/shared"
chown root:root "$VAULT_ROOT/shared"
chmod 755 "$VAULT_ROOT/shared"

# 從 S3 (via CloudFront) 下載內建插件原始碼到 EFS shared 目錄
PLUGINS_SRC_URL="${PLUGINS_SRC_URL:-https://cubelv.com/app/plugins-src.tar.gz}"
PLUGINS_DST="$VAULT_ROOT/shared/plugins-src"
echo "下載內建插件原始碼: $PLUGINS_SRC_URL ..."
if curl -fsSL "$PLUGINS_SRC_URL" -o /tmp/plugins-src.tar.gz; then
  rm -rf "$PLUGINS_DST"
  mkdir -p "$PLUGINS_DST"
  tar -xzf /tmp/plugins-src.tar.gz -C "$PLUGINS_DST"
  chmod -R a+rX "$PLUGINS_DST"
  rm -f /tmp/plugins-src.tar.gz
  echo "✅ 內建插件原始碼同步完成"
else
  echo "⚠️ 下載插件原始碼失敗，跳過（$PLUGINS_SRC_URL）"
fi

# 打印環境配置（不包含敏感數據）
echo "啟動配置:"
echo "- 端口: ${PORT:-8080}"
echo "- Vault 根目錄: $VAULT_ROOT"
echo "- PostgreSQL: $([ -n "$POSTGRES_URI" ] && echo "已配置" || echo "未配置")"
echo "- Redis: $([ -n "$REDIS_URI" ] && echo "已配置" || echo "未配置")"
echo "- 最大並發任務: ${MAX_CONCURRENT_TASKS:-3}"
echo "- Claude CLI: $(command -v claude &>/dev/null && echo "已安裝" || echo "未安裝")"

# 替換 CLAUDE.md 模板中的 placeholder 為實際環境變數值
sed -i "s|{AI_SERVICE_URL}|${AI_SERVICE_URL:-http://chatbot.svc.local:8000}|g" /home/mirror/.claude/CLAUDE.md

# 啟動主程序
echo "啟動 vault-mirror-service..."
exec /app/vault-mirror-service
