"""Execute recovery shell commands against disposable fixture directories only.

Used by the WSLC mock; never connects to Docker or mounts a real volume.
"""
import argparse
import base64
import json
import pathlib
import shlex
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--scenario', required=True)
    parser.add_argument('--stage', choices=['preflight', 'restore'], required=True)
    parser.add_argument('--script', required=True)
    args = parser.parse_args()
    shell = shutil.which('sh')
    if not shell:
        raise RuntimeError('recovery sandbox requires sh')
    with tempfile.TemporaryDirectory(prefix='uas-recovery-fixture-') as directory:
        root = pathlib.Path(directory)
        backup, target = root / 'backup', root / 'target'
        backup.mkdir()
        target.mkdir()
        fixtures = {'search.db': b'backup database fixture', 'master.key': b'k' * 32,
                    'credentials.json': b'{"fixture":true}'}
        for name, content in fixtures.items():
            (backup / name).write_bytes(content)
        sentinel = target / 'current-data'
        sentinel.write_bytes(b'preserve existing data')
        if args.scenario != 'valid':
            timing, state, name = args.scenario.split(':')
            if timing not in ('early', 'late') or state not in ('missing', 'empty') or name not in fixtures:
                raise ValueError('unknown recovery scenario')
            if timing == 'early' or args.stage == 'restore':
                if state == 'missing':
                    (backup / name).unlink()
                else:
                    (backup / name).write_bytes(b'')
        command = base64.b64decode(args.script).decode('utf-8')
        # Quote paths for the actual POSIX shell, including MSYS sh on Windows.
        command = command.replace('/backup/', shlex.quote(backup.as_posix()) + '/')
        command = command.replace('/target', shlex.quote(target.as_posix()))
        script = root / 'run.sh'
        script.write_text(command + '\n', encoding='utf-8')
        result = subprocess.run([shell, script.as_posix()], capture_output=True, timeout=10)
        preserved = sentinel.is_file() and sentinel.read_bytes() == b'preserve existing data'
        restored = all((target / name).is_file() and (target / name).read_bytes() == content
                       for name, content in fixtures.items())
        print(json.dumps({'exit_code': result.returncode, 'target_preserved': preserved,
                          'restored': restored}))


if __name__ == '__main__':
    main()
