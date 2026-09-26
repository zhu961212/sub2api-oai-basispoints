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

# 独立校验：不复用打包器的自检逻辑；只有明确签名构建才自动传入配套公钥。
# Follow the same output/source/key flags as the packager.
SOURCE=manifest.source.json
DIST=dist
PACKAGE=""
SIGNING_KEY=""
KEY_ID=""
args=("$@")
for ((i=0; i<${#args[@]}; i++)); do
  arg="${args[i]}"
  flag="${arg%%=*}"
  case "$flag" in
    -source|--source|-dist|--dist|-output|--output|-signing-key|--signing-key|-key-id|--key-id)
      if [[ "$arg" == *=* ]]; then value="${arg#*=}"; else i=$((i+1)); value="${args[i]}"; fi
      case "$flag" in
        -source|--source) SOURCE="$value" ;;
        -dist|--dist) DIST="$value" ;;
        -output|--output) PACKAGE="$value" ;;
        -signing-key|--signing-key) SIGNING_KEY="$value" ;;
        -key-id|--key-id) KEY_ID="$value" ;;
      esac ;;
  esac
done
PYTHON=""
for candidate in python3 python; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c 'import sys; assert sys.version_info.major == 3' >/dev/null 2>&1; then
    PYTHON="$candidate"
    break
  fi
done
if [ -n "$SIGNING_KEY" ] && [ -z "$PYTHON" ]; then
  echo "Python is required for independent signature verification." >&2
  exit 1
fi
if [ -n "$PYTHON" ]; then
  if [ -z "$PACKAGE" ]; then
    PACKAGE=$("$PYTHON" -c 'import json,os,sys; m=json.load(open(sys.argv[1],encoding="utf-8")); print(os.path.join(sys.argv[2],m["id"]+"-"+m["version"]+".s2plugin"))' "$SOURCE" "$DIST")
  fi
  verify_args=(tools/verify_package.py "$PACKAGE")
  if [ -n "$SIGNING_KEY" ]; then
    KEY_ID=$("$PYTHON" -c 'import sys; print(sys.argv[1].strip())' "$KEY_ID")
    VERIFY_KEY=$("$PYTHON" -c 'import os,sys; print(os.path.splitext(sys.argv[1])[0]+".public")' "$SIGNING_KEY")
    if [ ! -f "$VERIFY_KEY" ]; then
      echo "Matching publisher public key is required: $VERIFY_KEY" >&2
      exit 1
    fi
    verify_args+=(--require-signature --public-key "$VERIFY_KEY" --expected-key-id "$KEY_ID")
  fi
  echo "==> python tools/verify_package.py $PACKAGE"
  "$PYTHON" "${verify_args[@]}"
fi
