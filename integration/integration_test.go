//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestCrossNodeResumeThroughCompose(t *testing.T) {
	control := getenv("CONTROL_URL", "http://localhost:18080")
	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	volumeID := fmt.Sprintf("itest-%d", time.Now().UnixNano())
	var volume map[string]any
	t.Logf("creating volume %s", volumeID)
	postJSON(t, control+"/volumes/create", map[string]any{
		"volume_id":  volumeID,
		"size_bytes": 1024,
		"chunk_size": 16,
	}, &volume)
	t.Logf("created volume response: %+v", volume)

	t.Log("creating session on node-1")
	start1 := startSession(t, control, volumeID, "node-1")
	t.Logf("node-1 session=%s node_url=%s", start1["session_id"], start1["node_url"])
	data := []byte("integration-cross-node-payload-" + volumeID)
	t.Logf("writing %d bytes at offset 10", len(data))
	putRaw(t, fmt.Sprintf("%s/sessions/%s/write?offset=10", start1["node_url"], start1["session_id"]), data)
	var snapshot map[string]any
	t.Log("committing node-1 session")
	postJSON(t, fmt.Sprintf("%s/sessions/%s/commit", start1["node_url"], start1["session_id"]), nil, &snapshot)
	t.Logf("committed snapshot response: %+v", snapshot)

	t.Log("resuming on node-1; expecting local cache hits")
	resume1 := startSession(t, control, volumeID, "node-1")
	read1 := getRaw(t, fmt.Sprintf("%s/sessions/%s/read?offset=10&length=%d", resume1["node_url"], resume1["session_id"], len(data)))
	if !bytes.Equal(read1, data) {
		t.Fatalf("node-1 read = %q, want %q", read1, data)
	}
	stats1 := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", resume1["node_url"], resume1["session_id"]))
	t.Logf("node-1 got hits=%v misses=%v remote_fetches=%v zero_fills=%v", stats1["cache_hits"], stats1["cache_misses"], stats1["remote_fetches"], stats1["zero_fills"])
	if asInt(stats1["cache_hits"]) <= 0 {
		t.Fatalf("expected node-1 cache hits, got %+v", stats1)
	}

	t.Log("resuming on node-2; expecting local cache misses and remote S3 fetches")
	resume2 := startSession(t, control, volumeID, "node-2")
	read2 := getRaw(t, fmt.Sprintf("%s/sessions/%s/read?offset=10&length=%d", resume2["node_url"], resume2["session_id"], len(data)))
	if !bytes.Equal(read2, data) {
		t.Fatalf("node-2 read = %q, want %q", read2, data)
	}
	stats2 := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", resume2["node_url"], resume2["session_id"]))
	t.Logf("node-2 got hits=%v misses=%v remote_fetches=%v zero_fills=%v", stats2["cache_hits"], stats2["cache_misses"], stats2["remote_fetches"], stats2["zero_fills"])
	if asInt(stats2["cache_misses"]) <= 0 || asInt(stats2["remote_fetches"]) <= 0 {
		t.Fatalf("expected node-2 misses and remote fetches, got %+v", stats2)
	}
}

func startSession(t *testing.T, control, volumeID, node string) map[string]string {
	t.Helper()
	var out map[string]string
	postJSON(t, control+"/sessions/start", map[string]string{
		"volume_id":  volumeID,
		"force_node": node,
		"runtime":    "http-block",
	}, &out)
	return out
}

func waitFor(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %s", url)
}

func postJSON(t *testing.T, url string, in any, out any) {
	t.Helper()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	resp, err := http.Post(url, "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("POST %s failed: %s %s", url, resp.Status, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s: %v body=%s", url, err, raw)
		}
	}
}

func putRaw(t *testing.T, url string, data []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("PUT %s failed: %s %s", url, resp.Status, raw)
	}
}

func getRaw(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("GET %s failed: %s %s", url, resp.Status, raw)
	}
	return raw
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	raw := getRaw(t, url)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v body=%s", url, err, raw)
	}
	return out
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
