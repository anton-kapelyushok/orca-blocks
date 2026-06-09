set -euo pipefail
CTR=/opt/overlaybd/snapshotter/ctr
NS=moby
NAME="${CONTAINER_NAME:-demo-jb-petclinic-read-${RUN_ID}}"
WORK="/root/orca-overlaybd-demo-${RUN_ID}"
LOG="$WORK/$NAME.log"
PROJECT_DIR="${PROJECT_DIR:-/home/workspace-agent/spring-petclinic}"
JAR_PATH="$PROJECT_DIR/target/spring-petclinic-4.0.0-SNAPSHOT.jar"
TAR_PATH="${TAR_PATH:-$PROJECT_DIR.tar.gz}"
GREP_PATTERN="${GREP_PATTERN:-Pet}"
mkdir -p "$WORK"

echo "image=$IMAGE_REF"
echo "container=$NAME"
echo "project_dir=$PROJECT_DIR"
echo "tar_path=$TAR_PATH"
echo "grep_pattern=$GREP_PATTERN"

echo "Executing local: cleanup stale read container/snapshot"
$CTR -n "$NS" tasks kill -s SIGKILL "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" tasks rm "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" containers rm "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" snapshots --snapshotter overlaybd rm "$NAME" >/dev/null 2>&1 || true

start_ms=$(date +%s%3N)
echo "Executing local: $CTR -n $NS rpull --plain-http $IMAGE_REF"
# DEMO-CMD: $CTR -n "$NS" rpull --plain-http "$IMAGE_REF"
$CTR -n "$NS" rpull --plain-http "$IMAGE_REF"
after_rpull_ms=$(date +%s%3N)
rpull_ms=$((after_rpull_ms - start_ms))

echo "Executing local: rewrite OverlayBD repoBlobUrl https registry URLs to http"
# DEMO-CMD: find /var/lib/containerd/io.containerd.snapshotter.v1.overlaybd/snapshots -path '*/block/config.v1.json' -type f -print0 | xargs -0 -r sed -i -e "s#https://${REGISTRY_HOST}#http://${REGISTRY_HOST}#g" -e "s#https://${MASTER_REGISTRY_HOST}#http://${MASTER_REGISTRY_HOST}#g"
find /var/lib/containerd/io.containerd.snapshotter.v1.overlaybd/snapshots \
  -path '*/block/config.v1.json' -type f -print0 |
  xargs -0 -r sed -i \
    -e "s#https://${REGISTRY_HOST}#http://${REGISTRY_HOST}#g" \
    -e "s#https://${MASTER_REGISTRY_HOST}#http://${MASTER_REGISTRY_HOST}#g"

READ_CMD="set -e; cd '$PROJECT_DIR'; test -d src; test -f '$JAR_PATH'; test -f '$TAR_PATH'; echo artifact_sizes; du -sh '$PROJECT_DIR/src' '$JAR_PATH' '$TAR_PATH' || true; grep_start=\$(date +%s%3N); grep -R -n -m 20 '$GREP_PATTERN' src >/tmp/orca-demo-grep.txt; grep_end=\$(date +%s%3N); cat /tmp/orca-demo-grep.txt; read_start=\$(date +%s%3N); bytes=\$(wc -c < '$TAR_PATH'); cat '$TAR_PATH' >/dev/null; read_end=\$(date +%s%3N); echo grep_ms=\$((grep_end-grep_start)); echo tarball_bytes=\$bytes; echo read_tarball_ms=\$((read_end-read_start)); echo __ORCA_DEMO_READ_DONE__"

echo "Executing local: $CTR -n $NS run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm $IMAGE_REF $NAME sh -lc <grep+read payload>"
# DEMO-CMD: $CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm "$IMAGE_REF" "$NAME" sh -lc "cd '$PROJECT_DIR'; grep -R -n -m 20 '$GREP_PATTERN' src; cat '$TAR_PATH' >/dev/null"
fifo="/tmp/$NAME.fifo"
rm -f "$fifo" "$LOG"
mkfifo "$fifo"
run_start_ms=$(date +%s%3N)
set +e
timeout "$TIMEOUT_SECONDS"s $CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm "$IMAGE_REF" "$NAME" sh -lc "$READ_CMD" >"$fifo" 2>&1 &
runner_pid=$!
set -e
exec 3<"$fifo"

first_user_text_ms=""
read_done_ms=""
grep_ms=""
read_tarball_ms=""
tarball_bytes=""
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while true; do
  if IFS= read -r -t 1 line <&3; then
    now_ms=$(date +%s%3N)
    printf '%s\n' "$line" | tee -a "$LOG"
    if [[ -z "$first_user_text_ms" ]]; then
      first_user_text_ms=$((now_ms - run_start_ms))
    fi
    if [[ "$line" == grep_ms=* ]]; then
      grep_ms="${line#grep_ms=}"
    fi
    if [[ "$line" == tarball_bytes=* ]]; then
      tarball_bytes="${line#tarball_bytes=}"
    fi
    if [[ "$line" == read_tarball_ms=* ]]; then
      read_tarball_ms="${line#read_tarball_ms=}"
    fi
    if [[ "$line" == *"__ORCA_DEMO_READ_DONE__"* ]]; then
      read_done_ms=$((now_ms - run_start_ms))
      break
    fi
  fi
  if ! kill -0 "$runner_pid" >/dev/null 2>&1; then
    break
  fi
  if (( $(date +%s) > deadline )); then
    echo "timeout waiting for read marker"
    break
  fi
done
exec 3<&-
set +e
wait "$runner_pid"
rc=$?
set -e
run_end_ms=$(date +%s%3N)
run_ms=$((run_end_ms - run_start_ms))
rm -f "$fifo"

echo
echo "| metric | value |"
echo "| --- | ---: |"
echo "| rpull | ${rpull_ms} ms |"
echo "| first user text appears | ${first_user_text_ms:-not seen} ms |"
echo "| grep command | ${grep_ms:-not seen} ms |"
echo "| tarball bytes | ${tarball_bytes:-not seen} |"
echo "| read tarball command | ${read_tarball_ms:-not seen} ms |"
echo "| grep+read marker after run | ${read_done_ms:-not seen} ms |"
echo "| grep+read command wall | ${run_ms} ms |"
echo "| exit code | ${rc} |"
exit "$rc"
