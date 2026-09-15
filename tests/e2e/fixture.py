"""Controlled package, Git and registry upstream; no public runtime dependencies."""
import collections
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import threading
import urllib.parse
import urllib.request
import zipfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path('/tmp/upstream')
COUNTS = collections.Counter()
LOCK = threading.Lock()
WHEEL_NAME = 'e2e_pkg-1.0-py3-none-any.whl'


def wheel():
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, 'w') as z:
        z.writestr('e2e_pkg.py', 'VALUE = "e2e-ok"\n')
        z.writestr('e2e_pkg-1.0.dist-info/METADATA', 'Metadata-Version: 2.1\nName: e2e-pkg\nVersion: 1.0\n')
        z.writestr('e2e_pkg-1.0.dist-info/WHEEL', 'Wheel-Version: 1.0\nGenerator: fixture\nRoot-Is-Purelib: true\nTag: py3-none-any\n')
        z.writestr('e2e_pkg-1.0.dist-info/RECORD', '')
    return buf.getvalue()


def npm_tar():
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode='w:gz') as tar:
        for name, value in {'package/package.json': '{"name":"e2e-pkg","version":"1.0.0","main":"index.js"}',
                            'package/index.js': 'module.exports="e2e-ok";\n'}.items():
            data = value.encode()
            info = tarfile.TarInfo(name)
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


WHEEL, NPM = wheel(), npm_tar()


def git(*args):
    return subprocess.check_output(['git', *args], stderr=subprocess.STDOUT)


def init_git():
    ROOT.mkdir(exist_ok=True)
    git('init', '-b', 'main', str(ROOT / 'work'))
    git('-C', str(ROOT / 'work'), 'config', 'user.email', 'fixture@example.invalid')
    git('-C', str(ROOT / 'work'), 'config', 'user.name', 'E2E fixture')
    advance_git()
    git('clone', '--bare', str(ROOT / 'work'), str(ROOT / 'repo.git'))


def advance_git():
    p = ROOT / 'work' / 'README'
    p.write_text(p.read_text() + 'updated\n' if p.exists() else 'e2e-ok\n')
    git('-C', str(ROOT / 'work'), 'add', 'README')
    git('-C', str(ROOT / 'work'), 'commit', '-m', 'fixture update')
    if (ROOT / 'repo.git').exists():
        git('-C', str(ROOT / 'work'), 'push', str(ROOT / 'repo.git'), 'main')


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def respond(self, data, content_type='application/octet-stream', status=200, headers=None, length=None):
        self.send_response(status)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(data) if length is None else length))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        if self.command != 'HEAD':
            self.wfile.write(data)

    def do_HEAD(self):
        self.do_GET()

    def do_POST(self):
        if self.path == '/advance':
            advance_git()
            self.respond(b'ok')
        else:
            self.do_GET()

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        if path == '/stats':
            with LOCK:
                data = json.dumps(dict(COUNTS)).encode()
            self.respond(data, 'application/json')
            return
        if path == '/health':
            self.respond(b'ok')
            return
        with LOCK:
            COUNTS[self.command + ' ' + path] += 1
            count = COUNTS[self.command + ' ' + path]
        if path.startswith('/repo.git/'):
            self.serve_git(path)
        elif path.startswith('/v2/'):
            self.serve_registry()
        elif path == '/simple/':
            self.respond(b'<a href="/simple/e2e-pkg/">e2e-pkg</a>', 'text/html')
        elif path == '/simple/e2e-pkg/':
            self.respond(('<a href="/files/' + WHEEL_NAME + '">' + WHEEL_NAME + '</a>').encode(), 'text/html')
        elif path == '/files/' + WHEEL_NAME:
            self.respond(WHEEL)
        elif path == '/e2e-pkg':
            host = self.headers['Host']
            self.respond(json.dumps({'name': 'e2e-pkg', 'dist-tags': {'latest': '1.0.0'}, 'versions': {
                '1.0.0': {'name': 'e2e-pkg', 'version': '1.0.0', 'dist': {
                    'tarball': 'http://' + host + '/e2e-pkg/-/e2e-pkg-1.0.0.tgz',
                    'shasum': hashlib.sha1(NPM).hexdigest()}}}}).encode(), 'application/json')
        elif path == '/e2e-pkg/-/e2e-pkg-1.0.0.tgz':
            self.respond(NPM)
        elif path.endswith(('InRelease', 'APKINDEX.tar.gz')):
            if len(self.headers.get_all('Host', [])) != 1:
                self.respond(b'duplicate Host', status=400)
            else:
                self.respond((path + ' version ' + str(count)).encode())
        elif path.endswith(('.deb', '.apk')):
            self.respond((path + ' immutable payload').encode())
        else:
            self.respond(b'not found', status=404)

    def serve_git(self, path):
        env = dict(os.environ, GIT_PROJECT_ROOT=str(ROOT), GIT_HTTP_EXPORT_ALL='1',
                   PATH_INFO=path, QUERY_STRING=urllib.parse.urlsplit(self.path).query,
                   REQUEST_METHOD=self.command, CONTENT_TYPE=self.headers.get('Content-Type', ''),
                   CONTENT_LENGTH=self.headers.get('Content-Length', '0'))
        body = self.rfile.read(int(env['CONTENT_LENGTH']))
        raw = subprocess.check_output(['git', 'http-backend'], input=body, env=env)
        head, body = raw.split(b'\r\n\r\n', 1)
        headers = dict(line.decode().split(': ', 1) for line in head.split(b'\r\n'))
        status = int(headers.pop('Status', '200').split()[0])
        self.respond(body, headers.pop('Content-Type', 'application/octet-stream'), status, headers)

    def serve_registry(self):
        request = urllib.request.Request(os.environ['REGISTRY_URL'] + self.path,
                                         method=self.command,
                                         headers={'Accept': self.headers.get('Accept', '*/*')})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(request, timeout=30) as response:
                self.respond(response.read(), response.headers.get('Content-Type'), headers={
                    k: response.headers[k] for k in ('Docker-Content-Digest',) if k in response.headers},
                    length=response.headers.get('Content-Length') if self.command == 'HEAD' else None)
        except urllib.error.HTTPError as error:
            self.respond(error.read(), status=error.code)

    def log_message(self, fmt, *args):
        print(fmt % args, flush=True)


if __name__ == '__main__':
    init_git()
    ThreadingHTTPServer(('0.0.0.0', 80), Handler).serve_forever()
