//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFirecrackerSessionNodeOneThenNodeTwo(t *testing.T) {
	control := getenv("CONTROL_URL", "http://localhost:18080")
	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	volumeID := fmt.Sprintf("fc-itest-%d", time.Now().UnixNano())
	payload := fmt.Sprintf("firecracker proof payload %d", time.Now().UnixNano())
	t.Logf("creating storage volume %s", volumeID)
	var volume map[string]any
	postJSON(t, control+"/volumes/create", map[string]any{
		"volume_id":  volumeID,
		"size_bytes": 64 * 1024 * 1024,
		"chunk_size": 1024 * 1024,
	}, &volume)
	t.Logf("created firecracker test volume: %+v", volume)

	t.Log("starting firecracker write session on node-1")
	writeSession := startFirecrackerSession(t, control, volumeID, "node-1", "write", payload, true)
	t.Logf("node-1 firecracker session=%s output=%q snapshot=%s work_dir=%s", writeSession["session_id"], writeSession["firecracker_output"], writeSession["snapshot_id"], writeSession["firecracker_work_dir"])
	logFirecrackerTimings(t, "node-1", writeSession)
	assertFirecrackerSessionData(t, "node-1", writeSession)
	if writeSession["snapshot_id"] == "" {
		t.Fatalf("expected firecracker write session to commit a snapshot: %+v", writeSession)
	}
	if !strings.Contains(writeSession["firecracker_output"], "orca-init: write ok") {
		t.Fatalf("firecracker write output did not confirm write: %+v", writeSession)
	}

	t.Log("starting firecracker read session on node-1; expecting local cache hits")
	readNode1 := startFirecrackerSession(t, control, volumeID, "node-1", "read", payload, false)
	assertFirecrackerRead(t, "node-1", readNode1)
	stats1 := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", readNode1["node_url"], readNode1["session_id"]))
	t.Logf("firecracker node-1 read got hits=%v misses=%v remote_fetches=%v zero_fills=%v dirty_chunks=%v",
		stats1["cache_hits"], stats1["cache_misses"], stats1["remote_fetches"], stats1["zero_fills"], stats1["dirty_chunks"])
	if asInt(stats1["cache_hits"]) <= 0 {
		t.Fatalf("expected firecracker node-1 cache hits, got %+v", stats1)
	}
	if asInt(stats1["dirty_chunks"]) != 0 {
		t.Fatalf("expected read-only firecracker node-1 session to leave no dirty chunks, got %+v", stats1)
	}
	stopSession(t, readNode1)

	t.Log("starting firecracker read session on node-2")
	readSession := startFirecrackerSession(t, control, volumeID, "node-2", "read", payload, false)
	assertFirecrackerRead(t, "node-2", readSession)

	t.Log("fetching node-2 stats; expecting local misses and remote object fetches")
	stats2 := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", readSession["node_url"], readSession["session_id"]))
	t.Logf("firecracker node-2 got hits=%v misses=%v remote_fetches=%v zero_fills=%v dirty_chunks=%v",
		stats2["cache_hits"], stats2["cache_misses"], stats2["remote_fetches"], stats2["zero_fills"], stats2["dirty_chunks"])
	if asInt(stats2["cache_misses"]) <= 0 || asInt(stats2["remote_fetches"]) <= 0 {
		t.Fatalf("expected firecracker node-2 cache misses and remote fetches, got %+v", stats2)
	}
	if asInt(stats2["dirty_chunks"]) != 0 {
		t.Fatalf("expected read-only firecracker session to leave no dirty chunks, got %+v", stats2)
	}

	t.Log("stopping node-2 firecracker read session")
	stopSession(t, readSession)
}

func startFirecrackerSession(t *testing.T, control, volumeID, node, mode, payload string, commitAfterRun bool) map[string]string {
	t.Helper()
	var out map[string]string
	postJSON(t, control+"/sessions/start", map[string]any{
		"volume_id":           volumeID,
		"force_node":          node,
		"runtime":             "firecracker",
		"firecracker_mode":    mode,
		"firecracker_payload": payload,
		"commit_after_run":    commitAfterRun,
	}, &out)
	if out["session_id"] == "" || out["node_url"] == "" || out["firecracker_output"] == "" {
		t.Fatalf("missing firecracker session fields: %+v", out)
	}
	if out["firecracker_work_dir"] == "" || out["firecracker_timings"] == "" {
		t.Fatalf("missing firecracker session debug fields: %+v", out)
	}
	return out
}

func assertFirecrackerRead(t *testing.T, node string, session map[string]string) {
	t.Helper()
	t.Logf("%s firecracker session=%s output=%q work_dir=%s", node, session["session_id"], session["firecracker_output"], session["firecracker_work_dir"])
	logFirecrackerTimings(t, node, session)
	assertFirecrackerSessionData(t, node, session)
	if !strings.Contains(session["firecracker_output"], "orca-init: proof ok") {
		t.Fatalf("firecracker read output did not confirm payload correctness: %+v", session)
	}
	if !strings.Contains(session["firecracker_output"], "orca-init: read ok") {
		t.Fatalf("firecracker read output did not confirm read: %+v", session)
	}
}

func logFirecrackerTimings(t *testing.T, node string, session map[string]string) {
	t.Helper()
	var timings []struct {
		Name       string `json:"name"`
		DurationMS int64  `json:"duration_ms"`
		Status     string `json:"status"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal([]byte(session["firecracker_timings"]), &timings); err != nil {
		t.Fatalf("decode firecracker timings for %s: %v body=%s", node, err, session["firecracker_timings"])
	}
	if len(timings) == 0 {
		t.Fatalf("expected firecracker timings for %s", node)
	}

	var lines strings.Builder
	lines.WriteString(node)
	lines.WriteString(" firecracker timings:\n")
	for _, timing := range timings {
		lines.WriteString(fmt.Sprintf("  %-26s %6dms  %s", timing.Name, timing.DurationMS, timing.Status))
		if timing.Error != "" {
			lines.WriteString("  error=")
			lines.WriteString(timing.Error)
		}
		lines.WriteByte('\n')
	}
	t.Log(strings.TrimRight(lines.String(), "\n"))
}

func assertFirecrackerSessionData(t *testing.T, service string, session map[string]string) {
	t.Helper()
	workDir := session["firecracker_work_dir"]
	t.Logf("checking retained firecracker session data on %s at %s", service, workDir)
	runComposeExec(t, service, fmt.Sprintf("test -d %s && test -f %s/serial.log && test -f %s/timings.json && test -f %s/firecracker.json", shellQuote(workDir), shellQuote(workDir), shellQuote(workDir), shellQuote(workDir)))
}
