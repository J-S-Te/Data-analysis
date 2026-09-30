#!/usr/bin/env bash
set -Eeuo pipefail
# Remote stdin entrypoint. A parallel dashboard release must not use platform
# assets until their paired reload and legacy image-pointer upgrade have finished.
[[ $# == 6 ]] || { echo 'expected deploy root, script, and four image references' >&2; exit 2; }
deploy_root="$1"
deploy_script="$2"
shift 2
[[ -d "$deploy_root" && -f "$deploy_root/.env" ]] || { echo 'deployment root/config missing' >&2; exit 1; }
project="$(awk -F= '$1 == "COMPOSE_PROJECT_NAME" {print substr($0,index($0,"=")+1); exit}' "$deploy_root/.env")"
project="${project:-basic-platform-production}"
[[ "$project" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || { echo 'invalid compose project' >&2; exit 1; }
timeout="${DASHBOARD_PLATFORM_WAIT_SECONDS:-900}"
[[ "$timeout" =~ ^[0-9]+$ && ${#timeout} -le 4 ]] && ((10#$timeout <= 1800)) || { echo 'invalid platform wait timeout' >&2; exit 1; }
deadline=$((SECONDS + 10#$timeout))
platform_ready() {
  local service container health
  [[ -f "$deploy_root/docker-compose.yml" && -x "$deploy_script" &&
     ! -e "$deploy_root/runtime/.control-plane-reload-required" ]] || return 1
  for service in platform-api subsystem-provisioner; do
    container="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$service")" || return 1
    [[ -n "$container" && "$container" != *$'\n'* ]] || return 1
    health="$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$container")" || return 1
    [[ "$health" == 'running healthy' ]] || return 1
  done
}
until platform_ready; do
  ((SECONDS < deadline)) || { echo 'platform assets/control plane not ready; finish platform deployment before dashboard release' >&2; exit 1; }
  echo 'Waiting for platform deployment; dashboard images are unchanged.'
  sleep 5
done
cd -- "$deploy_root"
exec bash "$deploy_script" data-analysis "$@"
