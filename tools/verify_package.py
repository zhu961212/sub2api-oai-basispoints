"""独立校验 .s2plugin：清单键集合、文件哈希、清单与包内容一致性、Ed25519 签名。

刻意不导入打包器的任何代码，也不依赖第三方库 —— Ed25519 验证用标准库的
纯 Python 实现，避免"用自己的自检证明自己"。

用法：
    python tools/verify_package.py dist/package.s2plugin
    python tools/verify_package.py pkg.s2plugin --public-key ../basispoints-private/publisher.public
    python tools/verify_package.py pkg.s2plugin --require-signature --public-key publisher.public --expected-key-id publisher-v1
"""
import argparse
import base64
import hashlib
import json
import os
import posixpath
import re
import stat
import sys
import zipfile

MANIFEST_KEYS = {
    "schema_version", "id", "name", "version", "description", "author",
    "requires", "capabilities", "runtimes", "ui", "files",
}
REQUIRES_KEYS = {
    "sub2api", "recommended_sub2api_version", "tested_sub2api_versions",
    "plugin_protocol", "transport_api", "ui_bridge",
}

# --- Ed25519 (RFC 8032) 参考实现，仅用于验签 ---------------------------------

_P = 2 ** 255 - 19
_L = 2 ** 252 + 27742317777372353535851937790883648493
_D = -121665 * pow(121666, _P - 2, _P) % _P
_I = pow(2, (_P - 1) // 4, _P)


def _x_recover(y):
    xx = (y * y - 1) * pow(_D * y * y + 1, _P - 2, _P)
    x = pow(xx, (_P + 3) // 8, _P)
    if (x * x - xx) % _P != 0:
        x = (x * _I) % _P
    if x % 2 != 0:
        x = _P - x
    return x


_BY = 4 * pow(5, _P - 2, _P) % _P
_B = [_x_recover(_BY), _BY]


def _edwards_add(left, right):
    x1, y1 = left
    x2, y2 = right
    product = _D * x1 * x2 * y1 * y2
    x3 = (x1 * y2 + x2 * y1) * pow(1 + product, _P - 2, _P)
    y3 = (y1 * y2 + x1 * x2) * pow(1 - product, _P - 2, _P)
    return [x3 % _P, y3 % _P]


def _scalar_mult(point, scalar):
    result = [0, 1]
    addend = point
    while scalar > 0:
        if scalar & 1:
            result = _edwards_add(result, addend)
        addend = _edwards_add(addend, addend)
        scalar >>= 1
    return result


def _decode_point(encoded):
    if len(encoded) != 32:
        raise ValueError("invalid point length")
    value = int.from_bytes(encoded, "little")
    y = value & ((1 << 255) - 1)
    if y >= _P:
        raise ValueError("non-canonical point")
    x = _x_recover(y)
    if x & 1 != (value >> 255) & 1:
        x = _P - x
    if x >= _P or (y*y - x*x - 1 - _D*x*x*y*y) % _P:
        raise ValueError("invalid curve point")
    return [x, y]


def verify_ed25519(public_key, signature, message):
    """校验签名；公钥/签名长度或编码非法时返回 False，不抛异常。"""
    if len(public_key) != 32 or len(signature) != 64:
        return False
    try:
        public_point = _decode_point(public_key)
        r_point = _decode_point(signature[:32])
        if _scalar_mult(public_point, 8) == [0, 1]:
            return False
    except (ValueError, ZeroDivisionError):
        return False
    scalar = int.from_bytes(signature[32:], "little")
    if scalar >= _L:
        return False
    digest = hashlib.sha512(signature[:32] + public_key + message).digest()
    challenge = int.from_bytes(digest, "little") % _L
    return _scalar_mult(_B, scalar) == _edwards_add(r_point, _scalar_mult(public_point, challenge))


def self_test():
    """一个 RFC 8032 测试向量，确保上面这段实现没有被改坏。"""
    public_key = bytes.fromhex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
    signature = bytes.fromhex(
        "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e06522490155"
        "5fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"
    )
    if not verify_ed25519(public_key, signature, b""):
        raise RuntimeError("Ed25519 自检失败：参考向量未通过")
    if verify_ed25519(public_key, signature, b"tampered"):
        raise RuntimeError("Ed25519 自检失败：被篡改的消息通过了验证")


# --- 包校验 -----------------------------------------------------------------


def _safe_path(path):
    return (isinstance(path, str) and bool(path) and path == path.strip()
            and not path.startswith("/") and ":" not in path and "\\" not in path
            and not any(ord(char) < 32 for char in path)
            and path not in (".", "..") and not path.startswith("../")
            and posixpath.normpath(path) == path)


def _object(value, allowed, required, label):
    if not isinstance(value, dict):
        raise ValueError("%s 必须是 JSON 对象" % label)
    if set(value) - allowed:
        raise ValueError("%s 含未知字段: %s" % (label, sorted(set(value) - allowed)))
    if required - set(value):
        raise ValueError("%s 缺少字段: %s" % (label, sorted(required - set(value))))


def _validate_manifest(manifest):
    _object(manifest, MANIFEST_KEYS, MANIFEST_KEYS - {"description", "author"}, "manifest")
    if type(manifest["schema_version"]) is not int or manifest["schema_version"] != 1:
        raise ValueError("schema_version 必须是整数 1")
    for key in ("id", "name", "version", "description", "author"):
        if key in manifest and not isinstance(manifest[key], str):
            raise ValueError("%s 必须是字符串" % key)
    if not re.fullmatch(r"[a-z0-9]+(?:[._-][a-z0-9]+)+", manifest["id"]) or len(manifest["id"]) > 160:
        raise ValueError("插件 ID 无效")
    if not manifest["name"].strip() or len(manifest["name"].encode("utf-8")) > 160:
        raise ValueError("插件名称为空或超过宿主 160 字节限制")
    version = manifest["version"]
    if not re.fullmatch(r"v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?", version):
        raise ValueError("插件版本不是有效的语义化版本")
    prerelease = version.split("+", 1)[0].partition("-")[2]
    if any(part.isdigit() and len(part) > 1 and part.startswith("0") for part in prerelease.split(".")):
        raise ValueError("插件预发布版本包含非法的前导零")
    requires = manifest["requires"]
    _object(requires, REQUIRES_KEYS, {"sub2api", "plugin_protocol", "transport_api", "ui_bridge"}, "requires")
    if not isinstance(requires["sub2api"], str) or not requires["sub2api"].strip():
        raise ValueError("requires.sub2api 不能为空")
    for key in ("plugin_protocol", "transport_api", "ui_bridge"):
        if type(requires[key]) is not int or requires[key] != 1:
            raise ValueError("%s 必须是整数 1" % key)
    if "recommended_sub2api_version" in requires and not isinstance(requires["recommended_sub2api_version"], str):
        raise ValueError("recommended_sub2api_version 必须是字符串")
    if "tested_sub2api_versions" in requires and (not isinstance(requires["tested_sub2api_versions"], list) or not all(isinstance(v, str) for v in requires["tested_sub2api_versions"])):
        raise ValueError("tested_sub2api_versions 必须是字符串数组")
    capabilities = manifest["capabilities"]
    if not isinstance(capabilities, list) or not capabilities:
        raise ValueError("capabilities 不能为空")
    for capability in capabilities:
        fields = {"id", "platform", "account_type"}
        _object(capability, fields, fields, "capability")
        if capability != {"id": "openai.oauth.outbound_transport.v1", "platform": "openai", "account_type": "oauth"}:
            raise ValueError("能力声明不受宿主支持")
    runtimes = manifest["runtimes"]
    if not isinstance(runtimes, dict) or not runtimes:
        raise ValueError("runtimes 不能为空")
    for key, entry in runtimes.items():
        if not re.fullmatch(r"[a-z0-9]+-[a-z0-9]+", key):
            raise ValueError("运行时平台键无效: %s" % key)
        _object(entry, {"path"}, {"path"}, "runtime")
        if not _safe_path(entry["path"]):
            raise ValueError("运行时路径无效")
    _object(manifest["ui"], {"entrypoint"}, {"entrypoint"}, "ui")
    entrypoint = manifest["ui"]["entrypoint"]
    if not _safe_path(entrypoint) or not entrypoint.startswith("ui/"):
        raise ValueError("UI 入口必须位于 ui/ 目录")
    files = manifest["files"]
    if not isinstance(files, dict) or not files:
        raise ValueError("files 不能为空")
    for path, digest in files.items():
        if not _safe_path(path) or not isinstance(digest, str) or not re.fullmatch(r"[a-f0-9]{64}", digest):
            raise ValueError("插件文件声明无效: %s" % path)
    for path in [entrypoint] + [entry["path"] for entry in runtimes.values()]:
        if path not in files:
            raise ValueError("运行时或 UI 入口未包含在文件哈希声明中: %s" % path)


def verify(package_path, public_key_path="", require_signature=False, expected_key_id=""):
    try:
        with zipfile.ZipFile(package_path) as archive:
            return _verify_archive(archive, package_path, public_key_path, require_signature, expected_key_id)
    except (OSError, ValueError, TypeError, KeyError, RuntimeError, zipfile.BadZipFile, NotImplementedError) as error:
        return "校验结果: 失败\n  - %s" % error, False


def _verify_archive(archive, package_path, public_key_path, require_signature, expected_key_id):
    problems = []
    names = archive.namelist()
    if not names or len(names) > 512:
        raise ValueError("插件包文件数量必须为 1–512")
    if len(set(names)) != len(names):
        raise ValueError("插件包包含重复路径")
    for entry in archive.infolist():
        # ZipInfo normalizes backslashes on Windows and truncates NULs. Check
        # the original archive name so neither transformation hides bad input.
        path = entry.orig_filename[:-1] if entry.is_dir() else entry.orig_filename
        if not _safe_path(path):
            raise ValueError("插件包包含不安全路径: %s" % entry.filename)
        if stat.S_ISLNK(entry.external_attr >> 16):
            raise ValueError("插件包不允许符号链接: %s" % entry.filename)
    if sum(entry.file_size for entry in archive.infolist()) > 2 * 1024**3:
        raise ValueError("插件包解压体积超过宿主允许的最大配置上限")
    if archive.getinfo("manifest.json").file_size > 2 * 1024**2:
        raise ValueError("manifest.json 超过宿主 2 MiB 限制")
    manifest = json.loads(archive.read("manifest.json"))
    _validate_manifest(manifest)

    for path, expected in manifest["files"].items():
        if path not in names:
            problems.append("缺少已声明文件: %s" % path)
            continue
        digest = hashlib.sha256()
        with archive.open(path) as source:
            for chunk in iter(lambda: source.read(64 * 1024), b""):
                digest.update(chunk)
        actual = digest.hexdigest()
        if actual != expected:
            problems.append("哈希不匹配: %s" % path)

    declared = set(manifest["files"]) | {"manifest.json", "signature.json"}
    extra = [name for name in names if name not in declared and not name.endswith("/")]
    if extra:
        problems.append("包含未声明文件: %s" % extra)

    for key, entry in manifest["runtimes"].items():
        if entry["path"] not in names:
            problems.append("运行时缺失: %s -> %s" % (key, entry["path"]))
    if manifest["ui"]["entrypoint"] not in names:
        problems.append("UI 入口缺失: %s" % manifest["ui"]["entrypoint"])
    signature_line = "签名: 无（未签名，仅限本地调试）"
    if "signature.json" not in names and (require_signature or public_key_path or expected_key_id):
        problems.append("发布校验要求有效签名，但包中没有 signature.json")
    if "signature.json" in names:
        if archive.getinfo("signature.json").file_size > 64 * 1024:
            raise ValueError("signature.json 超过宿主 64 KiB 限制")
        signature = json.loads(archive.read("signature.json"))
        _object(signature, {"algorithm", "key_id", "signature"}, {"algorithm", "key_id", "signature"}, "signature")
        key_id = signature.get("key_id", "")
        if not isinstance(key_id, str) or not key_id.strip():
            problems.append("签名密钥 ID 无效")
        if expected_key_id and key_id != expected_key_id:
            problems.append("签名密钥 ID 与指定发布者不一致")
        try:
            raw_signature = base64.b64decode(signature.get("signature", ""), validate=True)
            if len(raw_signature) != 64:
                raise ValueError("invalid length")
        except (ValueError, TypeError):
            problems.append("签名不是合法的 64 字节 base64 Ed25519 签名")
            raw_signature = b""
        if signature.get("algorithm") != "ed25519":
            problems.append("签名算法不是 ed25519: %s" % signature.get("algorithm"))
        if not public_key_path:
            signature_line = "签名: 存在（key_id=%s，未提供公钥，未验证）" % key_id
            if require_signature or expected_key_id:
                problems.append("发布验签必须提供 --public-key")
        elif not os.path.exists(public_key_path):
            problems.append("公钥文件不存在: %s" % public_key_path)
        else:
            with open(public_key_path, "r", encoding="utf-8") as key_file:
                text = key_file.read().strip()
            try:
                public_key = base64.b64decode(text, validate=True)
                raw_signature = base64.b64decode(signature.get("signature", ""), validate=True)
            except Exception:
                problems.append("公钥或签名不是合法的 base64")
            else:
                # 签名对象是 manifest.json 的精确原始字节，不能重新序列化后再验。
                if verify_ed25519(public_key, raw_signature, archive.read("manifest.json")):
                    signature_line = "签名: 有效（key_id=%s，ed25519）" % key_id
                else:
                    problems.append("签名无效（key_id=%s）" % key_id)
                    signature_line = "签名: 无效（key_id=%s）" % key_id

    lines = [
        "插件: %s %s" % (manifest["id"], manifest["version"]),
        "包大小: %d 字节，条目数: %d" % (os.path.getsize(package_path), len(names)),
    ]
    for name in sorted(names):
        lines.append("  %-42s %d 字节" % (name, archive.getinfo(name).file_size))
    lines.append(signature_line)
    lines.append("校验结果: " + ("通过" if not problems else "失败"))
    for problem in problems:
        lines.append("  - " + problem)
    return "\n".join(lines), not problems


def main():
    parser = argparse.ArgumentParser(description="独立校验 Sub2API 插件包")
    parser.add_argument("package", help="path/to/plugin.s2plugin")
    parser.add_argument("--public-key", default="", help="base64 Ed25519 公钥文件，用于验签")
    parser.add_argument("--require-signature", action="store_true", help="发布校验：要求已签名并用 --public-key 验证通过")
    parser.add_argument("--expected-key-id", default="", help="要求 signature.key_id 与宿主 trusted_publishers 的键一致")
    args = parser.parse_args()
    try:
        self_test()
    except RuntimeError as error:
        print(str(error))
        return 2
    report, ok = verify(args.package, args.public_key, args.require_signature, args.expected_key_id)
    print(report)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
