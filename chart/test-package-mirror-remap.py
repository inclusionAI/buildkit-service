#!/usr/bin/env python3
"""Exercise rendered apt-cacher-ng remaps against a changing local upstream.

Requires Docker and Python 3.12+. Helm runs locally when available, otherwise in a
container. No cluster, credentials, or public package repositories are used.
"""
import argparse
import io
import itertools
import os
import sys
import tarfile
import pathlib
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from unittest.mock import patch

CHART = pathlib.Path(__file__).resolve().parent
ORIGIN = r'''
from http.server import BaseHTTPRequestHandler, HTTPServer
counts = {}
class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def do_GET(self):
        print('ORIGIN', self.path, self.headers.get_all('Host'), flush=True)
        if self.path != '/__health' and self.headers.get_all('Host') not in (['origin.test'], ['origin.test:80']):
            self.send_error(400, 'unexpected upstream Host')
            return
        counts[self.path] = counts.get(self.path, 0) + 1
        data = (self.path + ' version ' + str(counts[self.path])).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(data)))
        self.send_header('Content-Type', 'application/octet-stream')
        self.end_headers()
        self.wfile.write(data)
    def log_message(self, *args):
        pass
HTTPServer(('0.0.0.0', 80), Handler).serve_forever()
'''


class FixedHTTPProxy(urllib.request.ProxyHandler):
    """Route fixture requests through the explicit proxy regardless of NO_PROXY."""

    def proxy_open(self, request, proxy, proxy_type):
        request.set_proxy(urllib.parse.urlsplit(proxy).netloc, proxy_type)


def run(*args):
    return subprocess.check_output(args, text=True, timeout=600).strip()


def config_value(rendered, key):
    marker = '  ' + key + ': |\n'
    block = rendered.split(marker, 1)[1]
    result = []
    for line in block.splitlines():
        if line and not line.startswith('    '):
            break
        result.append(line[4:])
    return '\n'.join(result) + '\n'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--chart-dir', type=pathlib.Path, default=CHART)
    parser.add_argument('--baseline-ref', help='also require the unmodified chart at this Git ref to fail metadata tests')
    args = parser.parse_args()
    if args.baseline_ref:
        with tempfile.TemporaryDirectory(prefix='mirror-baseline-') as baseline:
            archive = subprocess.check_output(['git', '-C', str(CHART.parent),
                                               'archive', args.baseline_ref, 'chart'])
            with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
                tar.extractall(baseline, filter='data')
            result = subprocess.run([sys.executable, str(pathlib.Path(__file__).resolve()),
                                     '--chart-dir', str(pathlib.Path(baseline) / 'chart')],
                                    text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            if result.returncode != 2 or result.stdout.splitlines().count('PASS expected baseline failure') != 1:
                raise AssertionError('baseline did not reproduce both metadata failures\n' + result.stdout)
            print('\n'.join(line for line in result.stdout.splitlines()
                            if line.startswith(('PASS ', 'FAIL ', 'ORIGIN '))), flush=True)
            print('PASS negative control:', args.baseline_ref, flush=True)
    chart_dir = args.chart_dir.resolve()
    name = 'mirror-remap-' + uuid.uuid4().hex[:12]
    cache, origin = name + '-cache', name + '-origin'
    image = name + ':test'
    try:
        with tempfile.TemporaryDirectory(prefix=name) as temp:
            work = pathlib.Path(temp)
            helm = ['helm'] if shutil.which('helm') else [
                'docker', 'run', '--rm', '-v', str(chart_dir) + ':/chart:ro',
                'alpine/helm:3.17.3']
            chart = str(chart_dir) if helm == ['helm'] else '/chart'
            rendered = run(*helm, 'template', 'test', chart,
                           '--set', 'buildkit.enabled=false',
                           '--set', 'packageMirror.aptYum.env.aptMirror=http://origin.test',
                           '--set', 'packageMirror.aptYum.env.alpineMirror=http://origin.test/alpine',
                           '--show-only', 'templates/package-mirror-configmap.yaml')
            for key in ('acng-package-mirror.conf', 'backends_debian', 'backends_ubuntu'):
                (work / key).write_text(config_value(rendered, key))
            run('docker', 'build', '-q', '-t', image, str(chart_dir / 'images/apt-cacher-ng'))
            run('docker', 'network', 'create', name)
            run('docker', 'run', '-d', '--name', origin, '--network', name,
                '--network-alias', 'origin.test', '-p', '127.0.0.1::80', 'python:3.12-alpine', 'python', '-c', ORIGIN)
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            def wait_ready(url):
                for attempt in range(30):
                    try:
                        with opener.open(url, timeout=2) as response:
                            response.read()
                        return
                    except (urllib.error.URLError, OSError):
                        if attempt == 29:
                            raise
                        time.sleep(1)
            origin_address = run('docker', 'port', origin, '80/tcp').splitlines()[0]
            wait_ready('http://' + origin_address + '/__health')
            mounts = []
            for key, target in [('acng-package-mirror.conf', 'zz-package-mirror.conf'),
                                ('backends_debian', 'backends_debian'),
                                ('backends_ubuntu', 'backends_ubuntu')]:
                mounts += ['-v', str(work / key) + ':/etc/apt-cacher-ng/' + target + ':ro']
            run('docker', 'run', '-d', '--name', cache, '--network', name,
                '-p', '127.0.0.1::3142', *mounts, image)
            address = run('docker', 'port', cache, '3142/tcp').splitlines()[0]
            base = 'http://' + address
            proxy = urllib.request.build_opener(FixedHTTPProxy({'http': base}))
            bypasses = itertools.cycle(('*', 'deb.debian.org'))
            def fetch(path):
                url, client = ('http://deb.debian.org' + path, proxy) if path.startswith('/debian/') else (base + path, opener)
                # Consecutive fetches exercise wildcard and domain-specific bypass.
                bypass = next(bypasses)
                with patch.dict(os.environ, {'NO_PROXY': bypass, 'no_proxy': bypass}):
                    with client.open(url, timeout=10) as response:
                        return response.read()
            wait_ready(base + '/acng-report.html')
            # Metadata bypass must not forward the client's Host alongside the
            # remapped upstream Host (rejected by strict HTTP/1.1 servers).
            metadata_errors = []
            for path in ('/alpine/v3.22/main/x86_64/APKINDEX.tar.gz',
                         '/debian/dists/bookworm/InRelease'):
                try:
                    first, second = fetch(path), fetch(path)
                    assert first == (path + ' version 1').encode(), (path, first)
                    assert second == (path + ' version 2').encode(), (path, second)
                    print('PASS fresh metadata:', path, flush=True)
                except urllib.error.HTTPError as error:
                    if error.code not in (400, 500):
                        raise
                    metadata_errors.append(path)
                    print('FAIL fresh metadata:', path, error, flush=True)
            # Moving the exclusion must not disable package payload caching.
            for path in ('/alpine/v3.22/main/x86_64/example-1.0.apk',
                         '/debian/pool/main/e/example/example_1.0_amd64.deb'):
                first, second = fetch(path), fetch(path)
                assert first == (path + ' version 1').encode(), (path, first)
                assert first == second, (path, first, second)
                print('PASS cached payload:', path, flush=True)
            origin_log = run('docker', 'logs', origin)
            print(origin_log, flush=True)
            for path in ('/alpine/v3.22/main/x86_64/example-1.0.apk',
                         '/debian/pool/main/e/example/example_1.0_amd64.deb'):
                assert sum(line.startswith('ORIGIN ' + path + ' ') for line in origin_log.splitlines()) == 1
            if metadata_errors:
                assert len(metadata_errors) == 2
                expected_hosts = (['origin.test', address], ['origin.test', 'deb.debian.org'])
                for path, hosts in zip(metadata_errors, expected_hosts):
                    assert origin_log.splitlines().count('ORIGIN ' + path + ' ' + repr(hosts)) == 1
                print('PASS expected baseline failure', flush=True)
                return 2
            if not metadata_errors:
                for path, count in (('/alpine/v3.22/main/x86_64/APKINDEX.tar.gz', 2),
                                    ('/debian/dists/bookworm/InRelease', 2),
                                    ('/alpine/v3.22/main/x86_64/example-1.0.apk', 1),
                                    ('/debian/pool/main/e/example/example_1.0_amd64.deb', 1)):
                    assert sum(line.startswith('ORIGIN ' + path + ' ') for line in origin_log.splitlines()) == count
                files = run('docker', 'exec', cache, 'find', '/var/cache/apt-cacher-ng', '-type', 'f')
                assert 'APKINDEX.tar.gz' not in files and 'InRelease' not in files
                print('PASS upstream request counts and uncached metadata', flush=True)
            assert not metadata_errors, 'metadata remapping failed: ' + ', '.join(metadata_errors)
    except Exception:
        for container in (origin, cache):
            subprocess.run(['docker', 'logs', container], timeout=15)
        subprocess.run(['docker', 'exec', cache, 'cat',
                        '/var/cache/apt-cacher-ng/apt-cacher.err'], timeout=15)
        raise
    finally:
        for container in (cache, origin):
            subprocess.run(['docker', 'rm', '-f', container], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL)
        subprocess.run(['docker', 'network', 'rm', name], stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL)
        subprocess.run(['docker', 'image', 'rm', image], stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL)


if __name__ == '__main__':
    sys.exit(main())
