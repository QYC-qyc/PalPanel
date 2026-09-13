# PalPanel 后端镜像（前端 embed 后此文件无需大改，仅构建参数变化）
# 构建：docker build -t palpanel .
# 运行：docker run -d -p 8080:8080 -v palpanel-data:/app/data palpanel

FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/panel .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/panel /app/panel
# 数据库/密钥默认落 WORKDIR 下的 ./data（即 /app/data），挂卷持久化；
# 配置可用 config.yaml 挂载到 /app/config.yaml 覆盖（listen/data_dir）
EXPOSE 8080
ENTRYPOINT ["/app/panel"]
