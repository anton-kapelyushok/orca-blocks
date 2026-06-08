set -euo pipefail
CTR=/opt/overlaybd/snapshotter/ctr
NS=moby
CONFIG=/etc/overlaybd-snapshotter/config.json
IMAGE_REF="${IMAGE_REF:-${REGISTRY_HOST}/${REPO}:${BASE_TAG}}"
NAME="${CONTAINER_NAME:-demo-jb-petclinic-mutable-${RUN_ID}}"
WORK="/root/orca-overlaybd-demo-${RUN_ID}"
LOG="$WORK/$NAME.log"
ENV_FILE="$WORK/mutable.env"
PROJECT_DIR="${PROJECT_DIR:-/home/workspace-agent/spring-petclinic}"
JAR_PATH="$PROJECT_DIR/target/spring-petclinic-4.0.0-SNAPSHOT.jar"
mkdir -p "$WORK"

echo "image=$IMAGE_REF"
echo "container=$NAME"
echo "project_dir=$PROJECT_DIR"

echo "Executing local: assert OverlayBD rwMode=overlayfs"
if [[ "$(jq -r .rwMode "$CONFIG")" != "overlayfs" ]]; then
  echo "Expected rwMode=overlayfs. Run demo cleanup first."
  cat "$CONFIG"
  exit 1
fi
echo "rwMode now: $(jq -r .rwMode "$CONFIG")"

echo "Executing local: allow cni0 forwarding for clone/build payload"
# DEMO-CMD: iptables -C FORWARD -i cni0 -j ACCEPT || iptables -I FORWARD 1 -i cni0 -j ACCEPT
iptables -C FORWARD -i cni0 -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -i cni0 -j ACCEPT
# DEMO-CMD: iptables -C FORWARD -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT || iptables -I FORWARD 1 -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
iptables -C FORWARD -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null ||
  iptables -I FORWARD 1 -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

echo "Executing local: cleanup stale mutable container/snapshot"
$CTR -n "$NS" tasks kill -s SIGKILL "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" tasks rm "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" containers rm "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" snapshots --snapshotter overlaybd rm "$NAME" >/dev/null 2>&1 || true

echo "Executing local: $CTR -n $NS rpull --plain-http $IMAGE_REF"
# DEMO-CMD: $CTR -n "$NS" rpull --plain-http "$IMAGE_REF"
start_ms=$(date +%s%3N)
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

PROJECT_PARENT="$(dirname "$PROJECT_DIR")"
PROJECT_NAME="$(basename "$PROJECT_DIR")"
BUILD_CMD="set -eux; sudo sh -c 'printf \"nameserver 1.1.1.1\\nnameserver 8.8.8.8\\n\" > /etc/resolv.conf' || true; cd '$PROJECT_PARENT'; rm -rf '$PROJECT_NAME'; clone_start=\$(date +%s%3N); git clone --depth 1 https://github.com/spring-projects/spring-petclinic.git '$PROJECT_NAME'; clone_end=\$(date +%s%3N); cd '$PROJECT_NAME'; build_start=\$(date +%s%3N); ./mvnw -q -DskipTests package; build_end=\$(date +%s%3N); test -f '$JAR_PATH'; du -sh /home/workspace-agent/.m2 '$PROJECT_DIR' '$JAR_PATH' || true; echo clone_ms=\$((clone_end-clone_start)); echo first_build_ms=\$((build_end-build_start)); echo __ORCA_DEMO_BUILD_DONE__; while :; do sleep 3600; done"

echo "Executing local: $CTR -n $NS run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc $IMAGE_REF $NAME sh -lc <clone+build+sleep payload>"
# DEMO-CMD: $CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc "$IMAGE_REF" "$NAME" sh -lc "set -eux; sudo sh -c 'printf \"nameserver 1.1.1.1\nnameserver 8.8.8.8\n\" > /etc/resolv.conf' || true; cd '$PROJECT_PARENT'; rm -rf '$PROJECT_NAME'; git clone --depth 1 https://github.com/spring-projects/spring-petclinic.git '$PROJECT_NAME'; cd '$PROJECT_NAME'; ./mvnw -q -DskipTests package; test -f '$JAR_PATH'; while :; do sleep 3600; done" > >(tee -a "$LOG") 2>&1 &
fifo="/tmp/$NAME.fifo"
rm -f "$fifo" "$LOG"
mkfifo "$fifo"
run_start_ms=$(date +%s%3N)
set +e
$CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc "$IMAGE_REF" "$NAME" sh -lc "$BUILD_CMD" >"$fifo" 2>&1 &
runner_pid=$!
set -e
exec 3<"$fifo"

first_user_text_ms=""
task_ready_ms=""
build_done_ms=""
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while true; do
  if IFS= read -r -t 0.1 line <&3; then
    now_ms=$(date +%s%3N)
    printf '%s\n' "$line" | tee -a "$LOG"
    if [[ -z "$first_user_text_ms" ]]; then
      first_user_text_ms=$((now_ms - run_start_ms))
    fi
    if [[ "$line" == "__ORCA_DEMO_BUILD_DONE__" ]]; then
      build_done_ms=$((now_ms - run_start_ms))
      break
    fi
  fi
  if [[ -z "$task_ready_ms" ]] && $CTR -n "$NS" tasks ls -q 2>/dev/null | grep -Fx "$NAME" >/dev/null; then
    task_ready_ms=$(( $(date +%s%3N) - run_start_ms ))
  fi
  if ! kill -0 "$runner_pid" >/dev/null 2>&1; then
    echo "container process exited before build marker appeared"
    break
  fi
  if (( $(date +%s) > deadline )); then
    echo "timeout waiting for build marker"
    break
  fi
done
exec 3<&-

if [[ -z "$task_ready_ms" || -z "$build_done_ms" ]]; then
  $CTR -n "$NS" tasks kill -s SIGKILL "$NAME" || true
  $CTR -n "$NS" tasks rm "$NAME" || true
  $CTR -n "$NS" containers rm "$NAME" || true
  wait "$runner_pid" || true
  rm -f "$fifo"
  exit 1
fi

echo "Executing local: find task pid and containerd overlay upperdir"
# DEMO-CMD: $CTR -n "$NS" tasks ls | awk -v name="$NAME" '$1==name {print $2}'; $CTR -n "$NS" snapshots --snapshotter overlaybd mounts /tmp/probe "$NAME"
PID="$($CTR -n "$NS" tasks ls | awk -v name="$NAME" '$1==name {print $2}')"
if [[ -z "$PID" ]]; then
  echo "Could not find task pid for $NAME"
  exit 1
fi
MOUNT_CMD="$($CTR -n "$NS" snapshots --snapshotter overlaybd mounts /tmp/orca-demo-probe "$NAME")"
UPPERDIR="$(printf '%s' "$MOUNT_CMD" | sed -n 's/.*upperdir=\([^,]*\).*/\1/p')"
WORKDIR="$(printf '%s' "$MOUNT_CMD" | sed -n 's/.*workdir=\([^,]*\).*/\1/p')"
LOWERDIR="$(printf '%s' "$MOUNT_CMD" | sed -n 's/.*lowerdir=\([^,]*\).*/\1/p')"
if [[ -z "$UPPERDIR" || ! -d "$UPPERDIR" ]]; then
  echo "Could not resolve overlay upperdir from snapshot mount command:"
  echo "$MOUNT_CMD"
  exit 1
fi
SNAPSHOT_DIR="$(dirname "$UPPERDIR")"
OBD_CONFIG_PATH="$SNAPSHOT_DIR/block/config.v1.json"
if [[ ! -f "$OBD_CONFIG_PATH" ]]; then
  TOP_LOWERDIR="${LOWERDIR%%:*}"
  if [[ "$TOP_LOWERDIR" == */block/mountpoint ]]; then
    OBD_CONFIG_PATH="${TOP_LOWERDIR%/mountpoint}/config.v1.json"
  elif [[ "$TOP_LOWERDIR" == */fs ]]; then
    OBD_CONFIG_PATH="$(dirname "$TOP_LOWERDIR")/block/config.v1.json"
  fi
  if [[ ! -f "$OBD_CONFIG_PATH" ]]; then
    echo "Could not find OverlayBD image config for active snapshot or top lowerdir:"
    echo "active_snapshot_config=$SNAPSHOT_DIR/block/config.v1.json"
    echo "top_lowerdir=$TOP_LOWERDIR"
    echo "lowerdir_config=$OBD_CONFIG_PATH"
    exit 1
  fi
fi
UID_MAP="$(tr '\n' ';' <"/proc/$PID/uid_map")"
GID_MAP="$(tr '\n' ';' <"/proc/$PID/gid_map")"
echo "pid=$PID"
echo "mount_command=$MOUNT_CMD"
echo "upperdir=$UPPERDIR"
echo "workdir=$WORKDIR"
echo "lowerdir=$LOWERDIR"
echo "overlaybd_config=$OBD_CONFIG_PATH"
echo "uid_map=$UID_MAP"
echo "gid_map=$GID_MAP"

echo "Executing local: stop task but keep container/snapshot for upperdir diff export"
# DEMO-CMD: $CTR -n "$NS" tasks kill -s SIGTERM "$NAME"; $CTR -n "$NS" tasks rm "$NAME"
stop_start_ms=$(date +%s%3N)
$CTR -n "$NS" tasks kill -s SIGTERM "$NAME" || true
sleep 2
$CTR -n "$NS" tasks kill -s SIGKILL "$NAME" || true
$CTR -n "$NS" tasks rm "$NAME" || true
wait "$runner_pid" || true
stop_end_ms=$(date +%s%3N)
stop_ms=$((stop_end_ms - stop_start_ms))
rm -f "$fifo"

cat >"$ENV_FILE" <<EOF_INNER
NAME=$NAME
IMAGE_REF=$IMAGE_REF
UPPERDIR=$UPPERDIR
WORKDIR=$WORKDIR
LOWERDIR=$LOWERDIR
OBD_CONFIG_PATH=$OBD_CONFIG_PATH
UID_MAP='$UID_MAP'
GID_MAP='$GID_MAP'
TOUCH_PATH=$JAR_PATH
PROJECT_DIR=$PROJECT_DIR
LOG=$LOG
EOF_INNER

echo
echo "| metric | value |"
echo "| --- | ---: |"
echo "| rpull | ${rpull_ms} ms |"
echo "| first user text appears | ${first_user_text_ms:-not seen} ms |"
echo "| task ready after run | ${task_ready_ms} ms |"
echo "| clone+first build marker after run | ${build_done_ms} ms |"
echo "| stop task | ${stop_ms} ms |"
echo "mutable_env_file=$ENV_FILE"
