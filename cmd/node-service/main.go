package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anton-k/orca-blocks/pkg/nbd"
	"github.com/anton-k/orca-blocks/pkg/storage"
)

type app struct {
	backend          *storage.Backend
	nbdExports       map[string]*nbd.StorageDevice
	nbdMu            sync.RWMutex
	nbdPublicAddr    string
	nbdCommitBatch   int
	nbdDefaultCommit bool
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

	a := &app{
		backend:          storage.NewBackend(nodeID, repo, store, cache),
		nbdExports:       map[string]*nbd.StorageDevice{},
		nbdPublicAddr:    getenv("NBD_PUBLIC_ADDR", ""),
		nbdCommitBatch:   int(mustInt64(getenv("NBD_COMMIT_BATCH_CHUNKS", "16"))),
		nbdDefaultCommit: getenv("NBD_COMMIT_ON_DISCONNECT", "false") == "true",
	}
	if nbdAddr := getenv("NBD_ADDR", ""); nbdAddr != "" {
		ln, err := net.Listen("tcp", nbdAddr)
		must(err)
		log.Printf("%s NBD listener on %s", nodeID, nbdAddr)
		go func() {
			server := &nbd.Server{
				Resolve: a.resolveNBDExport,
				Logger:  log.Default(),
			}
			if err := server.Serve(ctx, ln); err != nil {
				log.Printf("NBD listener stopped: %v", err)
			}
		}()
	}
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
		VolumeID           string `json:"volume_id"`
		Frontend           string `json:"frontend"`
		CommitOnDisconnect *bool  `json:"commit_on_disconnect"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	session, err := a.backend.StartSession(r.Context(), req.VolumeID)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]string{
		"session_id":       session.ID,
		"volume_id":        session.Volume.VolumeID,
		"node_id":          a.backend.NodeID,
		"base_snapshot_id": session.BaseSnapshotID,
	}
	if req.Frontend == "nbd" {
		if a.nbdPublicAddr == "" {
			_ = a.backend.Stop(session.ID)
			writeError(w, fmt.Errorf("NBD frontend is not enabled on this node"))
			return
		}
		commitOnDisconnect := a.nbdDefaultCommit
		if req.CommitOnDisconnect != nil {
			commitOnDisconnect = *req.CommitOnDisconnect
		}
		a.registerNBDExport(session.ID, &nbd.StorageDevice{
			Backend:            a.backend,
			SessionID:          session.ID,
			SizeBytes:          session.Volume.SizeBytes,
			CommitOnDisconnect: commitOnDisconnect,
			CommitOptions: storage.CommitOptions{
				UploadBatchChunks: a.nbdCommitBatch,
			},
			OnDisconnect: func() {
				a.unregisterNBDExport(session.ID)
			},
		})
		resp["nbd_addr"] = a.nbdPublicAddr
		resp["nbd_export_name"] = session.ID
		resp["nbd_commit_on_disconnect"] = strconv.FormatBool(commitOnDisconnect)
	}
	writeJSON(w, http.StatusCreated, resp)
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
	a.unregisterNBDExport(r.PathValue("id"))
	writeJSON(w, http.StatusCreated, snapshot)
}

func (a *app) stop(w http.ResponseWriter, r *http.Request) {
	a.unregisterNBDExport(r.PathValue("id"))
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

func (a *app) registerNBDExport(sessionID string, device *nbd.StorageDevice) {
	a.nbdMu.Lock()
	defer a.nbdMu.Unlock()
	a.nbdExports[sessionID] = device
}

func (a *app) unregisterNBDExport(sessionID string) {
	a.nbdMu.Lock()
	defer a.nbdMu.Unlock()
	delete(a.nbdExports, sessionID)
}

func (a *app) resolveNBDExport(exportName string) (nbd.Device, error) {
	a.nbdMu.RLock()
	defer a.nbdMu.RUnlock()
	device, ok := a.nbdExports[exportName]
	if !ok {
		return nil, fmt.Errorf("unknown NBD export %q", exportName)
	}
	return device, nil
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

func mustInt64(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	must(err)
	return n
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
