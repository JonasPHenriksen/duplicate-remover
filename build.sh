#!/bin/sh
# Builds photo_cleanup for Windows, macOS and Linux into dist/.
# Needs Go (https://go.dev/dl/). The results need nothing installed to run.
set -eu
cd "$(dirname "$0")"
rm -rf dist
mkdir dist

build() { # GOOS GOARCH output-name
	CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -trimpath -ldflags="-s -w" -o "dist/$3" .
}
build windows amd64 photo_cleanup-windows-x64.exe
build windows arm64 photo_cleanup-windows-arm64.exe
build linux amd64 photo_cleanup-linux-x64
build linux arm64 photo_cleanup-linux-arm64
build darwin arm64 photo_cleanup-macos-arm64
build darwin amd64 photo_cleanup-macos-x64

# One file for every Mac, Apple Silicon and Intel, when lipo is around (it is on
# macOS). Then sign it ad hoc, which Apple Silicon requires of every program.
if command -v lipo >/dev/null 2>&1; then
	lipo -create -output dist/photo_cleanup-macos dist/photo_cleanup-macos-arm64 dist/photo_cleanup-macos-x64
	rm dist/photo_cleanup-macos-arm64 dist/photo_cleanup-macos-x64
	codesign --sign - --force dist/photo_cleanup-macos
fi
ls -l dist
