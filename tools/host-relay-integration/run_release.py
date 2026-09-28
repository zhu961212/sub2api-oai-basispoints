#!/usr/bin/env python3
"""Verify a signed package against an exact official host Git tree.

Uses test-only Go overlays and synthetic local identities/upstreams. The host
release version is explicit because upstream tags can retain an older VERSION
file; it is supplied to the real installer and compatibility evaluator. Native
account diagnostics are excluded because their release endpoint is fixed.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import zipfile


OFFICIAL_HOSTS = {
    '0.2.8': 'fd80b08c90b55edcad5b00171b53f08721d30da1',
    '0.2.9': '4c00df2e0183e2c70b7fa8ba45914205e36aad0c',
}


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def verify_host_tree(repo, host, commit):
    listing = subprocess.check_output(
        ['git', 'ls-tree', '-r', '-z', commit, '--', 'backend'], cwd=repo)
    expected = {}
    normalized = []
    for entry in listing.split(b'\0'):
        if not entry:
            continue
        metadata, raw_name = entry.split(b'\t', 1)
        mode, kind, object_id = metadata.decode().split()
        if kind != 'blob' or mode not in ('100644', '100755'):
            raise ValueError(f'unsupported host tree entry: {entry!r}')
        name = raw_name.decode('utf-8').removeprefix('backend/')
        path = host / name
        if not path.is_file() or path.is_symlink():
            raise ValueError(f'missing or unsafe host file: {path}')
        data = path.read_bytes()
        actual = hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()
        if actual != object_id:
            lf_data = data.replace(b'\r\n', b'\n')
            lf_hash = hashlib.sha1(b'blob ' + str(len(lf_data)).encode() + b'\0' + lf_data).hexdigest()
            if lf_hash != object_id:
                raise ValueError(f'host file differs from pinned Git tree: {path}')
            normalized.append(name)
        expected[name] = hashlib.sha256(data).hexdigest()
    actual_names = {path.relative_to(host).as_posix() for path in host.rglob('*') if path.is_file()}
    if actual_names != set(expected):
        raise ValueError(f'host tree contains unexpected/missing files: {sorted(actual_names ^ set(expected))}')
    return expected, normalized


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host-source', required=True, type=Path)
    parser.add_argument('--host-version', required=True, choices=sorted(OFFICIAL_HOSTS))
    parser.add_argument('--host-ref', required=True, help='locally fetched official tag/ref/commit')
    parser.add_argument('--package', required=True, type=Path)
    parser.add_argument('--public-key', required=True, type=Path)
    parser.add_argument('--proof', required=True, type=Path)
    parser.add_argument('--go', default='go')
    args = parser.parse_args()
    here = Path(__file__).resolve().parent
    repo = here.parent.parent
    host = args.host_source.resolve() / 'backend'
    package = args.package.resolve()
    key = args.public_key.resolve()
    commit = subprocess.check_output(['git', 'rev-parse', args.host_ref + '^{commit}'], cwd=repo, text=True).strip()
    if commit != OFFICIAL_HOSTS[args.host_version]:
        parser.error(f'{args.host_version} requires official commit {OFFICIAL_HOSTS[args.host_version]}; got {commit}')
    before, normalized_files = verify_host_tree(repo, host, commit)
    version_file = (host / 'cmd/server/VERSION').read_text(encoding='utf-8').strip()
    source_head = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()
    source_dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=repo, text=True).strip())
    subprocess.run([sys.executable, '-X', 'utf8', str(repo / 'tools/verify_package.py'), str(package),
                    '--public-key', str(key), '--require-signature', '--expected-key-id', 'oai-basispoints-v1'], check=True)
    member = 'runtimes/windows-amd64/oai-basispoints.exe' if sys.platform == 'win32' else 'runtimes/linux-amd64/oai-basispoints'
    targets = {
        str(host / 'internal/service/zz_basispoints_signed_release_test.go'): str(here / 'signed_release_host_test.go.txt'),
        str(host / 'internal/service/zz_signed_40874_test.go'): str(here / 'signed_package_host_test.go.txt'),
        str(host / 'internal/service/zz_bps_diagnostic_fixture_test.go'): str(repo / 'tools/host-diagnostic-integration/diagnostic_jobs_host_test.go.txt'),
    }
    if any(Path(path).exists() for path in targets):
        parser.error('overlay targets must not already exist')
    args.proof.parent.mkdir(parents=True, exist_ok=True)
    print(f'Official host {args.host_version}: {commit}; source VERSION={version_file}; all {len(before)} backend files verified.', flush=True)
    with tempfile.TemporaryDirectory(prefix='signed-release-', dir=repo / 'build') as temporary:
        scratch = Path(temporary)
        with zipfile.ZipFile(package) as archive:
            manifest = json.loads(archive.read('manifest.json'))
            binary = scratch / Path(member).name
            binary.write_bytes(archive.read(member))
            binary.chmod(0o700)
        overlay = scratch / 'overlay.json'
        overlay.write_text(json.dumps({'Replace': targets}), encoding='utf-8')
        env = dict(os.environ, GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local', GOWORK='off',
                   BPS_REPLAY_BINARY=str(binary), BPS_REPLAY_VERSION=manifest['version'],
                   BPS_HOST_TEST_PACKAGE=str(package), BPS_HOST_TEST_PUBLIC_KEY=str(key),
                   BPS_HOST_TEST_VERSION=manifest['version'], BPS_HOST_TEST_HOST_VERSION=args.host_version)
        command = [args.go, 'test', '-mod=readonly', '-overlay', str(overlay), '-count=1', '-timeout=180s',
                   '-run', '^(TestBasisPointsSignedReleaseOfficialHostInstall|TestSigned40874HostReplay)$', '-v', './internal/service']
        print('+ ' + ' '.join(command), flush=True)
        result = subprocess.run(command, cwd=host, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        sys.stdout.buffer.write(result.stdout)
        unchanged = (before, normalized_files) == verify_host_tree(repo, host, commit)
        proof = {
            'version': manifest['version'], 'host_version': args.host_version, 'host_ref': commit,
            'host_ref_input': args.host_ref, 'host_source_version_file': version_file,
            'host_source': str(host.parent), 'host_files': before, 'host_files_unchanged': unchanged,
            'host_crlf_normalized_files': normalized_files,
            'source_head': source_head, 'source_dirty': source_dirty,
            'package_sha256': sha(package), 'runtime_member': member, 'runtime_sha256': sha(binary),
            'command': command, 'exit_code': result.returncode,
            'result': 'passed' if result.returncode == 0 and unchanged else 'failed',
            'limitations': ['Synthetic local accounts and upstreams only; no real account or paid request.',
                            'Native/background diagnostics excluded from the signed binary run.',
                            'Only the current platform runtime is executed; other package files are signature/hash verified.'],
        }
    args.proof.write_text(json.dumps(proof, indent=2), encoding='utf-8')
    args.proof.with_suffix('.log').write_bytes(result.stdout)
    if not unchanged:
        raise RuntimeError('host source changed during signed release verification')
    raise SystemExit(result.returncode)


if __name__ == '__main__':
    main()
