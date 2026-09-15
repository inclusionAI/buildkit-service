"""Run inside the fixture client Pod; assert behavior using upstream counters."""
import hashlib
import io
import json
import re
import socket
import sys
import time
import urllib.parse
import urllib.request
import zipfile

ORIGIN = 'http://fixture.e2e.svc.cluster.local'
CLIENT = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def fetch(url, method='GET', headers=None):
    try:
        with CLIENT.open(urllib.request.Request(url, method=method, headers=headers or {'Accept': '*/*'}), timeout=30) as response:
            return response.read()
    except urllib.error.URLError as error:
        raise RuntimeError(f'{method} {url}: {error}') from error


def check(condition, message):
    if not condition:
        raise RuntimeError(message)


def stats():
    return json.loads(fetch(ORIGIN + '/stats'))


def delta(before, path):
    return stats().get('GET ' + path, 0) - before.get('GET ' + path, 0)


def fetch_debian(path):
    # Absolute-form proxy request, independent of inherited NO_PROXY settings.
    request = urllib.request.Request('http://deb.debian.org' + path)
    request.set_proxy('apt-yum.e2e.svc.cluster.local', 'http')
    with CLIENT.open(request, timeout=30) as response:
        return response.read()


def fetch_distribution(path):
    if path.startswith('/debian/'):
        return fetch_debian(path)
    return fetch('http://apk.e2e.svc.cluster.local' + path)


def check_metadata_refresh():
    for path in ('/alpine/v3.22/main/x86_64/APKINDEX.tar.gz', '/debian/dists/bookworm/InRelease'):
        before = stats()
        first, second = fetch_distribution(path), fetch_distribution(path)
        check(first != second and delta(before, path) == 2, 'Metadata must refresh: ' + path)


def check_package_payload_cache():
    for path in ('/alpine/v3.22/main/x86_64/example-1.apk', '/debian/pool/main/e/example/example_1.deb'):
        before = stats()
        first, second = fetch_distribution(path), fetch_distribution(path)
        check(first == second and first == (path + ' immutable payload').encode(), 'Payload mismatch: ' + path)
        check(delta(before, path) == 1, 'Payload must reach upstream only once: ' + path)


def check_wheel_cache():
    before = stats()
    index = fetch('http://pip.e2e.svc.cluster.local/index/e2e-pkg/', headers={'Accept': 'text/html'}).decode()
    match = re.search(r'href="([^"]+\.whl[^\"]*)"', index)
    check(match is not None, 'Package index did not contain the fixture wheel')
    href = match.group(1)
    url = urllib.parse.urljoin('http://pip.e2e.svc.cluster.local/index/e2e-pkg/', href)
    payload = fetch(url)
    check(payload == fetch(url), 'Wheel cache contents differ')
    with zipfile.ZipFile(io.BytesIO(payload)) as wheel:
        check(wheel.read('e2e_pkg.py') == b'VALUE = "e2e-ok"\n', 'Cached wheel payload mismatch')
    check(delta(before, '/files/e2e_pkg-1.0-py3-none-any.whl') == 1, 'Wheel not cached')


def check_npm_cache():
    metadata = json.loads(fetch('http://npm.e2e.svc.cluster.local/e2e-pkg'))
    distribution = metadata['versions']['1.0.0']['dist']
    url = distribution['tarball']
    path = '/e2e-pkg/-/e2e-pkg-1.0.0.tgz'
    before = stats()
    payload = fetch(url)
    check(hashlib.sha1(payload).hexdigest() == distribution['shasum'], 'npm tarball digest mismatch')
    # Verdaccio streams the response before committing the tarball to disk.
    # Wait for a measured cache hit, then require stable hits without upstream IO.
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        snapshot = stats()
        check(fetch(url) == payload, 'npm cache contents differ')
        if delta(snapshot, path) == 0:
            break
        time.sleep(0.1)
    else:
        raise RuntimeError('npm tarball cache did not settle within 10 seconds')
    check(delta(before, path) >= 1, 'Fresh npm mirror did not fetch the fixture tarball')
    snapshot = stats()
    check(fetch(url) == fetch(url) == payload, 'npm cache contents differ')
    check(delta(snapshot, path) == 0, 'npm cache hit unexpectedly contacted upstream')


def check_git_cache(expected):
    # Git cache is a separate Deployment and survives the main mirror restart.
    before = stats()
    url = 'http://git.e2e.svc.cluster.local/cache/fixture.e2e.svc.cluster.local/repo.git/info/refs?service=git-upload-pack'
    first, second = fetch(url), fetch(url)
    check(first == second and b'refs/heads/main' in first, 'Git refs invalid')
    check(delta(before, '/repo.git/info/refs') == expected, 'Git cache unexpectedly refetched')


def check_registry_layer_cache(expected):
    # Registry mirror must cache immutable layers, not merely return the same bytes.
    headers = {'Accept': 'application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'}
    base = 'http://registry-fixture.e2e.svc.cluster.local/v2/base/'
    with CLIENT.open(urllib.request.Request(base + 'manifests/seed', headers=headers), timeout=30) as response:
        manifest = json.load(response)
    layer = manifest['layers'][0]
    path = '/v2/base/blobs/' + layer['digest']
    before = stats()
    first, second = fetch(base + 'blobs/' + layer['digest']), fetch(base + 'blobs/' + layer['digest'])
    check(first == second and len(first) == layer['size'], 'Registry layer bytes mismatch')
    check('sha256:' + hashlib.sha256(first).hexdigest() == layer['digest'], 'Registry layer digest mismatch')
    check(delta(before, path) == expected, 'Registry layer cache unexpectedly refetched')


def wait_for_mirror_routes(timeout=60):
    """Wait for client-visible Service routing without warming package caches."""
    deadline = time.monotonic() + timeout
    for service in ('apk', 'apt-yum', 'pip', 'npm', 'git', 'registry-fixture'):
        host = service + '.e2e.svc.cluster.local'
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise RuntimeError(f'Mirror Service {host}:80 did not become reachable within {timeout}s')
            try:
                with socket.create_connection((host, 80), timeout=min(2, remaining)):
                    break
            except OSError:
                time.sleep(min(0.2, max(0, deadline - time.monotonic())))


def main():
    after_restart = '--after-restart' in sys.argv
    print('Waiting for client-visible mirror Service routes', flush=True)
    wait_for_mirror_routes()
    for check_cache in (check_metadata_refresh, check_package_payload_cache,
                        check_wheel_cache, check_npm_cache):
        print('Checking ' + check_cache.__name__, flush=True)
        check_cache()
    # These separate Deployments keep their caches when the main mirror restarts.
    expected_fetches = 0 if after_restart else 1
    check_git_cache(expected_fetches)
    check_registry_layer_cache(expected_fetches)
    print(json.dumps({'result': 'PASS', 'after_restart': after_restart,
                      'upstream_counts': stats()}, indent=2))


if __name__ == '__main__':
    main()
