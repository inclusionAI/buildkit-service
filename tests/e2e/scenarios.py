"""Core scenarios in execution order; run.py owns the environment lifecycle."""
import json
import re
import time

from run import HERE, require


class Scenarios:
    def __init__(self, run):
        self.run = run

    def verify(self):
        run = self.run
        run.step('test.package-caches', self.package_caches)
        run.step('test.batch-client', self.prepare_batch_client)
        run.step('test.first-build-push-pull', self.build_fixture, 1)
        run.step('test.run-cache-hit', self.build_fixture, 2, True)
        run.step('test.worker-recovery', self.worker_recovery)
        run.step('test.mirror-recovery', self.mirror_recovery)
        run.step('test.source-image-ids', self.verify_image_ids)

    def package_caches(self, after_restart=False):
        script = (HERE / 'verify.py').read_text()
        self.run.client('python3', '-c', script, *(['--after-restart'] if after_restart else []),
                        timeout=600, name='package-tests' + ('-after-restart' if after_restart else ''))

    def prepare_batch_client(self):
        pod = self.run.fixture('batch.json')
        pod['spec']['containers'][0]['image'] = self.run.state['images']['service']['tag']
        self.run.apply(pod)
        self.run.wait_ready()

    def worker_recovery(self):
        self.restart('buildkitd')
        self.build_fixture(3)

    def mirror_recovery(self):
        self.restart('mirror')
        self.package_caches(after_restart=True)
        self.build_fixture(4)

    def build_fixture(self, number, expect_cache=False):
        target = f'registry.e2e.test:5000/result:round-{number}'
        self.write_build_context(target)
        output = self.submit_batch_build(number)
        if expect_cache:
            run_vertices = re.findall(r'(#\d+) \[[^\n]*\] RUN ', output)
            require(run_vertices and all(vertex + ' CACHED' in output for vertex in run_vertices),
                    'Second real build did not reuse RUN cache')
        self.check_batch_result(number)
        self.pull_and_run(target, number)

    def write_build_context(self, target):
        self.run.kubectl('-n', 'e2e', 'exec', 'batch', '--', 'mkdir', '-p', '/tmp/source/test')
        for name, content in [('Dockerfile', (HERE / 'fixtures/Dockerfile.build').read_text()),
                              ('metadata.json', json.dumps({'target': target}))]:
            # Small fixtures fit in argv; avoid relying on exec stdin delivery.
            path = '/tmp/source/test/' + name
            self.run.kubectl('-n', 'e2e', 'exec', 'batch', '--', 'sh', '-ec',
                             "printf '%s' \"$2\" > \"$1\"", 'write-fixture', path, content)
            actual = self.run.kubectl('-n', 'e2e', 'exec', 'batch', '--', 'cat', path,
                                      name='context-' + name)
            require(actual == content, 'Build context transfer mismatch: ' + path)

    def submit_batch_build(self, number):
        output = self.run.kubectl('-n', 'e2e', 'exec', 'batch', '--', 'buildctl-batch', 'build', '--oci',
                             '--image-dirs', '/tmp/source', '--addrs', 'tcp://buildkit-service.e2e.svc.cluster.local:9094',
                             '--result', f'/tmp/result-{number}.jsonl', '--logs', f'/tmp/logs-{number}.jsonl',
                             '--timeout', '120', '--retry', '0', '--verbose', timeout=180, name=f'build-round-{number}')
        return output + (self.run.directory / f'build-round-{number}.stderr.log').read_text()

    def check_batch_result(self, number):
        self.run.kubectl('-n', 'e2e', 'exec', 'batch', '--', 'buildctl-batch', 'export', '--oci',
                              '--from-result', f'/tmp/result-{number}.jsonl', '--result', f'/tmp/export-{number}.jsonl', '--with-fail')
        result = self.run.kubectl('-n', 'e2e', 'exec', 'batch', '--', 'cat', f'/tmp/export-{number}.jsonl')
        entries = [json.loads(line) for line in result.splitlines() if line.strip()]
        require(len(entries) == 1 and entries[0].get('success') is True, 'Batch did not report exactly one successful build')
        (self.run.directory / f'result-{number}.json').write_text(json.dumps(entries, indent=2))

    def pull_and_run(self, target, number):
        name = 'pull-' + str(number)
        pod = self.run.fixture('pull.json')
        pod['metadata']['name'] = name
        pod['spec']['containers'][0]['image'] = target
        self.run.apply(pod)
        self.run.kubectl('-n', 'e2e', 'wait', '--for=jsonpath={.status.phase}=Succeeded', 'pod/' + name, '--timeout=90s', timeout=110)
        require(self.run.kubectl('-n', 'e2e', 'logs', name).strip() == 'e2e-build-ok', 'Pulled image output mismatch')
        (self.run.directory / f'pull-round-{number}.json').write_text(
            self.run.kubectl('-n', 'e2e', 'get', 'pod', name, '-o', 'json'))
        self.run.kubectl('-n', 'e2e', 'delete', 'pod', name)

    def restart(self, component):
        selector = 'app.kubernetes.io/component=buildkitd' if component == 'buildkitd' else 'app.kubernetes.io/name=package-mirror'
        pods = json.loads(self.run.kubectl('-n', 'e2e', 'get', 'pods', '-l', selector, '-o', 'json'))['items']
        require(len(pods) == 1, 'Expected one ' + component + ' pod')
        old = pods[0]['metadata']['uid']
        self.run.kubectl('-n', 'e2e', 'delete', 'pod', pods[0]['metadata']['name'], '--wait=true', timeout=120)
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            current = json.loads(self.run.kubectl('-n', 'e2e', 'get', 'pods', '-l', selector, '-o', 'json'))['items']
            if len(current) == 1 and current[0]['metadata']['uid'] != old and not current[0]['metadata'].get('deletionTimestamp') and any(
                    c.get('type') == 'Ready' and c.get('status') == 'True'
                    for c in current[0].get('status', {}).get('conditions', [])):
                return
            time.sleep(2)
        raise RuntimeError(component + ' failed to recover')

    def verify_image_ids(self):
        pods = json.loads(self.run.kubectl('-n', 'e2e', 'get', 'pods', '-o', 'json'))['items']
        expected = {v['tag']: v['id'] for v in self.run.state['images'].values()}
        seen = set()
        for pod in pods:
            for container in pod.get('status', {}).get('containerStatuses', []):
                ref = container['image'].removeprefix('docker.io/library/')
                if ref in expected:
                    require(container['imageID'].endswith(expected[ref]), 'Unexpected deployed image ID: ' + ref)
                    seen.add(ref)
        require(set(expected) <= seen, 'Some built/loaded images were not verified in running Pods')
        (self.run.directory / 'pods-final.json').write_text(json.dumps(pods, indent=2))
