#!/usr/bin/env bash
# Static, stripped binaries for Raspberry Pi (armv6/armv7/arm64) and desktops.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(date +%Y.%m.%d)}"
mkdir -p dist
for t in linux/arm/6 linux/arm/7 linux/arm64 linux/amd64 darwin/arm64 darwin/amd64 windows/amd64; do
  IFS=/ read -r os arch arm <<<"$t"
  out="dist/lls-$os-$arch${arm:+v$arm}"; [ "$os" = windows ] && out="$out.exe"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=${arm:-} \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/lls
  echo "$out"
done
