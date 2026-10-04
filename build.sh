#!/bin/sh
# Builds photo_cleanup for Windows, macOS and Linux into dist/, in both editions:
# the regular one, and the simple one (photo_cleanup-simple-*) with large text,
# plain words and pictures, in Danish or English.
# Needs Go (https://go.dev/dl/). The results need nothing installed to run.
set -eu
cd "$(dirname "$0")"
rm -rf dist
mkdir dist

build() { # GOOS GOARCH output-name, for $edition
	CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -trimpath -ldflags="-s -w${edition:+ -X main.edition=$edition}" -o "dist/$3" .
}
for edition in "" simple; do
	name=photo_cleanup${edition:+-$edition}
	build windows amd64 "$name-windows-x64.exe"
	build windows arm64 "$name-windows-arm64.exe"
	build linux amd64 "$name-linux-x64"
	build linux arm64 "$name-linux-arm64"
	build darwin arm64 "$name-macos-arm64"
	build darwin amd64 "$name-macos-x64"

	# One file for every Mac, Apple Silicon and Intel, when lipo is around (it is
	# on macOS). Then sign it ad hoc, which Apple Silicon requires of every program.
	if command -v lipo >/dev/null 2>&1; then
		lipo -create -output "dist/$name-macos" "dist/$name-macos-arm64" "dist/$name-macos-x64"
		rm "dist/$name-macos-arm64" "dist/$name-macos-x64"
		codesign --sign - --force "dist/$name-macos"
	fi
done
ls -l dist
