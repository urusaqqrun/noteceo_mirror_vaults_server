FROM golang:1.24-bullseye AS builder
WORKDIR /app

RUN sed -i 's/deb.debian.org/mirrors.aliyun.com/g' /etc/apt/sources.list && \
    sed -i 's/security.debian.org/mirrors.aliyun.com/g' /etc/apt/sources.list

RUN apt-get update && \
    apt-get install -y ca-certificates tzdata git && \
    rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
ENV GOTOOLCHAIN=auto
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o vault-mirror-service ./main.go

# -------- Final Stage --------
FROM debian:bullseye-slim

RUN apt-get update && \
    apt-get install -y ca-certificates tzdata bash curl netcat-openbsd jq findutils && \
    rm -rf /var/lib/apt/lists/* && \
    curl -fsSL https://deb.nodesource.com/setup_20.x | bash - && \
    apt-get install -y nodejs && \
    rm -rf /var/lib/apt/lists/* && \
    npm install -g esbuild

# 建立非 root 使用者（Claude CLI 拒絕以 root + --dangerously-skip-permissions 運行）
RUN groupadd -r mirror && useradd -r -g mirror -m -s /bin/bash mirror

# 以 mirror 身份安裝 Claude CLI
USER mirror
RUN curl -fsSL https://claude.ai/install.sh | bash && \
    /home/mirror/.local/bin/claude --version || echo "⚠️ Claude CLI 安裝失敗"
ENV PATH="/home/mirror/.local/bin:${PATH}"

# 回到 root 複製檔案、設定權限
USER root
WORKDIR /app
COPY --from=builder /app/vault-mirror-service /app/vault-mirror-service
COPY ./config/ /app/config/
COPY ./entrypoint.sh /app/

# esbuild JS API for plugin bundling (config/esbuild-plugin-bundle.mjs)
RUN cd /app/config && npm init -y > /dev/null 2>&1 && npm install esbuild && rm -f package.json package-lock.json

# Claude CLI hooks 設定 + 讓所有 UID 都能讀取 CLI 設定與執行 CLI
RUN mkdir -p /home/mirror/.claude && \
    cp /app/config/claude-hooks-settings.json /home/mirror/.claude/settings.json && \
    cp /app/config/CLAUDE.md.template /home/mirror/.claude/CLAUDE.md && \
    echo '# clean bashrc for hook compatibility' > /home/mirror/.bashrc && \
    chmod +x /app/config/hooks/*.sh && \
    chown -R mirror:mirror /home/mirror/.claude /home/mirror/.bashrc && \
    chmod -R a+rwX /home/mirror/.claude/ && \
    chmod -R a+rX /home/mirror/.local/

ENV HOME=/home/mirror

RUN sed -i 's/\r$//' /app/entrypoint.sh && \
    chmod +x /app/entrypoint.sh

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD curl -f http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/entrypoint.sh"]
