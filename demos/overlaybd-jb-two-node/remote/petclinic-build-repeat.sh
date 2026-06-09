set -euo pipefail
CTR=/opt/overlaybd/snapshotter/ctr
NS=moby
NAME="${CONTAINER_NAME:-demo-jb-petclinic-repeat-${RUN_ID}}"
WORK="/root/orca-overlaybd-demo-${RUN_ID}"
LOG="$WORK/$NAME.log"
PROJECT_DIR="${PROJECT_DIR:-/home/workspace-agent/spring-petclinic}"
JAR_PATH="$PROJECT_DIR/target/spring-petclinic-4.0.0-SNAPSHOT.jar"
mkdir -p "$WORK"

echo "image=$IMAGE_REF"
echo "container=$NAME"
echo "project_dir=$PROJECT_DIR"

echo "Executing local: allow cni0 forwarding for Maven payload"
# DEMO-CMD: iptables -C FORWARD -i cni0 -j ACCEPT || iptables -I FORWARD 1 -i cni0 -j ACCEPT
iptables -C FORWARD -i cni0 -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -i cni0 -j ACCEPT
# DEMO-CMD: iptables -C FORWARD -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT || iptables -I FORWARD 1 -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
iptables -C FORWARD -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null ||
  iptables -I FORWARD 1 -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

echo "Executing local: cleanup stale repeat container/snapshot"
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

BUILD_CMD="set -e; sudo sh -c 'printf \"nameserver 1.1.1.1\\nnameserver 8.8.8.8\\n\" > /etc/resolv.conf' >/dev/null 2>&1 || true; cd '$PROJECT_DIR'; test -f pom.xml; test -f '$JAR_PATH'; echo before_sizes; du -sh /home/workspace-agent/.m2 '$PROJECT_DIR' '$JAR_PATH' || true; build_start=\$(date +%s%3N); ./mvnw -q -DskipTests package; build_end=\$(date +%s%3N); echo after_sizes; du -sh /home/workspace-agent/.m2 '$PROJECT_DIR' '$JAR_PATH' || true; echo repeat_build_ms=\$((build_end-build_start)); echo __ORCA_DEMO_REPEAT_BUILD_DONE__"

echo "Executing local: $CTR -n $NS run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm $IMAGE_REF $NAME sh -lc <repeat build payload>"
# DEMO-CMD: $CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm "$IMAGE_REF" "$NAME" sh -lc "set -e; sudo sh -c 'printf \"nameserver 1.1.1.1\nnameserver 8.8.8.8\n\" > /etc/resolv.conf' >/dev/null 2>&1 || true; cd '$PROJECT_DIR'; test -f pom.xml; test -f '$JAR_PATH'; ./mvnw -q -DskipTests package"
fifo="/tmp/$NAME.fifo"
rm -f "$fifo" "$LOG"
mkfifo "$fifo"
run_start_ms=$(date +%s%3N)
set +e
timeout "$TIMEOUT_SECONDS"s $CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm "$IMAGE_REF" "$NAME" sh -lc "$BUILD_CMD" >"$fifo" 2>&1 &
runner_pid=$!
set -e
exec 3<"$fifo"

first_user_text_ms=""
repeat_done_ms=""
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while true; do
  if IFS= read -r -t 1 line <&3; then
    now_ms=$(date +%s%3N)
    printf '%s\n' "$line" | tee -a "$LOG"
    if [[ -z "$first_user_text_ms" ]]; then
      first_user_text_ms=$((now_ms - run_start_ms))
    fi
    if [[ "$line" == *"__ORCA_DEMO_REPEAT_BUILD_DONE__"* ]]; then
      repeat_done_ms=$((now_ms - run_start_ms))
      break
    fi
  fi
  if ! kill -0 "$runner_pid" >/dev/null 2>&1; then
    break
  fi
  if (( $(date +%s) > deadline )); then
    echo "timeout waiting for repeat build marker"
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
echo "| repeat build marker after run | ${repeat_done_ms:-not seen} ms |"
echo "| repeat build command wall | ${run_ms} ms |"
echo "| exit code | ${rc} |"
exit "$rc"
