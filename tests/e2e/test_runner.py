"""Failure-path checks for ownership and cleanup, independent of Docker."""
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

import run


class CleanupTests(unittest.TestCase):
    def create_run(self, directory):
        test = run.Run(Path(directory))
        self.addCleanup(test.run_lock.close)
        test.state = {'name': 'bks-e2e-example', 'endpoint': 'unix:///test.sock',
                      'registry': 'bks-e2e-example-registry', 'images': {}}
        return test

    def test_foreign_registry_is_never_removed(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            calls = []
            def docker(*args, **kw):
                calls.append(args)
                return json.dumps([{'Config': {'Labels': {run.LABEL: 'another-run'}}}])
            with patch.object(test, 'docker', side_effect=docker):
                with self.assertRaisesRegex(RuntimeError, 'ownership mismatch'):
                    test.down()
            self.assertFalse(any(call[0] == 'rm' for call in calls))

    def test_missing_resources_allow_repeated_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            with patch.object(test, 'docker', return_value=''):
                test.down()
                test.down()
            self.assertEqual(test.state['phase'], 'cleaned')

    def test_retagged_image_is_preserved_and_other_owned_tags_are_cleaned(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state['registry'] = None
            test.state['images'] = {'changed': {'tag': 'changed:test', 'id': 'sha256:original'},
                                    'owned': {'tag': 'owned:test', 'id': 'sha256:owned'}}
            def docker(*args, **kwargs):
                if args[:2] == ('image', 'ls'):
                    return 'present'
                if args[:2] == ('image', 'inspect'):
                    image_id = 'sha256:replacement' if args[2] == 'changed:test' else 'sha256:owned'
                    return json.dumps([{'Id': image_id, 'Config': {'Labels': {}}}])
                return ''
            with patch.object(test, 'docker', side_effect=docker) as command:
                with self.assertRaisesRegex(RuntimeError, 'Image ownership mismatch'):
                    test.down()
            removed = [call.args[2] for call in command.call_args_list if call.args[:2] == ('image', 'rm')]
            self.assertEqual(removed, ['owned:test'])
            self.assertEqual(test.state['status'], 'cleanup-failed')

    def test_cluster_failure_does_not_skip_registry_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            with patch.object(test, 'remove_cluster', side_effect=RuntimeError('cluster unavailable')), \
                 patch.object(test, 'remove_registry') as registry, patch.object(test, 'remove_image_tags') as images:
                with self.assertRaisesRegex(RuntimeError, 'cluster unavailable'):
                    test.down()
            registry.assert_called_once()
            images.assert_called_once()
            self.assertNotEqual(test.state.get('phase'), 'cleaned')

    def test_failed_up_is_recorded_and_cannot_be_repeated(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state['phase'] = 'built'
            def failed_up():
                test.step('up.kind', lambda: (_ for _ in ()).throw(RuntimeError('node failed')))
            with patch.object(test, 'up', side_effect=failed_up) as up:
                with self.assertRaisesRegex(RuntimeError, 'node failed'):
                    test.perform('up')
                with self.assertRaisesRegex(RuntimeError, 'Previous action'):
                    test.perform('up')
            up.assert_called_once()
            state = json.loads(test.path.read_text())
            self.assertEqual(state['phase'], 'built')
            self.assertEqual(state['failure']['stage'], 'up.kind')

    def test_failed_build_can_be_retried(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state.update(phase='preflight', action='build', status='failed')
            with patch.object(test, 'build'):
                test.perform('build')
            self.assertEqual(test.state['phase'], 'built')
            self.assertEqual(test.state['status'], 'complete')

    def test_full_run_preserves_first_error_and_always_attempts_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state = {}
            def preflight():
                test.state.update(phase='preflight', name='bks-e2e-example')
            with patch.object(test, 'preflight', side_effect=preflight), \
                 patch.object(test, 'build', side_effect=RuntimeError('build failed')), \
                 patch.object(test, 'logs', side_effect=RuntimeError('logs failed')) as logs, \
                 patch.object(test, 'down', side_effect=RuntimeError('cleanup failed')) as down:
                with self.assertRaisesRegex(RuntimeError, '^build failed$'):
                    test.execute('all')
            logs.assert_called_once()
            down.assert_called_once()

    def test_failure_recording_error_does_not_hide_build_error(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state['phase'] = 'preflight'
            original_save = test.save
            def save():
                if test.state.get('failure'):
                    raise OSError('disk full')
                original_save()
            with patch.object(test, 'save', side_effect=save), \
                 patch.object(test, 'build', side_effect=RuntimeError('original build error')):
                with self.assertRaisesRegex(RuntimeError, '^original build error$'):
                    test.perform('build')
            self.assertEqual(test.state['failure']['error'], 'original build error')

    def test_rejected_full_run_does_not_clean_existing_run(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            with patch.object(test, 'down') as down:
                with self.assertRaisesRegex(RuntimeError, 'new run directory'):
                    test.execute('all')
            down.assert_not_called()

    def test_command_output_excludes_diagnostics(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            result = test.command([run.sys.executable, '-c',
                                   'import sys; print("{} "); print("diagnostic", file=sys.stderr)'], name='structured')
            self.assertEqual(json.loads(result), {})
            self.assertIn('diagnostic', (test.directory / 'structured.stderr.log').read_text())

    def test_keep_exports_logs_without_cleanup_on_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state = {}
            def preflight():
                test.state.update(phase='preflight', name='bks-e2e-example')
            with patch.object(test, 'preflight', side_effect=preflight), \
                 patch.object(test, 'build', side_effect=RuntimeError('build failed')), \
                 patch.object(test, 'logs') as logs, patch.object(test, 'down') as down:
                with self.assertRaisesRegex(RuntimeError, 'build failed'):
                    test.execute('all', keep=True)
            logs.assert_called_once()
            down.assert_not_called()

    def test_command_timeout_names_stage_log_and_stops_descendants(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            marker = test.directory / 'orphan-created-resource'
            started = test.directory / 'child-started'
            child = f'import time; from pathlib import Path; time.sleep(1); Path({str(marker)!r}).touch()'
            parent = (f'import subprocess, sys, time; from pathlib import Path; '
                      f'subprocess.Popen([sys.executable, "-c", {child!r}]); '
                      f'Path({str(started)!r}).touch(); time.sleep(10)')
            with self.assertRaisesRegex(RuntimeError, 'kind-create.log'):
                test.command([run.sys.executable, '-c', parent], timeout=0.5, name='kind-create')
            self.assertTrue(started.exists())
            time.sleep(1)
            self.assertFalse(marker.exists(), 'A surviving child kept mutating resources after timeout')

    def test_cleanup_failure_blocks_build_retry(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state.update(phase='built', action='build', status='cleanup-failed')
            with patch.object(test, 'build') as build:
                with self.assertRaisesRegex(RuntimeError, 'Previous action'):
                    test.perform('build')
            build.assert_not_called()
            self.assertNotIn('Retry build:', test.failure_message(RuntimeError('cleanup failed')))

    def test_import_alias_is_recorded_before_interrupted_tag(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state['architecture'] = 'amd64'
            def docker(*args, **kwargs):
                if args[:2] == ('image', 'inspect'):
                    return json.dumps([{'Id': 'sha256:dependency'}])
                if args[0] == 'tag':
                    raise KeyboardInterrupt()
                return ''
            with patch.object(test, 'docker', side_effect=docker):
                with self.assertRaises(KeyboardInterrupt):
                    test.load_images()
            state = json.loads(test.path.read_text())
            self.assertEqual(state['images']['pip'],
                             {'tag': 'bks-e2e-example-pip:test', 'id': 'sha256:dependency'})

    def test_failed_log_export_is_reported_and_other_diagnostics_continue(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state.update(cluster_attempted=True, phase='tested', result='PASS')
            (test.directory / 'kubeconfig').touch()
            with patch.object(test, 'command', side_effect=RuntimeError('kind export failed')), \
                 patch.object(test, 'docker', return_value=''), patch.object(test, 'kubectl', return_value='') as kubectl, \
                 patch.object(test, 'export_pod_logs') as pods:
                with self.assertRaisesRegex(RuntimeError, 'Some diagnostics'):
                    test.perform('logs')
            pods.assert_called_once()
            self.assertEqual(kubectl.call_count, 3)
            self.assertEqual(test.state['diagnostic_errors'], ['kind export failed'])
            self.assertEqual(test.state['result'], 'FAIL')

    def test_missing_cached_tool_never_falls_back_to_system_path(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            with patch.object(run, 'ROOT', test.directory), patch.object(run.subprocess, 'Popen') as process:
                with self.assertRaisesRegex(RuntimeError, 'make e2e-setup'):
                    test.command(['kind', 'version'])
            process.assert_not_called()

    def test_corrupt_state_releases_directory_lock(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / 'state.json'
            state.write_text('{broken')
            with self.assertRaisesRegex(RuntimeError, 'Cannot load run state'):
                run.Run(Path(directory))
            state.unlink()
            recovered = self.create_run(directory)
            self.assertIsNotNone(recovered.run_lock)

    def test_resumed_run_rejects_changed_dependencies_but_allows_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state.update(phase='built', status='complete')
            test.lock = dict(test.lock, changed=True)
            with patch.object(test, 'up') as up:
                with self.assertRaisesRegex(RuntimeError, 'Dependencies changed'):
                    test.perform('up')
            up.assert_not_called()
            with patch.object(test, 'down') as down:
                test.perform('down')
            down.assert_called_once()

    def test_kind_failure_restores_scoped_platform_and_proxy_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.state.update(node_architecture='arm64', architecture='amd64')
            test.env.update(DOCKER_DEFAULT_PLATFORM='linux/amd64', HTTP_PROXY='http://127.0.0.1:7890')
            original = test.env.copy()
            def create(*args, **kwargs):
                self.assertEqual(test.env['DOCKER_DEFAULT_PLATFORM'], 'linux/arm64')
                self.assertNotIn('HTTP_PROXY', test.env)
                raise RuntimeError('kind failed')
            with patch.object(test, 'docker', return_value=''), patch.object(test, 'command', side_effect=create):
                with self.assertRaisesRegex(RuntimeError, 'kind failed'):
                    test.create_kind_cluster()
            self.assertEqual(test.env, original)
            self.assertTrue(test.state['cluster_attempted'])

    def test_isolated_environment_does_not_inherit_context_or_tls(self):
        with tempfile.TemporaryDirectory() as directory:
            test = self.create_run(directory)
            test.env.update(DOCKER_CONTEXT='production', DOCKER_TLS_VERIFY='1', DOCKER_CERT_PATH='/private')
            test.configure_environment()
            self.assertNotIn('DOCKER_CONTEXT', test.env)
            self.assertNotIn('DOCKER_CERT_PATH', test.env)
            self.assertNotIn('DOCKER_TLS_VERIFY', test.env)
            self.assertEqual(test.env['DOCKER_DEFAULT_PLATFORM'], 'linux/amd64')
            self.assertEqual(test.env['DOCKER_CONFIG'], str(Path(directory).resolve() / 'docker'))

    def test_make_preserves_literal_run_directory_without_shell_interpolation(self):
        with tempfile.TemporaryDirectory() as directory:
            launcher = Path(directory) / 'fake-uv'
            launcher.write_text('#!' + run.sys.executable + '\nimport os, json\nprint(json.dumps(os.environ["E2E_RUN_DIR"]))\n')
            launcher.chmod(0o755)
            value = str(Path(directory) / 'spaces $HOME `printf injected` "quotes"')
            output = run.subprocess.check_output(['make', '--no-print-directory', '-s', '-C', str(run.ROOT),
                                                 'e2e-up', 'UV=' + str(launcher), 'E2E_RUN_DIR=' + value], text=True)
            self.assertEqual(json.loads(output), value)

    def test_failure_commands_quote_paths_and_target_only_this_run(self):
        with tempfile.TemporaryDirectory(prefix="e2e space's ") as directory:
            test = self.create_run(directory)
            test.state['phase'] = 'up'
            (test.directory / 'kubeconfig').touch()
            message = test.failure_message(RuntimeError('fixture failed'))
            commands = dict(line.split(': ', 1) for line in message.splitlines() if ': ' in line)
            self.assertEqual(run.shlex.split(commands['State']), ['cat', str(test.path)])
            pods = run.shlex.split(commands['Pods (if API is available)'])
            self.assertEqual(pods[0], str(run.ROOT / 'artifacts/e2e-tools/bin/kubectl'))
            self.assertEqual(pods[1:3], ['--kubeconfig', str(test.directory / 'kubeconfig')])
            self.assertIn('--request-timeout=10s', pods)
            self.assertEqual(run.shlex.split(commands['Cleanup']),
                             ['make', '-C', str(run.ROOT), 'e2e-down', f'E2E_RUN_DIR={test.directory}'])
            test.state['phase'] = 'cleaned'
            self.assertNotIn('Pods (if API is available)', test.failure_message(RuntimeError('old failure')))


class BuildContextTests(unittest.TestCase):
    def test_context_round_trip_preserves_literal_content(self):
        from scenarios import Scenarios
        from unittest.mock import Mock
        import subprocess
        with tempfile.TemporaryDirectory() as directory:
            def kubectl(*args, **kwargs):
                command = list(args[args.index('--') + 1:])
                command = [arg.replace('/tmp/source/test', directory) for arg in command]
                return subprocess.check_output(command, text=True)
            runner = Mock()
            runner.kubectl.side_effect = kubectl
            target = 'registry/result:$HOME`echo injected`"quote'
            Scenarios(runner).write_build_context(target)
            self.assertEqual(json.loads((Path(directory) / 'metadata.json').read_text()), {'target': target})
            self.assertEqual((Path(directory) / 'Dockerfile').read_text(),
                             (run.HERE / 'fixtures/Dockerfile.build').read_text())

    def test_empty_transferred_dockerfile_fails_before_build(self):
        from scenarios import Scenarios
        from unittest.mock import Mock
        runner = Mock()
        runner.kubectl.return_value = ''
        with self.assertRaisesRegex(RuntimeError, 'Build context transfer mismatch: .*Dockerfile'):
            Scenarios(runner).build_fixture(1)
        self.assertFalse(any('buildctl-batch' in call.args for call in runner.kubectl.call_args_list))


class CacheAssertionTests(unittest.TestCase):
    def test_service_routes_wait_for_connection_without_warming_cache(self):
        import verify
        from unittest.mock import MagicMock
        connection = MagicMock()
        with patch.object(verify.socket, 'create_connection', side_effect=[ConnectionRefusedError(), *([connection] * 6)]) as connect, \
             patch.object(verify.time, 'sleep'), patch.object(verify, 'fetch') as fetch:
            verify.wait_for_mirror_routes()
        self.assertEqual(connect.call_count, 7)
        self.assertEqual(connect.call_args_list[0].args, connect.call_args_list[1].args)
        fetch.assert_not_called()

    def test_service_routes_timeout_names_unreachable_service(self):
        import verify
        with patch.object(verify.time, 'monotonic', side_effect=[0, 0, 1, 61]), \
             patch.object(verify.time, 'sleep'), \
             patch.object(verify.socket, 'create_connection', side_effect=ConnectionRefusedError()):
            with self.assertRaisesRegex(RuntimeError, 'apk.e2e.svc.cluster.local:80.*60s'):
                verify.wait_for_mirror_routes()

    def test_identical_corrupt_wheels_do_not_count_as_success(self):
        import verify
        index = b'<a href="/files/e2e_pkg-1.0-py3-none-any.whl">wheel</a>'
        with patch.object(verify, 'stats', return_value={}), patch.object(verify, 'delta', return_value=1), \
             patch.object(verify, 'fetch', side_effect=[index, b'corrupt', b'corrupt']):
            with self.assertRaises(verify.zipfile.BadZipFile):
                verify.check_wheel_cache()


if __name__ == '__main__':
    unittest.main()
