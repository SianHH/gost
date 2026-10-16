#!/usr/bin/env bash
#
# Build the host-platform shared library, for driving the FFI from a desktop
# Flutter app. This is the fast iteration loop: no device or emulator needed.
#
# Usage:
#   bash scripts/build-native.sh
#   TAGS=gost_all bash scripts/build-native.sh
#
# Output:
#   build/gost.dll    (Windows)
#   build/gost.so     (Linux)
#   build/gost.dylib  (macOS)
#   build/gost.h
#
# CGO is required: -buildmode=c-shared is a cgo feature and the Go install on
# this machine defaults to CGO_ENABLED=0. A C compiler must be on PATH — on
# Windows that means e.g. the TDM-GCC that ships with many Go setups.

set -euo pipefail

cd "$(dirname "$0")/.."

OUT_DIR="${OUT_DIR:-build}"

case "$(uname -s)" in
Darwin) EXT=dylib ;;
Linux)  EXT=so ;;
*)      EXT=dll ;;
esac

VERSION="$(sed -n 's/.*version = "\(.*\)"/\1/p' ../gost/cmd/gost/version.go | head -1)"
VERSION="${VERSION:-dev}"

if ! command -v cc >/dev/null 2>&1 && ! command -v gcc >/dev/null 2>&1 && [ -z "${CC:-}" ]; then
	echo "error: no C compiler found; c-shared builds need cgo." >&2
	exit 1
fi

mkdir -p "$OUT_DIR"

echo "==> $(go env GOOS)/$(go env GOARCH) (CGO_ENABLED=1)"
CGO_ENABLED=1 go build \
	-buildmode=c-shared \
	-trimpath \
	-tags="${TAGS:-}" \
	-ldflags="-s -w -X github.com/go-gost/sdk/engine.version=$VERSION" \
	-o "$OUT_DIR/gost.$EXT" \
	.

echo
echo "version: $VERSION"
ls -l "$OUT_DIR/gost.$EXT" "$OUT_DIR/gost.h" 2>/dev/null || ls -l "$OUT_DIR"/gost.*
