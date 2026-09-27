#!/usr/bin/env python3
"""Test relay failure classification using real host code and a Go overlay.

Usage: python tools/host-relay-integration/run.py --host-source <Sub2API>
Uses synthetic streams without credentials, services, or host source edits.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host-source', required=True, type=Path)
    parser.add_argument('--go', default='go')
    parser.add_argument('--expected-host-version', default='0.2.8')
    args = parser.parse_args()
    here = Path(__file__).resolve().parent
    host = args.host_source.resolve() / 'backend'
    version = (host / 'cmd/server/VERSION').read_text(encoding='utf-8').strip()
    if version != args.expected_host_version:
        parser.error(f'host version must be {args.expected_host_version}; got {version}')
    watched = [host / path for path in [
        'go.mod', 'go.sum',
        'internal/service/openai_gateway_passthrough.go',
        'internal/service/openai_gateway_response_handling.go',
        'internal/handler/openai_gateway_handler.go',
    ]]
    before = {str(path): digest(path) for path in watched}
    target = host / 'internal/service/zz_basispoints_relay_scope_integration_test.go'
    if target.exists():
        parser.error(f'overlay target must not exist: {target}')
    build = here.parent.parent / 'build'
    build.mkdir(exist_ok=True)
    print(f'Host under test: {host.parent} (version {version})', flush=True)
    print('Host SHA256: ' + json.dumps(before, indent=2), flush=True)
    env = dict(os.environ, GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local', GOWORK='off')
    try:
        with tempfile.TemporaryDirectory(prefix='host-relay-scope-', dir=build) as scratch:
            overlay = Path(scratch) / 'overlay.json'
            overlay.write_text(json.dumps({'Replace': {str(target): str(here / 'relay_scope_host_test.go.txt')}}), encoding='utf-8')
            command = [args.go, 'test', '-mod=readonly', '-overlay', str(overlay),
                       '-count=1', '-timeout=120s', '-run', '^TestBasispointsRelayScope',
                       '-v', './internal/service']
            print('+ ' + ' '.join(command), flush=True)
            subprocess.run(command, cwd=host, env=env, check=True)
    finally:
        after = {str(path): digest(path) for path in watched}
        if after != before or target.exists():
            raise RuntimeError('host source changed during the overlay suite')
    print(f'PASS: actual host {version} classifiers and SSE readers; watched host files unchanged.')


if __name__ == '__main__':
    main()
