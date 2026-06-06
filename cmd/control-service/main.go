package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/anton-k/orca-blocks/pkg/storage"
)

type node struct {
	ID        string `json:"node_id"`
	URL       string `json:"url"`
	PublicURL string `json:"public_url"`
}

type app struct {
	repo   storage.Repository
	nodes  map[string]node
	client *http.Client
}

func main() {
	ctx := context.Background()
	repo, err := storage.NewPostgresRepo(ctx, mustenv("DATABASE_URL"))
	must(err)
	defer repo.Close()
	must(repo.Init(ctx))

	a := &app{
		repo: repo,
		nodes: map[string]node{
			"node-1": {
				ID:        "node-1",
				URL:       getenv("NODE_1_URL", "http://localhost:8081"),
				PublicURL: getenv("NODE_1_PUBLIC_URL", getenv("NODE_1_URL", "http://localhost:8081")),
			},
			"node-2": {
				ID:        "node-2",
				URL:       getenv("NODE_2_URL", "http://localhost:8082"),
				PublicURL: getenv("NODE_2_PUBLIC_URL", getenv("NODE_2_URL", "http://localhost:8082")),
			},
		},
		client: &http.Client{Timeout: 5 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("GET /nodes", a.listNodes)
	mux.HandleFunc("POST /volumes/create", a.createVolume)
	mux.HandleFunc("GET /volumes/{id}", a.getVolume)
	mux.HandleFunc("POST /sessions/start", a.startSession)
	mux.HandleFunc("POST /scheduler/force", a.forceLastNode)

	addr := ":" + getenv("PORT", "8080")
	log.Printf("control-service listening on %s", addr)
	must(http.ListenAndServe(addr, logRequests(mux)))
}

func (a *app) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes := make([]node, 0, len(a.nodes))
	for _, n := range a.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	writeJSON(w, http.StatusOK, nodes)
}

func (a *app) createVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VolumeID  string `json:"volume_id"`
		SizeBytes int64  `json:"size_bytes"`
		ChunkSize int64  `json:"chunk_size"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.VolumeID == "" {
		req.VolumeID = "vol-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	volume, err := a.repo.CreateVolume(r.Context(), storage.Volume{
		VolumeID:  req.VolumeID,
		SizeBytes: req.SizeBytes,
		ChunkSize: req.ChunkSize,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, volume)
}

func (a *app) getVolume(w http.ResponseWriter, r *http.Request) {
	volume, err := a.repo.GetVolume(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, volume)
}

func (a *app) startSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Runtime            string `json:"runtime"`
		VolumeID           string `json:"volume_id"`
		ForceNode          string `json:"force_node"`
		Frontend           string `json:"frontend"`
		CommitOnDisconnect *bool  `json:"commit_on_disconnect"`
		Format             bool   `json:"format"`
		FSType             string `json:"fs_type"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	volume, err := a.repo.GetVolume(r.Context(), req.VolumeID)
	if err != nil {
		writeError(w, err)
		return
	}
	selected, err := a.selectNode(r.Context(), volume, req.ForceNode)
	if err != nil {
		writeError(w, err)
		return
	}

	startReq := map[string]any{"volume_id": req.VolumeID}
	if req.Runtime != "" {
		startReq["runtime"] = req.Runtime
	}
	if req.Frontend != "" {
		startReq["frontend"] = req.Frontend
	}
	if req.CommitOnDisconnect != nil {
		startReq["commit_on_disconnect"] = *req.CommitOnDisconnect
	}
	if req.Format {
		startReq["format"] = req.Format
	}
	if req.FSType != "" {
		startReq["fs_type"] = req.FSType
	}
	body, _ := json.Marshal(startReq)
	resp, err := a.client.Post(selected.URL+"/sessions/start", "application/json", bytes.NewReader(body))
	if err != nil {
		writeError(w, err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		http.Error(w, string(raw), resp.StatusCode)
		return
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		writeError(w, err)
		return
	}
	out["node_url"] = selected.PublicURL
	writeJSON(w, http.StatusCreated, out)
}

func (a *app) forceLastNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VolumeID string `json:"volume_id"`
		NodeID   string `json:"node_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, ok := a.nodes[req.NodeID]; !ok {
		http.Error(w, "unknown node", http.StatusBadRequest)
		return
	}
	if err := a.repo.UpdateLastNode(r.Context(), req.VolumeID, req.NodeID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"volume_id": req.VolumeID, "last_node": req.NodeID})
}

func (a *app) selectNode(ctx context.Context, volume storage.Volume, forced string) (node, error) {
	if forced != "" {
		n, ok := a.nodes[forced]
		if !ok {
			return node{}, fmt.Errorf("unknown forced node %q", forced)
		}
		if !a.available(ctx, n) {
			return node{}, fmt.Errorf("forced node %q is unavailable", forced)
		}
		return n, nil
	}
	if volume.LastNode != "" {
		if n, ok := a.nodes[volume.LastNode]; ok && a.available(ctx, n) {
			return n, nil
		}
	}
	ids := make([]string, 0, len(a.nodes))
	for id := range a.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := a.nodes[id]
		if a.available(ctx, n) {
			return n, nil
		}
	}
	return node{}, fmt.Errorf("no nodes available")
}

func (a *app) available(ctx context.Context, n node) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.URL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, storage.ErrNotFound) {
		status = http.StatusNotFound
	} else if strings.Contains(err.Error(), "unknown") || strings.Contains(err.Error(), "unavailable") {
		status = http.StatusBadRequest
	}
	http.Error(w, err.Error(), status)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.String(), time.Since(start))
	})
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func mustenv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env %s", k)
	}
	return v
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
