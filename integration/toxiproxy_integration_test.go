//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func TestMinIOProxyThrottleStillAllowsColdNodeFetch(t *testing.T) {
	control := getenv("CONTROL_URL", "http://localhost:18080")
	toxiproxy := getenv("TOXIPROXY_URL", "http://localhost:18474")

	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")
	t.Logf("waiting for toxiproxy API at %s", toxiproxy)
	waitFor(t, toxiproxy+"/proxies")

	volumeID := fmt.Sprintf("toxiproxy-itest-%d", time.Now().UnixNano())
	var volume map[string]any
	t.Logf("creating volume %s with enough data to make a 10 Mbit/s cap visible", volumeID)
	postJSON(t, control+"/volumes/create", map[string]any{
		"volume_id":  volumeID,
		"size_bytes": 4 * 1024 * 1024,
		"chunk_size": 256 * 1024,
	}, &volume)

	t.Log("creating session on node-1")
	start1 := startSession(t, control, volumeID, "node-1")
	data := bytes.Repeat([]byte("toxiproxy-minio-throttle-cold-fetch-"+volumeID+"\n"), 32768)
	t.Logf("writing %d bytes at offset 0 on node-1", len(data))
	putRaw(t, fmt.Sprintf("%s/sessions/%s/write?offset=0", start1["node_url"], start1["session_id"]), data)
	var snapshot map[string]any
	t.Log("committing node-1 session before enabling MinIO throttling")
	postJSON(t, fmt.Sprintf("%s/sessions/%s/commit", start1["node_url"], start1["session_id"]), nil, &snapshot)
	t.Logf("committed snapshot response: %+v", snapshot)

	t.Log("creating cold session on node-2")
	start2 := startSession(t, control, volumeID, "node-2")

	t.Log("reading from node-2; expecting local misses, remote fetches, and Compose-configured MinIO throttling")
	started := time.Now()
	got := getRaw(t, fmt.Sprintf("%s/sessions/%s/read?offset=0&length=%d", start2["node_url"], start2["session_id"], len(data)))
	elapsed := time.Since(started)
	if !bytes.Equal(got, data) {
		t.Fatalf("node-2 read through throttled MinIO returned %d bytes, want %d", len(got), len(data))
	}
	stats := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", start2["node_url"], start2["session_id"]))
	t.Logf("node-2 through toxiproxy got hits=%v misses=%v remote_fetches=%v zero_fills=%v elapsed=%s", stats["cache_hits"], stats["cache_misses"], stats["remote_fetches"], stats["zero_fills"], elapsed)
	if asInt(stats["cache_misses"]) <= 0 || asInt(stats["remote_fetches"]) <= 0 {
		t.Fatalf("expected node-2 cache misses and remote fetches through toxiproxy, got %+v", stats)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("expected throttled MinIO read to take at least 500ms, got %s", elapsed)
	}
}
