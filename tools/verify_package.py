"""独立校验 .s2plugin：清单键集合、文件哈希、清单与包内容一致性、Ed25519 签名。

刻意不导入打包器的任何代码，也不依赖第三方库 —— Ed25519 验证用标准库的
纯 Python 实现，避免"用自己的自检证明自己"。

用法：
    python tools/verify_package.py dist/package.s2plugin
    python tools/verify_package.py pkg.s2plugin --public-key ../basispoints-private/publisher.public
"""
import argparse
import base64
import hashlib
import json
import os
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
    value = int.from_bytes(encoded, "little")
    y = value & ((1 << 255) - 1)
    x = _x_recover(y)
    if x & 1 != (value >> 255) & 1:
        x = _P - x
    return [x, y]


def verify_ed25519(public_key, signature, message):
    """校验签名；公钥/签名长度或编码非法时返回 False，不抛异常。"""
    if len(public_key) != 32 or len(signature) != 64:
        return False
    try:
        public_point = _decode_point(public_key)
        r_point = _decode_point(signature[:32])
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


def verify(package_path, public_key_path=""):
    problems = []
    archive = zipfile.ZipFile(package_path)
    names = archive.namelist()
    manifest = json.loads(archive.read("manifest.json"))

    unknown = set(manifest) - MANIFEST_KEYS
    if unknown:
        problems.append("清单含未知字段（宿主解析会失败）: %s" % sorted(unknown))
    unknown_requires = set(manifest.get("requires", {})) - REQUIRES_KEYS
    if unknown_requires:
        problems.append("requires 含未知字段: %s" % sorted(unknown_requires))

    for path, expected in manifest["files"].items():
        if path not in names:
            problems.append("缺少已声明文件: %s" % path)
            continue
        actual = hashlib.sha256(archive.read(path)).hexdigest()
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
    for capability in manifest["capabilities"]:
        if capability.get("id") != "openai.oauth.outbound_transport.v1" or capability.get("platform") != "openai" or capability.get("account_type") != "oauth":
            problems.append("能力声明不受宿主支持: %s" % capability)
    requires = manifest["requires"]
    if requires.get("plugin_protocol") != 1 or requires.get("transport_api") != 1 or requires.get("ui_bridge") != 1:
        problems.append("协议版本不是 1/1/1: %s" % requires)

    signature_line = "签名: 无（未签名，仅限本地调试）"
    if "signature.json" in names:
        signature = json.loads(archive.read("signature.json"))
        key_id = signature.get("key_id", "")
        if signature.get("algorithm") != "ed25519":
            problems.append("签名算法不是 ed25519: %s" % signature.get("algorithm"))
        if not public_key_path:
            signature_line = "签名: 存在（key_id=%s，未提供公钥，未验证）" % key_id
        elif not os.path.exists(public_key_path):
            problems.append("公钥文件不存在: %s" % public_key_path)
        else:
            text = open(public_key_path, "r", encoding="utf-8").read().strip()
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
    args = parser.parse_args()
    try:
        self_test()
    except RuntimeError as error:
        print(str(error))
        return 2
    report, ok = verify(args.package, args.public_key)
    print(report)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
