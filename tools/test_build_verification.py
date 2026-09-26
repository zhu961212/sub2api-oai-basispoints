"""Run real build wrappers/verifier with compilation stubbed and public fixture keys."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
import zipfile

from test_build_scripts import BASH, POWERSHELL, ROOT

WINDOWS_POWERSHELL = shutil.which("powershell.exe") if os.name == "nt" else None


class BuildVerificationIntegrationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fixtures = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.fixtures.cleanup)
        cls.fixture_root = Path(cls.fixtures.name)
        files = {"runtimes/linux-amd64/plugin": b"synthetic runtime", "ui/index.html": b"synthetic ui"}
        cls.manifest = {
            "schema_version": 1, "id": "local.build-fixture", "name": "构建夹具。", "version": "1.2.3",
            "requires": {"sub2api": ">=0.2.8 <0.3.0", "plugin_protocol": 1, "transport_api": 1, "ui_bridge": 1},
            "capabilities": [{"id": "openai.oauth.outbound_transport.v1", "platform": "openai", "account_type": "oauth"}],
            "runtimes": {"linux-amd64": {"path": "runtimes/linux-amd64/plugin"}},
            "ui": {"entrypoint": "ui/index.html"},
            "files": {name: hashlib.sha256(data).hexdigest() for name, data in files.items()},
        }
        manifest_raw = json.dumps(cls.manifest, sort_keys=True, ensure_ascii=False).encode()
        manifest_path = cls.fixture_root / "manifest.json"
        manifest_path.write_bytes(manifest_raw)
        signer = cls.fixture_root / "sign_fixture.go"
        # Public deterministic seed; no real signing or publisher key is read.
        signer.write_text("""package main
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
        result = subprocess.run(["go", "run", str(signer), str(manifest_path)], check=True,
                                capture_output=True, text=True, timeout=60)
        fixture = json.loads(result.stdout)
        cls.public_key = fixture["public_key"]
        for signed in (False, True):
            with zipfile.ZipFile(cls.fixture_root / ("signed.s2plugin" if signed else "unsigned.s2plugin"), "w") as archive:
                archive.writestr("manifest.json", manifest_raw)
                for name, data in files.items():
                    archive.writestr(name, data)
                if signed:
                    archive.writestr("signature.json", json.dumps({"algorithm": "ed25519", "key_id": "test-key",
                                                                  "signature": fixture["signature"]}))

    def run_wrapper(self, shell, signed, stale_public_key, package_signed=None):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "tools").mkdir()
            (root / "keys").mkdir()
            (root / "dist").mkdir()
            (root / "manifest.source.json").write_text(json.dumps(self.manifest, ensure_ascii=False), encoding="utf-8")
            (root / "keys/publisher.private").write_text("unused fixture key", encoding="ascii")
            (root / "keys/publisher.public").write_text(self.public_key, encoding="ascii")
            if stale_public_key:
                (root / "build/keys").mkdir(parents=True)
                (root / "build/keys/publisher.public").write_text("unrelated stale key", encoding="ascii")
            shutil.copyfile(ROOT / "tools/verify_package.py", root / "tools/verify_package.py")
            package_signed = signed if package_signed is None else package_signed
            fixture = self.fixture_root / ("signed.s2plugin" if package_signed else "unsigned.s2plugin")
            shutil.copyfile(fixture, root / "dist/local.build-fixture-1.2.3.s2plugin")
            env = dict(os.environ, BUILD_TEST_PYTHON=Path(sys.executable).as_posix(), PYTHONUTF8="1")
            if shell in ("powershell", "windows-powershell"):
                shutil.copyfile(ROOT / "build.ps1", root / "build.ps1")
                (root / "runner.ps1").write_text(r'''
function go { $global:LASTEXITCODE = 0 }
function node { $global:LASTEXITCODE = 0 }
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
function python { & $env:BUILD_TEST_PYTHON @args; $global:LASTEXITCODE = $LASTEXITCODE }
$parameters = ConvertFrom-Json $env:BUILD_TEST_ARGUMENTS
$buildArguments = @{}
foreach ($property in $parameters.PSObject.Properties) { $buildArguments[$property.Name] = $property.Value }
& "$PSScriptRoot/build.ps1" @buildArguments
''', encoding="utf-8")
                env["BUILD_TEST_ARGUMENTS"] = json.dumps(
                    {"SigningKey": "keys/publisher.private", "KeyId": "test-key"} if signed else {})
                executable = WINDOWS_POWERSHELL if shell == "windows-powershell" else POWERSHELL
                command = [executable, "-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "runner.ps1"]
            else:
                shutil.copyfile(ROOT / "build.sh", root / "build.sh")
                (root / "runner.sh").write_text('''#!/usr/bin/env bash
go() { return 0; }
node() { return 0; }
python3() { "$BUILD_TEST_PYTHON" "$@"; }
python() { "$BUILD_TEST_PYTHON" "$@"; }
source ./build.sh "$@"
''', encoding="utf-8", newline=chr(10))
                command = [BASH, "runner.sh"]
                if signed:
                    command.extend(["-signing-key", "keys/publisher.private", "-key-id", "test-key"])
            return subprocess.run(command, cwd=root, env=env, capture_output=True, text=True,
                                  encoding="utf-8", timeout=30)

    def assert_signing_matrix(self, shell):
        for signed in (False, True):
            for stale_public_key in (False, True):
                with self.subTest(shell=shell, signed=signed, stale_public_key=stale_public_key):
                    result = self.run_wrapper(shell, signed, stale_public_key)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn("校验结果: 通过", result.stdout)
                    self.assertIn("签名: 有效" if signed else "签名: 无", result.stdout)

    @unittest.skipUnless(POWERSHELL, "PowerShell is unavailable")
    def test_powershell_real_verifier_signing_and_stale_key_matrix(self):
        self.assert_signing_matrix("powershell")

    @unittest.skipUnless(WINDOWS_POWERSHELL, "Windows PowerShell is unavailable")
    def test_windows_powershell_utf8_manifest_signing_and_stale_key_matrix(self):
        self.assert_signing_matrix("windows-powershell")

    @unittest.skipUnless(BASH, "Bash is unavailable")
    def test_bash_real_verifier_signing_and_stale_key_matrix(self):
        self.assert_signing_matrix("bash")

    @unittest.skipUnless(POWERSHELL, "PowerShell is unavailable")
    def test_powershell_real_verifier_rejects_unsigned_release(self):
        result = self.run_wrapper("powershell", True, True, package_signed=False)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("没有 signature.json", result.stdout)

    @unittest.skipUnless(WINDOWS_POWERSHELL, "Windows PowerShell is unavailable")
    def test_windows_powershell_real_verifier_rejects_unsigned_release(self):
        result = self.run_wrapper("windows-powershell", True, True, package_signed=False)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("没有 signature.json", result.stdout)

    @unittest.skipUnless(BASH, "Bash is unavailable")
    def test_bash_real_verifier_rejects_unsigned_release(self):
        result = self.run_wrapper("bash", True, True, package_signed=False)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("没有 signature.json", result.stdout)

    def test_explicit_verifier_public_key_still_requires_signature(self):
        public_key = self.fixture_root / "publisher.public"
        public_key.write_text(self.public_key, encoding="ascii")
        result = subprocess.run([sys.executable, "-X", "utf8", str(ROOT / "tools/verify_package.py"),
                                 str(self.fixture_root / "unsigned.s2plugin"), "--public-key", str(public_key)],
                                capture_output=True, text=True, encoding="utf-8", timeout=30)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("没有 signature.json", result.stdout)


if __name__ == "__main__":
    unittest.main()
