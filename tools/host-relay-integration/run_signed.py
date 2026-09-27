#!/usr/bin/env python3
"""Replay #40874 through a verified package and the original host code."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import zipfile


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host-source', type=Path, required=True)
    parser.add_argument('--package', type=Path, required=True)
    parser.add_argument('--public-key', type=Path, required=True)
    parser.add_argument('--proof', type=Path)
    args = parser.parse_args()
    here = Path(__file__).resolve().parent
    repo = here.parent.parent
    host = args.host_source.resolve() / 'backend'
    package = args.package.resolve()
    key = args.public_key.resolve()
    assert (host / 'cmd/server/VERSION').read_text().strip() == '0.2.8'
    subprocess.run([sys.executable, '-X', 'utf8', str(repo / 'tools/verify_package.py'), str(package), '--public-key', str(key), '--require-signature', '--expected-key-id', 'oai-basispoints-v1'], check=True)
    member = 'runtimes/windows-amd64/oai-basispoints.exe' if sys.platform == 'win32' else 'runtimes/linux-amd64/oai-basispoints'
    watched = [host / name for name in ['go.mod', 'go.sum', 'internal/service/openai_gateway_passthrough.go', 'internal/service/openai_gateway_response_handling.go', 'internal/service/plugin_runtime.go']]
    before = {str(path): sha(path) for path in watched}
    targets = {
        str(host / 'internal/service/zz_signed_40874_test.go'): str(here / 'signed_package_host_test.go.txt'),
        str(host / 'internal/service/zz_bps_diagnostic_fixture_test.go'): str(repo / 'tools/host-diagnostic-integration/diagnostic_jobs_host_test.go.txt'),
    }
    assert all(not Path(path).exists() for path in targets)
    (repo / 'build').mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='signed-relay-', dir=repo / 'build') as scratch:
        scratch = Path(scratch)
        with zipfile.ZipFile(package) as archive:
            manifest = json.loads(archive.read('manifest.json'))
            binary = scratch / Path(member).name
            binary.write_bytes(archive.read(member))
            binary.chmod(0o700)
        overlay = scratch / 'overlay.json'
        overlay.write_text(json.dumps({'Replace': targets}), encoding='utf-8')
        env = dict(os.environ, GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local', GOWORK='off', BPS_REPLAY_BINARY=str(binary), BPS_REPLAY_VERSION=manifest['version'])
        command = ['go', 'test', '-mod=readonly', '-overlay', str(overlay), '-count=1', '-timeout=120s', '-run', '^TestSigned40874HostReplay$', '-v', './internal/service']
        result = subprocess.run(command, cwd=host, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        sys.stdout.buffer.write(result.stdout)
        proof = {'version': manifest['version'], 'package_sha256': sha(package), 'runtime_sha256': sha(binary), 'host_files': before, 'source_head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip(), 'exit_code': result.returncode}
    assert before == {str(path): sha(path) for path in watched}
    assert all(not Path(path).exists() for path in targets)
    if args.proof:
        args.proof.parent.mkdir(parents=True, exist_ok=True)
        args.proof.write_text(json.dumps(proof, indent=2), encoding='utf-8')
        args.proof.with_suffix('.log').write_bytes(result.stdout)
    raise SystemExit(result.returncode)


if __name__ == '__main__':
    main()
