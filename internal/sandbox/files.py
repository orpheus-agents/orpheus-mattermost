"""Mattermost file hooks. Python 3 standard library only; no template installation."""
import contextlib
import fcntl
import hashlib
import json
import os
import re
import signal
import stat
import sys
import unicodedata
import urllib.request
import uuid

BASE = '.orpheus/mattermost'


def require(ok, message):
    if not ok:
        raise ValueError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def encode(value):
    # Match Go's compact JSON and HTML escaping for artifact identities.
    text = json.dumps(value, ensure_ascii=False, separators=(',', ':'), allow_nan=False)
    for char in '<>&\u2028\u2029':
        text = text.replace(char, '\\u%04x' % ord(char))
    return text.encode('utf-8')


def valid_id(value):
    return isinstance(value, str) and re.fullmatch('[a-z0-9]{26}', value) is not None


def valid_run(value):
    try:
        return str(uuid.UUID(value)) == value
    except (ValueError, TypeError, AttributeError):
        return False


def safe_name(name):
    name = name.replace('\\', '/').split('/')[-1]
    result = ''
    for char in name:
        char = '_' if unicodedata.category(char) == 'Cc' or char in '/\\:' else char
        if len((result + char).encode()) > 180:
            break
        result += char
    return result.strip(' .') or 'attachment'


def batch_path(anchor):
    return f'{BASE}/input/batches/{anchor}'


def manifest_path(anchor):
    return batch_path(anchor) + '/manifest.json'


def index_path(run):
    return f'{BASE}/runs/{run}/input-index.json'


def validate_request(request):
    require(request['schema'] == 1 and request['source_id'] and
            all(valid_id(request[k]) for k in ('channel_id', 'root_id', 'anchor_post_id')),
            'invalid input identity')
    for file in request.get('files') or []:
        require(all(valid_id(file[k]) for k in ('post_id', 'file_id', 'channel_id')) and
                file['path'] == f"{BASE}/input/files/{file['file_id']}/{safe_name(file['name'])}",
                'invalid attachment path or identity')
    require(all(request['limits'][k] > 0 for k in
                ('max_file_bytes', 'max_image_bytes', 'max_batch_bytes', 'max_per_post')),
            'invalid attachment limits')


class Store:
    """All components opened relative to directory FDs; never follow symlinks."""
    def __init__(self, workspace):
        self.root = os.open(workspace, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)

    def close(self):
        os.close(self.root)

    @contextlib.contextmanager
    def parent(self, name, create=False):
        parts = name.split('/')
        require(all(p not in ('', '.', '..') for p in parts), 'invalid workspace path')
        fd = os.dup(self.root)
        try:
            for part in parts[:-1]:
                if create:
                    try:
                        os.mkdir(part, 0o700, dir_fd=fd)
                    except FileExistsError:
                        pass
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                os.close(fd)
                fd = child
            yield fd, parts[-1]
        finally:
            os.close(fd)

    def mkdir(self, name):
        with self.parent(name + '/.entry', True):
            pass

    def read(self, name, limit):
        with self.parent(name) as (fd, leaf):
            raw = os.open(leaf, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
            with os.fdopen(raw, 'rb') as stream:
                info = os.fstat(stream.fileno())
                require(stat.S_ISREG(info.st_mode) and info.st_size <= limit, 'invalid file type or size')
                data = stream.read(limit + 1)
                require(len(data) <= limit, 'file limit exceeded')
                return data

    def write(self, name, data):
        with self.parent(name, True) as (fd, leaf):
            try:
                require(not stat.S_ISLNK(os.stat(leaf, dir_fd=fd, follow_symlinks=False).st_mode),
                        'workspace symlink rejected')
            except FileNotFoundError:
                pass
            temp = '.tmp-' + uuid.uuid4().hex
            try:
                raw = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=fd)
                with os.fdopen(raw, 'wb') as stream:
                    stream.write(data)
                    stream.flush()
                    os.fsync(stream.fileno())
                os.replace(temp, leaf, src_dir_fd=fd, dst_dir_fd=fd)
                os.fsync(fd)
            finally:
                try:
                    os.unlink(temp, dir_fd=fd)
                except FileNotFoundError:
                    pass

    def json(self, name, value):
        self.write(name, encode(value))

    def load(self, name, limit=1 << 20):
        return json.loads(self.read(name, limit))

    @contextlib.contextmanager
    def lock(self, name):
        with self.parent(name, True) as (fd, leaf):
            raw = os.open(leaf, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600, dir_fd=fd)
            with os.fdopen(raw, 'rb') as stream:
                require(stat.S_ISREG(os.fstat(raw).st_mode), 'invalid lock file')
                fcntl.flock(stream, fcntl.LOCK_EX)
                yield

    def batch(self, request):
        frozen = {k: v for k, v in request.items() if k not in ('previous_index', 'delivered_batches')}
        raw = encode(frozen)
        try:
            old = self.load(batch_path(request['anchor_post_id']) + '/request.json', 65536)
            require(old == frozen, 'batch identity conflict')
        except FileNotFoundError:
            pass
        return raw, dict(schema=1, source_id=request['source_id'],
                         anchor_post_id=request['anchor_post_id'], request_hash=digest(raw), files=[])

    def save_file(self, file, data):
        require(file['status'] == 'ready' and len(data) == file['size_bytes'] and
                digest(data) == file['sha256'], 'invalid downloaded file')
        self.write(file['path'], data)
        self.json(f"{BASE}/input/files/{file['file_id']}/cache.json", file)

    def cached(self, file, info):
        try:
            old = self.load(f"{BASE}/input/files/{file['file_id']}/cache.json", 4096)
            if any(old[k] != file[k] for k in ('path', 'file_id', 'post_id')):
                return None
            if old['size_bytes'] != info['size'] or old['mime'] != info['mime_type']:
                return None
            data = self.read(file['path'], old['size_bytes'])
            if len(data) == old['size_bytes'] and digest(data) == old['sha256']:
                return dict(file, size_bytes=old['size_bytes'], mime=old['mime'],
                            sha256=old['sha256'], status='ready')
        except (FileNotFoundError, ValueError, KeyError):
            pass
        return None

    def prepare(self, source, request):
        validate_request(request)
        with self.lock(batch_path(request['anchor_post_id']) + '/.lock'):
            raw, manifest = self.batch(request)
            self.write(batch_path(request['anchor_post_id']) + '/request.json', raw)
            total, counts = 0, {}
            for original in request.get('files') or []:
                file = dict(original)
                counts[file['post_id']] = counts.get(file['post_id'], 0) + 1
                if counts[file['post_id']] > request['limits']['max_per_post']:
                    file['status'] = 'file_limit_exceeded'
                if file.get('status', '') not in ('', 'ready'):
                    manifest['files'].append(file)
                    continue
                if not 0 <= file['size_bytes'] <= request['limits']['max_batch_bytes'] - total:
                    file['status'] = 'batch_limit_exceeded'
                else:
                    with self.lock(f"{BASE}/input/files/{file['file_id']}/.lock"):
                        info, status = inspect_input(source, request, file)
                        if status:
                            file['status'] = status
                        elif info['size'] > request['limits']['max_batch_bytes'] - total:
                            file['status'] = 'batch_limit_exceeded'
                        else:
                            cached = self.cached(file, info)
                            if cached:
                                file = cached
                            else:
                                try:
                                    data = source.download(file['file_id'], info['size'])
                                    require(len(data) == info['size'], 'download size mismatch')
                                except (OSError, ValueError):
                                    file['status'] = 'download_failed'
                                else:
                                    file.update(size_bytes=len(data), mime=info['mime_type'],
                                                sha256=digest(data), status='ready')
                                    self.save_file(file, data)
                if file.get('status') == 'ready':
                    total += file['size_bytes']
                manifest['files'].append(file)
            self.json(manifest_path(request['anchor_post_id']), manifest)
            return manifest

    def import_input(self, stream):
        def record():
            raw = stream.readline(65536)
            require(raw.endswith(b'\n'), 'invalid import record')
            return json.loads(raw)
        request = record()
        validate_request(request)
        with self.lock(batch_path(request['anchor_post_id']) + '/.lock'):
            raw, manifest = self.batch(request)
            total, counts = 0, {}
            for expected in request.get('files') or []:
                file = record()
                require(all(file[k] == expected[k] for k in ('file_id', 'post_id', 'path', 'channel_id')),
                        'imported file mismatch')
                counts[file['post_id']] = counts.get(file['post_id'], 0) + 1
                if file.get('status') == 'ready':
                    limit = request['limits']['max_image_bytes' if file['mime'].startswith('image/') else 'max_file_bytes']
                    require(0 <= file['size_bytes'] <= min(limit, request['limits']['max_batch_bytes'] - total)
                            and counts[file['post_id']] <= request['limits']['max_per_post'], 'import exceeds limit')
                    data = stream.read(file['size_bytes'])
                    with self.lock(f"{BASE}/input/files/{file['file_id']}/.lock"):
                        self.save_file(file, data)
                    total += file['size_bytes']
                manifest['files'].append(file)
            require(stream.read(1) == b'', 'unexpected import data')
            self.write(batch_path(request['anchor_post_id']) + '/request.json', raw)
            self.json(manifest_path(request['anchor_post_id']), manifest)

    def verify_manifest(self, name, source):
        parts = name.split('/')
        require(len(parts) == 6 and valid_id(parts[4]) and name == manifest_path(parts[4]),
                'invalid batch index path')
        manifest = self.load(name)
        require(manifest['schema'] == 1 and manifest['source_id'] == source and
                manifest['anchor_post_id'] == parts[4], 'invalid input manifest')
        raw = self.read(batch_path(parts[4]) + '/request.json', 65536)
        require(digest(raw) == manifest['request_hash'], 'input manifest request mismatch')

    def begin_run(self, run, request):
        require(valid_run(run), 'invalid run ID')
        previous, seen = request.get('previous_index', ''), set()
        while previous:
            parts = previous.split('/')
            require(len(parts) == 5 and valid_run(parts[3]) and previous == index_path(parts[3])
                    and previous not in seen, 'invalid input index chain')
            seen.add(previous)
            page = self.load(previous)
            require(page['schema'] == 1, 'invalid input index')
            for batch in page['batches']:
                self.verify_manifest(batch, request['source_id'])
            previous = page.get('previous', '')
        batches = (request.get('delivered_batches') or []) + [manifest_path(request['anchor_post_id'])]
        for batch in batches:
            self.verify_manifest(batch, request['source_id'])
        self.json(index_path(run), dict(schema=1, previous=request.get('previous_index', ''), batches=batches))
        outbox = f'{BASE}/output/{run}/outbox'
        self.mkdir(outbox)
        self.json(BASE + '/current-run.json', dict(run_id=run, input_index=index_path(run), outbox=outbox))

    def walk(self, name):
        # Keep the directory FD open throughout iteration, including recursive descent.
        with self.parent(name + '/.entry') as (fd, _):
            for leaf in sorted(os.listdir(fd)):
                info = os.stat(leaf, dir_fd=fd, follow_symlinks=False)
                child = name + '/' + leaf
                if stat.S_ISDIR(info.st_mode):
                    yield from self.walk(child)
                else:
                    require(stat.S_ISREG(info.st_mode), 'output must be regular files')
                    yield child, info

    def seal(self, export):
        base = f"{BASE}/output/{export['run_id']}"
        try:
            output = self.load(base + '/sealed/manifest.json', 16384)
            validate_output(output, export)
            return output
        except FileNotFoundError:
            pass
        output = {k: export[k] for k in ('source_id', 'bot_id', 'channel_id', 'session_id', 'run_id')}
        output.update(schema=1, stage='sealed', files=[])
        outbox, temp = base + '/outbox', base + '/.snapshot-' + uuid.uuid4().hex
        self.mkdir(outbox)
        self.mkdir(temp)
        try:
            for name, before in self.walk(outbox):
                require(len(output['files']) < export['limits']['max_output_files'], 'output file count exceeded')
                data = self.read(name, export['limits']['max_output_bytes'])
                with self.parent(name) as (fd, leaf):
                    after = os.stat(leaf, dir_fd=fd, follow_symlinks=False)
                require((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) ==
                        (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) and
                        data == self.read(name, export['limits']['max_output_bytes']), 'output changed while sealing')
                relative = name[len(outbox) + 1:]
                artifact = dict(artifact_id=digest(encode([export['run_id'], relative, digest(data)])),
                                file_id='', name=safe_name(relative), path='snapshot/' + relative,
                                sha256=digest(data), size_bytes=len(data))
                self.write(temp + '/' + artifact['path'], data)
                output['files'].append(artifact)
            validate_output(output, export)
            self.json(temp + '/manifest.json', output)
            with self.parent(temp) as (fd, leaf):
                os.rename(leaf, 'sealed', src_dir_fd=fd, dst_dir_fd=fd)
                os.fsync(fd)
            return output
        finally:
            self.remove_tree(temp)

    def remove_tree(self, name):
        try:
            with self.parent(name + '/.entry') as (fd, _):
                for leaf in os.listdir(fd):
                    if stat.S_ISDIR(os.stat(leaf, dir_fd=fd, follow_symlinks=False).st_mode):
                        self.remove_tree(name + '/' + leaf)
                    else:
                        os.unlink(leaf, dir_fd=fd)
            with self.parent(name) as (fd, leaf):
                os.rmdir(leaf, dir_fd=fd)
        except FileNotFoundError:
            pass

    def export_output(self, target, export):
        require(all(valid_run(export[k]) for k in ('run_id', 'session_id')) and
                all(valid_id(export[k]) for k in ('bot_id', 'channel_id')) and export['source_id'],
                'invalid export identity')
        base = f"{BASE}/output/{export['run_id']}"
        with self.lock(base + '/.export.lock'):
            output = self.seal(export)
            manifest = base + '/upload-manifest.json'
            try:
                old = self.load(manifest, 16384)
                validate_output(old, export)
                require([f['artifact_id'] for f in old['files']] == [f['artifact_id'] for f in output['files']],
                        'snapshot mismatch')
                output = old
            except FileNotFoundError:
                pass
            output['stage'] = 'uploading'
            self.json(manifest, output)
            for file in output['files']:
                if file.get('file_id'):
                    continue
                data = self.read(base + '/sealed/' + file['path'], export['limits']['max_output_bytes'])
                require(len(data) == file['size_bytes'] and digest(data) == file['sha256'], 'sealed snapshot damaged')
                uploaded = target.upload(export['channel_id'], file['name'], data)
                require(valid_id(uploaded['id']) and uploaded['size'] == file['size_bytes'], 'invalid upload response')
                file['file_id'] = uploaded['id']
                self.json(manifest, output)
            output['stage'] = 'ready'
            validate_output(output, export)
            self.json(manifest, output)
            return output


def validate_output(output, export):
    require(output['schema'] == 1 and all(output[k] == export[k] for k in
            ('source_id', 'bot_id', 'channel_id', 'session_id', 'run_id')) and
            output['stage'] in ('sealed', 'uploading', 'ready') and
            len(output['files']) <= export['limits']['max_output_files'], 'invalid output identity or limits')
    seen = set()
    for file in output['files']:
        path = file['path']
        require(path.startswith('snapshot/') and all(p not in ('', '.', '..') for p in path.split('/'))
                and file['name'] == safe_name(file['name']) and len(file['sha256']) == 64
                and 0 <= file['size_bytes'] <= export['limits']['max_output_bytes']
                and file['artifact_id'] == digest(encode([output['run_id'], path[9:], file['sha256']]))
                and file['artifact_id'] not in seen
                and (output['stage'] != 'ready' or valid_id(file['file_id'])), 'invalid output artifact')
        seen.add(file['artifact_id'])
    require(len(encode(output)) <= 16384, 'output manifest exceeds limit')


def inspect_input(source, request, file):
    try:
        post = source.post(file['post_id'])
        allowed = post['channel_id'] == request['channel_id'] or any(
            pair['source'] == post['channel_id'] and pair['destination'] == request['channel_id']
            for pair in request.get('allowed_channel_pairs', []))
        require(not post.get('delete_at') and post['channel_id'] == file['channel_id'] and allowed
                and file['file_id'] in (post.get('file_ids') or []), 'unavailable')
        info = source.file(file['file_id'])
        require(info['post_id'] == file['post_id'] and info['id'] == file['file_id'], 'unavailable')
    except (OSError, ValueError, KeyError):
        return {}, 'unavailable'
    limit = request['limits']['max_image_bytes' if info['mime_type'].startswith('image/') else 'max_file_bytes']
    return info, '' if 0 <= info['size'] <= limit else 'file_limit_exceeded'


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Mattermost:
    def __init__(self, base, token):
        self.base = base.rstrip('/') + '/api/v4/'
        self.token = token
        self.http = urllib.request.build_opener(NoRedirect)

    def request(self, path, limit=1 << 20, data=None, content_type=None):
        headers = {'Authorization': 'Bearer ' + self.token}
        if content_type:
            headers['Content-Type'] = content_type
        request = urllib.request.Request(self.base + path, data=data, headers=headers)
        with self.http.open(request, timeout=30) as response:
            raw = response.read(limit + 1)
            require(len(raw) <= limit, 'HTTP response exceeds limit')
            return raw

    def post(self, post_id):
        return json.loads(self.request('posts/' + post_id))

    def file(self, file_id):
        return json.loads(self.request('files/' + file_id + '/info'))

    def download(self, file_id, limit):
        return self.request('files/' + file_id, limit)

    def upload(self, channel, name, data):
        boundary = uuid.uuid4().hex
        filename = name.replace('\\', '\\\\').replace('"', '\\"')
        prefix = (f'--{boundary}\r\nContent-Disposition: form-data; name="channel_id"\r\n\r\n{channel}\r\n'
                  f'--{boundary}\r\nContent-Disposition: form-data; name="files"; filename="{filename}"\r\n'
                  'Content-Type: application/octet-stream\r\n\r\n').encode()
        body = prefix + data + f'\r\n--{boundary}--\r\n'.encode()
        result = json.loads(self.request('files', data=body, content_type='multipart/form-data; boundary=' + boundary))
        require(len(result['file_infos']) == 1, 'invalid upload response')
        return result['file_infos'][0]


def main(command):
    store = Store(os.environ['ORPHEUS_WORKSPACE_PATH'])
    try:
        if command == 'import-input':
            timeout = float(os.environ.get('MM_IMPORT_TIMEOUT', '120'))
            require(timeout > 0, 'invalid import timeout')
            def expired(_signum, _frame):
                raise TimeoutError('import timed out')
            signal.signal(signal.SIGALRM, expired)
            signal.setitimer(signal.ITIMER_REAL, timeout)
            try:
                store.import_input(sys.stdin.buffer)
            finally:
                signal.setitimer(signal.ITIMER_REAL, 0)
            return
        raw = os.environ['MM_INPUT_MANIFEST']
        require(len(raw.encode()) <= 65536, 'input request exceeds ENV limit')
        request = json.loads(raw)
        validate_request(request)
        source = Mattermost(os.environ['MM_BASE_URL'], os.environ[os.environ['MM_TOKEN_ENV']])
        bot = json.loads(source.request('users/me'))
        require(bot['id'] == os.environ['MM_EXPECTED_BOT_ID'], 'mattermost bot identity mismatch')
        require(request['source_id'] == os.environ['MM_SOURCE_ID'] and
                request['channel_id'] == os.environ['MM_CHANNEL_ID'], 'input request identity mismatch')
        if command == 'prepare-input':
            store.prepare(source, request)
            store.begin_run(os.environ['ORPHEUS_RUN_ID'], request)
        elif command == 'export-output':
            output = store.export_output(source, dict(source_id=request['source_id'], bot_id=bot['id'],
                channel_id=request['channel_id'], session_id=os.environ['ORPHEUS_SESSION_ID'],
                run_id=os.environ['ORPHEUS_RUN_ID'], limits=request['limits']))
            print(encode(output).decode())
        else:
            raise ValueError('unknown file command')
    finally:
        store.close()


if __name__ == '__main__':
    try:
        main(sys.argv[1])
    except Exception:
        # Do not expose credentials, HTTP bodies or file contents in hook logs.
        print('Mattermost file hook failed', file=sys.stderr)
        sys.exit(1)
