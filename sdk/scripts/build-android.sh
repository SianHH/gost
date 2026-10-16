#!/usr/bin/env bash
#
# Build libgost.so for Android and stage it where a Flutter project expects
# native libraries.
#
# Usage:
#   bash scripts/build-android.sh              # arm64-v8a + x86_64
#   bash scripts/build-android.sh arm64-v8a    # one ABI
#   ANDROID_NDK_HOME=/path/to/ndk bash scripts/build-android.sh
#
# Output:
#   jniLibs/<abi>/libgost.so
#   jniLibs/libgost.h
#
# Copy jniLibs/ into android/app/src/main/jniLibs/ of the Flutter project.

set -euo pipefail

cd "$(dirname "$0")/.."

API_LEVEL="${API_LEVEL:-21}"
OUT_DIR="${OUT_DIR:-jniLibs}"

# --- locate the NDK ---------------------------------------------------------

if [ -z "${ANDROID_NDK_HOME:-}" ]; then
	for candidate in \
		"${LOCALAPPDATA:-}/Android/Sdk/ndk" \
		"${ANDROID_HOME:-}/ndk" \
		"${ANDROID_SDK_ROOT:-}/ndk" \
		"$HOME/Android/Sdk/ndk" \
		"$HOME/Library/Android/sdk/ndk"
	do
		[ -d "$candidate" ] || continue
		# Highest version wins; NDK directories are version-named.
		ANDROID_NDK_HOME="$(ls -d "$candidate"/* 2>/dev/null | sort -V | tail -1)"
		[ -n "$ANDROID_NDK_HOME" ] && break
	done
fi

if [ -z "${ANDROID_NDK_HOME:-}" ] || [ ! -d "$ANDROID_NDK_HOME" ]; then
	echo "error: Android NDK not found." >&2
	echo "Set ANDROID_NDK_HOME, or install an NDK via the Android SDK manager." >&2
	exit 1
fi

case "$(uname -s)" in
Darwin) NDK_HOST=darwin-x86_64 ;;
Linux)  NDK_HOST=linux-x86_64 ;;
*)      NDK_HOST=windows-x86_64 ;;
esac

NDK_BIN="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/$NDK_HOST/bin"
if [ ! -d "$NDK_BIN" ]; then
	echo "error: $NDK_BIN does not exist (unsupported host?)" >&2
	exit 1
fi

# Resolve the clang wrapper for a target triple.
#
# On Windows the NDK ships both a bash script and a .cmd batch file per triple.
# Go's exec cannot run the bash one — it has no shebang handling — so the .cmd is
# used there. Its path is kept in mixed (drive-letter, forward-slash) form, which
# Windows accepts and which avoids backslashes being treated as escapes.
clang_for() {
	local triple="$1"
	local base="$NDK_BIN/${triple}${API_LEVEL}-clang"

	if [ -f "$base.cmd" ]; then
		if command -v cygpath >/dev/null 2>&1; then
			cygpath -m "$base.cmd"
		else
			echo "$base.cmd"
		fi
	elif [ -x "$base" ]; then
		echo "$base"
	else
		echo "error: no clang wrapper for $triple at API level $API_LEVEL" >&2
		return 1
	fi
}

# --- version ----------------------------------------------------------------

VERSION="$(sed -n 's/.*version = "\(.*\)"/\1/p' ../gost/cmd/gost/version.go | head -1)"
VERSION="${VERSION:-dev}"

# --- build ------------------------------------------------------------------

# ABI -> GOARCH / clang target triple.
# android/arm64 and android/amd64 both need CGO: without it the pure-Go DNS
# resolver is used, which reads /etc/resolv.conf — a file Android does not have,
# so every hostname lookup fails. See x/internal/net/dialer/dialer.go.
build_abi() {
	local abi="$1" goarch="$2" goarm="$3" triple="$4"
	local cc
	cc="$(clang_for "$triple")" || return 1

	echo "==> $abi ($goarch${goarm:+, GOARM=$goarm})"
	mkdir -p "$OUT_DIR/$abi"

	# `env` rather than bash's VAR=value prefix form: the prefix form does not
	# survive the conditional GOARM expansion and swallows the next assignment.
	env GOOS=android \
		GOARCH="$goarch" \
		GOARM="$goarm" \
		CC="$cc" \
		CGO_ENABLED=1 \
		go build \
		-buildmode=c-shared \
		-trimpath \
		-tags="${TAGS:-}" \
		-ldflags="-s -w -X github.com/go-gost/sdk/engine.version=$VERSION" \
		-o "$OUT_DIR/$abi/libgost.so" \
		.
}

targets=("$@")
if [ ${#targets[@]} -eq 0 ]; then
	targets=(arm64-v8a x86_64)
fi

for abi in "${targets[@]}"; do
	case "$abi" in
	arm64-v8a)   build_abi arm64-v8a   arm64 ""  aarch64-linux-android ;;
	armeabi-v7a) build_abi armeabi-v7a arm   7   armv7a-linux-androideabi ;;
	x86_64)      build_abi x86_64      amd64 ""  x86_64-linux-android ;;
	x86)         build_abi x86         "386" ""  i686-linux-android ;;
	*)
		echo "error: unknown ABI '$abi' (want arm64-v8a, armeabi-v7a, x86_64 or x86)" >&2
		exit 1
		;;
	esac
done

# The generated header is ABI-independent, so one copy is enough.
if [ -f libgost.h ]; then
	cp libgost.h "$OUT_DIR/libgost.h"
else
	# go build writes the header next to the output, named after the -o path.
	for h in "$OUT_DIR"/*/libgost.h; do
		[ -f "$h" ] && cp "$h" "$OUT_DIR/libgost.h" && break
	done
fi

echo
echo "version: $VERSION"
ls -1 "$OUT_DIR"/*/libgost.so
[ -f "$OUT_DIR/libgost.h" ] && echo "$OUT_DIR/libgost.h"
