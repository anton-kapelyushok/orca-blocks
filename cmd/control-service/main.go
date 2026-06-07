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
	"github.com/google/uuid"
)

type node struct {
	ID        string `json:"node_id"`
	URL       string `json:"url"`
	PublicURL string `json:"public_url"`
}

type app struct {
	repo         storage.Repository
	nodes        map[string]node
	client       *http.Client
	baseImageURL string
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
		client:       &http.Client{Timeout: 5 * time.Minute},
		baseImageURL: getenv("BASE_IMAGE_URL", "http://localhost:18083"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("GET /nodes", a.listNodes)
	mux.HandleFunc("POST /volumes/create", a.createVolume)
	mux.HandleFunc("GET /volumes/{id}", a.getVolume)
	mux.HandleFunc("GET /images", a.listImages)
	mux.HandleFunc("POST /buildImage", a.buildImage)
	mux.HandleFunc("GET /getImageVolume", a.getImageVolume)
	mux.HandleFunc("GET /envs/{id}", a.getEnv)
	mux.HandleFunc("POST /startEnv", a.startEnv)
	mux.HandleFunc("POST /resumeEnv", a.resumeEnv)
	mux.HandleFunc("GET /tty/{node}/{session}/output", a.ttyOutput)
	mux.HandleFunc("POST /tty/{node}/{session}/input", a.ttyInput)
	mux.HandleFunc("POST /tty/{node}/{session}/stop", a.ttyStop)
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
		Runtime            string   `json:"runtime"`
		VolumeID           string   `json:"volume_id"`
		ForceNode          string   `json:"force_node"`
		Frontend           string   `json:"frontend"`
		CommitOnDisconnect *bool    `json:"commit_on_disconnect"`
		Format             bool     `json:"format"`
		FSType             string   `json:"fs_type"`
		FirecrackerMode    string   `json:"firecracker_mode"`
		FirecrackerPayload string   `json:"firecracker_payload"`
		VCPUCount          int      `json:"cpu_count"`
		MemoryMiB          int      `json:"memory_mib"`
		ImageEnv           []string `json:"image_env"`
		ImageWorkingDir    string   `json:"image_working_dir"`
		CommitAfterRun     *bool    `json:"commit_after_run"`
		SaveMemory         bool     `json:"save_memory_snapshot"`
		RestoreMemory      string   `json:"restore_memory_snapshot_path"`
		RestoreVMState     string   `json:"restore_vmstate_snapshot_path"`
		RestoreDevice      string   `json:"restore_firecracker_device"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	volume, err := a.repo.GetVolume(r.Context(), req.VolumeID)
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
	if req.FirecrackerMode != "" {
		startReq["firecracker_mode"] = req.FirecrackerMode
	}
	if req.FirecrackerPayload != "" {
		startReq["firecracker_payload"] = req.FirecrackerPayload
	}
	if req.VCPUCount > 0 {
		startReq["cpu_count"] = req.VCPUCount
	}
	if req.MemoryMiB > 0 {
		startReq["memory_mib"] = req.MemoryMiB
	}
	if len(req.ImageEnv) > 0 {
		startReq["image_env"] = req.ImageEnv
	}
	if req.ImageWorkingDir != "" {
		startReq["image_working_dir"] = req.ImageWorkingDir
	}
	if req.CommitAfterRun != nil {
		startReq["commit_after_run"] = *req.CommitAfterRun
	}
	if req.SaveMemory {
		startReq["save_memory_snapshot"] = req.SaveMemory
	}
	if req.RestoreMemory != "" {
		startReq["restore_memory_snapshot_path"] = req.RestoreMemory
	}
	if req.RestoreVMState != "" {
		startReq["restore_vmstate_snapshot_path"] = req.RestoreVMState
	}
	if req.RestoreDevice != "" {
		startReq["restore_firecracker_device"] = req.RestoreDevice
	}
	out, err := a.startNodeSession(r.Context(), volume, req.ForceNode, startReq)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *app) buildImage(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, err)
		return
	}
	status, out, err := a.postJSON(r.Context(), a.baseImageURL+"/buildImage", raw)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, out)
}

func (a *app) listImages(w http.ResponseWriter, r *http.Request) {
	images, err := a.repo.ListBaseImages(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, images)
}

func (a *app) getImageVolume(w http.ResponseWriter, r *http.Request) {
	baseImageID := r.URL.Query().Get("base_image_id")
	imageRef := r.URL.Query().Get("image")
	var (
		image storage.BaseImage
		err   error
	)
	switch {
	case baseImageID != "":
		image, err = a.repo.GetBaseImage(r.Context(), baseImageID)
	case imageRef != "":
		image, err = a.repo.GetBaseImageByRef(r.Context(), imageRef)
	default:
		http.Error(w, "base_image_id or image query parameter is required", http.StatusBadRequest)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, image)
}

func (a *app) getEnv(w http.ResponseWriter, r *http.Request) {
	env, err := a.repo.GetEnv(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

type envRunRequest struct {
	BaseImageID string `json:"base_image_id"`
	Image       string `json:"image"`
	Command     string `json:"command"`
	Cmd         string `json:"cmd"`
	ForceNode   string `json:"force_node"`
	VCPUCount   int    `json:"cpu_count"`
	MemoryMiB   int    `json:"memory_mib"`
	TTY         bool   `json:"tty"`
}

func (a *app) startEnv(w http.ResponseWriter, r *http.Request) {
	var req envRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	command := strings.TrimSpace(firstNonEmpty(req.Command, req.Cmd))
	base, err := a.resolveBaseImage(r.Context(), req.BaseImageID, req.Image)
	if err != nil {
		writeError(w, err)
		return
	}
	if command == "" && !req.TTY {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	baseVolume, err := a.repo.GetVolume(r.Context(), base.VolumeID)
	if err != nil {
		writeError(w, err)
		return
	}
	envID := "env-" + uuid.NewString()
	envVolumeID := "env-vol-" + uuid.NewString()
	if _, err := a.repo.CreateVolume(r.Context(), storage.Volume{
		VolumeID:         envVolumeID,
		SizeBytes:        baseVolume.SizeBytes,
		ChunkSize:        baseVolume.ChunkSize,
		LatestSnapshotID: base.SnapshotID,
	}); err != nil {
		writeError(w, err)
		return
	}
	env, err := a.repo.CreateEnv(r.Context(), storage.Env{
		EnvID:            envID,
		BaseImageID:      base.BaseImageID,
		ImageRef:         base.ImageRef,
		VolumeID:         envVolumeID,
		LatestSnapshotID: base.SnapshotID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out, err := a.runEnvCommand(r.Context(), env, req.ForceNode, command, req.TTY, req.VCPUCount, req.MemoryMiB)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *app) resumeEnv(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnvID     string `json:"env_id"`
		Command   string `json:"command"`
		Cmd       string `json:"cmd"`
		ForceNode string `json:"force_node"`
		VCPUCount int    `json:"cpu_count"`
		MemoryMiB int    `json:"memory_mib"`
		TTY       bool   `json:"tty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	command := strings.TrimSpace(firstNonEmpty(req.Command, req.Cmd))
	if req.EnvID == "" {
		http.Error(w, "env_id is required", http.StatusBadRequest)
		return
	}
	env, err := a.repo.GetEnv(r.Context(), req.EnvID)
	if err != nil {
		writeError(w, err)
		return
	}
	if command == "" && !req.TTY {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	out, err := a.runEnvCommand(r.Context(), env, req.ForceNode, command, req.TTY, req.VCPUCount, req.MemoryMiB)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *app) resolveBaseImage(ctx context.Context, baseImageID, imageRef string) (storage.BaseImage, error) {
	if baseImageID != "" {
		return a.repo.GetBaseImage(ctx, baseImageID)
	}
	if imageRef == "" {
		return storage.BaseImage{}, fmt.Errorf("base_image_id or image is required")
	}
	image, err := a.repo.GetBaseImageByRef(ctx, imageRef)
	if err == nil {
		return image, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return storage.BaseImage{}, err
	}
	return storage.BaseImage{}, fmt.Errorf("base image %q is not built; call buildImage first: %w", imageRef, storage.ErrNotFound)
}

func (a *app) runEnvCommand(ctx context.Context, env storage.Env, forcedNode, command string, tty bool, vcpuCount, memoryMiB int) (map[string]any, error) {
	volume, err := a.repo.GetVolume(ctx, env.VolumeID)
	if err != nil {
		return nil, err
	}
	mode := "image-rootfs-run"
	commitAfterRun := true
	if tty {
		mode = "image-rootfs-tty"
		commitAfterRun = false
	}
	startReq := map[string]any{
		"volume_id":           env.VolumeID,
		"runtime":             "firecracker",
		"firecracker_mode":    mode,
		"firecracker_payload": command,
		"commit_after_run":    commitAfterRun,
	}
	if base, err := a.repo.GetBaseImage(ctx, env.BaseImageID); err == nil {
		if len(base.Env) > 0 {
			startReq["image_env"] = base.Env
		}
		if base.WorkingDir != "" {
			startReq["image_working_dir"] = base.WorkingDir
		}
		if base.User != "" {
			startReq["image_user"] = base.User
		}
	}
	if vcpuCount > 0 {
		startReq["cpu_count"] = vcpuCount
	}
	if memoryMiB > 0 {
		startReq["memory_mib"] = memoryMiB
	}
	out, err := a.startNodeSession(ctx, volume, forcedNode, startReq)
	if err != nil {
		return nil, err
	}
	if snapshotID, ok := out["snapshot_id"].(string); ok && snapshotID != "" {
		if err := a.repo.UpdateEnvSnapshot(ctx, env.EnvID, snapshotID); err != nil {
			return nil, err
		}
		env.LatestSnapshotID = snapshotID
	}
	out["env_id"] = env.EnvID
	out["base_image_id"] = env.BaseImageID
	out["image_ref"] = env.ImageRef
	out["env_volume_id"] = env.VolumeID
	out["latest_snapshot_id"] = env.LatestSnapshotID
	if vcpuCount > 0 {
		out["cpu_count"] = float64(vcpuCount)
	}
	if memoryMiB > 0 {
		out["memory_mib"] = float64(memoryMiB)
	}
	if tty {
		out["tty"] = true
	}
	return out, nil
}

func (a *app) ttyOutput(w http.ResponseWriter, r *http.Request) {
	target, ok := a.ttyTarget(w, r)
	if !ok {
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target+"/output?offset="+r.URL.Query().Get("offset"), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	a.proxyJSON(w, req)
}

func (a *app) ttyInput(w http.ResponseWriter, r *http.Request) {
	target, ok := a.ttyTarget(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeError(w, err)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target+"/input", bytes.NewReader(body))
	if err != nil {
		writeError(w, err)
		return
	}
	req.Header.Set("Content-Type", "text/plain")
	a.proxyJSON(w, req)
}

func (a *app) ttyStop(w http.ResponseWriter, r *http.Request) {
	target, ok := a.ttyTarget(w, r)
	if !ok {
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target+"/stop", nil)
	if err != nil {
		writeError(w, err)
		return
	}
	status, out, err := a.doJSON(req)
	if err != nil {
		writeError(w, err)
		return
	}
	if status >= 300 {
		writeJSON(w, status, out)
		return
	}
	if envID := r.URL.Query().Get("env_id"); envID != "" {
		if snapshotID, ok := out["snapshot_id"].(string); ok && snapshotID != "" {
			if err := a.repo.UpdateEnvSnapshot(r.Context(), envID, snapshotID); err != nil {
				writeError(w, err)
				return
			}
			out["latest_snapshot_id"] = snapshotID
		}
	}
	writeJSON(w, status, out)
}

func (a *app) ttyTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	n, ok := a.nodes[r.PathValue("node")]
	if !ok {
		http.Error(w, "unknown node", http.StatusBadRequest)
		return "", false
	}
	sessionID := r.PathValue("session")
	if sessionID == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return "", false
	}
	return n.URL + "/sessions/" + sessionID + "/tty", true
}

func (a *app) proxyJSON(w http.ResponseWriter, req *http.Request) {
	status, out, err := a.doJSON(req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, out)
}

func (a *app) doJSON(req *http.Request) (int, map[string]any, error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	out := map[string]any{}
	if len(body) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(body, &out); err != nil {
			return resp.StatusCode, nil, err
		}
	} else if len(body) > 0 {
		out["error"] = strings.TrimSpace(string(body))
	}
	return resp.StatusCode, out, nil
}

func (a *app) startNodeSession(ctx context.Context, volume storage.Volume, forcedNode string, startReq map[string]any) (map[string]any, error) {
	selected, err := a.selectNode(ctx, volume, forcedNode)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(startReq)
	if err != nil {
		return nil, err
	}
	status, out, err := a.postJSON(ctx, selected.URL+"/sessions/start", raw)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("node %s start session failed with status %d: %+v", selected.ID, status, out)
	}
	out["node_url"] = selected.PublicURL
	return out, nil
}

func (a *app) postJSON(ctx context.Context, target string, raw []byte) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	out := map[string]any{}
	if len(body) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(body, &out); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("decode %s response: %w body=%s", target, err, body)
		}
	} else if len(body) > 0 {
		out["error"] = strings.TrimSpace(string(body))
	}
	return resp.StatusCode, out, nil
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
