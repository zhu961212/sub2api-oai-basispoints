"""Check native diagnostic fixture isolation without compiling or sending requests."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
ENTRY = ROOT / "tools/host-diagnostic-integration/run.py"
spec = importlib.util.spec_from_file_location("diagnostic_integration_runner", ENTRY)
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class NativeDiagnosticIsolationTests(unittest.TestCase):
    def test_overlay_replaces_only_the_source_endpoint_without_editing_project(self):
        source = ROOT / "internal/transport/native_degradation.go"
        before = source.read_bytes()
        with tempfile.TemporaryDirectory() as temporary:
            overlay = runner.native_fixture_overlay(ROOT, Path(temporary))
            replacements = json.loads(overlay.read_text(encoding="utf-8"))["Replace"]
            self.assertEqual(len(replacements), 2)
            replaced = Path(replacements[str(source.resolve())]).read_text(encoding="utf-8")
            self.assertNotIn("https://chatgpt.com/", replaced)
            self.assertIn("var nativeDegradationResponsesURL = nativeDegradationFixtureURL()", replaced)
            self.assertEqual(replaced, source.read_text(encoding="utf-8").replace(
                runner.NATIVE_ENDPOINT_DECLARATION,
                "var nativeDegradationResponsesURL = nativeDegradationFixtureURL()"))
            virtual = ROOT / "internal/transport/zz_native_degradation_fixture.go"
            self.assertFalse(virtual.exists())
            helper = Path(replacements[str(virtual.resolve())]).read_text(encoding="utf-8")
            self.assertEqual(helper, runner.NATIVE_FIXTURE_HELPER)
            self.assertTrue(all(Path(path).is_relative_to(temporary) for path in replacements.values()))
        self.assertEqual(source.read_bytes(), before)

    def test_changed_or_ambiguous_endpoint_fails_before_an_overlay_is_created(self):
        for text in ("package transport", runner.NATIVE_ENDPOINT_DECLARATION * 2):
            with self.subTest(source=text), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                original = root / "internal/transport/native_degradation.go"
                original.parent.mkdir(parents=True)
                original.write_text(text, encoding="utf-8")
                scratch = root / "scratch"
                scratch.mkdir()
                with self.assertRaisesRegex(RuntimeError, "refusing an unisolated"):
                    runner.native_fixture_overlay(root, scratch)
                self.assertEqual(list(scratch.iterdir()), [])

    def test_test_only_helper_has_no_public_endpoint_fallback(self):
        helper = runner.NATIVE_FIXTURE_HELPER
        for guard in ('os.Getenv("BPS_DIAGNOSTIC_NATIVE_FIXTURE_URL")',
                      'endpoint.Scheme != "http"', 'endpoint.Hostname() != "127.0.0.1"',
                      'endpoint.Host != "127.0.0.1:" + endpoint.Port()',
                      'endpoint.User != nil', 'endpoint.Path != ""',
                      'endpoint.RawQuery != ""', 'endpoint.ForceQuery',
                      'endpoint.Fragment != ""', 'endpoint.Opaque != ""',
                      'port < 1 || port > 65535'):
            self.assertIn(guard, helper)
        self.assertEqual(helper.count("panic("), 2)
        self.assertEqual(helper.count("return "), 1)
        self.assertIn('return raw + "/backend-api/codex/responses"', helper)
        self.assertNotIn("chatgpt.com", helper)
        self.assertNotIn("responses_url", helper)
        production = (ROOT / "internal/transport/native_degradation.go").read_text(encoding="utf-8")
        self.assertNotIn(runner.NATIVE_FIXTURE_ENV, production)
        self.assertIn(runner.NATIVE_ENDPOINT_DECLARATION, production)

    def test_prebuilt_binary_is_rejected_before_git_build_or_execution(self):
        argv = [str(ENTRY), "--host-source", "unused-host", "--plugin-binary", "release.exe"]
        stderr = io.StringIO()
        with (patch.object(sys, "argv", argv),
              patch.object(runner.subprocess, "check_output") as git,
              patch.object(runner, "run") as execute, contextlib.redirect_stderr(stderr)):
            with self.assertRaises(SystemExit) as error:
                runner.main()
        self.assertEqual(error.exception.code, 2)
        git.assert_not_called()
        execute.assert_not_called()
        self.assertIn("--plugin-binary is unsupported", stderr.getvalue())
        self.assertIn("must not receive synthetic fixture credentials", stderr.getvalue())

    def test_fixture_sets_loopback_environment_before_spawning_child(self):
        fixture = (ENTRY.parent / "diagnostic_jobs_host_test.go.txt").read_text(encoding="utf-8")
        setting = 't.Setenv("BPS_DIAGNOSTIC_NATIVE_FIXTURE_URL", h.upstream.URL)'
        self.assertEqual(fixture.count(setting), 1)
        self.assertLess(fixture.index(setting), fixture.index("h.start()"))
        self.assertIn('require.Equal(t, "loopback-v1", os.Getenv("BPS_DIAGNOSTIC_SOURCE_OVERLAY")', fixture)
        self.assertIn('r.URL.Path != "/backend-api/codex/responses"', fixture)
        self.assertIn('[]bpsDiagnosticRequest{{"synthetic-account-7", "gpt-5.4"}}', fixture)

    def test_source_execution_always_builds_with_overlay_and_clears_inherited_origin(self):
        for race in (False, True):
            with self.subTest(race=race), tempfile.TemporaryDirectory() as temporary:
                calls = []

                def fake_run(command, cwd, env=None):
                    calls.append((command, cwd, env))
                    if command[:2] == ["git", "archive"]:
                        with tarfile.open(command[command.index("-o") + 1], "w"):
                            pass
                    elif command[:2] == ["go", "build"]:
                        self.assertIn("-overlay", command)
                        overlay = Path(command[command.index("-overlay") + 1])
                        self.assertEqual(len(json.loads(overlay.read_text())["Replace"]), 2)
                        self.assertEqual("-race" in command, race)
                        Path(command[command.index("-o") + 1]).write_bytes(b"synthetic isolated binary")
                    elif command[:2] == ["go", "test"]:
                        self.assertIn("-overlay", command)
                        self.assertEqual(env["BPS_DIAGNOSTIC_SOURCE_OVERLAY"], "loopback-v1")
                        self.assertNotIn(runner.NATIVE_FIXTURE_ENV, env)
                        self.assertEqual("-race" in command, race)
                    else:
                        self.fail("unexpected execution requested")

                argv = [str(ENTRY), "--host-source", temporary] + (["--race"] if race else [])
                with (patch.object(sys, "argv", argv),
                      patch.object(runner.subprocess, "check_output", side_effect=[runner.BASELINE, "", ""]),
                      patch.object(runner, "run", side_effect=fake_run),
                      patch.dict(os.environ, {runner.NATIVE_FIXTURE_ENV: "https://outside.invalid"}),
                      contextlib.redirect_stdout(io.StringIO())):
                    runner.main()
                self.assertEqual([call[0][:2] for call in calls],
                                 [["git", "archive"], ["go", "build"], ["go", "test"]])


if __name__ == "__main__":
    unittest.main()
