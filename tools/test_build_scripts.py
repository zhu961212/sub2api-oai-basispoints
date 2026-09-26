"""Exercise build-wrapper argument handling without compiling or using real keys."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
POWERSHELL = shutil.which("pwsh")
BASH = shutil.which("bash")


class BuildScriptsTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.manifest = {"id": "local.custom-plugin", "version": "1.2.3"}
        (self.root / "manifest.source.json").write_text(json.dumps(self.manifest), encoding="utf-8")
        (self.root / "keys").mkdir()
        for extension in ("private", "public"):
            (self.root / ("keys/publisher." + extension)).write_text("synthetic fixture", encoding="ascii")
        self.log = self.root / "verify-arguments.json"
        self.env = dict(os.environ, BUILD_TEST_VERIFY_LOG=str(self.log))

    def run_powershell(self, arguments, artifact, failure=None):
        shutil.copyfile(ROOT / "build.ps1", self.root / "build.ps1")
        runner = self.root / "runner.ps1"
        runner.write_text(r'''
$ErrorActionPreference = "Stop"
function go {
    if ($args.Count -gt 1 -and $args[0] -eq "run") {
        $artifact = [IO.Path]::GetFullPath($env:BUILD_TEST_PACKAGE)
        [IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($artifact)) | Out-Null
        [IO.File]::WriteAllText($artifact, "synthetic fixture")
    }
    $global:LASTEXITCODE = 0
}
function node { $global:LASTEXITCODE = 0 }
function python {
    [IO.File]::WriteAllText($env:BUILD_TEST_VERIFY_LOG, (ConvertTo-Json -InputObject @($args) -Compress))
    $global:LASTEXITCODE = [int]$env:BUILD_TEST_VERIFY_EXIT
}
$buildArguments = ConvertFrom-Json -AsHashtable $env:BUILD_TEST_ARGUMENTS
& "$PSScriptRoot/build.ps1" @buildArguments
''', encoding="utf-8")
        env = dict(self.env, BUILD_TEST_ARGUMENTS=json.dumps(arguments), BUILD_TEST_PACKAGE=artifact)
        result = subprocess.run([POWERSHELL, "-NoLogo", "-NoProfile", "-File", str(runner)],
                                cwd=self.root, env=env, capture_output=True, text=True, encoding="utf-8", timeout=30)
        if failure:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn(failure, result.stdout + result.stderr)
            return json.loads(self.log.read_text(encoding="utf-8-sig")) if self.log.exists() else None
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(self.log.exists(), "Independent package verification was silently skipped")
        return json.loads(self.log.read_text(encoding="utf-8-sig"))

    @unittest.skipUnless(POWERSHELL, "PowerShell is unavailable")
    def test_powershell_default_package_uses_manifest_id(self):
        artifact = "dist/local.custom-plugin-1.2.3.s2plugin"
        arguments = self.run_powershell({}, artifact)
        self.assertEqual(Path(arguments[3]), Path(artifact))

    @unittest.skipUnless(POWERSHELL, "PowerShell is unavailable")
    def test_powershell_custom_output_metacharacters_are_verified(self):
        artifact = "dist/custom [signed] package.s2plugin"
        arguments = self.run_powershell({"Output": artifact, "SigningKey": "keys/publisher.private",
                                         "KeyId": "publisher-v1"}, artifact)
        self.assertEqual(arguments[3], artifact)
        self.assertIn("--require-signature", arguments)
        self.assertEqual(arguments[arguments.index("--public-key") + 1], "keys/publisher.public")
        self.assertEqual(arguments[arguments.index("--expected-key-id") + 1], "publisher-v1")

    @unittest.skipUnless(POWERSHELL, "PowerShell is unavailable")
    def test_powershell_key_id_matches_packager_normalization(self):
        artifact = "dist/custom.s2plugin"
        arguments = self.run_powershell({"Output": artifact, "SigningKey": "keys/publisher.private",
                                         "KeyId": " publisher-v1 "}, artifact)
        self.assertEqual(arguments[arguments.index("--expected-key-id") + 1], "publisher-v1")

    def test_bash_entrypoint_has_unix_line_endings(self):
        self.assertNotIn(b"\r\n", (ROOT / "build.sh").read_bytes(), "CRLF breaks the Unix interpreter and shell continuation")

    @unittest.skipUnless(POWERSHELL, "PowerShell is unavailable")
    def test_powershell_missing_matching_public_key_fails_closed(self):
        (self.root / "keys/publisher.public").unlink()
        artifact = "dist/custom.s2plugin"
        arguments = self.run_powershell({"Output": artifact, "SigningKey": "keys/publisher.private",
                                         "KeyId": "publisher-v1"}, artifact, "Matching publisher public key is required")
        self.assertIsNone(arguments)

    @unittest.skipUnless(POWERSHELL, "PowerShell is unavailable")
    def test_powershell_failed_signature_verification_fails_build(self):
        self.env["BUILD_TEST_VERIFY_EXIT"] = "1"
        artifact = "dist/custom.s2plugin"
        arguments = self.run_powershell({"Output": artifact, "SigningKey": "keys/publisher.private",
                                         "KeyId": "publisher-v1"}, artifact, "Independent package verification failed")
        self.assertIn("--require-signature", arguments)

    @unittest.skipUnless(BASH and os.name != "nt", "Native Bash is unavailable")
    def test_bash_custom_source_output_and_signature_flags(self):
        shutil.copyfile(ROOT / "build.sh", self.root / "build.sh")
        (self.root / "custom source.json").write_text(json.dumps(self.manifest), encoding="utf-8")
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        scripts = {
            "go": '#!/bin/sh\nif [ "$1" = run ]; then mkdir -p "$(dirname "$BUILD_TEST_PACKAGE")"; printf fixture > "$BUILD_TEST_PACKAGE"; fi\n',
            "node": "#!/bin/sh\nexit 0\n",
            "python3": '#!/bin/sh\nif [ "$1" = tools/verify_package.py ]; then exec "$BUILD_TEST_PYTHON" "$BUILD_TEST_RECORDER" "$@"; else exec "$BUILD_TEST_PYTHON" "$@"; fi\n',
        }
        for name, content in scripts.items():
            path = bin_dir / name
            path.write_text(content, encoding="utf-8")
            path.chmod(0o755)
        recorder = self.root / "record.py"
        recorder.write_text('import json,os,sys; open(os.environ["BUILD_TEST_VERIFY_LOG"],"w").write(json.dumps(sys.argv[1:]))', encoding="utf-8")
        for equals in (False, True):
            with self.subTest(equals=equals):
                artifact = "custom dist/custom [signed] package.s2plugin"
                flags = {"source": "custom source.json", "dist": "custom dist", "output": artifact,
                         "signing-key": "keys/publisher.private", "key-id": " publisher-v1 "}
                arguments = []
                for name, value in flags.items():
                    arguments.extend(["--" + name + "=" + value] if equals else ["-" + name, value])
                env = dict(self.env, PATH=str(bin_dir) + os.pathsep + self.env["PATH"],
                           BUILD_TEST_PACKAGE=artifact, BUILD_TEST_PYTHON=sys.executable, BUILD_TEST_RECORDER=str(recorder))
                result = subprocess.run([BASH, "build.sh", *arguments], cwd=self.root, env=env,
                                        capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                verified = json.loads(self.log.read_text(encoding="utf-8"))
                self.assertEqual(verified[1], artifact)
                self.assertEqual(verified[verified.index("--public-key") + 1], "keys/publisher.public")
                self.assertEqual(verified[verified.index("--expected-key-id") + 1], "publisher-v1")


if __name__ == "__main__":
    unittest.main()
