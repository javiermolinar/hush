#!/bin/sh
# Build Hush.app: the menu bar app (HushMenu) with the hush CLI beside it.
#   app/macos/build.sh HUSH_BINARY OUT_DIR
# Signs ad hoc. Run `hush service install` from Contents/MacOS/hush so the
# login item is attributed to Hush. Set HUSH_ICON_RENDER=1 to re-render the
# icons from their SVG sources (needs rsvg-convert and iconutil).
set -eu
[ "$(uname -s)" = Darwin ] || { echo "Hush.app is macOS-only" >&2; exit 2; }
[ $# -eq 2 ] || { echo "usage: $0 HUSH_BINARY OUT_DIR" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
hush=$1 out=$2 app=$2/Hush.app
[ -x "$hush" ] || { echo "not an executable: $hush" >&2; exit 2; }
if [ "${HUSH_ICON_RENDER:-}" = 1 ]; then
  set=$(mktemp -d)/Hush.iconset
  mkdir -p "$set"
  for s in 16 32 128 256 512; do
    rsvg-convert -w "$s" -h "$s" "$here/icon/Hush.svg" -o "$set/icon_${s}x${s}.png"
    rsvg-convert -w $((s * 2)) -h $((s * 2)) "$here/icon/Hush.svg" -o "$set/icon_${s}x${s}@2x.png"
  done
  iconutil -c icns "$set" -o "$here/icon/Hush.icns"
  rsvg-convert -w 18 -h 18 "$here/icon/MenuTemplate.svg" -o "$here/icon/MenuTemplate.png"
  rsvg-convert -w 36 -h 36 "$here/icon/MenuTemplate.svg" -o "$here/icon/MenuTemplate@2x.png"
fi
mkdir -p "$out"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$here/Info.plist" "$app/Contents/Info.plist"
cp "$here/icon/Hush.icns" "$here/icon/MenuTemplate.png" "$here/icon/MenuTemplate@2x.png" "$app/Contents/Resources/"
swiftc -O -target "$(uname -m)-apple-macos13.0" -o "$app/Contents/MacOS/HushMenu" "$here/HushMenu.swift"
cp "$hush" "$app/Contents/MacOS/hush"
chmod 755 "$app/Contents/MacOS/HushMenu" "$app/Contents/MacOS/hush"
codesign --force --deep --sign - "$app"
codesign --verify --deep --strict "$app"
echo "$app"
