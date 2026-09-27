#!/usr/bin/env python3
"""Run real plugin processes through an unmodified pinned host service package.

Usage: python tools/host-diagnostic-integration/run.py --host-source <Sub2API>
The host Go overlay adds only tests; the pinned host service is unchanged.
A separate test-only plugin overlay confines native diagnostics to the local
fixture. This verifies source integration, not a release binary or installation.
Accounts, persistence, and upstream responses are synthetic local fixtures.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

BASELINE = "a3eb7ef302961cba716dc78b39b93b60c467db0e"
NATIVE_ENDPOINT_DECLARATION = (
    'const nativeDegradationResponsesURL = '
    '"https://chatgpt.com/backend-api/codex/responses"'
)
NATIVE_FIXTURE_ENV = "BPS_DIAGNOSTIC_NATIVE_FIXTURE_URL"
SOURCE_OVERLAY_MODE = "loopback-v1"
NATIVE_FIXTURE_HELPER = r'''// Only compiled by the source integration test's explicit Go overlay.
package transport

import (
    "net/url"
    "os"
    "strconv"
)

func nativeDegradationFixtureURL() string {
    raw := os.Getenv("BPS_DIAGNOSTIC_NATIVE_FIXTURE_URL")
    endpoint, err := url.Parse(raw)
    if err != nil || endpoint == nil || endpoint.Scheme != "http" ||
        endpoint.Hostname() != "127.0.0.1" || endpoint.User != nil ||
        endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.ForceQuery ||
        endpoint.Fragment != "" || endpoint.Opaque != "" ||
        endpoint.Host != "127.0.0.1:" + endpoint.Port() {
        panic("source integration requires an explicit local native fixture origin")
    }
    port, err := strconv.Atoi(endpoint.Port())
    if err != nil || port < 1 || port > 65535 {
        panic("source integration requires a valid local native fixture port")
    }
    return raw + "/backend-api/codex/responses"
}
'''


def native_fixture_overlay(plugin, scratch):
    """Create an isolated source-only build; fail closed if its anchor changes."""
    original = plugin / "internal/transport/native_degradation.go"
    source = original.read_text(encoding="utf-8")
    if source.count(NATIVE_ENDPOINT_DECLARATION) != 1:
        raise RuntimeError("native diagnostic endpoint changed; refusing an unisolated integration build")
    replacement = scratch / "native_degradation_fixture.go"
    replacement.write_text(source.replace(
        NATIVE_ENDPOINT_DECLARATION,
        "var nativeDegradationResponsesURL = nativeDegradationFixtureURL()",
    ), encoding="utf-8")
    helper = scratch / "native_degradation_fixture_helper.go"
    helper.write_text(NATIVE_FIXTURE_HELPER, encoding="utf-8")
    overlay = scratch / "plugin-native-fixture-overlay.json"
    overlay.write_text(json.dumps({"Replace": {
        str(original.resolve()): str(replacement.resolve()),
        str((plugin / "internal/transport/zz_native_degradation_fixture.go").resolve()):
        str(helper.resolve()),
    }}), encoding="utf-8")
    return overlay

def run(args, cwd, env=None):
    print("+ " + " ".join(map(str, args)), flush=True)
    subprocess.run(args, cwd=cwd, env=env, check=True)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host-source", required=True, type=Path)
    parser.add_argument("--go", default="go")
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--plugin-binary", type=Path,
                        help="Unsupported: a release executable cannot safely redirect its fixed native diagnostic endpoint")
    args = parser.parse_args()
    if args.plugin_binary is not None:
        parser.error("--plugin-binary is unsupported for native diagnostic fixtures: a release executable "
                     "has a fixed OpenAI endpoint and must not receive synthetic fixture credentials. "
                     "Omit this option to run the explicitly overlaid source integration test; "
                     "that mode does not verify a release binary or package installation.")
    here = Path(__file__).resolve().parent
    plugin = here.parent.parent
    host = args.host_source.resolve()
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=host, text=True).strip()
    if actual != BASELINE:
        parser.error(f"host HEAD must be {BASELINE}; got {actual}")
    dirty = subprocess.check_output(["git", "status", "--porcelain", "--", "backend"], cwd=host, text=True)
    if dirty.strip():
        parser.error("host backend must be a clean baseline checkout")
    manifest = json.loads((plugin / "manifest.source.json").read_text(encoding="utf-8"))
    print(f"Plugin under test: {manifest['id']} {manifest['version']}", flush=True)
    with tempfile.TemporaryDirectory(prefix="bps-unpatched-host-") as scratch:
        scratch = Path(scratch)
        # Export Git objects, so sparse checkouts and generated local patches
        # cannot accidentally become the host implementation under test.
        archive = scratch / "host.tar"
        run(["git", "archive", "--format=tar", "-o", str(archive), BASELINE, "backend"], host)
        exported = scratch / "host"
        exported.mkdir()
        with tarfile.open(archive) as source:
            source.extractall(exported, filter="data")
        binary = scratch / ("oai-basispoints.exe" if os.name == "nt" else "oai-basispoints")
        plugin_overlay = native_fixture_overlay(plugin, scratch)
        print("Test-only native fixture overlay enabled; this is not release binary/package verification.", flush=True)
        build = [args.go, "build", "-overlay", str(plugin_overlay)]
        if args.race:
            build.append("-race")
        run([*build, "-o", str(binary), "./cmd/oai-basispoints"], plugin)
        print(f"Plugin SHA256: {hashlib.sha256(binary.read_bytes()).hexdigest()}", flush=True)
        overlay = scratch / "overlay.json"
        overlay.write_text(json.dumps({"Replace": {
            str(exported / "backend/internal/service/zz_basispoints_diagnostic_integration_test.go"):
            str(here / "diagnostic_jobs_host_test.go.txt")
        }}), encoding="utf-8")
        env = dict(os.environ, BPS_DIAGNOSTIC_PLUGIN_BINARY=str(binary),
                   BPS_DIAGNOSTIC_PLUGIN_ID=manifest["id"],
                   BPS_DIAGNOSTIC_PLUGIN_VERSION=manifest["version"],
                   BPS_DIAGNOSTIC_SOURCE_OVERLAY=SOURCE_OVERLAY_MODE)
        env.pop(NATIVE_FIXTURE_ENV, None)  # Each harness supplies its own server origin.
        command = [args.go, "test", "-overlay", str(overlay), "-count=1", "-timeout=120s",
                   "-run", "^TestBasispointsUnpatchedHostDiagnostics$", "-v"]
        if args.race:
            command.append("-race")
        run([*command, "./internal/service"], exported / "backend", env)
    after = subprocess.check_output(["git", "status", "--porcelain", "--", "backend"], cwd=host, text=True)
    if after != dirty:
        raise RuntimeError("host backend checkout changed during the test")
    print(f"PASS: source integration with local native fixture at host {BASELINE}; checkout unchanged.")

if __name__ == "__main__":
    main()
