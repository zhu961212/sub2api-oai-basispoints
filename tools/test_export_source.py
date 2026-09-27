import importlib.util
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import zipfile

spec = importlib.util.spec_from_file_location("export_source", Path(__file__).with_name("export_source.py"))
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)

class SourceExportBoundaryTests(unittest.TestCase):
    def test_source_and_declared_contract_are_preserved(self):
        for name in ("README.md", "manifest.source.json", "go.mod", "go.sum",
                     ".github/workflows/ci.yml", "internal/attachments/cache_regression_test.go",
                     "third_party/sub2api/LICENSE", "third_party/sub2api/go.mod",
                     "third_party/sub2api/pkg/pluginapi/v1/plugin.pb.go",
                     "docs/使用说明.md", "docs/host-patches/sub2api-0.2.8-config-test-scoped.patch",
                     "tools/host-diagnostic-integration/diagnostic_jobs_host_test.go.txt",
                     "tools/host-relay-integration/relay_scope_host_test.go.txt",
                     "tools/test_export_source.py", "ui/index.html"):
            with self.subTest(name=name):
                self.assertTrue(exporter.source_path_allowed(name))

    def test_build_outputs_keys_and_credentials_are_excluded_even_if_tracked(self):
        for name in ("dist/plugin.s2plugin", "build/key.private", ".git/config",
                     "tools/publisher.private", "docs/key.pem", ".env", "ui/.env.local",
                     "internal/auth.json", "ui/credentials.json", "tools/accounts-export.json",
                     "docs/settings.local.json", "tools/__pycache__/script.py", "tools/private.patch",
                     "tools/node_modules/index.js", "/outside.go", "../README.md"):
            with self.subTest(name=name):
                self.assertFalse(exporter.source_path_allowed(name))

    def test_source_archive_keeps_only_the_two_fixed_text_fixtures(self):
        required = ("README.md", "go.mod", "manifest.source.json")
        fixtures = (
            "tools/host-diagnostic-integration/diagnostic_jobs_host_test.go.txt",
            "tools/host-relay-integration/relay_scope_host_test.go.txt",
        )
        rejected = (
            "tools/notes.txt", "docs/notes.txt", "ui/credentials.txt",
            "tools/host-diagnostic-integration/credentials.txt",
            "tools/host-relay-integration/credentials.json",
            "tools/host-diagnostic-integration/other_test.go.txt",
            "tools/host-relay-integration/diagnostic_jobs_host_test.go.txt",
            "tools/host-diagnostic-integration/relay_scope_host_test.go.txt",
            "tools/credentials/diagnostic_jobs_host_test.go.txt",
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in required + fixtures + rejected:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('{"version":"test"}' if name == "manifest.source.json" else "test fixture", encoding="utf-8")
            listing = ("\0".join(required + fixtures + rejected) + "\0").encode("utf-8")
            with patch.object(exporter.subprocess, "run", return_value=SimpleNamespace(stdout=listing)):
                result = exporter.export_source(root, root / "source.zip")
            with zipfile.ZipFile(result["path"]) as bundle:
                names = {name.removeprefix("sub2api-oai-basispoints-test/") for name in bundle.namelist()}
            self.assertEqual(names, set(required + fixtures))
            self.assertEqual(result["files"], len(required + fixtures))

if __name__ == "__main__":
    unittest.main()
