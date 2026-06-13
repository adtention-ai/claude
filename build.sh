#!/bin/sh
# Reproducible build of the adtention client binaries.
#
# Pinned to a fixed toolchain AND environment (Go 1.21.8 inside a Linux container), so the
# output is byte-for-byte identical on any host. Pinning only the Go version + flags is not
# enough: the build host OS leaks into the binary, so a Mac build and a Linux build differ.
# CI runs this same script, so the committed bin/ matches a fresh build exactly, and a
# third party can reproduce the published binaries from this public source.
#
# The version is stamped from .claude-plugin/plugin.json so there is one source of truth.
set -eu

cd "$(dirname "$0")"

VERSION="v$(grep '"version"' .claude-plugin/plugin.json | head -1 \
  | sed -E 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
echo "Building adtention $VERSION in golang:1.21.8 (reproducible)"

docker run --rm -e V="$VERSION" -v "$PWD":/w -w /w golang:1.21.8 sh -euc '
  for pair in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
    os=${pair%/*}; arch=${pair#*/}
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
      go build -C src -trimpath -ldflags "-s -w -X main.version=$V" \
      -o "../bin/adtention-$os-$arch" .
    echo "  built adtention-$os-$arch"
  done
  # Fixed ordering keeps SHA256SUMS itself deterministic. sha256sum (Linux) and
  # shasum -a 256 (macOS) produce the same "hash  name" format for verification.
  cd bin && sha256sum \
    adtention-darwin-amd64 \
    adtention-darwin-arm64 \
    adtention-linux-amd64 \
    adtention-linux-arm64 > SHA256SUMS
'
echo "Wrote bin/SHA256SUMS"
