#!/usr/bin/env python3
"""One persistent local kind environment. No test fixtures or cloud accounts."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import uuid
import time
import socket

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / 'artifacts/dev'
NAMESPACE = 'buildkit-dev'
REGISTRY = 'registry.buildkit-dev.svc.cluster.local:5000'


class Dev:
    def __init__(self):
        DIRECTORY.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.lock = (DIRECTORY / '.lock').open('a')
        try:
            try:
                fcntl.flock(self.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise RuntimeError('Another dev command is active; wait for it or interrupt that command before retrying') from error
            self.path = DIRECTORY / 'state.json'
            self.state = json.loads(self.path.read_text()) if self.path.exists() else {}
            self.dependencies = json.loads((ROOT / 'tests/e2e/dependencies.json').read_text())
            self.env = os.environ.copy()
            self.stage = 'initialize'
            if self.state:
                self.configure_environment()
        except BaseException:
            self.lock.close()
            raise

    def save(self):
        temporary = self.path.with_suffix('.tmp')
        temporary.write_text(json.dumps(self.state, indent=2) + '\n')
        temporary.replace(self.path)

    def configure_environment(self):
        for key in ('DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH', 'BUILDX_BUILDER',
                    'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'http_proxy', 'https_proxy', 'all_proxy'):
            self.env.pop(key, None)
        self.env.update(DOCKER_HOST=self.state['endpoint'], DOCKER_CONFIG=str(DIRECTORY / 'docker'),
                        KUBECONFIG=str(DIRECTORY / 'kubeconfig'), KIND_EXPERIMENTAL_PROVIDER='docker')
        self.env.pop('DOCKER_DEFAULT_PLATFORM', None)

    def tool_path(self, name):
        if name in ('kind', 'kubectl', 'helm'):
            path = ROOT / 'artifacts/e2e-tools/bin' / name
            if not path.is_file() or not os.access(path, os.X_OK):
                raise RuntimeError(f'{name} is missing or not executable at {path}; run make dev-setup')
            return str(path)
        path = shutil.which(name, path=self.env.get('PATH', ''))
        if not path:
            raise RuntimeError(f'{name} is not available on PATH; install it before running dev commands')
        return path

    def check_dependencies(self, action):
        # Check only the tools needed by this action; cleanup must not require Helm/buildx.
        tools = {
            'up': ('docker', 'kind', 'kubectl', 'helm'),
            'rebuild': ('docker', 'kind', 'kubectl', 'helm'),
            'status': ('docker', 'kind', 'kubectl', 'helm'),
            'logs': ('kind', 'kubectl', 'docker'),
            'down': ('docker', 'kind', 'kubectl'),
            'reset': ('docker', 'kind'),
        }[action]
        versions = {'docker': ('--version',), 'kind': ('version',),
                    'kubectl': ('version', '--client=true', '-o', 'json'),
                    'helm': ('version', '--short')}
        for tool in tools:
            try:
                self.command(tool, *versions[tool], timeout=15)
            except (RuntimeError, OSError, subprocess.TimeoutExpired) as error:
                raise RuntimeError(f'{tool} preflight failed: {error}; see {DIRECTORY / "commands.log"}') from error
        if 'docker' in tools:
            try:
                self.command('docker', 'info', '--format', '{{json .ServerVersion}}', timeout=20)
            except (RuntimeError, OSError, subprocess.TimeoutExpired) as error:
                raise RuntimeError('Docker daemon is unreachable at the selected endpoint; start Docker and check '
                                   f'the endpoint in state.json (if present). Logs: {DIRECTORY / "commands.log"}') from error
        if action in ('up', 'rebuild'):
            try:
                self.command('docker', 'buildx', 'version', timeout=15)
            except (RuntimeError, OSError, subprocess.TimeoutExpired) as error:
                raise RuntimeError('Docker buildx is unavailable; install or repair the Docker buildx plugin. '
                                   f'Logs: {DIRECTORY / "commands.log"}') from error

    def command(self, *args, input=None, timeout=600):
        args = [str(a) for a in args]
        args[0] = self.tool_path(args[0])
        with (DIRECTORY / 'commands.log').open('a') as log:
            log.write('\n' + self.stage + ': ' + repr(args) + '\n')
            log.flush()
            process = subprocess.Popen(args, cwd=ROOT, env=self.env, text=True,
                                       stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,
                                       stdout=subprocess.PIPE, stderr=log, start_new_session=True)
            try:
                output, _ = process.communicate(input, timeout=timeout)
            except (subprocess.TimeoutExpired, KeyboardInterrupt):
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                output, _ = process.communicate()
                log.write(output or '')
                raise
            log.write(output)
            if process.returncode:
                raise RuntimeError(f'{args[0]} exited {process.returncode}')
            return output

    def kube(self, *args, **kwargs):
        return self.command('kubectl', '--kubeconfig', DIRECTORY / 'kubeconfig',
                            '--request-timeout=30s', '-n', NAMESPACE, *args, **kwargs)

    def step(self, name, function):
        self.stage = name
        print('== ' + name + ' ==', flush=True)
        function()

    def initialize(self):
        if self.state:
            return
        context = self.command('docker', 'context', 'show').strip()
        endpoint = os.environ.get('DOCKER_HOST') or self.command('docker', 'context', 'inspect', context,
                                                                '--format', '{{.Endpoints.docker.Host}}').strip()
        if not endpoint.startswith('unix://'):
            raise RuntimeError('dev requires a local Unix Docker endpoint')
        for key in ('DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH'):
            self.env.pop(key, None)
        self.env['DOCKER_HOST'] = endpoint
        info = json.loads(self.command('docker', 'info', '--format', '{{json .}}'))
        architecture = {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(info['Architecture'], info['Architecture'])
        if info['OSType'] != 'linux' or architecture not in ('amd64', 'arm64') or info['MemTotal'] < 6 * 1024**3:
            raise RuntimeError('dev requires Linux Docker with at least 6 GiB memory')
        config = DIRECTORY / 'docker'
        config.mkdir(exist_ok=True, mode=0o700)
        plugins = info.get('ClientInfo', {}).get('Plugins', [])
        dirs = sorted({str(Path(p['Path']).parent) for p in plugins if p.get('Name') == 'buildx'})
        (config / 'config.json').write_text(json.dumps({'auths': {}, 'cliPluginsExtraDirs': dirs}))
        self.state = {'name': 'bks-dev-' + uuid.uuid4().hex[:10], 'endpoint': endpoint,
                      'architecture': architecture, 'images': {}, 'node_id': None}
        self.save()
        self.configure_environment()

    def image(self, name):
        entry = self.dependencies['images'][name]
        architecture = self.state['architecture'] if name == 'node' else 'amd64'
        return entry['repository'] + '@' + entry['platforms'][architecture]

    def images_available(self):
        images = self.state['images']
        if set(images) != {'service', 'apt', 'git', 'pip', 'npm', 'registry', 'maven'}:
            return False
        for tag in images.values():
            # A retained state file is not proof that Docker still has the image.
            result = self.command('docker', 'image', 'ls', '--quiet', '--no-trunc', tag).strip()
            if not result:
                return False
        return True

    def build_images(self):
        generation = uuid.uuid4().hex[:8]
        images = {}
        for name, dockerfile, overrides in (
            ('service', 'buildkit', {'BASE_IMAGE': self.image('buildkit'), 'GO_IMAGE': self.image('go'), 'ALPINE_IMAGE': self.image('alpine')}),
            ('apt', 'apt-cacher-ng', {'BASE_IMAGE': self.image('debian')}),
            ('git', 'git-cache', {'BASE_IMAGE': self.image('python')})):
            tag = self.state['name'] + '-' + name + ':' + generation
            args = ['docker', 'build', '--platform=linux/amd64', '--label', 'buildkit.dev.owner=' + self.state['name'],
                    '-f', 'chart/images/' + dockerfile + '/Dockerfile', '-t', tag]
            for key, value in overrides.items():
                args += ['--build-arg', key + '=' + value]
            if os.environ.get('DEV_BUILD_PROXY'):
                for key in ('HTTP_PROXY', 'HTTPS_PROXY'):
                    args += ['--build-arg', key + '=' + os.environ['DEV_BUILD_PROXY']]
            self.command(*args, '.' if name == 'service' else 'chart/images/' + dockerfile, timeout=1200)
            images[name] = tag
        for name in ('pip', 'npm', 'registry', 'maven'):
            self.command('docker', 'pull', '--platform=linux/amd64', self.image(name))
            tag = self.state['name'] + '-' + name + ':dependency'
            self.command('docker', 'tag', self.image(name), tag)
            images[name] = tag
        self.state['images'] = images
        self.save()

    def owned_node(self):
        ids = self.command('docker', 'ps', '-aq', '--no-trunc', '--filter',
                           'label=io.x-k8s.kind.cluster=' + self.state['name']).split()
        if not ids:
            return None
        if len(ids) != 1 or ids[0] != self.state.get('node_id'):
            raise RuntimeError('kind node ownership mismatch; inspect state and commands.log')
        return ids[0]

    def connect_cluster(self):
        if not self.owned_node():
            raise RuntimeError('No owned kind node; run make dev-up')
        # Regenerate only the private kubeconfig from the verified kind cluster.
        self.command('kind', 'export', 'kubeconfig', '--name', self.state['name'],
                     '--kubeconfig', DIRECTORY / 'kubeconfig')
        (DIRECTORY / 'kubeconfig').chmod(0o600)

    def cluster(self):
        node = self.owned_node()
        if node:
            running = self.command('docker', 'inspect', '--format', '{{.State.Running}}', node).strip()
            if running != 'true':
                self.command('docker', 'start', node)
            self.connect_cluster()
            return
        config = json.loads((ROOT / 'deploy/local/kind.json').read_text())
        for mapping in config['nodes'][0].get('extraPortMappings', []):
            try:
                with socket.socket() as probe:
                    probe.bind((mapping['listenAddress'], mapping['hostPort']))
            except OSError as error:
                raise RuntimeError(f"Local port {mapping['hostPort']} is unavailable; stop its owner or change deploy/local/kind.json before retrying") from error
        config['nodes'][0]['image'] = self.image('node')
        path = DIRECTORY / 'kind.json'
        path.write_text(json.dumps(config))
        try:
            self.command('kind', 'create', 'cluster', '--name', self.state['name'], '--config', path,
                         '--kubeconfig', DIRECTORY / 'kubeconfig', '--retain', '--wait', '120s')
        finally:
            ids = self.command('docker', 'ps', '-aq', '--no-trunc', '--filter',
                               'label=io.x-k8s.kind.cluster=' + self.state['name']).split()
            if len(ids) == 1:
                self.state['node_id'] = ids[0]
                self.save()
            if (DIRECTORY / 'kubeconfig').exists():
                (DIRECTORY / 'kubeconfig').chmod(0o600)

    def load_images(self):
        node = self.owned_node()
        if not node:
            raise RuntimeError('No owned node; run make dev-up')
        self.command('kind', 'load', 'docker-image', '--name', self.state['name'], *self.state['images'].values())

    def apply_registry(self):
        namespace = json.loads((ROOT / 'deploy/local/namespace.json').read_text())
        self.apply_manifest('namespace.json', namespace)
        manifest = json.loads((ROOT / 'deploy/local/registry.json').read_text())
        manifest['items'][2]['spec']['template']['spec']['containers'][0]['image'] = self.state['images']['registry']
        self.apply_manifest('registry.json', manifest)

    def apply_manifest(self, name, manifest):
        path = DIRECTORY / name
        path.write_text(json.dumps(manifest))
        self.kube('apply', '-f', path)

    def helm(self, *args):
        return self.command('helm', *args, '-n', NAMESPACE, '--kubeconfig', DIRECTORY / 'kubeconfig')

    def recover_helm(self):
        releases = json.loads(self.helm('list', '--all', '-o', 'json'))
        release = next((r for r in releases if r['name'] == 'buildkit-service'), None)
        if not release or not release['status'].startswith('pending-'):
            return
        history = json.loads(self.helm('history', 'buildkit-service', '-o', 'json'))
        (DIRECTORY / ('helm-history-' + str(time.time_ns()) + '.json')).write_text(json.dumps(history, indent=2))
        deployed = [r for r in history if r['status'] == 'deployed']
        target = deployed[-1]['revision'] if deployed else history[-1]['revision']
        # With no completed install, replay its recorded manifest as a new revision.
        # Do not wait on old configuration: the following upgrade applies fixes.
        self.helm('rollback', 'buildkit-service', str(target), '--wait=false')

    def deploy(self):
        self.recover_helm()
        values = json.loads((ROOT / 'deploy/local/values.json').read_text())
        values['image'] = self.state['images']['service']
        for name, section in [('apt', 'aptYum'), ('git', 'git'), ('pip', 'pip'), ('npm', 'npm'), ('maven', 'maven')]:
            repository, tag = self.state['images'][name].rsplit(':', 1)
            values['packageMirror'][section]['image'].update(repository=repository, tag=tag)
        path = DIRECTORY / 'values.json'
        path.write_text(json.dumps(values, indent=2))
        self.command('helm', 'upgrade', '--install', 'buildkit-service', ROOT / 'chart', '-n', NAMESPACE,
                     '--kubeconfig', DIRECTORY / 'kubeconfig', '-f', path, '--wait', '--timeout', '5m')

    def deployments(self):
        items = json.loads(self.kube('get', 'deployments', '-o', 'json'))['items']
        return [d for d in items if d['metadata'].get('annotations', {}).get('meta.helm.sh/release-name') == 'buildkit-service'
                or (d['metadata']['name'] == 'registry' and d['spec']['selector']['matchLabels'] == {'app': 'dev-registry'})]

    def wait_ready(self):
        for deployment in self.deployments():
            self.kube('rollout', 'status', 'deployment/' + deployment['metadata']['name'], '--timeout=180s', timeout=200)
        node = self.owned_node()
        ip = self.kube('get', 'service', 'registry', '-o', 'jsonpath={.spec.clusterIP}').strip()
        self.command('docker', 'exec', node, 'mkdir', '-p', '/etc/containerd/certs.d/' + REGISTRY)
        self.command('docker', 'exec', node, 'sh', '-ec', "printf '%s' \"$2\" > \"$1\"", 'registry-routing',
                     '/etc/containerd/certs.d/' + REGISTRY + '/hosts.toml',
                     '[host."http://' + ip + ':5000"]\n  capabilities = ["pull", "resolve"]\n')
        print('Ready: linux/amd64 workloads (' + ('native' if self.state['architecture'] == 'amd64' else 'emulated') + ')')

    def up(self, rebuild=False):
        self.step('preflight.tools', lambda: self.check_dependencies('rebuild' if rebuild else 'up'))
        self.step('preflight', self.initialize)
        if rebuild or not self.images_available():
            self.step('build', self.build_images)
        self.step('kind', self.cluster)
        self.step('images', self.load_images)
        self.step('registry', self.apply_registry)
        self.step('helm', self.deploy)
        self.step('ready', self.wait_ready)
        self.status()

    def status(self):
        print(self.helm('list', '--all'))
        print(self.kube('get', 'pods,svc,pvc', '-o', 'wide'))
        print(f'Kubeconfig: {DIRECTORY / "kubeconfig"}\nRegistry: http://127.0.0.1:15000\nPush target inside cluster: {REGISTRY}/<name>:<tag>')

    def down(self):
        if not self.owned_node():
            return
        for deployment in self.deployments():
            name = deployment['metadata']['name']
            self.kube('scale', 'deployment/' + name, '--replicas=0')
            selector = ','.join(k + '=' + v for k, v in deployment['spec']['selector']['matchLabels'].items())
            self.kube('wait', '--for=delete', 'pods', '-l', selector, '--timeout=120s')
        print('Applications stopped; kind and PVC data retained. Resume with make dev-up.')

    def reset(self):
        if os.environ.get('DEV_RESET_CONFIRM') != self.state['name']:
            raise RuntimeError('Deletes this cluster and all PVC data. Retry make dev-reset DEV_RESET_CONFIRM=' + self.state['name'])
        node = self.owned_node()
        if node:
            self.command('kind', 'delete', 'cluster', '--name', self.state['name'])
        self.state['node_id'] = None
        self.save()
        print('Dedicated cluster and its PVC data deleted; logs and source images retained.')

    def logs(self):
        destination = DIRECTORY / ('logs-' + str(time.time_ns()))
        destination.mkdir()
        errors = []
        try:
            self.command('kind', 'export', 'logs', destination / 'kind', '--name', self.state['name'])
        except (RuntimeError, OSError, subprocess.TimeoutExpired) as error:
            errors.append(str(error))
        for resource in ('pods', 'events', 'pvc'):
            try:
                (destination / (resource + '.log')).write_text(self.kube('describe', resource))
            except (RuntimeError, OSError, subprocess.TimeoutExpired) as error:
                errors.append(str(error))
        try:
            pods = json.loads(self.kube('get', 'pods', '-o', 'json'))['items']
        except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as error:
            errors.append('List pods: ' + str(error))
            pods = []
        for pod in pods:
            name = pod['metadata']['name']
            try:
                (destination / (name + '.log')).write_text(self.kube('logs', name, '--all-containers=true', '--tail=1000'))
            except (RuntimeError, OSError, subprocess.TimeoutExpired) as error:
                errors.append(name + ': ' + str(error))
        print('Diagnostics: ' + str(destination))
        if errors:
            (destination / 'errors.log').write_text('\n'.join(errors))
            raise RuntimeError('Some diagnostics failed; see ' + str(destination / 'errors.log'))


def main():
    def interrupt(signum, frame):
        raise KeyboardInterrupt('Interrupted; deployment resources retained')
    signal.signal(signal.SIGTERM, interrupt)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('up', 'rebuild', 'status', 'logs', 'down', 'reset'))
    args = parser.parse_args()
    dev = None
    try:
        dev = Dev()
        if args.action in ('up', 'rebuild'):
            dev.up(rebuild=args.action == 'rebuild')
        elif not dev.state:
            raise RuntimeError('No dev environment yet; run make dev-up')
        else:
            dev.step('preflight.tools', lambda: dev.check_dependencies(args.action))
            if args.action not in ('reset', 'down'):
                dev.connect_cluster()
            elif args.action == 'down' and dev.owned_node():
                dev.connect_cluster()
            dev.stage = args.action
            getattr(dev, args.action)()
    except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired, KeyboardInterrupt) as error:
        print(f'Failed at {dev.stage if dev else "initialize"}: {error}\nLogs: {DIRECTORY}/commands.log\n'
              'Environment retained. Use make dev-status / dev-logs; retry dev-up or dev-rebuild after correcting the error.', file=sys.stderr)
        return 1
    finally:
        if dev:
            dev.lock.close()
    return 0


if __name__ == '__main__':
    sys.exit(main())
