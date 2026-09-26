"""Independent package validation tests; fixture signatures come from Go crypto/ed25519."""
import base64
import copy
import hashlib
import json
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
import warnings
import zipfile

import verify_package as verifier


class PackageVerificationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fixture_dir = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.fixture_dir.cleanup)
        root = Path(cls.fixture_dir.name)
        cls.files = {"runtimes/linux-amd64/plugin": b"test-runtime", "ui/index.html": b"test-ui"}
        cls.manifest = {
            "schema_version": 1, "id": "local.test-plugin", "name": "Fixture", "version": "1.2.3",
            "requires": {"sub2api": ">=0.2.8 <0.3.0", "plugin_protocol": 1, "transport_api": 1, "ui_bridge": 1},
            "capabilities": [{"id": "openai.oauth.outbound_transport.v1", "platform": "openai", "account_type": "oauth"}],
            "runtimes": {"linux-amd64": {"path": "runtimes/linux-amd64/plugin"}},
            "ui": {"entrypoint": "ui/index.html"},
            "files": {path: hashlib.sha256(data).hexdigest() for path, data in cls.files.items()},
        }
        cls.manifest_raw = json.dumps(cls.manifest, sort_keys=True).encode()
        manifest_path = root / "manifest.json"
        manifest_path.write_bytes(cls.manifest_raw)
        # Public, deterministic fixture seed. Never reads or creates publisher keys.
        script = root / "sign_fixture.go"
        script.write_text("""package main
import ("crypto/ed25519"; "encoding/base64"; "encoding/json"; "os")
func main() {
    message, err := os.ReadFile(os.Args[1]); if err != nil { panic(err) }
    key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
    json.NewEncoder(os.Stdout).Encode(map[string]string{
        "public_key": base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
        "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(key, message)),
    })
}
""", encoding="utf-8")
        result = subprocess.run(["go", "run", str(script), str(manifest_path)], check=True, capture_output=True, text=True, timeout=60)
        fixture = json.loads(result.stdout)
        cls.public_key = root / "publisher.public"
        cls.public_key.write_text(fixture["public_key"], encoding="utf-8")
        cls.signature = {"algorithm": "ed25519", "key_id": "test-key", "signature": fixture["signature"]}

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "fixture.s2plugin"

    def write_package(self, manifest=None, signature=None, extras=(), omit=(), raw_manifest=None):
        raw = self.manifest_raw if manifest is None else json.dumps(manifest, sort_keys=True).encode()
        if raw_manifest is not None:
            raw = raw_manifest
        with warnings.catch_warnings(), zipfile.ZipFile(self.path, "w") as archive:
            warnings.simplefilter("ignore", UserWarning)
            archive.writestr("manifest.json", raw)
            for path, data in self.files.items():
                if path not in omit:
                    archive.writestr(path, data)
            if signature is not None:
                archive.writestr("signature.json", json.dumps(signature))
            for path, data in extras:
                if isinstance(path, str) and "\\" in path:
                    entry = zipfile.ZipInfo("placeholder")
                    entry.filename = path
                    path = entry
                archive.writestr(path, data)

    def assert_rejected(self, expected, **kwargs):
        report, ok = verifier.verify(self.path, **kwargs)
        self.assertFalse(ok, report)
        self.assertIn(expected, report)

    def test_unsigned_debug_package(self):
        self.write_package()
        report, ok = verifier.verify(self.path)
        self.assertTrue(ok, report)

    def test_release_requires_signature_even_with_public_key(self):
        self.write_package()
        self.assert_rejected("没有 signature.json", require_signature=True)
        self.assert_rejected("没有 signature.json", public_key_path=self.public_key)

    def test_signed_release_verified_with_go_signature(self):
        self.write_package(signature=self.signature)
        report, ok = verifier.verify(self.path, self.public_key, True, "test-key")
        self.assertTrue(ok, report)
        self.assertIn("签名: 有效", report)

    def test_signature_requires_key_for_release(self):
        self.write_package(signature=self.signature)
        self.assert_rejected("--public-key", require_signature=True)

    def test_key_identity_must_match_host_trust_entry(self):
        self.write_package(signature=self.signature)
        self.assert_rejected("指定发布者不一致", public_key_path=self.public_key, expected_key_id="other-key")

    def test_tampered_signature_and_manifest(self):
        signature = dict(self.signature, signature=base64.b64encode(b"x" * 64).decode())
        self.write_package(signature=signature)
        self.assert_rejected("签名无效", public_key_path=self.public_key)
        manifest = dict(self.manifest, version="1.2.4")
        self.write_package(manifest=manifest, signature=self.signature)
        self.assert_rejected("签名无效", public_key_path=self.public_key)

    def test_malformed_signature(self):
        for signature in (dict(self.signature, signature="invalid"), dict(self.signature, key_id=""), dict(self.signature, algorithm="rsa")):
            with self.subTest(signature=signature):
                self.write_package(signature=signature)
                self.assert_rejected("签名")

    def test_duplicate_zip_path(self):
        self.write_package(extras=[("ui/index.html", b"test-ui")])
        self.assert_rejected("重复路径")

    def test_unsafe_paths_and_directories(self):
        for path in ("../escape", "/absolute", "ui/../escape", "ui//double", "ui/./dot", "ui\\backslash", "C:/absolute", " ../folder/"):
            with self.subTest(path=path):
                self.write_package(extras=[(path, b"")])
                self.assert_rejected("不安全路径")

    def test_symlinks(self):
        link = zipfile.ZipInfo("ui/link")
        link.create_system = 3
        link.external_attr = (stat.S_IFLNK | 0o777) << 16
        self.write_package(extras=[(link, b"index.html")])
        self.assert_rejected("符号链接")

    def test_hash_and_file_membership(self):
        manifest = copy.deepcopy(self.manifest)
        manifest["files"]["ui/index.html"] = "0" * 64
        self.write_package(manifest=manifest)
        self.assert_rejected("哈希不匹配")
        self.write_package(omit=["ui/index.html"])
        self.assert_rejected("缺少已声明文件")
        self.write_package(extras=[("undeclared", b"")])
        self.assert_rejected("未声明文件")

    def test_strict_nested_manifest_fields(self):
        for section in ("requires", "ui"):
            manifest = copy.deepcopy(self.manifest)
            manifest[section]["unknown"] = 1
            self.write_package(manifest=manifest)
            self.assert_rejected("未知字段")
        manifest = copy.deepcopy(self.manifest)
        manifest["runtimes"]["linux-amd64"]["unknown"] = 1
        self.write_package(manifest=manifest)
        self.assert_rejected("未知字段")

    def test_invalid_and_oversized_manifests_fail_cleanly(self):
        for raw in (b"null", b"[]", b"{", self.manifest_raw + b"{}"):
            self.write_package(raw_manifest=raw)
            self.assert_rejected("失败")
        self.write_package(raw_manifest=b" " * (2 * 1024**2 + 1))
        self.assert_rejected("2 MiB")

    def test_runtime_must_be_hashed(self):
        manifest = copy.deepcopy(self.manifest)
        manifest["runtimes"]["linux-amd64"]["path"] = "signature.json"
        self.write_package(manifest=manifest, signature=self.signature)
        self.assert_rejected("未包含在文件哈希声明")

    def test_point_encoding_and_forged_identity_key(self):
        identity = b"\x01" + b"\x00" * 31
        signature = identity + b"\x00" * 32
        self.assertFalse(verifier.verify_ed25519(identity, signature, b"forged"))
        for point in (verifier._P.to_bytes(32, "little"), ((1 << 255) | 1).to_bytes(32, "little")):
            with self.assertRaises(ValueError):
                verifier._decode_point(point)
        verifier.self_test()


if __name__ == "__main__":
    unittest.main()
