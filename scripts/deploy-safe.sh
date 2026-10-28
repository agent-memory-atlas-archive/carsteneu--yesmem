#!/usr/bin/env bash
# Run the maintenance window in its own systemd unit. A lost terminal or an
# OpenCode/yesmem restart must not cancel the process that brings services back.
set -Eeuo pipefail
umask 077

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ ${1:-} != --worker ]]; then
  : "${YESMEM_BACKUP_REMOTE:?Set YESMEM_BACKUP_REMOTE to the approved rclone backup directory}"
  [[ -x "$repo/yesmem" ]] || { echo 'Build yesmem before starting deployment.' >&2; exit 1; }
  stamp=$(date +%Y%m%d-%H%M%S)
  run_dir="$repo/.yesmem/deploy/$stamp"
  mkdir -p "$run_dir"
  unit="yesmem-deploy-$stamp"
  systemd-run --user --quiet --unit="$unit" --property=Type=exec \
    --working-directory="$repo" --setenv="PATH=$PATH" \
    /bin/bash "$repo/scripts/deploy-safe.sh" --worker "$run_dir" "$YESMEM_BACKUP_REMOTE"
  printf 'Deployment unit: %s\nLog: %s/deploy.log\n' "$unit" "$run_dir"
  exit
fi

run_dir=$2
remote="${3%/}/$(basename -- "$run_dir")"
data="$HOME/.claude/yesmem"
backup="$run_dir/backup"
exec >"$run_dir/deploy.log" 2>&1
exec 9>"$repo/.yesmem/deploy.lock"
flock -n 9 || { echo 'Another safe deployment is running.'; exit 1; }
cd "$repo"

frozen=false
installing=false
healthy=false
recover() {
  rc=$?
  trap - EXIT
  if $frozen; then
    systemctl --user thaw yesmem.service yesmem-proxy.service || true
  fi
  if $installing && ! $healthy; then
    echo 'Deployment failed; restoring the previous executable and restarting services.'
    if [[ -f "$backup/yesmem.previous" ]]; then
      flock -w 60 "$HOME/.local/bin/.yesmem-update.lock" /bin/bash -c '
        tmp=$(mktemp "$2.rollback.XXXXXX")
        install -m 755 "$1" "$tmp" && mv -f "$tmp" "$2"
      ' bash "$backup/yesmem.previous" "$HOME/.local/bin/yesmem" || true
    fi
    systemctl --user restart yesmem.service yesmem-proxy.service || true
  fi
  printf '%s\n' "$rc" >"$run_dir/exit-code"
  exit "$rc"
}
trap recover EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Refuse before touching services if the archive or an active agent prevents
# maintenance. The freeze below also stops scheduler children and the updater.
rclone lsf "${3%/}/" --dirs-only --max-depth 1 >"$run_dir/archive-preflight.txt"
active=$(sqlite3 -readonly "$data/yesmem.db" "SELECT count(*) FROM agents WHERE status IN ('running','spawning','starting');")
[[ $active == 0 ]] || { echo "Refusing deployment with $active active agents."; exit 1; }
systemctl --user is-active --quiet yesmem.service
systemctl --user is-active --quiet yesmem-proxy.service
mkdir -p "$backup"
cp -p "$HOME/.local/bin/yesmem" "$backup/yesmem.previous"

# Freeze rather than leave a stopped daemon available to MCP auto-start while
# copying. This is a file snapshot of quiescent writers, including their WALs.
frozen=true
systemctl --user freeze yesmem.service yesmem-proxy.service
for database in "$data"/*.db; do
  [[ -f $database ]] || continue
  cp --reflink=auto -p "$database" "$backup/"
  for suffix in -wal -shm -journal; do
    if [[ -f "$database$suffix" ]]; then
      cp --reflink=auto -p "$database$suffix" "$backup/"
    fi
  done
done
chmod 600 "$backup/"*.db*
printf 'Snapshot complete: %s\n' "$backup"

# systemctl stop thaws a frozen unit before stopping it. From here the EXIT
# handler restores the executable and services if installation is interrupted.
installing=true
systemctl --user stop yesmem-proxy.service yesmem.service
frozen=false
make -o build deploy
for attempt in {1..30}; do
  if systemctl --user is-active --quiet yesmem.service && \
      systemctl --user is-active --quiet yesmem-proxy.service && \
      curl -fsS --max-time 3 http://127.0.0.1:9099/health >"$run_dir/health.json"; then
    healthy=true
    break
  fi
  sleep 1
done
$healthy || { echo 'Services did not become healthy.'; exit 1; }
"$HOME/.local/bin/yesmem" version

# Verify by downloading the remote bytes, then remove only our backup copy.
# Upload failures leave services running and the local rollback intact.
rclone copy "$backup" "$remote" --transfers 2 --checkers 2
rclone check "$backup" "$remote" --download --one-way --checkers 2
printf '%s\n' "$remote" >"$run_dir/backup-remote"
rm -rf -- "$backup"
echo 'Deployment healthy; backup verified in remote archive.'
