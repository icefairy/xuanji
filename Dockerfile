# ---- 构建阶段 ----
FROM golang:1.26-alpine AS builder

WORKDIR /build

# 依赖缓存（go.mod 不变时不重复拉取）
COPY go.mod go.sum ./
RUN go mod download

# 拷贝源码并编译
COPY . .
# CGO_ENABLED=0 静态编译，无 glibc 依赖，可跑在任意精简镜像
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o xuanji-server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.20

WORKDIR /app

# 时区（日志/统计用东八区）+ CA 证书（HTTPS 上游）
RUN apk add --no-cache tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

COPY --from=builder /build/xuanji-server /app/xuanji-server

# 数据卷：数据库持久化
VOLUME ["/data"]

EXPOSE 8787

ENTRYPOINT ["/app/xuanji-server"]
CMD ["--port", "8787", "--db", "/data/xuanji.db"]
