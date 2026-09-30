"""Hook diagnostics must identify failures without exposing source data."""
import ast
import contextlib
import errno
import http.client
import io
import json
import os
from pathlib import Path
import re
import signal
import socket
import ssl
import tempfile
import unittest
import urllib.error
import uuid
from unittest.mock import patch

import files as f

ID = 'a' * 26
SECRET = 'private-token-and-file-contents'


class DiagnosticsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.request = dict(schema=1, source_id='chat', channel_id=ID, root_id=ID,
                            anchor_post_id=ID, files=[], limits=dict(max_file_bytes=100,
                            max_image_bytes=100, max_batch_bytes=100, max_per_post=10,
                            max_output_files=10, max_output_bytes=100))
        self.run = str(uuid.uuid4())
        self.env = dict(ORPHEUS_WORKSPACE_PATH=self.temp.name, ORPHEUS_RUN_ID=self.run,
                        ORPHEUS_SESSION_ID=str(uuid.uuid4()), MM_INPUT_MANIFEST=json.dumps(self.request),
                        MM_BASE_URL='https://example.invalid/' + SECRET, MM_TOKEN_ENV='MM_TEST_TOKEN',
                        MM_TEST_TOKEN=SECRET, MM_EXPECTED_BOT_ID=ID, MM_SOURCE_ID='chat', MM_CHANNEL_ID=ID)

    def failure(self, command):
        with self.assertRaises(f.HookFailure) as caught:
            f.main(command)
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            f.report_failure(caught.exception)
        text = output.getvalue()
        self.assertNotIn(SECRET, text)
        self.assertNotIn(self.temp.name, text)
        self.assertNotIn('Traceback', text)
        self.assertEqual(text.count('\n'), 1)
        return text

    def test_safe_exception_details(self):
        errors = [
            (urllib.error.HTTPError('https://example.invalid/' + SECRET, 401, SECRET,
                                    {'Authorization': SECRET}, io.BytesIO(SECRET.encode())), 'HTTP 401'),
            (urllib.error.URLError(SECRET), 'network request failed'),
            (urllib.error.URLError(socket.gaierror(-2, SECRET)), 'gaierror: errno=-2; DNS lookup failed'),
            (urllib.error.URLError(ConnectionRefusedError(errno.ECONNREFUSED, SECRET)), 'ECONNREFUSED'),
            (urllib.error.URLError(TimeoutError(SECRET)), 'operation timed out'),
            (socket.timeout(SECRET), 'operation timed out'),
            (http.client.RemoteDisconnected(SECRET), 'RemoteDisconnected: I/O operation failed'),
            (http.client.BadStatusLine(SECRET), 'BadStatusLine: unexpected failure'),
            (ssl.SSLCertVerificationError(1, SECRET), 'TLS certificate verification failed'),
            (ssl.SSLError(1, SECRET), 'TLS connection failed'),
            (PermissionError(errno.EACCES, SECRET, SECRET), 'errno=13 (EACCES)'),
            (OSError(SECRET), 'OSError: I/O operation failed'),
            (json.JSONDecodeError(SECRET, SECRET, 1), 'invalid JSON'),
            (KeyError(SECRET), 'required field or environment variable is missing'),
            (ValueError(SECRET), 'ValueError'),
            (TypeError(SECRET), 'TypeError'),
            (AttributeError(SECRET), 'AttributeError'),
            (IndexError(SECRET), 'IndexError'),
            (RuntimeError(SECRET), 'RuntimeError'),
            (Exception(SECRET), 'unexpected failure'),
        ]
        for error, expected in errors:
            with self.subTest(kind=type(error).__name__, expected=expected):
                text = f.safe_error(error)
                self.assertIn(expected, text)
                self.assertNotIn(SECRET, text)

    def test_python_39_socket_timeout_is_classified(self):
        # Before Python 3.10 socket.timeout was not an alias of TimeoutError.
        class LegacySocketTimeout(OSError):
            pass
        with patch.object(f.socket, 'timeout', LegacySocketTimeout):
            text = f.safe_error(LegacySocketTimeout(SECRET))
        self.assertEqual(text, 'LegacySocketTimeout: operation timed out')
        self.assertNotIn(SECRET, text)

    def test_validation_messages_and_stage_names_are_static(self):
        # ProtocolError text is safe only while require() messages stay fixed.
        tree = ast.parse(Path(f.__file__).read_text())
        detail = re.compile(r'[A-Za-z0-9][A-Za-z0-9 ._:=;()/-]*\Z')
        stage = re.compile(r'[a-z][a-z0-9_]{0,63}\Z')
        for node in ast.walk(tree):
            if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
                if node.func.id == 'require':
                    self.assertIsInstance(node.args[1], ast.Constant)
                    self.assertIsInstance(node.args[1].value, str)
                    self.assertRegex(node.args[1].value, detail)
                    self.assertLessEqual(len('validation failed: ' + node.args[1].value), 384)
                if node.func.id == 'step':
                    self.assertIsInstance(node.args[0], ast.Constant)
                    self.assertIsInstance(node.args[0].value, str)
                    self.assertRegex(node.args[0].value, stage)

    def test_fileless_input_still_verifies_bot_and_initializes_run(self):
        with patch.dict(os.environ, self.env, clear=True), \
                patch.object(f.Mattermost, 'request', return_value=f.encode(dict(id=ID))) as request:
            f.main('prepare-input')
        request.assert_called_once_with('users/me')
        current = json.loads((Path(self.temp.name) / f.BASE / 'current-run.json').read_text())
        self.assertEqual(current['run_id'], self.run)

    def test_configuration_and_identity_stages(self):
        cases = [
            ('workspace_access', 'ORPHEUS_WORKSPACE_PATH', None, 'KeyError'),
            ('input_request', 'MM_INPUT_MANIFEST', SECRET, 'invalid JSON'),
            ('input_request', 'MM_INPUT_MANIFEST', json.dumps(dict(self.request, channel_id=SECRET)),
             'invalid input identity'),
            ('mattermost_configuration', 'MM_TEST_TOKEN', None, 'KeyError'),
            ('bot_verification', 'MM_EXPECTED_BOT_ID', SECRET, 'mattermost bot identity mismatch'),
            ('input_identity', 'MM_SOURCE_ID', SECRET, 'input request identity mismatch'),
            ('run_initialization', 'ORPHEUS_RUN_ID', SECRET, 'invalid run ID'),
        ]
        for stage, name, value, expected in cases:
            with self.subTest(stage=stage, name=name):
                env = dict(self.env)
                if value is None:
                    del env[name]
                else:
                    env[name] = value
                with patch.dict(os.environ, env, clear=True), \
                        patch.object(f.Mattermost, 'request', return_value=f.encode(dict(id=ID))):
                    text = self.failure('prepare-input')
                self.assertIn('stage=' + stage, text)
                self.assertIn(expected, text)

    def test_network_and_workspace_stages(self):
        cases = [
            (f.Mattermost, 'request', urllib.error.HTTPError(SECRET, 503, SECRET, {}, None),
             'prepare-input', 'bot_verification', 'HTTP 503'),
            (f.Store, 'prepare', PermissionError(errno.EACCES, SECRET, SECRET),
             'prepare-input', 'input_preparation', 'EACCES'),
            (f.Store, 'begin_run', f.ProtocolError('invalid input index chain'),
             'prepare-input', 'run_initialization', 'invalid input index chain'),
            (f.Store, 'export_output', urllib.error.HTTPError(SECRET, 403, SECRET, {}, None),
             'export-output', 'output_export', 'HTTP 403'),
        ]
        for target, method, error, command, stage, expected in cases:
            with self.subTest(stage=stage):
                with patch.dict(os.environ, self.env, clear=True), \
                        patch.object(f.Mattermost, 'request', return_value=f.encode(dict(id=ID))), \
                        patch.object(target, method, side_effect=error):
                    text = self.failure(command)
                self.assertIn('stage=' + stage, text)
                self.assertIn(expected, text)

    def test_import_diagnostics_and_timeout_cleanup(self):
        with patch.dict(os.environ, self.env, clear=True), \
                patch.object(f.Store, 'import_input', side_effect=TimeoutError(SECRET)):
            text = self.failure('import-input')
        self.assertIn('stage=input_import', text)
        self.assertIn('operation timed out', text)
        self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))
        with patch.dict(os.environ, dict(self.env, MM_IMPORT_TIMEOUT=SECRET), clear=True):
            text = self.failure('import-input')
        self.assertIn('stage=import_configuration', text)
        self.assertIn('ValueError', text)

    def test_nested_stage_keeps_inner_failure(self):
        with self.assertRaises(f.HookFailure) as caught:
            with f.step('outer'):
                with f.step('inner'):
                    raise ValueError(SECRET)
        self.assertEqual(caught.exception.stage, 'inner')

    def test_unscoped_failure_uses_unknown_stage(self):
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            f.report_failure(RuntimeError(SECRET))
        self.assertIn('stage=unknown; RuntimeError:', output.getvalue())
        self.assertNotIn(SECRET, output.getvalue())


if __name__ == '__main__':
    unittest.main()
