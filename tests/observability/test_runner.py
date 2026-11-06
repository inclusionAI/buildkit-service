import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('observability_runner',ROOT/'scripts/observability.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)


class RunnerTests(unittest.TestCase):
    def test_loopback_ports_data_and_pinned_dependencies(self):
        compose=json.loads((ROOT/'deploy/local/observability/compose.yaml').read_text())
        for service in compose['services'].values():
            self.assertNotIn('container_name',service)
            for port in service.get('ports',[]):self.assertTrue(port.startswith('127.0.0.1::'))
        for volume in compose['volumes'].values():self.assertTrue(volume['driver_opts']['device'].startswith('${OBS_RUN_DIR}/data/'))
        locked=json.loads((ROOT/'tests/e2e/dependencies.json').read_text())['observability_images']
        for entry in locked.values():self.assertRegex(entry['platforms']['amd64'],r'^sha256:[0-9a-f]{64}$')

    def test_configuration_is_repeatable_and_token_is_preserved(self):
        with tempfile.TemporaryDirectory() as directory,patch.dict(os.environ,{'OBS_RUN_DIR':directory}):
            stack=module.Stack()
            try:
                stack.state={'name':'bks-obs-0123456789'}
                stack.prepare_config()
                token=(Path(directory)/'api-token').read_bytes()
                first=json.loads((Path(directory)/'config/compose.yaml').read_text())
                stack.prepare_config()
                self.assertEqual((Path(directory)/'api-token').read_bytes(),token)
                self.assertEqual(json.loads((Path(directory)/'config/compose.yaml').read_text()),first)
                self.assertEqual((Path(directory)/'api-token').stat().st_mode & 0o777,0o600)
            finally:stack.lock.close()

    def test_ownership_mismatch_cannot_stop_resources(self):
        with tempfile.TemporaryDirectory() as directory,patch.dict(os.environ,{'OBS_RUN_DIR':directory}):
            stack=module.Stack()
            try:
                stack.state={'name':'bks-obs-0123456789','containers':{'worker':{'id':'owned-id'}}}
                with patch.object(stack,'docker',return_value=json.dumps([{'Config':{'Labels':{'com.docker.compose.project':'another-task'}}}])):
                    with patch.object(stack,'compose') as compose:
                        with self.assertRaises(RuntimeError):stack.down()
                        compose.assert_not_called()
            finally:stack.lock.close()

    def test_worker_pool_waits_for_both_ip_addresses(self):
        with tempfile.TemporaryDirectory() as directory,patch.dict(os.environ,{'OBS_RUN_DIR':directory}):
            stack=module.Stack()
            try:
                project='bks-obs-0123456789'
                stack.state={'name':project,'containers':{'worker1':{'id':'one'},'worker2':{'id':'two'}}}
                def inspect(_command,container):
                    ip='172.18.0.15' if container=='one' else '172.18.0.7'
                    return json.dumps([{'NetworkSettings':{'Networks':{project+'_default':{'IPAddress':ip}}}}])
                partial=[{'metric':{'worker':'tcp://worker1:9094'}},{'metric':{'worker':'tcp://172.18.0.7:9094'}}]
                complete=[{'metric':{'worker':'tcp://172.18.0.15:9094'}},{'metric':{'worker':'tcp://172.18.0.7:9094'}}]
                def check_convergence(fn):
                    self.assertFalse(fn())
                    self.assertTrue(fn())
                with patch.object(stack,'docker',side_effect=inspect),patch.object(stack,'query',side_effect=[partial,complete]):
                    with patch.object(module,'eventually',side_effect=check_convergence):
                        workers=stack.resolved_workers()
                self.assertEqual(workers,{'172.18.0.15':'worker1','172.18.0.7':'worker2'})
            finally:stack.lock.close()

    def test_worker_outage_is_restored_after_failed_assertion(self):
        with tempfile.TemporaryDirectory() as directory,patch.dict(os.environ,{'OBS_RUN_DIR':directory}):
            stack=module.Stack()
            try:
                project='bks-obs-0123456789'
                stack.state={'name':project,'containers':{'worker1':{'id':'owned-worker'}}}
                info=json.dumps([{'HostConfig':{'NetworkMode':project+'_default'}}])
                with patch.object(stack,'assert_owned'),patch.object(stack,'docker',return_value=info) as docker:
                    with self.assertRaisesRegex(ValueError,'injected test failure'):
                        with stack.worker_network_outage('worker1'):
                            raise ValueError('injected test failure')
                edits=[call.args for call in docker.call_args_list if call.args[0]=='exec']
                self.assertEqual(len(edits),2)
                self.assertEqual(edits[0][:5],('exec','owned-worker','iptables','-w','5'))
                self.assertEqual(edits[0][5],'-I')
                self.assertEqual(edits[1][5],'-D')
                self.assertEqual(edits[0][6:],edits[1][6:])
            finally:stack.lock.close()

    def test_worker_outage_rejects_host_network(self):
        with tempfile.TemporaryDirectory() as directory,patch.dict(os.environ,{'OBS_RUN_DIR':directory}):
            stack=module.Stack()
            try:
                stack.state={'name':'bks-obs-0123456789','containers':{'worker1':{'id':'owned-worker'}}}
                info=json.dumps([{'HostConfig':{'NetworkMode':'host'}}])
                with patch.object(stack,'assert_owned'),patch.object(stack,'docker',return_value=info) as docker:
                    with self.assertRaisesRegex(RuntimeError,'private network namespace'):
                        with stack.worker_network_outage('worker1'):self.fail('Host network accepted')
                self.assertFalse(any(call.args[0]=='exec' for call in docker.call_args_list))
            finally:stack.lock.close()


if __name__=='__main__':unittest.main()
