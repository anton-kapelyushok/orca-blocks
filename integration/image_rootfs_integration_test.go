//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildImageRootfsBootsFirecrackerThroughService(t *testing.T) {
	if getenv("BASE_IMAGE_FIRECRACKER_TEST", "") != "true" {
		t.Skip("set BASE_IMAGE_FIRECRACKER_TEST=true on a Linux host with base-image-service, node-service, KVM, and Firecracker assets")
	}

	baseImageURL := getenv("BASE_IMAGE_URL", "http://localhost:18083")
	control := getenv("CONTROL_URL", "http://localhost:18080")
	t.Logf("waiting for base image service at %s", baseImageURL)
	waitFor(t, baseImageURL+"/healthz")
	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	t.Log("building alpine:3.22 through base-image-service")
	built := buildBaseImage(t, baseImageURL, "alpine:3.22")
	baseImageID := requireStringField(t, built, "base_image_id")
	volumeID := requireStringField(t, built, "volume_id")
	snapshotID := requireStringField(t, built, "snapshot_id")
	t.Logf("built base image id=%s volume=%s snapshot=%s duration_ms=%v",
		baseImageID, volumeID, snapshotID, built["duration_ms"])

	proof := fmt.Sprintf("image-rootfs-service-proof-%d", time.Now().UnixNano())
	command := fmt.Sprintf("grep -qx alpine:3.22 /etc/orca-image-ref && echo %s", proof)
	t.Log("starting Firecracker on node-1 with the built image volume as rootfs")
	session := startImageRootFSFirecrackerSession(t, control, volumeID, "node-1", command)
	t.Logf("image rootfs firecracker session=%s output=%q work_dir=%s",
		session["session_id"], session["firecracker_output"], session["firecracker_work_dir"])
	logFirecrackerTimings(t, "node-1 image-rootfs", session)
	assertFirecrackerSessionData(t, "node-1", session)

	output := session["firecracker_output"]
	if !strings.Contains(output, proof) {
		t.Fatalf("firecracker image rootfs output did not include proof %q: %+v", proof, session)
	}
	if !strings.Contains(output, "orca-init: command ok exit_code=0") {
		t.Fatalf("firecracker image rootfs command did not succeed: %+v", session)
	}
	if !strings.Contains(output, "orca-init: image-rootfs ok") {
		t.Fatalf("firecracker image rootfs did not report successful boot: %+v", session)
	}

	stats := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", session["node_url"], session["session_id"]))
	t.Logf("image rootfs boot got hits=%v misses=%v remote_fetches=%v zero_fills=%v dirty_chunks=%v",
		stats["cache_hits"], stats["cache_misses"], stats["remote_fetches"], stats["zero_fills"], stats["dirty_chunks"])
	stopSession(t, session)
}

func TestEnvAPIStartResumeNodeOneThenNodeTwo(t *testing.T) {
	if getenv("BASE_IMAGE_FIRECRACKER_TEST", "") != "true" {
		t.Skip("set BASE_IMAGE_FIRECRACKER_TEST=true on a Linux host with base-image-service, node-service, KVM, and Firecracker assets")
	}

	control := getenv("CONTROL_URL", "http://localhost:18080")
	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	missingImage := fmt.Sprintf("missing-env-image:%d", time.Now().UnixNano())
	t.Logf("checking startEnv returns an error for unbuilt image %s", missingImage)
	postJSONExpectStatus(t, control+"/startEnv", http.StatusNotFound, map[string]any{
		"image":   missingImage,
		"command": "true",
	})

	t.Log("building alpine:3.22 through control-service")
	var built map[string]any
	postJSON(t, control+"/buildImage", map[string]any{
		"image":          "alpine:3.22",
		"rootfs_size_mb": 512,
	}, &built)
	baseImageID := requireStringField(t, built, "base_image_id")
	t.Logf("built base image id=%s volume=%s snapshot=%s", baseImageID, built["volume_id"], built["snapshot_id"])

	payload := fmt.Sprintf("env-api-proof-%d", time.Now().UnixNano())
	startCommand := fmt.Sprintf("grep -qx alpine:3.22 /etc/orca-image-ref && echo %s > /orca-env-proof", payload)
	t.Log("starting env on node-1 from built alpine image and writing proof file")
	var started map[string]any
	postJSON(t, control+"/startEnv", map[string]any{
		"image":      "alpine:3.22",
		"command":    startCommand,
		"force_node": "node-1",
	}, &started)
	logEnvRunSummary(t, "node-1 write", started)
	envID := requireStringField(t, started, "env_id")
	_ = requireStringField(t, started, "latest_snapshot_id")
	if !strings.Contains(fmt.Sprint(started["firecracker_output"]), "orca-init: command ok exit_code=0") {
		t.Fatalf("startEnv command did not succeed: %+v", started)
	}
	logEnvSessionStats(t, "node-1 write", started)

	t.Log("resuming env on node-1 and verifying proof file from previous command")
	var readNode1 map[string]any
	postJSON(t, control+"/resumeEnv", map[string]any{
		"env_id":     envID,
		"command":    fmt.Sprintf("grep -qx %s /orca-env-proof", payload),
		"force_node": "node-1",
	}, &readNode1)
	logEnvRunSummary(t, "node-1 read", readNode1)
	if requireStringField(t, readNode1, "env_id") != envID {
		t.Fatalf("resumeEnv node-1 returned wrong env id: %+v", readNode1)
	}
	_ = requireStringField(t, readNode1, "latest_snapshot_id")
	if !strings.Contains(fmt.Sprint(readNode1["firecracker_output"]), "orca-init: command ok exit_code=0") {
		t.Fatalf("resumeEnv node-1 command did not succeed; persisted file was not readable: %+v", readNode1)
	}
	stats1 := logEnvSessionStats(t, "node-1 read", readNode1)
	if asInt(stats1["cache_hits"]) <= 0 {
		t.Fatalf("expected node-1 env read cache hits, got %+v", stats1)
	}

	t.Log("resuming env on node-2 and verifying proof file from node-1 snapshot")
	var readNode2 map[string]any
	postJSON(t, control+"/resumeEnv", map[string]any{
		"env_id":     envID,
		"command":    fmt.Sprintf("grep -qx %s /orca-env-proof", payload),
		"force_node": "node-2",
	}, &readNode2)
	logEnvRunSummary(t, "node-2 read", readNode2)
	if requireStringField(t, readNode2, "env_id") != envID {
		t.Fatalf("resumeEnv node-2 returned wrong env id: %+v", readNode2)
	}
	_ = requireStringField(t, readNode2, "latest_snapshot_id")
	if !strings.Contains(fmt.Sprint(readNode2["firecracker_output"]), "orca-init: command ok exit_code=0") {
		t.Fatalf("resumeEnv node-2 command did not succeed; persisted file was not readable: %+v", readNode2)
	}
	stats2 := logEnvSessionStats(t, "node-2 read", readNode2)
	if asInt(stats2["cache_misses"]) <= 0 || asInt(stats2["remote_fetches"]) <= 0 {
		t.Fatalf("expected node-2 env read cache misses and remote fetches, got %+v", stats2)
	}
}

func TestBaseImageServiceBuildImageAPI(t *testing.T) {
	if getenv("BASE_IMAGE_SERVICE_TEST", "") != "true" {
		t.Skip("set BASE_IMAGE_SERVICE_TEST=true to exercise base-image-service buildImage/getImageVolume")
	}

	baseImageURL := getenv("BASE_IMAGE_URL", "http://localhost:18083")
	t.Logf("waiting for base image service at %s", baseImageURL)
	waitFor(t, baseImageURL+"/healthz")

	t.Log("calling buildImage for alpine:3.22")
	built := buildBaseImage(t, baseImageURL, "alpine:3.22")
	baseImageID := requireStringField(t, built, "base_image_id")
	volumeID := requireStringField(t, built, "volume_id")
	snapshotID := requireStringField(t, built, "snapshot_id")

	t.Log("calling getImageVolume by base_image_id")
	gotByID := getJSON(t, baseImageURL+"/getImageVolume?base_image_id="+url.QueryEscape(baseImageID))
	t.Logf("getImageVolume by id response: %+v", gotByID)
	if gotByID["volume_id"] != volumeID || gotByID["snapshot_id"] != snapshotID {
		t.Fatalf("getImageVolume by id = %+v, want volume=%v snapshot=%v", gotByID, volumeID, snapshotID)
	}

	t.Log("calling getImageVolume by image ref")
	gotByImage := getJSON(t, baseImageURL+"/getImageVolume?image="+url.QueryEscape("alpine:3.22"))
	t.Logf("getImageVolume by image response: %+v", gotByImage)
	_ = requireStringField(t, gotByImage, "base_image_id")
	_ = requireStringField(t, gotByImage, "volume_id")
	_ = requireStringField(t, gotByImage, "snapshot_id")

	t.Logf("base image API roundtrip completed at %s", time.Now().Format(time.RFC3339))
}

func buildBaseImage(t *testing.T, baseImageURL, image string) map[string]any {
	t.Helper()
	var built map[string]any
	postJSON(t, baseImageURL+"/buildImage", map[string]any{
		"image":          image,
		"rootfs_size_mb": 512,
	}, &built)
	t.Logf("buildImage response: %+v", built)
	return built
}

func postJSONExpectStatus(t *testing.T, target string, wantStatus int, in any) []byte {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(target, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("POST %s got status=%s body=%s", target, resp.Status, body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s status=%d, want %d body=%s", target, resp.StatusCode, wantStatus, body)
	}
	return body
}

func logEnvSessionStats(t *testing.T, label string, session map[string]any) map[string]any {
	t.Helper()
	nodeURL := requireAnyStringField(t, session, "node_url")
	sessionID := requireAnyStringField(t, session, "session_id")
	stats := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", nodeURL, sessionID))
	t.Logf("%s env session got hits=%v misses=%v remote_fetches=%v zero_fills=%v dirty_chunks=%v",
		label, stats["cache_hits"], stats["cache_misses"], stats["remote_fetches"], stats["zero_fills"], stats["dirty_chunks"])
	return stats
}

func logEnvRunSummary(t *testing.T, label string, session map[string]any) {
	t.Helper()
	t.Logf("%s env=%s session=%s node=%s snapshot=%s output=%q",
		label,
		requireAnyStringField(t, session, "env_id"),
		requireAnyStringField(t, session, "session_id"),
		requireAnyStringField(t, session, "node_id"),
		requireAnyStringField(t, session, "latest_snapshot_id"),
		fmt.Sprint(session["firecracker_output"]),
	)
	logEnvFirecrackerTimings(t, label, session)
}

func logEnvFirecrackerTimings(t *testing.T, label string, session map[string]any) {
	t.Helper()
	raw := requireAnyStringField(t, session, "firecracker_timings")
	var timings []struct {
		Name       string `json:"name"`
		DurationMS int64  `json:"duration_ms"`
		Status     string `json:"status"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &timings); err != nil {
		t.Fatalf("decode firecracker timings for %s: %v body=%s", label, err, raw)
	}
	if len(timings) == 0 {
		t.Fatalf("expected firecracker timings for %s", label)
	}

	var lines strings.Builder
	lines.WriteString(label)
	lines.WriteString(" timings:\n")
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

func startImageRootFSFirecrackerSession(t *testing.T, control, volumeID, node, command string) map[string]string {
	t.Helper()
	var out map[string]string
	postJSON(t, control+"/sessions/start", map[string]any{
		"volume_id":           volumeID,
		"force_node":          node,
		"runtime":             "firecracker",
		"firecracker_mode":    "image-rootfs-smoke",
		"firecracker_payload": command,
	}, &out)
	if out["session_id"] == "" || out["node_url"] == "" || out["firecracker_output"] == "" {
		t.Fatalf("missing image-rootfs firecracker session fields: %+v", out)
	}
	if out["firecracker_work_dir"] == "" || out["firecracker_timings"] == "" {
		t.Fatalf("missing image-rootfs firecracker debug fields: %+v", out)
	}
	return out
}

func requireStringField(t *testing.T, values map[string]any, key string) string {
	t.Helper()
	value, ok := values[key].(string)
	if !ok || value == "" {
		t.Fatalf("response missing non-empty string field %q: %+v", key, values)
	}
	return value
}

func requireAnyStringField(t *testing.T, values map[string]any, key string) string {
	t.Helper()
	value, ok := values[key].(string)
	if !ok || value == "" {
		t.Fatalf("response missing non-empty string field %q: %+v", key, values)
	}
	return value
}
