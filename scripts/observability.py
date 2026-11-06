#!/usr/bin/env python3
"""Isolated local observability stack, source builds and backend assertions."""
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zipfile

ROOT = Path(__file__).resolve().parents[1]
CONFIG = ROOT / 'deploy/local/observability'
LABEL = 'com.docker.compose.project'


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def get_json(url, data=None, headers=None, method=None):
    request = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
    # All validation HTTP endpoints are explicit loopback bindings.
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=10) as r:
        return json.load(r)


def eventually(fn, timeout=90):
    end = time.monotonic() + timeout
    last = None
    while time.monotonic() < end:
        try:
            value = fn()
            if value:
                return value
        except (OSError, ValueError, KeyError, RuntimeError) as error:
            last = error
        time.sleep(1)
    raise RuntimeError(f'Timed out waiting for {fn.__name__}: {last}')


class Stack:
    def __init__(self):
        self.directory = Path(os.environ.get('OBS_RUN_DIR') or ROOT / 'artifacts/observability').resolve()
        self.directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.lock = (self.directory / '.lock').open('a')
        fcntl.flock(self.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.path = self.directory / 'state.json'
        self.state = json.loads(self.path.read_text()) if self.path.exists() else {}
        self.dependencies = json.loads((ROOT / 'tests/e2e/dependencies.json').read_text())
        self.env = dict(os.environ)
        for key in ('DOCKER_CONTEXT', 'DOCKER_DEFAULT_PLATFORM', 'BUILDX_BUILDER'):
            self.env.pop(key, None)
        if self.state:
            self.configure()

    def save(self):
        tmp = self.path.with_suffix('.tmp')
        tmp.write_text(json.dumps(self.state, indent=2) + '\n')
        tmp.replace(self.path)

    def configure(self):
        self.env.update(DOCKER_HOST=self.state['endpoint'], DOCKER_CONFIG=str(self.directory / 'docker'))
        self.env['OBS_RUN_DIR'] = str(self.directory)
        self.env['OBS_PROJECT'] = self.state['name']
        self.env['OBS_DOCKER_ROOT'] = self.state['docker_root']
        self.env['OBS_SERVICE_IMAGE'] = self.state['name'] + '-service:source'
        for name, entry in self.dependencies.get('observability_images', {}).items():
            self.env['OBS_IMAGE_' + name.upper()] = entry['repository'] + '@' + entry['platforms']['amd64']
        for name in ('python', 'registry'):
            self.env['OBS_IMAGE_' + name.upper()] = self.image(name)

    def command(self, args, timeout=120, label='command', check=True):
        with (self.directory / 'commands.jsonl').open('a') as f:
            f.write(json.dumps({'time':time.time(), 'argv':args}) + '\n')
        with (self.directory / (label+'.log')).open('a+') as out, (self.directory / (label+'.stderr.log')).open('a+') as err:
            position=out.tell()
            result=subprocess.run(args,cwd=ROOT,env=self.env,stdout=out,stderr=err,text=True,timeout=timeout)
            out.seek(position)
            output=out.read()
        if check and result.returncode:
            raise RuntimeError(f'{label} failed ({result.returncode}); see {self.directory / (label+".stderr.log")}')
        return output

    def docker(self, *args, **kw):
        return self.command(['docker', *map(str,args)], **kw)

    def compose(self, *args, **kw):
        return self.docker('compose', '--project-name', self.state['name'], '--file', self.directory / 'config/compose.yaml', *args, **kw)

    def image(self, name):
        entry = self.dependencies['images'][name]
        return entry['repository'] + '@' + entry['platforms']['amd64']

    def initialize(self):
        if self.state:
            return
        require(sys.platform=='linux','Local observability validation currently requires a Linux host')
        info = json.loads(self.docker('info','--format','{{json .}}', label='docker-info'))
        arch = info.get('Architecture')
        require(info['OSType']=='linux' and arch in ('amd64','x86_64'), 'Local observability validation requires native Linux amd64 Docker')
        context = self.command(['docker','context','show']).strip()
        details = json.loads(self.command(['docker','context','inspect',context]))[0]
        endpoint = os.environ.get('DOCKER_HOST') or details['Endpoints']['docker']['Host']
        require(endpoint.startswith('unix://'), 'Use a local Unix Docker endpoint; remote host bind mounts are unsupported')
        require(shutil.disk_usage(self.directory).free > 20*1024**3, 'At least 20 GiB free space required')
        self.state = {'schema':1,'name':'bks-obs-'+uuid.uuid4().hex[:10], 'endpoint':endpoint,'docker_root':info['DockerRootDir'],'architecture':'linux/amd64','phase':'prepared','containers':{},'followers':{}}
        docker_config = self.directory / 'docker'
        docker_config.mkdir(mode=0o700, exist_ok=True)
        plugins = sorted({str(Path(p['Path']).parent) for p in info.get('ClientInfo',{}).get('Plugins',[]) if p.get('Name') in ('buildx','compose')})
        (docker_config/'config.json').write_text(json.dumps({'auths':{},'cliPluginsExtraDirs':plugins}))
        (docker_config/'config.json').chmod(0o600)
        self.save(); self.configure()

    def prepare_config(self):
        target = self.directory / 'config'
        target.mkdir(exist_ok=True)
        volumes=json.loads((CONFIG/'compose.yaml').read_text())['volumes']
        for name in volumes:
            directory=self.directory/'data'/name
            if not directory.exists():
                directory.mkdir(parents=True,mode=0o777)
                directory.chmod(0o777)
        (self.directory/'logs').mkdir(exist_ok=True)
        for path in CONFIG.glob('*.yaml'):
            if path.name != 'compose.yaml':
                (target/path.name).write_text(path.read_text().replace('${OBS_PROJECT}', self.state['name']))
        for name in ('alerts.yaml','dashboard.json'):
            shutil.copyfile(ROOT/'chart/observability'/name, target/name)
        template = (ROOT/'chart/templates/_dir-size-exporter.tpl').read_text()
        script = template.split('{{- define "buildkit-service.dirSizeExporterScript" -}}',1)[1].split('{{- end -}}',1)[0]
        (target/'exporter.py').write_text(script)
        shutil.copyfile(CONFIG/'worker-entrypoint.sh',target/'worker-entrypoint.sh')
        (target/'buildkitd.toml').write_text('[registry."registry:5000"]\n  http = true\n[worker.oci]\n  gc = true\n  networkMode = "host"\n  gckeepstorage = 1024\n')
        compose=json.loads((CONFIG/'compose.yaml').read_text())
        if not Path('/dev/kmsg').exists():
            compose['services']['cadvisor'].pop('devices',None)
        bindings={'worker1':['buildkitd.toml','worker-entrypoint.sh'],'worker2':['buildkitd.toml','worker-entrypoint.sh'],'cache1':['exporter.py'],'cache2':['exporter.py'],'prometheus':['prometheus.yaml','alerts.yaml'],'collector':['collector.yaml'],'tempo':['tempo.yaml'],'loki':['loki.yaml'],'grafana':['datasources.yaml','dashboards.yaml','dashboard.json'],'alertmanager':['alertmanager.yaml']}
        for name,files in bindings.items():
            digest=hashlib.sha256(b''.join((target/file).read_bytes() for file in files)).hexdigest()
            compose['services'][name].setdefault('labels',{})['buildkit.observability.config-hash']=digest
        (target/'compose.yaml').write_text(json.dumps(compose,indent=2)+'\n')
        token = self.directory/'api-token'
        if not token.exists():
            token.write_text(secrets.token_hex(32))
            token.chmod(0o600)

    def build(self):
        self.initialize(); self.prepare_config()
        source = self.command(['git','rev-parse','HEAD']).strip()
        files = self.command(['git','ls-files','--cached','--others','--exclude-standard','-z']).split('\0')
        hashes = {name:hashlib.sha256((ROOT/name).read_bytes()).hexdigest() for name in files if name and (ROOT/name).is_file()}
        build_directory=self.directory/'builds'/uuid.uuid4().hex
        build_directory.mkdir(parents=True)
        (build_directory/'source-files.json').write_text(json.dumps(hashes,indent=2)+'\n')
        (self.directory/'source-files.json').write_text(json.dumps(hashes,indent=2)+'\n')
        self.state.update(source_head=source,source_hash=hashlib.sha256(json.dumps(hashes,sort_keys=True).encode()).hexdigest())
        self.save()
        print('Building current source image', flush=True)
        args = ['build','--platform','linux/amd64','--label','buildkit.observability.owner='+self.state['name'],'--label','buildkit.observability.source-head='+source,'--label','buildkit.observability.source-tree='+self.state['source_hash'],'-f','chart/images/buildkit/Dockerfile','-t',self.env['OBS_SERVICE_IMAGE']]
        for name,key in (('buildkit','BASE_IMAGE'),('go','GO_IMAGE'),('alpine','ALPINE_IMAGE')):
            args += ['--build-arg',key+'='+self.image(name)]
        for key in ('HTTP_PROXY','HTTPS_PROXY','http_proxy','https_proxy'):
            if self.env.get(key):
                args += ['--build-arg',key]
        self.docker(*args,'.',timeout=1800,label='build-service')
        image = json.loads(self.docker('image','inspect',self.env['OBS_SERVICE_IMAGE']))[0]
        require(image['Architecture']=='amd64','Source image is not amd64')
        self.state.update(service_image_id=image['Id'],phase='built')
        self.save()

    def capture(self):
        ids = self.compose('ps','--all','--quiet').split()
        containers = {}
        for cid in ids:
            info = json.loads(self.docker('inspect',cid))[0]
            require(info['Config']['Labels'].get(LABEL)==self.state['name'],'Container ownership mismatch')
            service = info['Config']['Labels']['com.docker.compose.service']
            containers[service] = {'id':info['Id'],'image_id':info['Image'],'running':info['State']['Running']}
            if service in ('daemon','worker1','worker2','observer1','observer2'):
                require(info['Image']==self.state['service_image_id'],f'{service} does not run the source-built image')
        self.state['containers'] = containers
        self.save()

    def assert_owned(self):
        for service, obj in self.state.get('containers',{}).items():
            info = json.loads(self.docker('inspect',obj['id']))[0]
            require(info['Config']['Labels'].get(LABEL)==self.state['name'],f'{service} ownership changed')

    def endpoint(self, service, port):
        return 'http://' + self.compose('port',service,str(port)).strip()

    def followers(self):
        self.stop_followers()
        for service in ('daemon','worker1','worker2'):
            cid = self.state['containers'][service]['id']
            with (self.directory/'logs'/(service+'.jsonl')).open('ab') as out:
                proc = subprocess.Popen(['docker','logs','--follow',cid], env=self.env, stdout=out, stderr=out, start_new_session=True)
            self.state['followers'][service]={'pid':proc.pid,'container':cid}
        self.save()

    def stop_followers(self):
        for record in self.state.get('followers',{}).values():
            pid = record['pid']
            try:
                cmdline = Path(f'/proc/{pid}/cmdline').read_bytes().replace(b'\0',b' ').decode()
                if record['container'] in cmdline and 'logs' in cmdline:
                    os.kill(pid,signal.SIGTERM)
            except (FileNotFoundError, ProcessLookupError):
                pass
        self.state['followers']={}

    def up(self):
        self.initialize(); self.assert_owned(); self.prepare_config()
        require(self.state.get('service_image_id'),'Run make observability-build first')
        for name in self.dependencies['observability_images']:
            print('Pulling '+name,flush=True)
            self.docker('pull','--platform','linux/amd64',self.env['OBS_IMAGE_'+name.upper()],timeout=600,label='pull-'+name)
        for name in ('python','registry'):
            self.docker('pull','--platform','linux/amd64',self.image(name),timeout=600,label='pull-'+name)
        try:
            self.compose('up','--detach','--no-build',timeout=180,label='compose-up')
        finally:
            self.capture()
        self.followers()
        self.state['endpoints']={s:self.endpoint(s,p) for s,p in [('daemon',8080),('registry',5000),('prometheus',9090),('alertmanager',9093),('tempo',3200),('loki',3100),('grafana',3000),('collector',13133)]}
        self.save()
        eventually(lambda:get_json(self.state['endpoints']['daemon']+'/healthz'))
        eventually(lambda:get_json(self.state['endpoints']['prometheus']+'/api/v1/status/buildinfo'))
        eventually(lambda:get_json(self.state['endpoints']['grafana']+'/api/health'))
        self.state['phase']='running';self.save()
        print(json.dumps(self.state['endpoints'],indent=2),flush=True)

    def api(self, path, data=None, method=None, headers=None):
        auth={'Authorization':'Bearer '+(self.directory/'api-token').read_text()}
        auth.update(headers or {})
        return get_json(self.state['endpoints']['daemon']+path,data=data,method=method,headers=auth)

    def submit(self, dockerfile, files=None, trace_id=None, **fields):
        buf=io.BytesIO()
        with zipfile.ZipFile(buf,'w') as archive:
            archive.writestr('Dockerfile',dockerfile)
            for name,body in (files or {}).items():archive.writestr(name,body)
        boundary='bks'+uuid.uuid4().hex
        body=bytearray()
        for key,value in {'image':'registry:5000/validation:'+uuid.uuid4().hex,'image_type':'oci','retry':'0',**fields}.items():
            body.extend(f'--{boundary}\r\nContent-Disposition: form-data; name="{key}"\r\n\r\n{value}\r\n'.encode())
        body.extend(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="source.zip"\r\nContent-Type: application/zip\r\n\r\n'.encode())
        body.extend(buf.getvalue());body.extend(f'\r\n--{boundary}--\r\n'.encode())
        headers={'Content-Type':'multipart/form-data; boundary='+boundary}
        if trace_id: headers['traceparent']='00-'+trace_id+'-'+uuid.uuid4().hex[:16]+'-01'
        return self.api('/v1/builds',bytes(body),method='POST',headers=headers)

    def terminal(self, task, expected='succeeded'):
        def read():
            record=self.api('/v1/builds/'+task['id'])
            if record['status'] in ('succeeded','failed','canceled'):
                require(record['status']==expected, f'Build expected {expected}, got {record}')
                return record
        return eventually(read,timeout=180)

    def query(self, expression):
        response=get_json(self.state['endpoints']['prometheus']+'/api/v1/query?'+urllib.parse.urlencode({'query':expression}))
        require(response.get('status')=='success','Prometheus query failed')
        return response['data']['result']

    def resolved_workers(self):
        workers={}
        for name in ('worker1','worker2'):
            info=json.loads(self.docker('inspect',self.state['containers'][name]['id']))[0]
            network=info['NetworkSettings']['Networks'][self.state['name']+'_default']
            workers[network['IPAddress']]=name
        # DNS can initially return a hostname fallback for just one worker.
        # Slot order changes once both addresses resolve, even for the same key.
        expected={'tcp://'+ip+':9094' for ip in workers}
        eventually(lambda:{r['metric']['worker'] for r in self.query('buildctl_daemon_worker_slots_capacity')}==expected)
        return workers

    @contextmanager
    def worker_network_outage(self, worker):
        require(worker in ('worker1','worker2'),'Unknown validation worker')
        self.assert_owned()
        container=self.state['containers'][worker]['id']
        info=json.loads(self.docker('inspect',container))[0]
        require(info['HostConfig']['NetworkMode']==self.state['name']+'_default',
                'Worker fault injection requires the owned private network namespace')
        rule=('INPUT','-p','tcp','-m','multiport','--dports','6060,9094',
              '-m','comment','--comment','buildkit-observability-failover','-j','REJECT','--reject-with','tcp-reset')
        # Keep discovery stable while making the selected worker unreachable.
        # These rules belong to this worker container's network namespace only.
        self.docker('exec',container,'iptables','-w','5','-I',*rule,label='block-worker-ports')
        try:
            yield
        finally:
            self.docker('exec',container,'iptables','-w','5','-D',*rule,label='restore-worker-ports')

    def test(self):
        self.assert_owned();self.capture()
        results=[]
        started=time.time()
        test_directory=self.directory/'tests'/uuid.uuid4().hex
        test_directory.mkdir(parents=True)
        self.state['last_test_directory']=str(test_directory);self.save()
        def save_report(status):
            (test_directory/'report.json').write_text(json.dumps({'status':status,'source_head':self.state['source_head'],'source_hash':self.state['source_hash'],'service_image_id':self.state['service_image_id'],'architecture':'linux/amd64','started':started,'results':results},indent=2)+'\n')
        def passed(name,details=None):
            results.append({'scenario':name,'status':'passed','details':details})
            save_report('running')
            print('PASS '+name,flush=True)
        try:
            eventually(lambda:len(self.query('up{buildkit_component=~"worker|worker-state|cache|daemon|collector"} == 1'))==8)
            passed('individual-worker-cache-daemon-collector-targets')
            # Seed a pinned base into the task's own registry; no cloud push.
            self.docker('pull','--platform','linux/amd64',self.image('alpine'),timeout=600,label='pull-fixture-base')
            destination=self.state['endpoints']['registry'].removeprefix('http://')+'/fixture:alpine'
            self.docker('tag',self.image('alpine'),destination)
            self.docker('push',destination,timeout=120,label='seed-local-registry')
            trace_id=uuid.uuid4().hex
            marker='observability-'+uuid.uuid4().hex
            source='FROM scratch\nCOPY marker.txt /marker.txt\n'
            success=self.terminal(self.submit(source,{'marker.txt':marker},trace_id=trace_id))
            require(success.get('trace_id')==trace_id,'Caller trace context did not survive async build')
            tag=success['image'].rsplit(':',1)[1]
            manifest=get_json(self.state['endpoints']['registry']+'/v2/validation/manifests/'+tag,headers={'Accept':'application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json'})
            require(manifest.get('schemaVersion')==2,'Build did not push a real manifest')
            passed('real-build-and-OCI-registry-push',{'build_id':success['id'],'trace_id':trace_id})
            eventually(lambda:self.query('buildkit_builds_total'))
            eventually(lambda:self.query('buildkit_builds_steps_total'))
            eventually(lambda:self.query('histogram_count(buildkit_build_duration_seconds) >= 1'))
            passed('native-BuildKit-completion-and-step-metrics')
            # Exercise the static CGO/LMDB batch binary in a separate owned client.
            batch_root=test_directory/'batch-input';context=batch_root/'image';context.mkdir(parents=True)
            batch_tag=uuid.uuid4().hex
            (context/'Dockerfile').write_text('FROM scratch\nCOPY marker.txt /marker.txt\n')
            (context/'marker.txt').write_text('batch-'+batch_tag)
            (context/'metadata.json').write_text(json.dumps({'target':'registry:5000/batch-validation:'+batch_tag}))
            output=test_directory/'batch-output';output.mkdir()
            network=self.state['name']+'_default'
            network_info=json.loads(self.docker('network','inspect',network))[0]
            require(network_info['Labels'].get(LABEL)==self.state['name'],'Batch network ownership mismatch')
            self.docker('run','--rm','--name',self.state['name']+'-batch-'+batch_tag[:8],'--label','buildkit.observability.owner='+self.state['name'],'--network',network,
                        '--volume',str(batch_root)+':/input:ro','--volume',str(output)+':/output',
                        '--env','OTEL_SERVICE_NAME=buildctl-batch','--env','OTEL_TRACES_EXPORTER=otlp','--env','OTEL_METRICS_EXPORTER=none','--env','OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4317','--env','OTEL_BSP_SCHEDULE_DELAY=100',
                        '--entrypoint','buildctl-batch',self.state['service_image_id'],'build','--image-dirs','/input','--addrs','tcp://worker1:9094','--result','/output/result','--logs','/output/failures.jsonl','--concurrency','1','--oci','--timeout','30',timeout=90,label='batch-'+batch_tag)
            event_lines=(self.directory/('batch-'+batch_tag+'.stderr.log')).read_text().splitlines()
            events=[json.loads(line) for line in event_lines if line.startswith('{')]
            batch_event=next(x for x in events if x.get('msg')=='batch_build_finished')
            batch_trace=batch_event['trace_id']
            (self.directory/'logs/batch.jsonl').write_text('\n'.join(json.dumps(x) for x in events)+'\n')
            require((output/'result').is_file(),'Batch did not persist an LMDB result')
            self.docker('run','--rm','--label','buildkit.observability.owner='+self.state['name'],
                        '--network','none','--volume',str(output)+':/output',
                        '--entrypoint','buildctl-batch',self.state['service_image_id'],'export','--from-result','/output/result','--result','/output/result.jsonl','--oci',timeout=30,label='batch-export-'+batch_tag)
            entries=[json.loads(line) for line in (output/'result.jsonl').read_text().splitlines()]
            require(len(entries)==1 and entries[0]['target']=='registry:5000/batch-validation:'+batch_tag and entries[0]['success'],'Batch LMDB did not retain the successful target')
            get_json(self.state['endpoints']['registry']+'/v2/batch-validation/manifests/'+batch_tag,headers={'Accept':'application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json'})
            def batch_trace_complete():
                data=get_json(self.state['endpoints']['tempo']+'/api/traces/'+batch_trace)
                text=json.dumps(data)
                return 'batch.build' in text and 'buildkit-worker1' in text
            eventually(batch_trace_complete)
            passed('batch-CLI-LMDB-OCI-push-and-native-trace',{'trace_id':batch_trace})

            def trace_complete():
                data=get_json(self.state['endpoints']['tempo']+'/api/traces/'+trace_id)
                text=json.dumps(data)
                return data if 'buildctl-daemon' in text and 'buildkit-worker' in text and 'buildkit.solve' in text else None
            trace=eventually(trace_complete)
            (test_directory/'trace.json').write_text(json.dumps(trace,indent=2)+'\n')
            passed('caller-to-async-task-to-native-worker-trace')
            failed=self.terminal(self.submit('FROM registry:5000/fixture:alpine\nRUN echo OBSERVABILITY_EXPECTED_FAILURE >&2; exit 17\n'),expected='failed')
            require('exit code: 17' in failed.get('error',''),'RUN did not execute the expected failing command')
            eventually(lambda:self.query('buildctl_daemon_builds_failed_total >= 1'))
            passed('real-build-failure-observed',{'build_id':failed['id']})
            memory_query='sum(container_memory_working_set_bytes{container_label_com_docker_compose_service=~"worker1|worker2"})'
            baseline_series=eventually(lambda:self.query(memory_query))
            baseline_memory=float(baseline_series[0]['value'][1])
            slow=self.submit('FROM registry:5000/fixture:alpine\nRUN dd if=/dev/zero of=/dev/shm/observability bs=1M count=32 && sleep 12\n',no_cache='true')
            eventually(lambda:self.api('/v1/builds/'+slow['id'])['status']=='running')
            eventually(lambda:self.query('sum(buildkit_worker_builds_active) >= 1'))
            eventually(lambda:self.query(memory_query+' > '+str(baseline_memory+16*1024*1024)),timeout=15)
            passed('RUN-workload-memory-accounted-by-worker-cgroup')
            queued=self.submit('FROM scratch\nLABEL queued="'+uuid.uuid4().hex+'"\n')
            eventually(lambda:self.query('buildctl_daemon_builds_queued >= 1'))
            eventually(lambda:self.query('buildctl_daemon_queue_oldest_age_seconds > 0'))
            before=self.query('buildctl_daemon_builds_canceled_total')
            before_value=float(before[0]['value'][1]) if before else 0
            for _ in range(2):self.api('/v1/builds/'+queued['id'],method='DELETE')
            self.terminal(queued,expected='canceled');self.terminal(slow)
            eventually(lambda:self.query('buildctl_daemon_builds_canceled_total == '+str(before_value+1)))
            eventually(lambda:self.query('buildctl_daemon_queue_wait_seconds_count >= 1'))
            passed('queue-watermarks-and-idempotent-queued-cancellation')
            workers=self.resolved_workers()
            preferred=self.terminal(self.submit(source,{'marker.txt':marker},trace_id=uuid.uuid4().hex))
            worker=workers.get(preferred.get('node_ip'))
            require(worker,'Could not identify the current preferred build worker')
            before=self.query('buildctl_daemon_retries_total{reason="worker_failover"}')
            retries_before=float(before[0]['value'][1]) if before else 0
            with self.worker_network_outage(worker):
                eventually(lambda:self.query(f'up{{job="buildkit-worker",instance="{worker}:6060"}} == 0'))
                # Identical source and unchanged discovery must attempt this worker.
                recovered=self.terminal(self.submit(source,{'marker.txt':marker},trace_id=uuid.uuid4().hex))
                require(recovered.get('node_ip')!=preferred.get('node_ip'),'Build did not move away from unreachable worker')
                eventually(lambda:self.query('buildctl_daemon_retries_total{reason="worker_failover"} >= '+str(retries_before+1)))
                passed('worker-outage-per-target-health-and-real-build-failover')
                print('Waiting for the production two-minute target-down alert',flush=True)
                def firing():
                    alerts=get_json(self.state['endpoints']['prometheus']+'/api/v1/alerts')['data']['alerts']
                    return any(a['labels'].get('alertname')=='BuildKitTargetDown' and a['labels'].get('instance')==worker+':6060' and a['state']=='firing' for a in alerts)
                eventually(firing,timeout=180)
                def delivered():
                    alerts=get_json(self.state['endpoints']['alertmanager']+'/api/v2/alerts')
                    return any(a['labels'].get('alertname')=='BuildKitTargetDown' and a['labels'].get('instance')==worker+':6060' for a in alerts)
                eventually(delivered)
                passed('real-alert-firing-and-Alertmanager-delivery')
            eventually(lambda:len(self.query('up{job="buildkit-worker"} == 1'))==2)
            def alert_resolved():
                alerts=get_json(self.state['endpoints']['prometheus']+'/api/v1/alerts')['data']['alerts']
                return not any(a['labels'].get('alertname')=='BuildKitTargetDown' and a['labels'].get('instance')==worker+':6060' for a in alerts)
            eventually(alert_resolved)
            passed('worker-recovery-and-alert-resolution')
            self.compose('stop','collector',timeout=30,label='stop-collector')
            try:
                eventually(lambda:self.query('up{job="otel-collector"} == 0'))
                began=time.monotonic()
                during_outage=self.terminal(self.submit('FROM scratch\nLABEL collector="'+uuid.uuid4().hex+'"\n'))
                require(time.monotonic()-began<30,'Collector outage blocked the build')
                passed('collector-outage-does-not-block-real-build',{'build_id':during_outage['id']})
            finally:
                self.compose('start','collector',timeout=30,label='restore-collector')
                self.state['endpoints']['collector']=self.endpoint('collector',13133)
                self.save()
            eventually(lambda:self.query('up{job="otel-collector"} == 1'))
            def outage_trace_recovered():
                text=json.dumps(get_json(self.state['endpoints']['tempo']+'/api/traces/'+during_outage['trace_id']))
                return 'buildctl-daemon' in text and 'buildkit-worker' in text
            eventually(outage_trace_recovered)
            def outage_logs_recovered():
                query=urllib.parse.urlencode({'query':'{service_name="buildctl-daemon"} |= "'+during_outage['id']+'"','start':str(int((started-60)*1e9)),'limit':'100'})
                text=json.dumps(get_json(self.state['endpoints']['loki']+'/loki/api/v1/query_range?'+query))
                return during_outage['trace_id'] in text and 'build_finished' in text
            eventually(outage_logs_recovered)
            passed('buffered-traces-and-logs-arrive-after-collector-recovery')
            selector='{service_name="buildctl-daemon"} |= "'+success['id']+'"'
            def correlated_logs():
                query=urllib.parse.urlencode({'query':selector,'start':str(int((started-60)*1e9)),'limit':'100'})
                data=get_json(self.state['endpoints']['loki']+'/loki/api/v1/query_range?'+query)
                text=json.dumps(data)
                return data if trace_id in text and 'build_finished' in text else None
            logs=eventually(correlated_logs)
            (test_directory/'correlated-logs.json').write_text(json.dumps(logs,indent=2)+'\n')
            passed('centralized-logs-correlate-build-and-trace')
            eventually(lambda:self.query('buildkit_service_cache_allocated_bytes'))
            budgets=self.query('buildkit_service_cache_limit_bytes')
            require(len(budgets)==2 and all(float(x['value'][1])==2147483648 for x in budgets),'Cache budget or per-worker identity lost')
            eventually(lambda:self.query('buildkit_service_filesystem_available_bytes > 0'))
            eventually(lambda:self.query('buildkit_service_filesystem_free_inodes > 0'))
            eventually(lambda:self.query('container_memory_working_set_bytes{container_label_com_docker_compose_service=~"worker1|worker2"}'))
            eventually(lambda:self.query('rate(container_cpu_usage_seconds_total{container_label_com_docker_compose_service=~"worker1|worker2"}[30s])'))
            eventually(lambda:self.query('buildkit_worker_cache_bytes{state="reclaimable"}'))
            passed('storage-budgets-free-space-inodes-cache-accounting-container-CPU-and-memory')
            end=time.time()
            history=get_json(self.state['endpoints']['prometheus']+'/api/v1/query_range?'+urllib.parse.urlencode({'query':'up{job="buildkit-worker"}','start':started,'end':end,'step':'2s'}))['data']['result']
            require(len(history)==2 and any(any(float(v[1])==0 for v in item['values']) for item in history),'Historical worker outage missing')
            (test_directory/'worker-history.json').write_text(json.dumps(history,indent=2)+'\n')
            self.compose('restart','prometheus',timeout=60,label='restart-prometheus')
            # Docker may allocate a new ephemeral host port on every start.
            self.state['endpoints']['prometheus']=self.endpoint('prometheus',9090)
            self.save()
            eventually(lambda:get_json(self.state['endpoints']['prometheus']+'/api/v1/status/buildinfo'))
            persisted=get_json(self.state['endpoints']['prometheus']+'/api/v1/query_range?'+urllib.parse.urlencode({'query':'up{job="buildkit-worker"}','start':started,'end':end,'step':'2s'}))['data']['result']
            samples=lambda series:{json.dumps(item['metric'],sort_keys=True):item['values'] for item in series}
            require(samples(persisted)==samples(history),'Historical samples did not survive Prometheus restart')
            passed('Prometheus-history-survives-server-restart')
            dashboard=get_json(self.state['endpoints']['grafana']+'/api/dashboards/uid/buildkit-service')
            require(len(dashboard['dashboard']['panels'])>=10,'Grafana dashboard not provisioned')
            datasource=get_json(self.state['endpoints']['grafana']+'/api/datasources/uid/loki')
            derived=datasource['jsonData']['derivedFields'][0]
            require(derived['datasourceUid']=='tempo' and derived['url']=='${__value.raw}' and re.search(derived['matcherRegex'],json.dumps({'trace_id':trace_id})).group(1)==trace_id,'Grafana log-to-trace link is invalid')
            passed('persistent-timeseries-and-provisioned-Grafana-dashboard')
            self.state.update(phase='tested',last_test=time.time(),report=str(test_directory/'report.json'))
            self.capture();self.save()
            save_report('passed')
            shutil.copyfile(test_directory/'report.json',self.directory/'report.json')
        except BaseException as error:
            results.append({'scenario':'failed','status':'failed','error':str(error)})
            save_report('failed')
            raise
        finally:
            self.logs()

    def logs(self):
        if self.state.get('containers'):
            self.compose('logs','--no-color','--tail','200',label='compose-logs')

    def down(self):
        require(self.state,'No owned stack')
        self.assert_owned();self.stop_followers();self.save()
        self.compose('stop',timeout=120,label='compose-stop')
        self.capture();self.state['phase']='stopped';self.save()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=('build','up','test','all','status','logs','down'))
    action=parser.parse_args().action
    stack=Stack()
    try:
        if action=='all':stack.build();stack.up();stack.test()
        elif action=='status':print(json.dumps(stack.state,indent=2))
        else:getattr(stack,action)()
    except BaseException:
        if stack.state:
            stack.state['last_failed_action']=action;stack.save()
            if action not in ('logs','down'):stack.logs()
        raise
    finally:
        stack.lock.close()


if __name__=='__main__':main()
