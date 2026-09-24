"""Offline file protocol regression tests; no external services or credentials."""
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from email.parser import BytesParser
from email.policy import default

import files as f

ID, FILE = 'a' * 26, 'b' * 26


def request():
    return dict(schema=1, bot_id=ID, source_id='chat', channel_id=ID, root_id=ID,
                anchor_post_id=ID, files=[dict(post_id=ID, file_id=FILE, channel_id=ID,
                origin='thread', name='image.png', path=f'{f.BASE}/input/files/{FILE}/image.png',
                size_bytes=3, mime='image/png')], limits=dict(max_per_post=5, max_file_bytes=100,
                max_image_bytes=100, max_batch_bytes=100, max_output_files=5, max_output_bytes=100))


class Source:
    def __init__(self):
        self.data = b'png'
        self.downloads = 0
        self.uploads = []
        self.denied = False
        self.fail_at = 0
        self.channel = ID
        self.file_post = ID

    def post(self, _id):
        if self.denied:
            raise OSError('denied')
        return dict(id=ID, channel_id=self.channel, file_ids=[FILE])

    def file(self, _id):
        return dict(id=FILE, post_id=self.file_post, mime_type='image/png', size=len(self.data))

    def download(self, _id, _limit):
        self.downloads += 1
        return self.data

    def upload(self, _channel, name, data):
        self.uploads.append((name, data))
        if len(self.uploads) == self.fail_at:
            raise OSError('lost response')
        return dict(id=FILE, size=len(data))


class FilesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.store = f.Store(self.temp.name)
        self.addCleanup(self.store.close)
        self.source = Source()
        self.req = request()
        self.run = str(uuid.uuid4())

    def export(self):
        return dict(source_id='chat', bot_id=ID, channel_id=ID, session_id=str(uuid.uuid4()),
                    run_id=self.run, limits=self.req['limits'])

    def outbox(self, name='report.txt', data=b'abc'):
        path = f'{f.BASE}/output/{self.run}/outbox/{name}'
        self.store.write(path, data)
        return path

    def payload(self, data=b'png', size=3):
        file = dict(self.req['files'][0], status='ready', sha256=f.digest(b'png'), size_bytes=size)
        return f.encode(self.req) + b'\n' + f.encode(file) + b'\n' + data

    def test_prepare_cache_restore_and_conflict(self):
        for _ in range(2):
            self.assertEqual(self.store.prepare(self.source, self.req)['files'][0]['status'], 'ready')
        self.assertEqual(self.source.downloads, 1)
        self.store.write(self.req['files'][0]['path'], b'bad')
        self.store.prepare(self.source, self.req)
        self.assertEqual(self.source.downloads, 2)
        self.req['files'][0]['origin'] = 'link'
        with self.assertRaisesRegex(ValueError, 'conflict'):
            self.store.prepare(self.source, self.req)

    def test_symlinks_traversal_and_special_files(self):
        os.symlink(self.temp.name, Path(self.temp.name) / '.orpheus')
        with self.assertRaises(OSError):
            self.store.prepare(self.source, self.req)
        for path in ('../escape', '/absolute', 'a//b', 'a/./b'):
            with self.assertRaises(ValueError):
                self.store.write(path, b'no')
        os.mkfifo(Path(self.temp.name) / 'pipe')
        with self.assertRaises(ValueError):
            self.store.read('pipe', 10)
        self.assertEqual(f.safe_name('../../тест.png'), 'тест.png')
        self.assertLessEqual(len(f.safe_name('т' * 200).encode()), 180)

    def test_limits_and_access_on_cached_file(self):
        self.store.prepare(self.source, self.req)
        self.req['anchor_post_id'] = 'c' * 26
        self.source.denied = True
        self.assertEqual(self.store.prepare(self.source, self.req)['files'][0]['status'], 'unavailable')
        self.assertEqual(self.source.downloads, 1)
        self.source.denied = False
        self.source.data = b'larger'
        self.req['limits']['max_batch_bytes'] = 4
        self.req['anchor_post_id'] = 'd' * 26
        self.assertEqual(self.store.prepare(self.source, self.req)['files'][0]['status'], 'batch_limit_exceeded')
        self.req['limits']['max_image_bytes'] = 1
        self.req['anchor_post_id'] = 'e' * 26
        self.assertEqual(self.store.prepare(self.source, self.req)['files'][0]['status'], 'file_limit_exceeded')

    def test_link_channel_and_post_association(self):
        self.source.channel = 'c' * 26
        self.req['files'][0]['channel_id'] = self.source.channel
        self.assertEqual(self.store.prepare(self.source, self.req)['files'][0]['status'], 'unavailable')
        self.req['anchor_post_id'] = 'd' * 26
        self.req['allowed_channel_pairs'] = [dict(source=self.source.channel, destination=ID)]
        self.assertEqual(self.store.prepare(self.source, self.req)['files'][0]['status'], 'ready')
        self.source.file_post = 'x' * 26
        self.req['anchor_post_id'] = 'e' * 26
        self.assertEqual(self.store.prepare(self.source, self.req)['files'][0]['status'], 'unavailable')

    def test_continuation_and_import_isolation(self):
        self.store.prepare(self.source, self.req)
        self.store.begin_run(self.run, self.req)
        current = self.store.read(f.BASE + '/current-run.json', 4096)
        self.req['anchor_post_id'] = 'c' * 26
        self.store.import_input(io.BytesIO(self.payload()))
        self.assertEqual(current, self.store.read(f.BASE + '/current-run.json', 4096))
        self.req['previous_index'] = f.index_path(self.run)
        self.req['delivered_batches'] = [f.manifest_path(ID)]
        self.store.prepare(self.source, self.req)
        next_run = str(uuid.uuid4())
        self.store.begin_run(next_run, self.req)
        self.assertEqual(self.store.load(f.index_path(next_run))['previous'], f.index_path(self.run))
        self.assertEqual(self.source.downloads, 1)

    def test_missing_corrupt_and_cyclic_indices(self):
        self.store.prepare(self.source, self.req)
        self.store.begin_run(self.run, self.req)
        self.req['previous_index'] = f.index_path(self.run)
        current = self.store.read(f.BASE + '/current-run.json', 4096)
        path = Path(self.temp.name) / f.index_path(self.run)
        path.unlink()
        for data in (None, b'not JSON', f.encode(dict(schema=1, batches=[], previous=f.index_path(self.run)))):
            if data:
                self.store.write(f.index_path(self.run), data)
            with self.assertRaises((ValueError, OSError)):
                self.store.begin_run(str(uuid.uuid4()), self.req)
            self.assertEqual(current, self.store.read(f.BASE + '/current-run.json', 4096))

    def test_import_only_commits_complete_validated_stream(self):
        for data, size in ((b'pn', 3), (b'bad', 3), (b'pngextra', 3), (b'', 1 << 30)):
            with self.subTest(data=data, size=size), self.assertRaises(ValueError):
                self.store.import_input(io.BytesIO(self.payload(data, size)))
            self.assertFalse((Path(self.temp.name) / f.manifest_path(ID)).exists())
        with self.assertRaises(ValueError):
            self.store.import_input(io.BytesIO(b' ' * 65536 + b'\n'))
        self.store.import_input(io.BytesIO(self.payload()))
        self.assertEqual(self.store.load(f.manifest_path(ID))['files'][0]['status'], 'ready')

    def test_import_timeout_releases_lock(self):
        env = dict(os.environ, ORPHEUS_WORKSPACE_PATH=self.temp.name, MM_IMPORT_TIMEOUT='0.15')
        process = subprocess.Popen([sys.executable, '-I', f.__file__, 'import-input'],
                                   env=env, stdin=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(lambda: process.kill() if process.poll() is None else None)
        process.stdin.write(f.encode(self.req) + b'\n')
        process.stdin.flush()
        self.assertNotEqual(process.wait(timeout=5), 0)
        process.stdin.close()
        process.stderr.close()
        self.store.import_input(io.BytesIO(self.payload()))

    def test_sealed_output_retry_and_partial_upload(self):
        export = self.export()
        first = self.outbox('a.txt')
        self.outbox('sub/b.txt', b'def')
        self.source.fail_at = 2
        with self.assertRaises(OSError):
            self.store.export_output(self.source, export)
        self.store.write(first, b'changed')
        self.outbox('new.txt')
        output = self.store.export_output(self.source, export)
        self.assertEqual([x['sha256'] for x in output['files']], [f.digest(b'abc'), f.digest(b'def')])
        self.assertEqual(self.source.uploads, [('a.txt', b'abc'), ('b.txt', b'def'), ('b.txt', b'def')])
        self.store.export_output(self.source, export)
        self.assertEqual(len(self.source.uploads), 3)

    def test_damaged_snapshot_and_manifest(self):
        export = self.export()
        self.outbox()
        self.source.fail_at = 1
        with self.assertRaises(OSError):
            self.store.export_output(self.source, export)
        sealed = f'{f.BASE}/output/{self.run}/sealed/snapshot/report.txt'
        self.store.write(sealed, b'bad')
        with self.assertRaisesRegex(ValueError, 'damaged'):
            self.store.export_output(self.source, export)
        self.store.write(f'{f.BASE}/output/{self.run}/upload-manifest.json', b'{}')
        with self.assertRaises((KeyError, ValueError)):
            self.store.export_output(self.source, export)

    def test_process_crash_during_snapshot_never_commits_partial_files(self):
        export = self.export()
        first = self.outbox('a.txt', b'old')
        self.outbox('b.txt', b'second')
        program = '''
import json, os, sys
sys.path.insert(0, sys.argv[1])
import files
store = files.Store(sys.argv[2])
write = store.write
def crash(name, data):
    write(name, data)
    if '/snapshot/' in name:
        os._exit(23)
store.write = crash
store.seal(json.loads(sys.argv[3]))
'''
        result = subprocess.run([sys.executable, '-I', '-c', program,
                                 str(Path(__file__).resolve().parent), self.temp.name,
                                 json.dumps(export)], check=False)
        self.assertEqual(result.returncode, 23)
        base = Path(self.temp.name) / f'{f.BASE}/output/{self.run}'
        self.assertTrue(list(base.glob('.snapshot-*')))
        self.assertFalse((base / 'sealed').exists())
        self.assertFalse((base / 'upload-manifest.json').exists())
        self.store.write(first, b'new')
        result = self.store.export_output(self.source, export)
        self.assertEqual(result['stage'], 'ready')
        self.assertEqual(self.source.uploads, [('a.txt', b'new'), ('b.txt', b'second')])

    def test_output_limits_symlinks_and_empty(self):
        export = self.export()
        self.assertEqual(self.store.export_output(self.source, export)['files'], [])
        self.run = str(uuid.uuid4())
        export = self.export()
        path = self.outbox()
        os.symlink('/etc/passwd', Path(self.temp.name) / (path + '.link'))
        with self.assertRaises(ValueError):
            self.store.export_output(self.source, export)
        (Path(self.temp.name) / (path + '.link')).unlink()
        export['limits']['max_output_bytes'] = 1
        with self.assertRaises(ValueError):
            self.store.export_output(self.source, export)
        export['limits']['max_output_bytes'] = 100
        export['limits']['max_output_files'] = 1
        self.outbox('extra.txt')
        with self.assertRaises(ValueError):
            self.store.export_output(self.source, export)
        self.assertEqual(self.source.uploads, [])

    def test_http_hooks_round_trip_and_identity(self):
        source = self.source
        uploads = []
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                self.assert_auth()
                routes = {'users/me': dict(id=ID), 'posts/' + ID: source.post(ID),
                          'files/' + FILE + '/info': source.file(FILE)}
                key = self.path.removeprefix('/api/v4/')
                body = b'png' if key == 'files/' + FILE else f.encode(routes[key])
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)

            def assert_auth(self):
                if self.headers['Authorization'] != 'Bearer test-secret':
                    raise ValueError('missing authorization')

            def do_POST(self):
                self.assert_auth()
                raw = self.rfile.read(int(self.headers['Content-Length']))
                message = BytesParser(policy=default).parsebytes(
                    ('Content-Type: ' + self.headers['Content-Type'] + '\r\n\r\n').encode() + raw)
                parts = list(message.iter_parts())
                uploads.append((parts[0].get_payload(decode=True), parts[1].get_filename(), parts[1].get_payload(decode=True)))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(f.encode(dict(file_infos=[dict(id=FILE, size=3)])))
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        env = dict(os.environ, ORPHEUS_WORKSPACE_PATH=self.temp.name, MM_INPUT_MANIFEST=json.dumps(self.req),
                   MM_BASE_URL=f'http://127.0.0.1:{server.server_port}', MM_TOKEN_ENV='MM_TEST_TOKEN',
                   MM_TEST_TOKEN='test-secret', MM_EXPECTED_BOT_ID=ID, MM_SOURCE_ID='chat', MM_CHANNEL_ID=ID,
                   ORPHEUS_RUN_ID=self.run, ORPHEUS_SESSION_ID=str(uuid.uuid4()))
        def run(command):
            return subprocess.run([sys.executable, '-I', f.__file__, command], env=env, capture_output=True, timeout=5)
        prepared = run('prepare-input')
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        self.outbox('тест".txt')
        exported = run('export-output')
        self.assertEqual(exported.returncode, 0, exported.stderr)
        output = json.loads(exported.stdout)
        self.assertEqual(output['files'][0]['file_id'], FILE)
        self.assertEqual(uploads, [(ID.encode(), 'тест".txt', b'abc')])
        env['MM_EXPECTED_BOT_ID'] = 'c' * 26
        rejected = run('prepare-input')
        self.assertNotEqual(rejected.returncode, 0)
        self.assertNotIn(b'test-secret', rejected.stderr)


if __name__ == '__main__':
    unittest.main()
