set -euo pipefail
CTR=/opt/overlaybd/snapshotter/ctr
NS=moby
SNAPSHOTTER=overlaybd
CONFIG=/etc/overlaybd-snapshotter/config.json
start_ms=$(date +%s%3N)

echo "hostname=$(hostname)"
echo "rwMode before cleanup: $(jq -r .rwMode "$CONFIG" 2>/dev/null || echo unknown)"

echo "Executing local: findmnt -rn -o TARGET,SOURCE,FSTYPE | grep overlaybd || true"
# DEMO-CMD: findmnt -rn -o TARGET,SOURCE,FSTYPE | grep overlaybd || true
findmnt -rn -o TARGET,SOURCE,FSTYPE | grep overlaybd || true

echo "Executing local: $CTR -n $NS tasks ls"
# DEMO-CMD: $CTR -n "$NS" tasks ls || true
$CTR -n "$NS" tasks ls || true

echo "Executing local: kill demo tasks"
for task in $($CTR -n "$NS" tasks ls -q 2>/dev/null | grep -E '^demo-jb-' || true); do
  echo "Executing local: $CTR -n $NS tasks kill -s SIGKILL $task"
# DEMO-CMD: $CTR -n "$NS" tasks kill -s SIGKILL "$task" || true
  $CTR -n "$NS" tasks kill -s SIGKILL "$task" || true
  echo "Executing local: $CTR -n $NS tasks rm $task"
# DEMO-CMD: $CTR -n "$NS" tasks rm "$task" || true
  $CTR -n "$NS" tasks rm "$task" || true
done

echo "Executing local: remove demo containers"
for container in $($CTR -n "$NS" containers ls -q 2>/dev/null | grep -E '^demo-jb-' || true); do
  echo "Executing local: $CTR -n $NS containers rm $container"
# DEMO-CMD: $CTR -n "$NS" containers rm "$container" || true
  $CTR -n "$NS" containers rm "$container" || true
done

echo "Executing local: remove demo snapshots"
for snapshot in $($CTR -n "$NS" snapshots --snapshotter "$SNAPSHOTTER" ls 2>/dev/null | awk 'NR > 1 {print $1}' | grep -E '^demo-jb-' || true); do
  echo "Executing local: $CTR -n $NS snapshots --snapshotter $SNAPSHOTTER rm $snapshot"
# DEMO-CMD: $CTR -n "$NS" snapshots --snapshotter "$SNAPSHOTTER" rm "$snapshot" || true
  $CTR -n "$NS" snapshots --snapshotter "$SNAPSHOTTER" rm "$snapshot" || true
done

remaining_tasks="$($CTR -n "$NS" tasks ls -q 2>/dev/null || true)"
remaining_containers="$($CTR -n "$NS" containers ls -q 2>/dev/null || true)"
if [[ -n "$remaining_tasks" || -n "$remaining_containers" ]]; then
  echo "Refusing to clear caches while non-demo runtime state is active."
  echo "remaining_tasks=$remaining_tasks"
  echo "remaining_containers=$remaining_containers"
  exit 1
fi

if [[ "$(jq -r .rwMode "$CONFIG")" != "overlayfs" ]]; then
  echo "Executing local: jq '.rwMode=\"overlayfs\"' $CONFIG"
# DEMO-CMD: jq '.rwMode="overlayfs" | .mirrorRegistry = ...' "$CONFIG" >"$tmp"
  tmp="$(mktemp)"
  jq '.rwMode="overlayfs" | .mirrorRegistry = (((.mirrorRegistry // []) + [{"host": env.REGISTRY_HOST, "insecure": true}, {"host": env.NODE2_REGISTRY_HOST, "insecure": true}]) | unique_by(.host))' "$CONFIG" >"$tmp"
  mv "$tmp" "$CONFIG"
  echo "Executing local: systemctl restart overlaybd-snapshotter"
# DEMO-CMD: systemctl restart overlaybd-snapshotter
  systemctl restart overlaybd-snapshotter
fi

echo "Executing local: remove local OverlayBD image refs for $REPO"
# DEMO-CMD: $CTR -n "$NS" images ls -q | grep -E "$REPO|air-workspace|overlaybd-jb-real" | xargs -r $CTR -n "$NS" images rm
$CTR -n "$NS" images ls -q 2>/dev/null |
  grep -E "$REPO|air-workspace|overlaybd-jb-real" |
  xargs -r $CTR -n "$NS" images rm || true

echo "Executing local: remove local OverlayBD snapshots"
# DEMO-CMD: remove OverlayBD snapshots leaf-first: list keys/parents, remove keys that are not parents, repeat
for _ in $(seq 1 80); do
  snapshot_rows="$($CTR -n "$NS" snapshots --snapshotter "$SNAPSHOTTER" ls 2>/dev/null | awk 'NR > 1 {print $1 " " $2}' || true)"
  [[ -n "$snapshot_rows" ]] || break

  keys_file="$(mktemp)"
  parents_file="$(mktemp)"
  leaves_file="$(mktemp)"
  printf '%s\n' "$snapshot_rows" | awk '{print $1}' | sort -u >"$keys_file"
  printf '%s\n' "$snapshot_rows" | awk 'NF > 1 && $2 != "" && $2 != "-" {print $2}' | sort -u >"$parents_file"
  comm -23 "$keys_file" "$parents_file" >"$leaves_file"

  if [[ ! -s "$leaves_file" ]]; then
    rm -f "$keys_file" "$parents_file" "$leaves_file"
    break
  fi

  while IFS= read -r snapshot; do
    [[ -n "$snapshot" ]] || continue
    echo "Executing local: $CTR -n $NS snapshots --snapshotter $SNAPSHOTTER rm $snapshot"
    $CTR -n "$NS" snapshots --snapshotter "$SNAPSHOTTER" rm "$snapshot" >/dev/null 2>&1 || true
  done <"$leaves_file"

  rm -f "$keys_file" "$parents_file" "$leaves_file"
done

echo "Executing local: remove local containerd content"
# DEMO-CMD: $CTR -n "$NS" content ls -q | xargs -r $CTR -n "$NS" content rm
$CTR -n "$NS" content ls -q 2>/dev/null |
  xargs -r $CTR -n "$NS" content rm || true

echo "Executing local: clear OverlayBD local caches, keep registry/MySQL durable state"
# DEMO-CMD: rm -rf /opt/overlaybd/registry_cache /opt/overlaybd/gzip_cache
rm -rf /opt/overlaybd/registry_cache /opt/overlaybd/gzip_cache
mkdir -p /opt/overlaybd/registry_cache /opt/overlaybd/gzip_cache

echo "Executing local: final active runtime check"
findmnt -rn -o TARGET,SOURCE,FSTYPE | grep overlaybd || true
$CTR -n "$NS" tasks ls || true
$CTR -n "$NS" containers ls || true
$CTR -n "$NS" images ls -q 2>/dev/null | grep -E "$REPO|air-workspace|overlaybd-jb-real" || true
du -sh /opt/overlaybd/registry_cache /opt/overlaybd/gzip_cache 2>/dev/null || true
echo "rwMode after cleanup: $(jq -r .rwMode "$CONFIG")"
end_ms=$(date +%s%3N)

echo
echo "| metric | value |"
echo "| --- | ---: |"
echo "| cleanup elapsed | $((end_ms - start_ms)) ms |"
