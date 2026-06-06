package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anton-k/orca-blocks/pkg/storage"
)

type app struct {
	backend *storage.Backend
}

func main() {
	ctx := context.Background()
	nodeID := getenv("NODE_ID", "node-1")

	repo, err := storage.NewPostgresRepo(ctx, mustenv("DATABASE_URL"))
	must(err)
	defer repo.Close()
	must(repo.Init(ctx))

	store, err := storage.NewS3Store(ctx,
		getenv("S3_ENDPOINT", "http://localhost:9000"),
		getenv("S3_REGION", "us-east-1"),
		getenv("S3_ACCESS_KEY", "minioadmin"),
		getenv("S3_SECRET_KEY", "minioadmin"),
		getenv("S3_BUCKET", "orca-blocks"),
	)
	must(err)

	cacheMax, err := strconv.ParseInt(getenv("CACHE_MAX_BYTES", "536870912"), 10, 64)
	must(err)
	cache, err := storage.NewLocalCache(getenv("CACHE_DIR", "/cache"), cacheMax)
	must(err)

	a := &app{backend: storage.NewBackend(nodeID, repo, store, cache)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true", "node_id": nodeID})
	})
	mux.HandleFunc("POST /volumes/create", a.createVolume)
	mux.HandleFunc("POST /sessions/start", a.startSession)
	mux.HandleFunc("GET /sessions/{id}/read", a.read)
	mux.HandleFunc("PUT /sessions/{id}/write", a.write)
	mux.HandleFunc("POST /sessions/{id}/commit", a.commit)
	mux.HandleFunc("POST /sessions/{id}/stop", a.stop)
	mux.HandleFunc("GET /sessions/{id}/stats", a.stats)

	addr := ":" + getenv("PORT", "8080")
	log.Printf("%s listening on %s", nodeID, addr)
	must(http.ListenAndServe(addr, logRequests(mux)))
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
	volume, err := a.backend.CreateVolume(r.Context(), req.VolumeID, req.SizeBytes, req.ChunkSize)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, volume)
}

func (a *app) startSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VolumeID string `json:"volume_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	session, err := a.backend.StartSession(r.Context(), req.VolumeID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"session_id":       session.ID,
		"volume_id":        session.Volume.VolumeID,
		"node_id":          a.backend.NodeID,
		"base_snapshot_id": session.BaseSnapshotID,
	})
}

func (a *app) read(w http.ResponseWriter, r *http.Request) {
	offset, ok := queryInt(w, r, "offset")
	if !ok {
		return
	}
	length, ok := queryInt(w, r, "length")
	if !ok {
		return
	}
	data, err := a.backend.Read(r.Context(), r.PathValue("id"), offset, length)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (a *app) write(w http.ResponseWriter, r *http.Request) {
	offset, ok := queryInt(w, r, "offset")
	if !ok {
		return
	}
	defer r.Body.Close()
	data, err := readLimited(r, 64*1024*1024)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.backend.Write(r.Context(), r.PathValue("id"), offset, data); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"written": len(data)})
}

func (a *app) commit(w http.ResponseWriter, r *http.Request) {
	snapshot, err := a.backend.Commit(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshot)
}

func (a *app) stop(w http.ResponseWriter, r *http.Request) {
	if err := a.backend.Stop(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"stopped": r.PathValue("id")})
}

func (a *app) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := a.backend.Stats(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func queryInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		http.Error(w, "missing query parameter "+name, http.StatusBadRequest)
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		http.Error(w, "invalid query parameter "+name, http.StatusBadRequest)
		return 0, false
	}
	return n, true
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
	} else if strings.Contains(err.Error(), "exceeds") || strings.Contains(err.Error(), "invalid") {
		status = http.StatusBadRequest
	}
	http.Error(w, err.Error(), status)
}

func readLimited(r *http.Request, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("request body exceeds %d bytes", max)
	}
	return data, nil
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
