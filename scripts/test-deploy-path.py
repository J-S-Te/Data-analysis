"""Execute the actual workflow's path resolution, without SSH or credentials."""
import os
from pathlib import Path
import subprocess

workflow = (Path(__file__).resolve().parents[1] / ".github/workflows/ci-cd.yml").read_text()
start = workflow.index('          [[ "$DEPLOY_PATH" == /* ]]')
end = workflow.index('          install -m 700', start)
script = workflow[start:end] + '\nprintf "%s" "$DEPLOY_SCRIPT"\n'
assert "vars.DASHBOARD_DEPLOY_SCRIPT ||" not in workflow
for directory, override, expected in [
    ("/opt/basic-platform", "", "/opt/basic-platform/bin/deploy-service.sh"),
    ("/opt/alternative/", "", "/opt/alternative/bin/deploy-service.sh"),
    ("/opt/space dir", "", "/opt/space dir/bin/deploy-service.sh"),
    ("/opt/basic-platform", "/custom/deploy.sh", "/custom/deploy.sh"),
    ("relative", "", None),
    ("/opt/basic-platform", "relative.sh", None),
]:
    result = subprocess.run(["bash", "-euc", script], env={**os.environ,
                            "DEPLOY_PATH": directory, "DEPLOY_SCRIPT": override},
                            capture_output=True, text=True)
    if expected is None:
        assert result.returncode != 0, (directory, override)
    else:
        assert result.returncode == 0, result.stderr
        assert result.stdout == expected, result.stdout
print("PASS: deployment script follows deployment root and validates overrides")
