"""Test the actual remote stdin entrypoint with isolated Docker fixtures."""
import os
from pathlib import Path
import subprocess
import tempfile

script = Path(__file__).with_name('deploy-remote.sh').read_text()
with tempfile.TemporaryDirectory() as directory:
    root = Path(directory)
    (root / 'runtime').mkdir()
    (root / '.env').write_text('COMPOSE_PROJECT_NAME=fixture\n')
    (root / 'docker-compose.yml').write_text('services: {}\n')
    docker = root / 'docker'
    docker.write_text('#!/bin/bash\nif [[ "$1" == ps ]]; then echo container; else echo "${FIXTURE_HEALTH}"; fi\n')
    docker.chmod(0o755)
    deploy = root / 'deploy.sh'
    deploy.write_text('#!/bin/bash\nprintf "%s\\n" "$@"\n')
    deploy.chmod(0o755)
    env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
               FIXTURE_HEALTH='running healthy', DASHBOARD_PLATFORM_WAIT_SECONDS='0')
    def run(success=True):
        result = subprocess.run(['bash', '-s', '--', str(root), str(deploy), 'api', 'aggregation', 'alert', 'migration'],
                                input=script, text=True, capture_output=True, env=env)
        assert (result.returncode == 0) == success, result.stderr
        return result
    assert run().stdout.splitlines() == ['data-analysis', 'api', 'aggregation', 'alert', 'migration']
    env['FIXTURE_HEALTH'] = 'restarting unhealthy'
    assert not run(False).stdout
    env['FIXTURE_HEALTH'] = 'running healthy'
    (root / 'runtime/.control-plane-reload-required').touch()
    assert not run(False).stdout
print('PASS: stdin dashboard deployment and unhealthy/pending reload rejection')
