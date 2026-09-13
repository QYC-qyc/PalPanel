#!/usr/bin/env bash
# scripts/build.sh — 三平台交叉编译（CGO 关闭）
set -euo pipefail
VERSION=${VERSION:-dev}
OUT=dist-bin
mkdir -p "$OUT"
for target in windows/amd64 linux/amd64 linux/arm64; do
  os=${target%/*}; arch=${target#*/}
  ext=""; [ "$os" = "windows" ] && ext=".exe"
  echo "building panel-$os-$arch$ext"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/panel-$os-$arch$ext" .
done
