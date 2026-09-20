#!/usr/bin/env bash
# Build Knot.app -- a native macOS app around the read-only client.
#
#   ./dist/macos/build-app.sh                 # app only, sing-box from PATH at runtime
#   ./dist/macos/build-app.sh --with-singbox  # copy the sing-box binary into the bundle
#
# Two executables end up in the bundle: the AppKit shell (Knot) and the knot
# binary it runs as a child (knot-helper). The shell owns the window and the
# child's lifetime; the child does all the actual work.
#
# The result is at dist/macos/build/Knot.app. Drag it to /Applications.
set -euo pipefail

cd "$(dirname "$0")/../.."
OUT="dist/macos/build"
APP="$OUT/Knot.app"
VERSION="${VERSION:-0.1.0}"
WITH_SINGBOX=0
[[ "${1:-}" == "--with-singbox" ]] && WITH_SINGBOX=1

command -v swiftc >/dev/null || {
  echo "需要 Swift 编译器。装一下 Xcode 命令行工具：xcode-select --install" >&2
  exit 1
}
SDK="$(xcrun --sdk macosx --show-sdk-path)"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "==> 编译后台程序 (arm64 + amd64)"
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$OUT/knot-arm64" .
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$OUT/knot-amd64" .
lipo -create -output "$APP/Contents/MacOS/knot-helper" "$OUT/knot-arm64" "$OUT/knot-amd64"
rm -f "$OUT/knot-arm64" "$OUT/knot-amd64"

echo "==> 编译界面 (arm64 + amd64)"
for arch in arm64 x86_64; do
  swiftc -O -target "${arch}-apple-macos11.0" -sdk "$SDK" \
    -o "$OUT/Knot-$arch" dist/macos/Knot.swift
done
lipo -create -output "$APP/Contents/MacOS/Knot" "$OUT/Knot-arm64" "$OUT/Knot-x86_64"
rm -f "$OUT/Knot-arm64" "$OUT/Knot-x86_64"
chmod +x "$APP/Contents/MacOS/Knot" "$APP/Contents/MacOS/knot-helper"

echo "==> 生成图标"
python3 dist/macos/icon.py "$APP/Contents/Resources/knot.icns" >/dev/null

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Knot</string>
  <key>CFBundleDisplayName</key><string>Knot</string>
  <key>CFBundleIdentifier</key><string>dev.bysir.knot.client</string>
  <key>CFBundleExecutable</key><string>Knot</string>
  <key>CFBundleIconFile</key><string>knot.icns</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${VERSION}</string>
  <key>CFBundleVersion</key><string>${VERSION}</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <!-- The panel is served over plain HTTP on loopback, which App Transport
       Security blocks by default. Loopback only: nothing here relaxes anything
       about the network. -->
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
PLIST

if [[ $WITH_SINGBOX == 1 ]]; then
  SB="$(command -v sing-box || true)"
  for p in /opt/homebrew/bin/sing-box /usr/local/bin/sing-box; do
    [[ -n "$SB" ]] && break
    [[ -x "$p" ]] && SB="$p"
  done
  if [[ -z "$SB" ]]; then
    echo "找不到 sing-box，无法打包进去。先 brew install sing-box，或者去掉 --with-singbox" >&2
    exit 1
  fi
  echo "==> 打包 sing-box ($SB)"
  cp "$SB" "$APP/Contents/MacOS/sing-box"
  chmod +x "$APP/Contents/MacOS/sing-box"
fi

# Ad-hoc signature. Not notarisation -- it just gives the bundle a stable
# identity so macOS stops re-asking about network access on every rebuild, and
# so a bundled sing-box is not refused outright on Apple silicon.
if command -v codesign >/dev/null; then
  echo "==> 临时签名"
  for f in sing-box knot-helper; do
    [[ -f "$APP/Contents/MacOS/$f" ]] && codesign --force -s - "$APP/Contents/MacOS/$f" 2>/dev/null || true
  done
  codesign --force -s - "$APP" 2>/dev/null || echo "   (签名失败，不影响本机使用)"
fi

echo
echo "完成: $APP"
echo "安装: cp -r $APP /Applications/"
if [[ $WITH_SINGBOX == 0 ]]; then
  echo "注意: 运行前需要 sing-box -- brew install sing-box"
fi
