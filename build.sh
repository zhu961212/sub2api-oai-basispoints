#!/usr/bin/env bash
# 构建、测试并打包 Sub2API 插件。
#
#   ./build.sh
#   ./build.sh -signing-key /secure/publisher.private -key-id my-publisher-v1
set -euo pipefail
cd "$(dirname "$0")"

echo "==> go test ./... -skip TestLive -count=1 -timeout 120s"
go test ./... -skip TestLive -count=1 -timeout 120s

echo "==> go vet ./..."
go vet ./...

if command -v node >/dev/null 2>&1; then
  echo "==> node --check ui/assets/*.js"
  node --check ui/assets/bridge-v1.js
  node --check ui/assets/app.js
  echo "==> node --test tools/ui.test.cjs"
  node --test tools/ui.test.cjs
else
  echo "    node not found, skipping UI syntax check"
fi

echo "==> go run ./tools/packager $*"
go run ./tools/packager "$@"

# 独立校验：不复用打包器的自检逻辑，重新算哈希并（在有公钥时）验证签名。
VERSION=$(grep -m1 '"version"' manifest.source.json | sed 's/.*: *"//; s/".*//')
PACKAGE="dist/local.oai-basispoints-${VERSION}.s2plugin"
if [ -f "$PACKAGE" ] && command -v python >/dev/null 2>&1; then
  echo "==> python tools/verify_package.py $PACKAGE"
  if [ -f build/keys/publisher.public ]; then
    python tools/verify_package.py "$PACKAGE" --public-key build/keys/publisher.public
  else
    python tools/verify_package.py "$PACKAGE"
  fi
fi
