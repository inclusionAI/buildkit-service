#!/usr/bin/env python3
"""Repository-owned kind E2E. All mutable resources belong to a unique run."""
import argparse
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import shlex
import time
import uuid
import urllib.request
import urllib.error

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
LABEL = 'buildkit-service.e2e.run'


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class Run:
    """One run directory, Docker endpoint and isolated cluster.

    phase records the last completed lifecycle action. status/action/stage
    distinguish an in-progress or failed action from a completed phase.
    """
    def __init__(self, directory):
        self.directory = directory.resolve()
        self.directory.mkdir(parents=True, exist_ok=True)
        self.run_lock = (self.directory / '.lock').open('a')
        try:
            fcntl.flock(self.run_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            self.run_lock.close()
            raise RuntimeError('Another process is using this run directory') from None
        self.path = self.directory / 'state.json'
        self.env = os.environ.copy()
        self.env["PATH"] = str(ROOT / "artifacts/e2e-tools/bin") + os.pathsep + self.env.get("PATH", "")
        try:
            self.state = json.loads(self.path.read_text()) if self.path.exists() else {}
            snapshot = self.directory / 'dependencies.json'
            self.lock = json.loads((snapshot if self.state and snapshot.exists() else HERE / 'dependencies.json').read_text())
            if self.state:
                self.configure_environment()
        except (ValueError, KeyError, OSError) as error:
            self.run_lock.close()
            raise RuntimeError(f'Cannot load run state/dependencies in {self.directory}: {error}') from error
        self.log_number = 0
        self.active_stage = "initialization"

    def step(self, name, action, *args):
        self.active_stage = name
        if self.state:
            self.state['stage'] = name
            self.save()
        print('== ' + name + ' ==', flush=True)
        return action(*args)

    def perform(self, action):
        if action in ('logs', 'down'):
            try:
                return self.step(action, getattr(self, action))
            except BaseException as error:
                if self.state and action == 'down':
                    self.state['status'] = 'cleanup-failed'
                self.record_failure(error)
                raise
        allowed = {'build': ('preflight', 'built'), 'up': ('built',), 'test': ('up',)}
        require(self.lock == json.loads((HERE / 'dependencies.json').read_text()),
                'Dependencies changed since this run started; clean up and use a new run directory')
        if action == 'build' and not self.state:
            try:
                self.step('build.preflight', self.preflight)
            except BaseException as error:
                self.record_failure(error)
                raise
        require(self.state.get('phase') in allowed[action],
                f"Cannot {action} from phase {self.state.get('phase', 'new')}")
        retry_build = self.can_retry_build() and action == 'build'
        require(self.state.get('status') not in ('running', 'failed', 'cleanup-failed') or retry_build,
                'Previous action did not finish; export logs, clean up, and use a new run directory')
        self.state.update(action=action, status='running')
        self.state.pop('failure', None)
        self.state.pop('result', None)
        self.save()
        try:
            self.step(action, getattr(self, action))
        except BaseException as error:
            self.record_failure(error)
            raise
        self.state.update(phase={'build': 'built', 'up': 'up', 'test': 'tested'}[action], status='complete')
        if action == 'test':
            self.state['result'] = 'PASS'
            print('PASS: core E2E (' + self.state['execution'] + ' linux/amd64)', flush=True)
        self.save()

    def can_retry_build(self):
        return (self.state.get('action') == 'build'
                and self.state.get('phase') in ('preflight', 'built')
                and self.state.get('status') in ('running', 'failed')
                and self.state.get('failure', {}).get('stage') != 'build.preflight')

    def record_failure(self, error):
        failure = self.state.get('failure') or {'stage': self.active_stage, 'error': str(error)}
        if self.state:
            self.state.update(result='FAIL', failure=failure)
            if self.state.get('status') != 'cleanup-failed':
                self.state['status'] = 'failed'
        try:
            (self.directory / 'failure.json').write_text(json.dumps(failure, indent=2) + '\n')
            if self.state:
                self.save()
        except OSError as recording_error:
            print(f'Could not save failure in {self.directory}: {recording_error}', file=sys.stderr)

    def execute(self, action, keep=False):
        """Full runs always diagnose then clean up, without hiding the first error."""
        primary_error = None
        full_run_started = False
        try:
            if action == 'all':
                require(not self.state, 'Full E2E requires a new run directory')
                full_run_started = True
                self.perform('build')
                self.perform('up')
                self.perform('test')
            else:
                self.perform(action)
        except BaseException as error:
            primary_error = error
            if full_run_started:
                self.record_failure(error)
            print(self.failure_message(error), file=sys.stderr, flush=True)
        finally:
            if full_run_started and self.state:
                for cleanup_action in (('logs',) if keep else ('logs', 'down')):
                    try:
                        self.perform(cleanup_action)
                    except BaseException as error:
                        print(self.failure_message(error), file=sys.stderr, flush=True)
                        if primary_error is None:
                            primary_error = error
                if keep:
                    print('Preserved run. ' + self.followup('down'), flush=True)
        if primary_error is not None:
            raise primary_error

    def followup(self, action):
        return shlex.join(['make', '-C', str(ROOT), f'e2e-{action}', f'E2E_RUN_DIR={self.directory}'])

    def failure_message(self, error):
        message = f'E2E failed at {self.active_stage}: {error}\nLogs and state: {self.directory}'
        if self.state:
            message += '\nState: ' + shlex.join(['cat', str(self.path)])
            kubeconfig = self.directory / 'kubeconfig'
            if kubeconfig.exists() and self.state.get('phase') != 'cleaned':
                message += '\nPods (if API is available): ' + shlex.join([
                    str(ROOT / 'artifacts/e2e-tools/bin/kubectl'), '--kubeconfig', str(kubeconfig),
                    '--request-timeout=10s', 'get', 'pods', '-A', '-o', 'wide'])
            message += '\nInspect: ' + self.followup('logs') + '\nCleanup: ' + self.followup('down')
            if self.can_retry_build():
                message += '\nRetry build: ' + self.followup('build')
            else:
                message += '\nAfter cleanup, start again with make e2e (a new run directory).'
        return message

    def save(self):
        tmp = self.path.with_suffix('.tmp')
        tmp.write_text(json.dumps(self.state, indent=2) + '\n')
        tmp.chmod(0o600)
        tmp.replace(self.path)

    def command(self, args, *, input=None, timeout=300, check=True, name=None):
        args = [str(arg) for arg in args]
        if args[0] in self.lock['tools']:
            tool = ROOT / 'artifacts/e2e-tools/bin' / args[0]
            require(tool.is_file() and os.access(tool, os.X_OK),
                    f'Cached {args[0]} is missing or not executable; run make e2e-setup')
            args[0] = str(tool)
        self.log_number += 1
        filename = name or f'command-{time.time_ns()}-{self.log_number}'
        dest = self.directory / (filename + '.log')
        stderr = self.directory / (filename + '.stderr.log')
        for path in (dest, stderr):
            if path.exists():
                path.rename(path.with_name(path.stem + '-' + str(time.time_ns()) + '.log'))
        try:
            with dest.open('w') as log, stderr.open('w') as errors:
                with subprocess.Popen(args, stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,
                                      text=True, stdout=log, stderr=errors, cwd=ROOT,
                                      env=self.env, start_new_session=True) as process:
                    try:
                        process.communicate(input, timeout=timeout)
                    except (subprocess.TimeoutExpired, KeyboardInterrupt):
                        # Stop kind/docker descendants before cleaning their resources.
                        # Killing only the CLI parent can leave a child creating nodes.
                        try:
                            os.killpg(process.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                        process.wait()
                        raise
        except (subprocess.TimeoutExpired, OSError) as error:
            raise RuntimeError(f'{args[0]} could not complete within {timeout}s: {error}; see {dest} and {stderr}') from error
        output = dest.read_text()
        if check and process.returncode:
            raise RuntimeError(f'{args[0]} failed ({process.returncode}); see {dest} and {stderr}\n{(output + stderr.read_text())[-4000:]}')
        return output

    def docker(self, *args, **kw):
        return self.command(['docker', *args], **kw)

    def kubectl(self, *args, **kw):
        return self.command(['kubectl', '--kubeconfig', self.directory / 'kubeconfig', '--request-timeout=30s', *args], **kw)

    def configure_environment(self):
        for key in ('DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH', 'BUILDX_BUILDER'):
            self.env.pop(key, None)
        self.env.update(DOCKER_HOST=self.state['endpoint'], DOCKER_CONFIG=str(self.directory / 'docker'),
                        DOCKER_DEFAULT_PLATFORM='linux/amd64', KIND_EXPERIMENTAL_PROVIDER='docker', KUBECONFIG=str(self.directory / 'kubeconfig'))

    def preflight(self):
        require(not self.state, 'Run directory already initialized; use another directory or a lifecycle subcommand')
        for binary in ('docker', 'git'):
            require(shutil.which(binary, path=self.env['PATH']), f'{binary} is required')
        context = self.command(['docker', 'context', 'show']).strip()
        endpoint = os.environ.get('DOCKER_HOST') or self.command([
            'docker', 'context', 'inspect', context, '--format', '{{.Endpoints.docker.Host}}']).strip()
        require(endpoint.startswith('unix://'), 'Core E2E currently requires a local Unix Docker socket')
        self.env.pop('DOCKER_CONTEXT', None)
        self.env.pop('DOCKER_TLS_VERIFY', None)
        self.env.pop('DOCKER_CERT_PATH', None)
        self.env['DOCKER_HOST'] = endpoint
        info = json.loads(self.command(['docker', 'info', '--format', '{{json .}}']))
        arch = {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(info['Architecture'], info['Architecture'])
        require(info['OSType'] == 'linux' and arch in ('arm64', 'amd64'), 'Linux arm64/amd64 Docker required')
        target = os.environ.get('E2E_PLATFORM', 'linux/amd64')
        require(target == 'linux/amd64', 'Current production Nydus profile supports linux/amd64 only')
        require(info['NCPU'] >= 4 and info['MemTotal'] >= 6 * 1024**3, 'Allocate at least 4 CPUs and 6 GiB to Docker')
        require(shutil.disk_usage(self.directory).free >= 10 * 1024**3, 'At least 10 GiB free host disk required')
        name = 'bks-e2e-' + uuid.uuid4().hex[:10]
        self.state = {'schema': 1, 'name': name, 'endpoint': endpoint, 'architecture': 'amd64', 'node_architecture': arch, 'daemon_architecture': arch, 'execution': 'native' if arch == 'amd64' else 'emulated',
                      'context': context, 'images': {}, 'phase': 'preflight', 'cluster_attempted': False,
                      'revision': self.command(['git', 'rev-parse', 'HEAD']).strip()}
        (self.directory / 'dependencies.json').write_text(json.dumps(self.lock, indent=2) + '\n')
        config = self.directory / 'docker'
        config.mkdir(mode=0o700)
        plugins = info.get('ClientInfo', {}).get('Plugins', [])
        dirs = sorted({str(Path(p['Path']).parent) for p in plugins if p.get('Name') == 'buildx'})
        (config / 'config.json').write_text(json.dumps({'auths': {}, 'cliPluginsExtraDirs': dirs}))
        (config / 'config.json').chmod(0o600)
        self.configure_environment()
        for binary, args in [('kind', ['version']), ('helm', ['version', '--short']), ('kubectl', ['version', '--client', '-o', 'json'])]:
            self.command([binary, *args], name=binary + '-version')
        self.save()
        print(f"Run: {self.directory}\nDocker: {context}; target {target}; {self.state['execution']}", flush=True)

    def image(self, name):
        entry = self.lock['images'][name]
        arch = self.state.get('node_architecture', self.state['architecture']) if name == 'node' else self.state['architecture']
        return entry['repository'] + '@' + entry['platforms'][arch]

    def build_image(self, name, dockerfile, args=None, context='.'):
        tag = self.state['name'] + '-' + name + ':test'
        self.state['images'][name] = {'tag': tag}
        self.save()
        cmd = ['build', '--platform', 'linux/' + self.state['architecture'], '--label', LABEL + '=' + self.state['name'],
               '-f', dockerfile, '-t', tag]
        for key, value in (args or {}).items():
            cmd += ['--build-arg', key + '=' + value]
        # Optional download transport; Docker's predefined proxy args are not
        # persisted in image configuration. Runtime fixtures never use it.
        previous_environment = self.env.copy()
        if os.environ.get('E2E_BUILD_PROXY'):
            for key in ('HTTP_PROXY', 'HTTPS_PROXY', 'http_proxy', 'https_proxy'):
                self.env[key] = os.environ['E2E_BUILD_PROXY']
                cmd += ['--build-arg', key]
        print('Building ' + name, flush=True)
        try:
            self.docker(*cmd, context, timeout=1200, name='build-' + name)
        finally:
            self.env = previous_environment
        info = json.loads(self.docker('image', 'inspect', tag))[0]
        require(info['Architecture'] == self.state['architecture'], 'Unexpected image architecture')
        self.state['images'][name]['id'] = info['Id']
        self.save()

    def build(self):
        self.step('build.source-record', self.record_source)
        self.step('build.dependencies', self.pull_dependencies)
        self.step('build.service', self.build_image, 'service', 'chart/images/buildkit/Dockerfile', {
            'BASE_IMAGE': self.image('buildkit'), 'GO_IMAGE': self.image('go'), 'ALPINE_IMAGE': self.image('alpine')})
        self.step('build.apt', self.build_image, 'apt', 'chart/images/apt-cacher-ng/Dockerfile',
                  {'BASE_IMAGE': self.image('debian')}, 'chart/images/apt-cacher-ng')
        self.step('build.git', self.build_image, 'git', 'chart/images/git-cache/Dockerfile',
                  {'BASE_IMAGE': self.image('python')}, 'chart/images/git-cache')
        self.step('build.fixture', self.build_image, 'fixture', 'tests/e2e/fixtures/Dockerfile.upstream',
                  {'BASE_IMAGE': self.state['images']['git']['tag']})
        self.docker('run', '--rm', '--entrypoint', 'dpkg-query', self.state['images']['apt']['tag'], '-W', 'apt-cacher-ng', name='apt-version')

    def record_source(self):
        (self.directory / 'source.diff').write_text(self.command(['git', 'diff', 'HEAD']))
        files = self.command(['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z']).split('\0')
        source = {name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
                  for name in files if name and (ROOT / name).is_file()}
        (self.directory / 'source-files.json').write_text(json.dumps(source, indent=2))

    def pull_dependencies(self):
        for dep in self.lock['images']:
            print('Pulling ' + dep, flush=True)
            arch = self.state['node_architecture'] if dep == 'node' else self.state['architecture']
            self.docker('pull', '--platform', 'linux/' + arch, self.image(dep), timeout=600, name='pull-' + dep)

    def apply(self, *objects):
        self.kubectl('apply', '-f', '-', input=json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': objects}))

    def wait_ready(self):
        self.kubectl('-n', 'e2e', 'wait', '--for=condition=Ready', 'pods', '--all', '--timeout=240s', timeout=270)

    def client(self, *args, **kw):
        return self.kubectl('-n', 'e2e', 'exec', 'client', '--', *args, **kw)

    def up(self):
        self.step('up.registry', self.start_registry)
        self.step('up.kind', self.create_kind_cluster)
        self.step('up.containerd-registry', self.connect_registry_to_nodes)
        self.step('up.images', self.load_images)
        self.step('up.dns', self.configure_cluster_dns)
        self.step('up.fixtures', self.deploy_fixtures)
        self.step('up.helm', self.deploy_chart)

    @staticmethod
    def fixture(name):
        return json.loads((HERE / 'fixtures' / name).read_text())

    def start_registry(self):
        name = self.state['name']
        self.state['registry'] = name + '-registry'
        self.save()
        self.docker('run', '-d', '--name', self.state['registry'], '--label', LABEL + '=' + name,
                    '-e', 'OTEL_TRACES_EXPORTER=none', '-p', '127.0.0.1::5000', self.image('registry'))
        # Give the seed an owned tag; the source image itself belongs to Docker's shared cache.
        seed = self.docker('port', self.state['registry'], '5000/tcp').strip() + '/base:seed'
        self.state['seed_tag'] = seed
        self.save()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        deadline = time.monotonic() + 30
        while True:
            try:
                with opener.open('http://' + seed.split('/')[0] + '/v2/', timeout=2) as response:
                    require(response.status == 200, 'Registry health check failed')
                break
            except (urllib.error.URLError, OSError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(1)
        self.state['seed_id'] = json.loads(self.docker('image', 'inspect', self.image('alpine')))[0]['Id']
        self.save()
        self.docker('tag', self.image('alpine'), seed)
        self.docker('push', seed, name='seed-registry')

    def create_kind_cluster(self):
        name = self.state['name']
        require(not self.docker('ps', '-aq', '--filter', 'label=io.x-k8s.kind.cluster=' + name).strip(),
                'Cluster name already exists; refusing to adopt existing nodes')
        shutil.copyfile(HERE / 'fixtures/kind.json', self.directory / 'kind.json')
        self.state['cluster_attempted'] = True
        self.save()
        print('Creating kind cluster ' + name, flush=True)
        # Keep the nested runtime native: emulated containerd cannot install the
        # control-plane seccomp profile on Apple Silicon. Workloads stay amd64.
        previous_environment = self.env.copy()
        self.env['DOCKER_DEFAULT_PLATFORM'] = 'linux/' + self.state['node_architecture']
        # All node images are preloaded. A host loopback proxy is not reachable
        # from nested containerd and must not become its systemd environment.
        for key in ('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'http_proxy', 'https_proxy', 'all_proxy'):
            self.env.pop(key, None)
        try:
            self.command(['kind', 'create', 'cluster', '--name', name, '--image', self.image('node'),
                          '--config', self.directory / 'kind.json', '--kubeconfig', self.directory / 'kubeconfig', '--wait', '180s', '--retain'],
                         timeout=360, name='kind-create')
        finally:
            self.env = previous_environment
            kubeconfig = self.directory / 'kubeconfig'
            if kubeconfig.exists():
                kubeconfig.chmod(0o600)
        node_state = json.loads(self.kubectl('get', 'nodes', '-o', 'json'))
        require(len(node_state['items']) == 2 and all(
            node['status']['nodeInfo']['architecture'] == self.state['node_architecture'] for node in node_state['items']),
            'kind must run two nodes matching the Docker daemon architecture')
        (self.directory / 'nodes.json').write_text(json.dumps(node_state, indent=2))

    def connect_registry_to_nodes(self):
        name = self.state['name']
        nodes = self.command(['kind', 'get', 'nodes', '--name', name]).split()
        network = next(iter(json.loads(self.docker('inspect', nodes[0]))[0]['NetworkSettings']['Networks']))
        self.state['network'] = network
        self.save()
        self.docker('network', 'connect', network, self.state['registry'])
        address = json.loads(self.docker('inspect', self.state['registry']))[0]['NetworkSettings']['Networks'][network]['IPAddress']
        self.state['registry_ip'] = address
        self.save()
        for node in nodes:
            hosts = '[host."http://' + address + ':5000"]\n  capabilities = ["pull", "resolve"]\n'
            self.docker('exec', node, 'mkdir', '-p', '/etc/containerd/certs.d/registry.e2e.test:5000')
            self.docker('exec', '-i', node, 'cp', '/dev/stdin', '/etc/containerd/certs.d/registry.e2e.test:5000/hosts.toml', input=hosts)

    def load_images(self):
        name = self.state['name']
        tags = [entry['tag'] for entry in self.state['images'].values()]
        # Load anonymous dependencies too: tests never need kubelet to contact public registries.
        for dep in ('pip', 'npm', 'registry'):
            tag = name + '-' + dep + ':test'
            self.state['images'][dep] = {'tag': tag, 'id': json.loads(self.docker('image', 'inspect', self.image(dep)))[0]['Id']}
            self.save()  # Record ownership before creating an alias, including interrupted imports.
            self.docker('tag', self.image(dep), tag)
            tags.append(tag)
        self.save()
        self.command(['kind', 'load', 'docker-image', '--name', name, *tags], timeout=600, name='kind-load')

    def configure_cluster_dns(self):
        address = self.state['registry_ip']
        self.apply({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': 'e2e'}})
        dns = json.loads(self.kubectl('-n', 'kube-system', 'get', 'configmap', 'coredns', '-o', 'json'))
        dns['data']['Corefile'] = dns['data']['Corefile'].replace('    kubernetes ',
            '    hosts {\n        ' + address + ' registry.e2e.test\n        fallthrough\n    }\n    kubernetes ', 1)
        self.kubectl('-n', 'kube-system', 'replace', '-f', '-', input=json.dumps(dns))
        self.kubectl('-n', 'kube-system', 'rollout', 'restart', 'deployment/coredns')
        self.kubectl('-n', 'kube-system', 'rollout', 'status', 'deployment/coredns', '--timeout=120s')

    def deploy_fixtures(self):
        fixture, service, client = self.fixture('upstream.json')
        for pod in (fixture, client):
            pod['spec']['containers'][0]['image'] = self.state['images']['fixture']['tag']
        fixture['spec']['containers'][0]['env'][0]['value'] = 'http://' + self.state['registry_ip'] + ':5000'
        self.apply(fixture, service, client)

    def deploy_chart(self):
        values = json.loads((HERE / 'values.json').read_text())
        values['image'] = self.state['images']['service']['tag']
        for dep, section in [('apt', 'aptYum'), ('git', 'git'), ('pip', 'pip'), ('npm', 'npm'), ('registry', 'registry')]:
            tag = self.state['images'][dep]['tag']
            values['packageMirror'][section]['image'] = {'repository': tag.split(':')[0], 'tag': 'test', 'pullPolicy': 'Never'}
        values_path = self.directory / 'values.json'
        values_path.write_text(json.dumps(values, indent=2))
        self.command(['helm', 'template', 'e2e', 'chart', '-n', 'e2e', '-f', values_path], name='helm-render')
        self.command(['helm', 'upgrade', '--install', 'e2e', 'chart', '--kubeconfig', self.directory / 'kubeconfig',
                      '-n', 'e2e', '-f', values_path, '--wait', '--timeout', '5m'], timeout=330, name='helm-install')
        self.wait_ready()

    def test(self):
        from scenarios import Scenarios
        Scenarios(self).verify()

    def logs(self):
        require(bool(self.state), 'No initialized run in this directory')
        if self.state.get('phase') == 'cleaned':
            print('Resources already cleaned; saved logs: ' + str(self.directory), flush=True)
            return
        errors = []

        def collect(action, *args, **kwargs):
            try:
                return action(*args, **kwargs)
            except (ValueError, RuntimeError) as error:
                errors.append(str(error))
                return ''

        if self.state.get('registry'):
            collect(self.docker, 'logs', self.state['registry'], name='registry')
        if self.state.get('cluster_attempted'):
            collect(self.command, ['kind', 'export', 'logs', str(self.directory / 'kind-logs'),
                                  '--name', self.state['name']], timeout=120, name='kind-export')
            if (self.directory / 'kubeconfig').exists():
                collect(self.kubectl, 'get', 'pods', '-A', '-o', 'wide', name='pods', timeout=45)
                collect(self.kubectl, '-n', 'e2e', 'get', 'events', '--sort-by=.lastTimestamp', name='events', timeout=45)
                collect(self.kubectl, '-n', 'e2e', 'describe', 'pods', name='describe', timeout=45)
                collect(self.export_pod_logs)
        self.state['diagnostic_errors'] = errors
        self.save()
        if errors:
            (self.directory / 'log-export-errors.log').write_text('\n'.join(errors))
            raise RuntimeError('Some diagnostics could not be exported; see log-export-errors.log')

    def export_pod_logs(self):
        pods = json.loads(self.kubectl('-n', 'e2e', 'get', 'pods', '-o', 'json', timeout=45))['items']
        errors = []
        for pod in pods:
            for container in pod['spec']['containers']:
                for previous in (False, True):
                    try:
                        self.kubectl('-n', 'e2e', 'logs', pod['metadata']['name'], '-c', container['name'],
                                     *(['--previous'] if previous else []), check=not previous, timeout=45,
                                     name=pod['metadata']['name'] + '-' + container['name'] + ('-previous' if previous else ''))
                    except RuntimeError as error:
                        errors.append(str(error))
        require(not errors, '; '.join(errors))

    def down(self):
        if not self.state or self.state.get('phase') == 'cleaned':
            return
        require(self.state['name'].startswith('bks-e2e-'), 'Invalid resource owner')
        errors = []
        # A failed removal must not prevent attempts to clean other owned resources.
        for action in (self.remove_cluster, self.remove_registry, self.remove_image_tags):
            try:
                action()
            except (RuntimeError, ValueError) as error:
                errors.append(str(error))
        self.state['cleanup_errors'] = errors
        self.state['status'] = 'cleanup-failed' if errors else 'complete'
        if not errors:
            self.state['phase'] = 'cleaned'
        self.save()
        require(not errors, 'Cleanup incomplete: ' + '; '.join(errors))

    def remove_cluster(self):
        name = self.state['name']
        if self.state.get('cluster_attempted'):
            nodes = self.docker('ps', '-aq', '--filter', 'label=io.x-k8s.kind.cluster=' + name).split()
            for node in nodes:
                info = json.loads(self.docker('inspect', node))[0]
                require(info['Config']['Labels'].get('io.x-k8s.kind.cluster') == name, 'Node ownership mismatch')
            if nodes:
                self.command(['kind', 'delete', 'cluster', '--name', name], timeout=180)

    def remove_registry(self):
        registry = self.state.get('registry')
        if registry:
            present = self.docker('ps', '-aq', '--filter', 'name=^/' + registry + '$').strip()
            if present:
                info = json.loads(self.docker('inspect', registry))[0]
                require((info['Config'].get('Labels') or {}).get(LABEL) == self.state['name'], 'Registry ownership mismatch')
                self.docker('rm', '-f', registry)

    def remove_image_tags(self):
        images = list(self.state.get('images', {}).values())
        if self.state.get('seed_tag'):
            images.append({'tag': self.state['seed_tag'], 'id': self.state.get('seed_id')})
        errors = []
        for entry in images:
            tag = entry['tag']
            try:
                present = self.docker('image', 'ls', '-q', '--filter', 'reference=' + tag).strip()
                if not present:
                    continue
                info = json.loads(self.docker('image', 'inspect', tag))[0]
                # Built images can be left behind before their ID was saved.
                owned = (info['Config'].get('Labels') or {}).get(LABEL) == self.state['name']
                require(info['Id'] == entry.get('id') if entry.get('id') else owned,
                        'Image ownership mismatch: ' + tag)
                self.docker('image', 'rm', tag)
            except (RuntimeError, ValueError) as error:
                errors.append(str(error))
        # kind's Docker network and downloaded dependency layers are shared; leave them alone.
        require(not errors, '; '.join(errors))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('all', 'build', 'up', 'test', 'logs', 'down'), default='all', nargs='?')
    parser.add_argument('--run-dir', type=Path, default=os.environ.get('E2E_RUN_DIR') or None)
    args = parser.parse_args()
    if not args.run_dir:
        require(args.action in ('all', 'build'), '--run-dir is required for existing runs')
        args.run_dir = ROOT / 'artifacts/e2e' / (datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S') + '-' + uuid.uuid4().hex[:6])
    require(args.run_dir.resolve().is_relative_to((ROOT / 'artifacts').resolve()),
            '--run-dir must be inside the repository ignored artifacts/ directory')
    run = Run(args.run_dir)
    try:
        run.execute(args.action, keep=os.environ.get('KEEP_CLUSTER') == '1')
    finally:
        run.run_lock.close()


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, OSError, KeyboardInterrupt) as error:
        # Detailed diagnostics were printed before cleanup; avoid a second traceback.
        sys.exit(str(error))
