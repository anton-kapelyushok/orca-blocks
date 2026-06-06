//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMountedSessionFilesystemNodeOneThenNodeTwo(t *testing.T) {
	control := getenv("CONTROL_URL", "http://localhost:18080")
	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	volumeID := fmt.Sprintf("mount-itest-%d", time.Now().UnixNano())
	payload := fmt.Sprintf("mounted filesystem payload %d", time.Now().UnixNano())
	t.Logf("creating volume %s", volumeID)
	var volume map[string]any
	postJSON(t, control+"/volumes/create", map[string]any{
		"volume_id":  volumeID,
		"size_bytes": 64 * 1024 * 1024,
		"chunk_size": 1024 * 1024,
	}, &volume)

	t.Log("start mounted session on node-1, format ext4, write file, commit")
	first := startMountedSession(t, control, volumeID, "node-1", true)
	writeMountedFile(t, "node-1", first["mount_path"], "proof.txt", payload)
	commitSession(t, first)

	t.Log("reconnect mounted session on node-1, read same file, commit")
	second := startMountedSession(t, control, volumeID, "node-1", false)
	got := readMountedFile(t, "node-1", second["mount_path"], "proof.txt")
	if got != payload {
		t.Fatalf("node-1 reconnect read = %q, want %q", got, payload)
	}
	stats1 := mountedSessionStats(t, second)
	t.Logf("mounted node-1 got hits=%v misses=%v remote_fetches=%v zero_fills=%v dirty_chunks=%v", stats1["cache_hits"], stats1["cache_misses"], stats1["remote_fetches"], stats1["zero_fills"], stats1["dirty_chunks"])
	if asInt(stats1["cache_hits"]) <= 0 {
		t.Fatalf("expected mounted node-1 cache hits, got %+v", stats1)
	}
	commitSession(t, second)

	t.Log("connect mounted session on node-2, read same file, stop without commit")
	third := startMountedSession(t, control, volumeID, "node-2", false)
	got = readMountedFile(t, "node-2", third["mount_path"], "proof.txt")
	if got != payload {
		t.Fatalf("node-2 reconnect read = %q, want %q", got, payload)
	}
	stats2 := mountedSessionStats(t, third)
	t.Logf("mounted node-2 got hits=%v misses=%v remote_fetches=%v zero_fills=%v dirty_chunks=%v", stats2["cache_hits"], stats2["cache_misses"], stats2["remote_fetches"], stats2["zero_fills"], stats2["dirty_chunks"])
	if asInt(stats2["cache_misses"]) <= 0 || asInt(stats2["remote_fetches"]) <= 0 {
		t.Fatalf("expected mounted node-2 cache misses and remote fetches, got %+v", stats2)
	}
	stopSession(t, third)
}

func startMountedSession(t *testing.T, control, volumeID, node string, format bool) map[string]string {
	t.Helper()
	var start map[string]string
	postJSON(t, control+"/sessions/start", map[string]any{
		"volume_id":  volumeID,
		"force_node": node,
		"runtime":    "mounted-fs",
		"format":     format,
		"fs_type":    "ext4",
	}, &start)
	if start["mount_path"] == "" || start["node_url"] == "" || start["session_id"] == "" {
		t.Fatalf("missing mounted session fields: %+v", start)
	}
	t.Logf("mounted session node=%s session=%s path=%s device=%s", node, start["session_id"], start["mount_path"], start["nbd_device"])
	return start
}

func writeMountedFile(t *testing.T, service, mountPath, name, payload string) {
	t.Helper()
	t.Logf("writing file through mounted disk on %s: %s/%s", service, mountPath, name)
	script := fmt.Sprintf("printf %%s %s > %s/%s && sync", shellQuote(payload), shellQuote(mountPath), shellQuote(name))
	runComposeExec(t, service, script)
}

func readMountedFile(t *testing.T, service, mountPath, name string) string {
	t.Helper()
	t.Logf("reading file through mounted disk on %s: %s/%s", service, mountPath, name)
	out := runComposeExec(t, service, fmt.Sprintf("cat %s/%s", shellQuote(mountPath), shellQuote(name)))
	return string(bytes.TrimRight(out, "\n"))
}

func commitSession(t *testing.T, session map[string]string) {
	t.Helper()
	var snapshot map[string]any
	postJSON(t, fmt.Sprintf("%s/sessions/%s/commit", session["node_url"], session["session_id"]), nil, &snapshot)
	t.Logf("committed mounted session snapshot=%v", snapshot["snapshot_id"])
}

func stopSession(t *testing.T, session map[string]string) {
	t.Helper()
	t.Logf("stopping mounted session %s on %s", session["session_id"], session["node_url"])
	postJSON(t, fmt.Sprintf("%s/sessions/%s/stop", session["node_url"], session["session_id"]), nil, nil)
}

func mountedSessionStats(t *testing.T, session map[string]string) map[string]any {
	t.Helper()
	t.Logf("fetching mounted session stats for %s on %s", session["session_id"], session["node_url"])
	return getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", session["node_url"], session["session_id"]))
}

func runComposeExec(t *testing.T, service, script string) []byte {
	t.Helper()
	cmd := exec.Command("docker", "compose", "exec", "-T", service, "sh", "-lc", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose exec %s failed: %v\n%s", service, err, out)
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
