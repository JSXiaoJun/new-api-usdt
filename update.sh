#!/bin/sh
# 更新已部署的扫链服务到指定版本（不传参数 = 取最新 Release）。
#
#   bash update.sh            # 更新到最新版
#   bash update.sh v0.1.1     # 更新/回滚到指定版本
#
# 只替换 bin/ 里的两个二进制，.env 和 data/ 不动，然后重建镜像并重启容器。
set -eu
cd "$(dirname "$0")"

VERSION="${1:-}"
REPO="JSXiaoJun/new-api-usdt"

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *)
    echo "不支持的架构: $(uname -m)" >&2
    exit 1
    ;;
esac

if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
fi
if [ -z "$VERSION" ]; then
  echo "无法确定最新版本，请显式指定，例如: bash update.sh v0.1.1" >&2
  exit 1
fi

URL="https://github.com/$REPO/releases/download/$VERSION/new-api-usdt-kit-linux-$ARCH.tar.gz"
echo "==> 更新到 $VERSION ($ARCH)"
echo "    来源: $URL"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fL -o "$TMP/kit.tar.gz" "$URL"
tar -xzf "$TMP/kit.tar.gz" -C "$TMP"

cp "$TMP/bin/usdt-scanner" bin/usdt-scanner
cp "$TMP/bin/usdt-deploy" bin/usdt-deploy
chmod +x bin/usdt-scanner bin/usdt-deploy

echo "==> 重建镜像"
docker compose build
echo "==> 重启容器"
docker compose up -d
sleep 2
docker compose ps
echo
echo "完成。看日志: docker compose logs -f --tail=50 usdt-scanner"
