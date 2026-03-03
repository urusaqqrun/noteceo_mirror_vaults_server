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
WORKDIR /app

RUN apt-get update && \
    apt-get install -y ca-certificates tzdata bash curl netcat-openbsd && \
    rm -rf /var/lib/apt/lists/*

# 安裝 Claude CLI
RUN curl -fsSL https://claude.ai/install.sh | sh || true

COPY --from=builder /app/vault-mirror-service /app/vault-mirror-service
COPY ./config/ /app/config/
COPY ./entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD curl -f http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/entrypoint.sh"]
