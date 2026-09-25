import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("export_source", Path(__file__).with_name("export_source.py"))
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)

class SourceExportBoundaryTests(unittest.TestCase):
    def test_source_and_declared_contract_are_preserved(self):
        for name in ("README.md", "manifest.source.json", "go.mod", "go.sum",
                     ".github/workflows/ci.yml", "internal/attachments/cache_regression_test.go",
                     "third_party/sub2api/LICENSE", "third_party/sub2api/go.mod",
                     "third_party/sub2api/pkg/pluginapi/v1/plugin.pb.go",
                     "docs/使用说明.md", "tools/test_export_source.py", "ui/index.html"):
            with self.subTest(name=name):
                self.assertTrue(exporter.source_path_allowed(name))

    def test_build_outputs_keys_and_credentials_are_excluded_even_if_tracked(self):
        for name in ("dist/plugin.s2plugin", "build/key.private", ".git/config",
                     "tools/publisher.private", "docs/key.pem", ".env", "ui/.env.local",
                     "internal/auth.json", "ui/credentials.json", "tools/accounts-export.json",
                     "docs/settings.local.json", "tools/__pycache__/script.py",
                     "tools/node_modules/index.js", "/outside.go", "../README.md"):
            with self.subTest(name=name):
                self.assertFalse(exporter.source_path_allowed(name))

if __name__ == "__main__":
    unittest.main()
