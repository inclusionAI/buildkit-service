"""Dependency failures must stop before environment mutations."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('dev', Path(__file__).resolve().parents[2] / 'scripts/dev.py')
dev = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dev)


class DependencyTests(unittest.TestCase):
    def runner(self):
        runner = dev.Dev.__new__(dev.Dev)
        runner.env = os.environ.copy()
        runner.command = Mock(return_value='version')
        return runner

    def test_missing_cached_tool_reports_setup_without_system_fallback(self):
        runner = self.runner()
        with tempfile.TemporaryDirectory() as directory, patch.object(dev, 'ROOT', Path(directory)), \
             patch.object(dev.shutil, 'which') as which:
            with self.assertRaisesRegex(RuntimeError, 'kind.*make dev-setup'):
                runner.tool_path('kind')
            which.assert_not_called()

    def test_nonexecutable_cached_tool_reports_setup(self):
        runner = self.runner()
        with tempfile.TemporaryDirectory() as directory, patch.object(dev, 'ROOT', Path(directory)):
            tool = Path(directory) / 'artifacts/e2e-tools/bin/helm'
            tool.parent.mkdir(parents=True)
            tool.write_text('not executable')
            with self.assertRaisesRegex(RuntimeError, 'helm.*make dev-setup'):
                runner.tool_path('helm')

    def test_missing_docker_reports_install(self):
        with patch.object(dev.shutil, 'which', return_value=None):
            with self.assertRaisesRegex(RuntimeError, 'docker.*install'):
                self.runner().tool_path('docker')

    def test_unreachable_daemon_stops_before_buildx(self):
        runner = self.runner()
        def command(*args, **kwargs):
            if args[:2] == ('docker', 'info'):
                raise RuntimeError('connection refused')
        runner.command.side_effect = command
        with self.assertRaisesRegex(RuntimeError, 'Docker daemon is unreachable'):
            runner.check_dependencies('up')
        self.assertFalse(any('buildx' in call.args for call in runner.command.call_args_list))

    def test_missing_buildx_blocks_initialization(self):
        runner = self.runner()
        runner.initialize = Mock()
        def command(*args, **kwargs):
            if args[:2] == ('docker', 'buildx'):
                raise RuntimeError('missing plugin')
        runner.command.side_effect = command
        with self.assertRaisesRegex(RuntimeError, 'buildx is unavailable'):
            runner.up()
        runner.initialize.assert_not_called()

    def test_reset_does_not_depend_on_helm_kubectl_or_buildx(self):
        runner = self.runner()
        runner.check_dependencies('reset')
        self.assertEqual({call.args[0] for call in runner.command.call_args_list}, {'docker', 'kind'})
        self.assertFalse(any('buildx' in call.args for call in runner.command.call_args_list))


class LifecycleTests(unittest.TestCase):
    def runner(self):
        runner = dev.Dev.__new__(dev.Dev)
        runner.state = {'name': 'bks-dev-test', 'node_id': 'owned-id', 'images': {}}
        runner.command = Mock()
        runner.kube = Mock()
        return runner

    def test_foreign_node_blocks_operations(self):
        runner = self.runner()
        runner.command.return_value = 'foreign-id\n'
        with self.assertRaisesRegex(RuntimeError, 'ownership mismatch'):
            runner.owned_node()

    def test_multiple_nodes_are_not_adopted(self):
        runner = self.runner()
        runner.command.return_value = 'owned-id\nsecond-id\n'
        with self.assertRaisesRegex(RuntimeError, 'ownership mismatch'):
            runner.owned_node()

    def test_reset_requires_exact_environment_confirmation(self):
        runner = self.runner()
        with patch.dict(os.environ, {'DEV_RESET_CONFIRM': 'another-environment'}):
            with self.assertRaisesRegex(RuntimeError, 'DEV_RESET_CONFIRM=bks-dev-test'):
                runner.reset()
        runner.command.assert_not_called()

    def test_pending_initial_install_replays_helm_revision_without_uninstall(self):
        import json
        runner = self.runner()
        runner.helm = Mock(side_effect=[json.dumps([{'name':'buildkit-service','status':'pending-install'}]),
                                       json.dumps([{'revision':1,'status':'pending-install'}]), ''])
        with tempfile.TemporaryDirectory() as directory, patch.object(dev, 'DIRECTORY', Path(directory)):
            runner.recover_helm()
        runner.helm.assert_called_with('rollback', 'buildkit-service', '1', '--wait=false')

    def test_pending_upgrade_uses_last_deployed_revision(self):
        import json
        runner = self.runner()
        runner.helm = Mock(side_effect=[json.dumps([{'name':'buildkit-service','status':'pending-upgrade'}]),
                                       json.dumps([{'revision':2,'status':'deployed'}, {'revision':3,'status':'pending-upgrade'}]), ''])
        with tempfile.TemporaryDirectory() as directory, patch.object(dev, 'DIRECTORY', Path(directory)):
            runner.recover_helm()
        runner.helm.assert_called_with('rollback', 'buildkit-service', '2', '--wait=false')

    def test_healthy_release_is_not_rolled_back(self):
        runner = self.runner()
        runner.helm = Mock(return_value='[{"name":"buildkit-service","status":"deployed"}]')
        runner.recover_helm()
        self.assertEqual(runner.helm.call_count, 1)

    def test_failed_build_keeps_previous_complete_image_generation(self):
        runner = self.runner()
        runner.state['images'] = {'service': 'previous'}
        runner.image = Mock(return_value='base@sha256:test')
        runner.command.side_effect = [None, RuntimeError('apt build failed')]
        with self.assertRaisesRegex(RuntimeError, 'apt build failed'):
            runner.build_images()
        self.assertEqual(runner.state['images'], {'service': 'previous'})

    def test_stop_does_not_scale_other_deployments(self):
        import json
        runner = self.runner()
        runner.owned_node = Mock(return_value='owned-id')
        owned = {'metadata': {'name':'buildkit-service','annotations':{'meta.helm.sh/release-name':'buildkit-service'}},
                 'spec': {'selector': {'matchLabels': {'app':'buildkit'}}}}
        foreign = {'metadata':{'name':'user-app'}, 'spec':{'selector':{'matchLabels':{'app':'user'}}}}
        runner.kube.return_value = json.dumps({'items':[owned,foreign]})
        runner.down()
        calls = [c.args for c in runner.kube.call_args_list]
        self.assertIn(('scale','deployment/buildkit-service','--replicas=0'), calls)
        self.assertFalse(any('--all' in c or 'deployment/user-app' in c for c in calls))

    def test_missing_node_does_not_use_stale_kubeconfig(self):
        runner = self.runner()
        runner.owned_node = Mock(return_value=None)
        with self.assertRaisesRegex(RuntimeError, 'No owned kind node'):
            runner.connect_cluster()
        runner.command.assert_not_called()

    def test_stopped_node_is_started_in_place(self):
        runner = self.runner()
        runner.owned_node = Mock(return_value='owned-id')
        runner.command.return_value = 'false'
        runner.connect_cluster = Mock()
        runner.cluster()
        runner.command.assert_any_call('docker', 'start', 'owned-id')
        runner.connect_cluster.assert_called_once()
        self.assertFalse(any('create' in c.args for c in runner.command.call_args_list))

    def test_pod_listing_failure_keeps_diagnostic_error_file(self):
        runner = self.runner()
        runner.kube.side_effect = RuntimeError('API unavailable')
        with tempfile.TemporaryDirectory() as directory, patch.object(dev, 'DIRECTORY', Path(directory)):
            with self.assertRaisesRegex(RuntimeError, 'Some diagnostics failed'):
                runner.logs()
            reports = list(Path(directory).glob('logs-*/errors.log'))
            self.assertEqual(len(reports), 1)
            self.assertIn('List pods: API unavailable', reports[0].read_text())

    def test_invalid_state_releases_lock(self):
        import json
        with tempfile.TemporaryDirectory() as directory, patch.object(dev, 'DIRECTORY', Path(directory)):
            path = Path(directory) / 'state.json'
            path.write_text('{')
            with self.assertRaises(json.JSONDecodeError):
                dev.Dev()
            path.write_text('{}')
            runner = dev.Dev()
            runner.lock.close()

    def test_live_lock_cannot_be_taken_by_another_command(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(dev, 'DIRECTORY', Path(directory)):
            first = dev.Dev()
            try:
                with self.assertRaisesRegex(RuntimeError, 'Another dev command'):
                    dev.Dev()
            finally:
                first.lock.close()


if __name__ == '__main__':
    unittest.main()
