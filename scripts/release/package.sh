#!/usr/bin/env bash
# 打出可随播放器安装包分发的插件资产：
#   dist-plugin/nagare-source-<版本>_<OS>_<arch>.{tar.gz|zip}
#     ├── nagare-source[.exe]   引擎（CGO_ENABLED=0 交叉编译）
#     ├── repo/                 只含 BT 规则的运行时根目录（nagare-source bundle --profile bt）
#     └── LICENSE
#   dist-plugin/checksums.txt   sha256，供下游钉死校验
# 归档名与 nagare 的资产命名一致（MacOS/Linux/Windows · x86_64/arm64），下游按 GOOS/GOARCH 拼名字。
set -euo pipefail

VERSION="${1:?usage: package.sh <version> [out-dir]}"
OUT="${2:-dist-plugin}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUILD="$OUT/.build"
rm -rf "$OUT"
mkdir -p "$BUILD"

# 运行时根目录只做一次，所有平台共用
go run "$ROOT/cmd/nagare-source" bundle --root "$ROOT" --out "$BUILD/repo" ${SOURCE_DATE_EPOCH:+--generated-at "$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ)"}

os_name() { case "$1" in darwin) echo MacOS;; linux) echo Linux;; windows) echo Windows;; esac; }
arch_name() { case "$1" in amd64) echo x86_64;; arm64) echo arm64;; esac; }

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  GOOS="${target%/*}"; GOARCH="${target#*/}"
  name="nagare-source-${VERSION}_$(os_name "$GOOS")_$(arch_name "$GOARCH")"
  stage="$BUILD/$name"
  mkdir -p "$stage"
  bin="nagare-source"; [ "$GOOS" = windows ] && bin="nagare-source.exe"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags "-s -w -X main.buildVersion=$VERSION" \
    -o "$stage/$bin" "$ROOT/cmd/nagare-source"
  cp -R "$BUILD/repo" "$stage/repo"
  cp "$ROOT/LICENSE" "$stage/LICENSE"
  if [ "$GOOS" = windows ]; then
    (cd "$BUILD" && rm -f "../$name.zip" && zip -qr -X "../$name.zip" "$name")
  else
    tar --sort=name ${SOURCE_DATE_EPOCH:+--mtime="@$SOURCE_DATE_EPOCH"} --owner=0 --group=0 --numeric-owner \
      -cf "$BUILD/$name.tar" -C "$BUILD" "$name" 2>/dev/null \
      || tar -cf "$BUILD/$name.tar" -C "$BUILD" "$name"
    gzip -n -c "$BUILD/$name.tar" > "$OUT/$name.tar.gz"
  fi
done

(cd "$OUT" && (command -v sha256sum >/dev/null && sha256sum ./*.tar.gz ./*.zip || shasum -a 256 ./*.tar.gz ./*.zip) | sed 's#\./##' > checksums.txt)
rm -rf "$BUILD"
ls -la "$OUT"
