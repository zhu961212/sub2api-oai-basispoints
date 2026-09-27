#!/usr/bin/env python3
"""Run real plugin processes through an unmodified pinned host service package.

Usage: python tools/host-diagnostic-integration/run.py --host-source <Sub2API>
The Go overlay adds only tests. No host source or production service is changed.
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

def run(args, cwd, env=None):
    print("+ " + " ".join(map(str, args)), flush=True)
    subprocess.run(args, cwd=cwd, env=env, check=True)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host-source", required=True, type=Path)
    parser.add_argument("--go", default="go")
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--plugin-binary", type=Path,
                        help="Use an already extracted plugin executable instead of building one; does not test package installation")
    args = parser.parse_args()
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
        if args.plugin_binary:
            binary = args.plugin_binary.resolve()
            if not binary.is_file():
                parser.error(f"plugin executable not found: {binary}")
            print(f"Using supplied plugin executable: {binary} (package installation is not tested)", flush=True)
        else:
            binary = scratch / ("oai-basispoints.exe" if os.name == "nt" else "oai-basispoints")
            build = [args.go, "build"]
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
                   BPS_DIAGNOSTIC_PLUGIN_VERSION=manifest["version"])
        command = [args.go, "test", "-overlay", str(overlay), "-count=1", "-timeout=120s",
                   "-run", "^TestBasispointsUnpatchedHostDiagnostics$", "-v"]
        if args.race:
            command.append("-race")
        run([*command, "./internal/service"], exported / "backend", env)
    after = subprocess.check_output(["git", "status", "--porcelain", "--", "backend"], cwd=host, text=True)
    if after != dirty:
        raise RuntimeError("host backend checkout changed during the test")
    print(f"PASS: original host pipeline at {BASELINE}; checkout unchanged.")

if __name__ == "__main__":
    main()
