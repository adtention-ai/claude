#!/bin/sh
# Canonical reproducible build of the adtention client binaries.
#
# CI uses these exact flags, so the output is byte-for-byte identical to a fresh
# build. That lets anyone verify the committed bin/ against the public source
# (see .github/workflows/ci.yml) and lets users check their download with
# bin/SHA256SUMS. The version is stamped from .claude-plugin/plugin.json so there
# is one source of truth.
set -eu

cd "$(dirname "$0")"

VERSION="v$(grep '"version"' .claude-plugin/plugin.json | head -1 \
  | sed -E 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
echo "Building adtention $VERSION (Go $(go env GOVERSION))"

build() {
  os=$1
  arch=$2
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -C src -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "../bin/adtention-$os-$arch" .
  echo "  built adtention-$os-$arch"
}

build darwin amd64
build darwin arm64
build linux  amd64
build linux  arm64

# Fixed ordering keeps SHA256SUMS itself deterministic.
( cd bin && shasum -a 256 \
    adtention-darwin-amd64 \
    adtention-darwin-arm64 \
    adtention-linux-amd64 \
    adtention-linux-arm64 > SHA256SUMS )
echo "Wrote bin/SHA256SUMS"
