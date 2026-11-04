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


if __name__=='__main__':unittest.main()
