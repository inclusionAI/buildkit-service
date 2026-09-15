"""Verify setup's integrity and repeat-run contract without downloading tools."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('setup_e2e', Path(__file__).resolve().parents[2] / 'scripts/setup-e2e.py')
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class SetupTests(unittest.TestCase):
    def configure(self, directory):
        root = Path(directory)
        (root / 'tests/e2e').mkdir(parents=True)
        lock = {'tools': {'kind': {'version': 'test', 'url': 'https://example.invalid/kind',
                                  'sha256': {'linux-amd64': hashlib.sha256(b'fixture-tool').hexdigest()}}}}
        (root / 'tests/e2e/dependencies.json').write_text(json.dumps(lock))
        return root

    def test_repeat_uses_verified_cache_and_repairs_modified_binary(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self.configure(directory)
            with patch.object(setup, 'ROOT', root), patch.object(setup.platform, 'system', return_value='Linux'), patch.object(setup.platform, 'machine', return_value='x86_64'):
                with patch.object(setup.urllib.request, 'urlopen', return_value=io.BytesIO(b'fixture-tool')) as download:
                    setup.main()
                    download.assert_called_once()
                binary = root / 'artifacts/e2e-tools/bin/kind'
                binary.write_bytes(b'modified')
                with patch.object(setup.urllib.request, 'urlopen', side_effect=AssertionError('unexpected network')):
                    setup.main()
                self.assertEqual(binary.read_bytes(), b'fixture-tool')
                binary.chmod(0o600)
                with patch.object(setup.urllib.request, 'urlopen', side_effect=AssertionError('unexpected network')):
                    setup.main()
                self.assertEqual(binary.stat().st_mode & 0o777, 0o755)

    def test_install_replaces_symlink_without_changing_external_binary(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            external = root / 'system-tool'
            external.write_bytes(b'fixture-tool')
            external.chmod(0o600)
            destination = root / 'cached-tool'
            destination.symlink_to(external)
            setup.install_binary('kind', b'fixture-tool', destination, 'linux', 'amd64')
            self.assertFalse(destination.is_symlink())
            self.assertEqual(destination.read_bytes(), b'fixture-tool')
            self.assertEqual(external.stat().st_mode & 0o777, 0o600)

    def test_invalid_download_is_not_installed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self.configure(directory)
            with patch.object(setup, 'ROOT', root), patch.object(setup.platform, 'system', return_value='Linux'), patch.object(setup.platform, 'machine', return_value='x86_64'), patch.object(setup.urllib.request, 'urlopen', return_value=io.BytesIO(b'corrupt')):
                with self.assertRaisesRegex(RuntimeError, 'Checksum mismatch'):
                    setup.main()
            self.assertFalse((root / 'artifacts/e2e-tools/bin/kind').exists())


if __name__ == '__main__':
    unittest.main()
