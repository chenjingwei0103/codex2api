#!/usr/bin/env bash
set -Eeuo pipefail

repo_dir="/opt/codex2api"
remote_name="origin"
branch_name="main"
service_name="codex2api-uds.service"
binary_path="/usr/local/bin/codex2api-uds"
lock_path="/run/lock/codex2api-update.lock"
log_path="/var/log/codex2api-update.log"
deployed_revision_path="/var/lib/codex2api/deployed-revision"

mkdir -p "$(dirname "$lock_path")"
exec 9>"$lock_path"
if ! flock -n 9; then
  exit 0
fi

log() {
  printf '[%s] %s\n' "$(date --iso-8601=seconds)" "$*" >>"$log_path"
}

cd "$repo_dir"

current_branch="$(git symbolic-ref --short -q HEAD || true)"
if [[ "$current_branch" != "$branch_name" ]]; then
  log "skip: current branch is ${current_branch:-detached}, expected $branch_name"
  exit 0
fi

if ! git fetch --prune "$remote_name" "$branch_name" >>"$log_path" 2>&1; then
  log "failed: git fetch"
  exit 1
fi

status_output="$(git status --porcelain=v1 --untracked-files=all)"
if [[ -n "$status_output" ]]; then
  log "skip: working tree is not clean"
  printf '%s\n' "$status_output" >>"$log_path"
  exit 0
fi

remote_ref="$remote_name/$branch_name"
local_revision="$(git rev-parse HEAD)"
remote_revision="$(git rev-parse "$remote_ref")"
deployed_revision="$(cat "$deployed_revision_path" 2>/dev/null || true)"
if [[ "$local_revision" == "$remote_revision" && "$deployed_revision" == "$remote_revision" ]]; then
  log "ok: already up to date at $local_revision"
  exit 0
fi
if [[ "$local_revision" == "$remote_revision" ]]; then
  log "rebuild: source revision is not recorded as deployed"
fi

if ! git merge-base --is-ancestor HEAD "$remote_ref"; then
  log "skip: local branch diverged from $remote_ref"
  exit 0
fi

if ! git merge --ff-only "$remote_ref" >>"$log_path" 2>&1; then
  log "failed: fast-forward update"
  exit 1
fi

build_dir="$(mktemp -d /tmp/codex2api-update.XXXXXX)"
image_tag="codex2api-update:$(date +%s)-$$"
container_id=""
cleanup() {
  if [[ -n "$container_id" ]]; then
    docker rm "$container_id" >>"$log_path" 2>&1 || true
  fi
  docker image rm "$image_tag" >>"$log_path" 2>&1 || true
  rm -rf "$build_dir"
}
trap cleanup EXIT

if ! docker build --target go-builder --tag "$image_tag" "$repo_dir" >>"$log_path" 2>&1; then
  log "failed: Docker build"
  exit 1
fi

container_id="$(docker create "$image_tag")"
if ! docker cp "$container_id:/codex2api" "$build_dir/codex2api-uds" >>"$log_path" 2>&1; then
  log "failed: extract compiled binary"
  exit 1
fi
if [[ ! -s "$build_dir/codex2api-uds" ]]; then
  log "failed: compiled binary is empty"
  exit 1
fi

install -m 0755 "$binary_path" "$build_dir/codex2api-uds.previous"
install -m 0755 "$build_dir/codex2api-uds" "$binary_path.new"
mv -f "$binary_path.new" "$binary_path"

if ! systemctl restart "$service_name" >>"$log_path" 2>&1; then
  log "failed: restart $service_name"
  install -m 0755 "$build_dir/codex2api-uds.previous" "$binary_path"
  systemctl restart "$service_name" >>"$log_path" 2>&1 || true
  exit 1
fi

for attempt in $(seq 1 45); do
  if systemctl is-active --quiet "$service_name"; then
    install -d -m 0755 "$(dirname "$deployed_revision_path")"
    printf '%s\n' "$remote_revision" >"$deployed_revision_path.new"
    install -m 0644 "$deployed_revision_path.new" "$deployed_revision_path"
    rm -f "$deployed_revision_path.new"
    log "updated: $local_revision -> $remote_revision"
    exit 0
  fi
  sleep 1
done

log "failed: $service_name did not become active"
install -m 0755 "$build_dir/codex2api-uds.previous" "$binary_path"
systemctl restart "$service_name" >>"$log_path" 2>&1 || true
exit 1
