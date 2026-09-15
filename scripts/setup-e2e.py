#!/usr/bin/env python3
"""Download and verify E2E tools into the repository's ignored cache."""
import hashlib
import fcntl
import io
import json
from pathlib import Path
import platform
import tarfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def host_platform():
    systems = {'Darwin': 'darwin', 'Linux': 'linux'}
    architectures = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64'}
    system, machine = platform.system(), platform.machine()
    if system not in systems or machine not in architectures:
        raise RuntimeError(f'Unsupported E2E tool host: {system}/{machine}')
    return systems[system], architectures[machine]


def verified_download(name, entry, cache, os_name, arch):
    version = entry['version']
    key = os_name + '-' + arch
    expected = entry['sha256'][key]
    archive_path = cache / 'downloads' / (name + '-' + version + '-' + key)
    payload = archive_path.read_bytes() if archive_path.exists() else None
    if payload is not None and hashlib.sha256(payload).hexdigest() == expected:
        return payload
    print('Downloading ' + name + ' ' + version, flush=True)
    url = entry['url'].format(version=version, os=os_name, arch=arch)
    with urllib.request.urlopen(url, timeout=90) as response:
        payload = response.read()
    if hashlib.sha256(payload).hexdigest() != expected:
        raise RuntimeError(f'Checksum mismatch: {name}; source: {url}; cache: {archive_path}')
    temporary = archive_path.with_suffix('.tmp')
    temporary.write_bytes(payload)
    temporary.replace(archive_path)
    return payload


def install_binary(name, payload, destination, os_name, arch):
    if name == 'helm':
        with tarfile.open(fileobj=io.BytesIO(payload)) as archive:
            # Read one verified member; never extract arbitrary archive paths.
            payload = archive.extractfile(os_name + '-' + arch + '/helm').read()
    if destination.is_symlink() or not destination.exists() or destination.read_bytes() != payload:
        temporary = destination.with_suffix('.tmp')
        temporary.write_bytes(payload)
        temporary.chmod(0o755)
        temporary.replace(destination)
    destination.chmod(0o755)


def main():
    os_name, arch = host_platform()
    tools = json.loads((ROOT / 'tests/e2e/dependencies.json').read_text())['tools']
    cache = ROOT / 'artifacts/e2e-tools'
    (cache / 'bin').mkdir(parents=True, exist_ok=True)
    (cache / 'downloads').mkdir(exist_ok=True)
    # Independent run directories share this cache; publish tools one setup at a time.
    with (cache / '.setup.lock').open('a') as setup_lock:
        fcntl.flock(setup_lock, fcntl.LOCK_EX)
        for name, entry in tools.items():
            payload = verified_download(name, entry, cache, os_name, arch)
            install_binary(name, payload, cache / 'bin' / name, os_name, arch)
            print('Verified ' + name + ' ' + entry['version'], flush=True)


if __name__ == '__main__':
    main()
