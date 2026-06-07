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
	if writeSession["memory_snapshot_path"] != "" || writeSession["vmstate_snapshot_path"] != "" {
		t.Fatalf("memory snapshots should be disabled by default, got %+v", writeSession)
	}
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

func TestFirecrackerMemoryRestore(t *testing.T) {
	control := getenv("CONTROL_URL", "http://localhost:18080")
	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	volumeID := fmt.Sprintf("fc-memory-itest-%d", time.Now().UnixNano())
	payload := fmt.Sprintf("firecracker memory restore payload %d", time.Now().UnixNano())
	t.Logf("creating storage volume %s", volumeID)
	var volume map[string]any
	postJSON(t, control+"/volumes/create", map[string]any{
		"volume_id":  volumeID,
		"size_bytes": 64 * 1024 * 1024,
		"chunk_size": 1024 * 1024,
	}, &volume)
	t.Logf("created firecracker memory test volume: %+v", volume)

	t.Log("starting firecracker write session on node-1 with memory snapshot explicitly enabled")
	writeSession := startFirecrackerSessionWithMemorySnapshot(t, control, volumeID, "node-1", "write", payload, true)
	t.Logf("node-1 firecracker session=%s output=%q snapshot=%s work_dir=%s", writeSession["session_id"], writeSession["firecracker_output"], writeSession["snapshot_id"], writeSession["firecracker_work_dir"])
	logFirecrackerTimings(t, "node-1 memory write", writeSession)
	assertFirecrackerSessionData(t, "node-1", writeSession)
	assertFirecrackerMemorySnapshot(t, "node-1", writeSession)
	assertTimingPresent(t, "node-1 memory write", writeSession, "create_memory_snapshot")
	if writeSession["snapshot_id"] == "" {
		t.Fatalf("expected firecracker write session to commit a snapshot: %+v", writeSession)
	}

	t.Log("restoring node-1 firecracker memory snapshot")
	restoreSession := restoreFirecrackerSession(t, control, volumeID, "node-1", writeSession)
	t.Logf("node-1 restored firecracker session=%s work_dir=%s restored_mem=%s restored_vmstate=%s",
		restoreSession["session_id"], restoreSession["firecracker_work_dir"], restoreSession["restored_memory_snapshot"], restoreSession["restored_vmstate_snapshot"])
	logFirecrackerTimings(t, "node-1 memory restore", restoreSession)
	assertFirecrackerSessionData(t, "node-1", restoreSession)
	assertTimingPresent(t, "node-1 memory restore", restoreSession, "restore_memory_snapshot")
	stopSession(t, restoreSession)
}

func TestFirecrackerDockerSmoke(t *testing.T) {
	if getenv("FIRECRACKER_DOCKER_TEST", "") != "true" {
		t.Skip("set FIRECRACKER_DOCKER_TEST=true and run Compose with FIRECRACKER_BOOT_MODE=rootfs")
	}
	if getenv("FIRECRACKER_BOOT_MODE", "") != "rootfs" {
		t.Skip("docker-in-guest smoke requires FIRECRACKER_BOOT_MODE=rootfs")
	}

	control := getenv("CONTROL_URL", "http://localhost:18080")
	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	volumeID := fmt.Sprintf("fc-docker-itest-%d", time.Now().UnixNano())
	payload := fmt.Sprintf("docker inside firecracker payload %d", time.Now().UnixNano())
	t.Logf("creating storage volume %s", volumeID)
	var volume map[string]any
	postJSON(t, control+"/volumes/create", map[string]any{
		"volume_id":  volumeID,
		"size_bytes": 64 * 1024 * 1024,
		"chunk_size": 1024 * 1024,
	}, &volume)
	t.Logf("created docker firecracker test volume: %+v", volume)

	t.Log("starting firecracker docker-smoke session on node-1")
	writeSession := startFirecrackerSession(t, control, volumeID, "node-1", "docker-smoke", payload, true)
	t.Logf("node-1 docker firecracker session=%s output=%q snapshot=%s work_dir=%s", writeSession["session_id"], writeSession["firecracker_output"], writeSession["snapshot_id"], writeSession["firecracker_work_dir"])
	logFirecrackerTimings(t, "node-1 docker-smoke", writeSession)
	assertFirecrackerSessionData(t, "node-1", writeSession)
	if writeSession["firecracker_boot_mode"] != "rootfs" {
		t.Fatalf("expected rootfs boot mode for docker smoke, got %+v", writeSession)
	}
	if writeSession["snapshot_id"] == "" {
		t.Fatalf("expected docker-smoke session to commit a snapshot: %+v", writeSession)
	}
	if !strings.Contains(writeSession["firecracker_output"], "orca-init: dockerd ready") {
		t.Fatalf("firecracker docker output did not confirm dockerd readiness: %+v", writeSession)
	}
	if !strings.Contains(writeSession["firecracker_output"], "orca-init: docker container ok") {
		t.Fatalf("firecracker docker output did not confirm container execution: %+v", writeSession)
	}
	if !strings.Contains(writeSession["firecracker_output"], "orca-init: docker-smoke ok") {
		t.Fatalf("firecracker docker output did not confirm docker-smoke completion: %+v", writeSession)
	}

	t.Log("starting firecracker docker-read session on node-1 to verify Docker-written data inside a container")
	readSession := startFirecrackerSession(t, control, volumeID, "node-1", "docker-read", payload, false)
	assertFirecrackerDockerRead(t, "node-1", readSession)
	stats := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", readSession["node_url"], readSession["session_id"]))
	t.Logf("docker proof node-1 got hits=%v misses=%v remote_fetches=%v zero_fills=%v dirty_chunks=%v",
		stats["cache_hits"], stats["cache_misses"], stats["remote_fetches"], stats["zero_fills"], stats["dirty_chunks"])
	if asInt(stats["cache_hits"]) <= 0 {
		t.Fatalf("expected docker proof node-1 cache hits, got %+v", stats)
	}
	stopSession(t, readSession)

	t.Log("starting firecracker docker-read session on node-2 to verify Docker-written data after remote fetch")
	readNode2 := startFirecrackerSession(t, control, volumeID, "node-2", "docker-read", payload, false)
	assertFirecrackerDockerRead(t, "node-2", readNode2)
	stats2 := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", readNode2["node_url"], readNode2["session_id"]))
	t.Logf("docker proof node-2 got hits=%v misses=%v remote_fetches=%v zero_fills=%v dirty_chunks=%v",
		stats2["cache_hits"], stats2["cache_misses"], stats2["remote_fetches"], stats2["zero_fills"], stats2["dirty_chunks"])
	if asInt(stats2["cache_misses"]) <= 0 || asInt(stats2["remote_fetches"]) <= 0 {
		t.Fatalf("expected docker proof node-2 cache misses and remote fetches, got %+v", stats2)
	}
	stopSession(t, readNode2)
}

func TestFirecrackerDockerMemoryRestore(t *testing.T) {
	if getenv("FIRECRACKER_DOCKER_TEST", "") != "true" {
		t.Skip("set FIRECRACKER_DOCKER_TEST=true and run Compose with FIRECRACKER_BOOT_MODE=rootfs")
	}
	if getenv("FIRECRACKER_BOOT_MODE", "") != "rootfs" {
		t.Skip("docker memory restore requires FIRECRACKER_BOOT_MODE=rootfs")
	}

	control := getenv("CONTROL_URL", "http://localhost:18080")
	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	volumeID := fmt.Sprintf("fc-docker-memory-itest-%d", time.Now().UnixNano())
	payload := fmt.Sprintf("docker memory restore payload %d", time.Now().UnixNano())
	t.Logf("creating storage volume %s", volumeID)
	var volume map[string]any
	postJSON(t, control+"/volumes/create", map[string]any{
		"volume_id":  volumeID,
		"size_bytes": 64 * 1024 * 1024,
		"chunk_size": 1024 * 1024,
	}, &volume)
	t.Logf("created docker memory test volume: %+v", volume)

	t.Log("starting firecracker docker-smoke session on node-1 with memory snapshot explicitly enabled")
	writeSession := startFirecrackerSessionWithMemorySnapshot(t, control, volumeID, "node-1", "docker-smoke", payload, true)
	t.Logf("node-1 docker memory session=%s output=%q snapshot=%s work_dir=%s", writeSession["session_id"], writeSession["firecracker_output"], writeSession["snapshot_id"], writeSession["firecracker_work_dir"])
	logFirecrackerTimings(t, "node-1 docker memory write", writeSession)
	assertFirecrackerSessionData(t, "node-1", writeSession)
	assertFirecrackerMemorySnapshot(t, "node-1", writeSession)
	assertTimingPresent(t, "node-1 docker memory write", writeSession, "create_memory_snapshot")
	if writeSession["snapshot_id"] == "" {
		t.Fatalf("expected docker-smoke memory session to commit a snapshot: %+v", writeSession)
	}
	if !strings.Contains(writeSession["firecracker_output"], "orca-init: dockerd ready") {
		t.Fatalf("firecracker docker output did not confirm dockerd readiness: %+v", writeSession)
	}
	if !strings.Contains(writeSession["firecracker_output"], "orca-init: docker-smoke ok") {
		t.Fatalf("firecracker docker output did not confirm docker-smoke completion: %+v", writeSession)
	}

	t.Log("restoring node-1 docker firecracker memory snapshot")
	restoreSession := restoreFirecrackerSession(t, control, volumeID, "node-1", writeSession)
	t.Logf("node-1 restored docker firecracker session=%s work_dir=%s restored_mem=%s restored_vmstate=%s",
		restoreSession["session_id"], restoreSession["firecracker_work_dir"], restoreSession["restored_memory_snapshot"], restoreSession["restored_vmstate_snapshot"])
	logFirecrackerTimings(t, "node-1 docker memory restore", restoreSession)
	assertFirecrackerSessionData(t, "node-1", restoreSession)
	assertTimingPresent(t, "node-1 docker memory restore", restoreSession, "restore_memory_snapshot")
	stopSession(t, restoreSession)
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

func startFirecrackerSessionWithMemorySnapshot(t *testing.T, control, volumeID, node, mode, payload string, commitAfterRun bool) map[string]string {
	t.Helper()
	var out map[string]string
	postJSON(t, control+"/sessions/start", map[string]any{
		"volume_id":            volumeID,
		"force_node":           node,
		"runtime":              "firecracker",
		"firecracker_mode":     mode,
		"firecracker_payload":  payload,
		"commit_after_run":     commitAfterRun,
		"save_memory_snapshot": true,
	}, &out)
	if out["session_id"] == "" || out["node_url"] == "" || out["firecracker_output"] == "" {
		t.Fatalf("missing firecracker session fields: %+v", out)
	}
	if out["firecracker_work_dir"] == "" || out["firecracker_timings"] == "" {
		t.Fatalf("missing firecracker session debug fields: %+v", out)
	}
	return out
}

func restoreFirecrackerSession(t *testing.T, control, volumeID, node string, source map[string]string) map[string]string {
	t.Helper()
	var out map[string]string
	postJSON(t, control+"/sessions/start", map[string]any{
		"volume_id":                     volumeID,
		"force_node":                    node,
		"runtime":                       "firecracker",
		"firecracker_mode":              "restore",
		"restore_memory_snapshot_path":  source["memory_snapshot_path"],
		"restore_vmstate_snapshot_path": source["vmstate_snapshot_path"],
		"restore_firecracker_device":    source["firecracker_device"],
	}, &out)
	if out["session_id"] == "" || out["node_url"] == "" || out["firecracker_work_dir"] == "" || out["firecracker_timings"] == "" {
		t.Fatalf("missing restored firecracker session fields: %+v", out)
	}
	if out["restored_memory_snapshot"] == "" || out["restored_vmstate_snapshot"] == "" {
		t.Fatalf("missing restored snapshot fields: %+v", out)
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

func assertFirecrackerDockerRead(t *testing.T, node string, session map[string]string) {
	t.Helper()
	t.Logf("%s firecracker session=%s output=%q work_dir=%s", node, session["session_id"], session["firecracker_output"], session["firecracker_work_dir"])
	logFirecrackerTimings(t, node, session)
	assertFirecrackerSessionData(t, node, session)
	if !strings.Contains(session["firecracker_output"], "orca-init: dockerd ready") {
		t.Fatalf("firecracker docker read output did not confirm dockerd readiness: %+v", session)
	}
	if !strings.Contains(session["firecracker_output"], "orca-init: docker container ok") {
		t.Fatalf("firecracker docker read output did not confirm container execution: %+v", session)
	}
	if !strings.Contains(session["firecracker_output"], "orca-init: docker-read ok") {
		t.Fatalf("firecracker docker read output did not confirm read: %+v", session)
	}
}

func assertTimingPresent(t *testing.T, node string, session map[string]string, name string) {
	t.Helper()
	var timings []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(session["firecracker_timings"]), &timings); err != nil {
		t.Fatalf("decode firecracker timings for %s: %v body=%s", node, err, session["firecracker_timings"])
	}
	for _, timing := range timings {
		if timing.Name == name {
			if timing.Status != "ok" {
				t.Fatalf("expected timing %s for %s to be ok, got %+v", name, node, timing)
			}
			return
		}
	}
	t.Fatalf("expected timing %s for %s in %+v", name, node, timings)
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

func assertFirecrackerMemorySnapshot(t *testing.T, service string, session map[string]string) {
	t.Helper()
	memPath := session["memory_snapshot_path"]
	statePath := session["vmstate_snapshot_path"]
	if memPath == "" || statePath == "" {
		t.Fatalf("expected firecracker memory snapshot fields on write session: %+v", session)
	}
	t.Logf("checking retained firecracker memory snapshot on %s: mem=%s vmstate=%s", service, memPath, statePath)
	runComposeExec(t, service, fmt.Sprintf("test -s %s && test -s %s", shellQuote(memPath), shellQuote(statePath)))
}
