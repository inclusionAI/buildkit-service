import json
from pathlib import Path
import shutil
import subprocess
import unittest
import yaml

ROOT=Path(__file__).resolve().parents[2]


class ChartTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helm=shutil.which('helm') or str(ROOT/'artifacts/e2e-tools/bin/helm')
        if not Path(cls.helm).is_file():raise unittest.SkipTest('Run make e2e-setup to install Helm')

    def render(self,*settings):
        args=[self.helm,'template','test','chart','--set','buildctlDaemon.auth.token=validation-placeholder']
        for setting in settings:args.extend(['--set',setting])
        result=subprocess.run(args,cwd=ROOT,capture_output=True,text=True,check=True)
        return list(yaml.safe_load_all(result.stdout))

    def test_default_does_not_require_operator_or_collector(self):
        docs=self.render()
        self.assertFalse(any(d.get('kind') in ('PodMonitor','PrometheusRule') for d in docs))
        for d in docs:
            if d.get('kind')=='Deployment':
                for c in d['spec']['template']['spec']['containers']:
                    self.assertFalse(any(e['name'].startswith('OTEL_') for e in c.get('env',[])))

    def test_pod_identity_otel_secret_refs_and_monitor_ports(self):
        docs=self.render('observability.workerMetrics.enabled=true','observability.workerState.enabled=true','observability.tracing.enabled=true','observability.tracing.endpoint=https://collector:4317','observability.tracing.headersSecretName=otel-headers','observability.tracing.caSecretName=otel-ca','observability.podMonitor.enabled=true','observability.prometheusRule.enabled=true','observability.dashboard.enabled=true')
        monitors=[d for d in docs if d.get('kind')=='PodMonitor']
        self.assertGreaterEqual(len(monitors),6)
        deployments=[d for d in docs if d.get('kind') in ('Deployment','StatefulSet')]
        for monitor in monitors:
            selector=monitor['spec']['selector']['matchLabels']
            candidates=[d for d in deployments if all(d['spec']['template']['metadata']['labels'].get(k)==v for k,v in selector.items())]
            self.assertTrue(candidates,monitor)
            for candidate in candidates:
                ports={p['name'] for c in candidate['spec']['template']['spec']['containers'] for p in c.get('ports',[])}
                for ep in monitor['spec']['podMetricsEndpoints']:self.assertIn(ep['port'],ports)
        worker=next(d for d in deployments if d['spec']['template']['metadata']['labels'].get('app.kubernetes.io/component')=='buildkitd')
        env={e['name']:e for e in worker['spec']['template']['spec']['containers'][0]['env']}
        self.assertEqual(env['OTEL_EXPORTER_OTLP_HEADERS']['valueFrom']['secretKeyRef']['name'],'otel-headers')
        self.assertIn('$(POD_UID)',env['OTEL_RESOURCE_ATTRIBUTES']['value'])
        self.assertEqual(env['OTEL_EXPORTER_OTLP_CERTIFICATE']['value'],'/var/run/secrets/otel/ca.crt')
        self.assertTrue(any(d.get('kind')=='PrometheusRule' for d in docs))
        dashboard=next(d for d in docs if d.get('kind')=='ConfigMap' and 'buildkit-service.json' in d.get('data',{}))
        self.assertGreater(len(json.loads(dashboard['data']['buildkit-service.json'])['panels']),10)

    def test_invalid_tracing_configuration_fails_render(self):
        with self.assertRaises(subprocess.CalledProcessError):self.render('observability.tracing.enabled=true')

    def test_worker_observability_without_generated_buildkit_config(self):
        docs=self.render('buildkitdConfig.enabled=false','observability.workerMetrics.enabled=true','observability.tracing.enabled=true','observability.tracing.endpoint=http://collector:4317')
        worker=next(d for d in docs if d.get('kind')=='Deployment' and d['spec']['template']['metadata']['labels'].get('app.kubernetes.io/component')=='buildkitd')
        command=worker['spec']['template']['spec']['containers'][0]['command']
        self.assertEqual(command[command.index('--debugaddr')+1],'0.0.0.0:6060')
        self.assertIn('--log-format=json',command)


if __name__=='__main__':unittest.main()
