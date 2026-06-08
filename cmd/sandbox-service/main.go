package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type app struct {
	controlURL string
	client     *http.Client
	tmpl       *template.Template
	mu         sync.Mutex
	sessions   map[string]envSession
}

type baseImage struct {
	BaseImageID     string `json:"base_image_id"`
	ImageRef        string `json:"image_ref"`
	ImageDigest     string `json:"image_digest"`
	VolumeID        string `json:"volume_id"`
	SnapshotID      string `json:"snapshot_id"`
	RootFSSizeBytes int64  `json:"rootfs_size_bytes"`
}

type envRecord struct {
	EnvID            string `json:"env_id"`
	BaseImageID      string `json:"base_image_id"`
	ImageRef         string `json:"image_ref"`
	VolumeID         string `json:"volume_id"`
	LatestSnapshotID string `json:"latest_snapshot_id"`
}

type nodeInfo struct {
	NodeID string `json:"node_id"`
	URL    string `json:"url"`
}

type pageData struct {
	Images         []baseImage
	Envs           []envRecord
	ActiveSessions []envSession
	Nodes          []nodeInfo
	Builds         []buildJobStatus
	BuildJob       *buildJobStatus
	Result         *runResult
	Build          *buildResult
	Notice         string
	Error          string
	Now            string
}

type buildResult struct {
	BaseImageID     string
	ImageRef        string
	ImageDigest     string
	VolumeID        string
	SnapshotID      string
	RootFSSizeBytes int64
	DurationMS      int64
	Timings         []timing
	RawTimings      string
}

type buildJobStatus struct {
	ID           string         `json:"id"`
	Image        string         `json:"image"`
	RootFSSizeMB int64          `json:"rootfs_size_mb"`
	State        string         `json:"state"`
	LastLine     string         `json:"last_line"`
	StartedAt    string         `json:"started_at"`
	FinishedAt   string         `json:"finished_at"`
	Error        string         `json:"error"`
	Result       map[string]any `json:"result"`
	Timings      []timing       `json:"build_timings"`
}

type runResult struct {
	Title        string
	EnvID        string
	SessionID    string
	NodeID       string
	ImageRef     string
	SnapshotID   string
	Stdout       string
	Stderr       string
	Console      string
	Timings      []timing
	RawTimings   string
	CacheSummary string
}

type envSession struct {
	EnvID      string
	SessionID  string
	NodeID     string
	ImageRef   string
	SnapshotID string
	Timings    []timing
	Closed     bool
}

type envPageData struct {
	EnvID       string
	SessionID   string
	NodeID      string
	CPUCount    string
	MemoryMiB   string
	ImageRef    string
	SnapshotID  string
	KeyTimings  []timing
	AllTimings  []timing
	RawTimings  string
	HasRunStats bool
}

type buildsPageData struct {
	Builds []buildJobStatus
	Error  string
	Now    string
}

type timing struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"`
	Error      string `json:"error"`
}

func main() {
	a := &app{
		controlURL: strings.TrimRight(getenv("CONTROL_URL", "http://localhost:18080"), "/"),
		client:     &http.Client{Timeout: 10 * time.Minute},
		tmpl:       template.Must(template.New("page").Parse(pageHTML)),
		sessions:   map[string]envSession{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("GET /", a.index)
	mux.HandleFunc("GET /builds", a.buildsPage)
	mux.HandleFunc("GET /env", a.env)
	mux.HandleFunc("GET /terminal", a.terminal)
	mux.HandleFunc("GET /tty/output", a.ttyOutput)
	mux.HandleFunc("POST /tty/input", a.ttyInput)
	mux.HandleFunc("POST /tty/stop", a.ttyStop)
	mux.HandleFunc("POST /build", a.buildImage)
	mux.HandleFunc("GET /build/status", a.buildStatus)
	mux.HandleFunc("POST /start", a.startEnv)
	mux.HandleFunc("POST /start-tty", a.startTTY)
	mux.HandleFunc("POST /resume", a.resumeEnv)
	mux.HandleFunc("POST /resume-tty", a.resumeTTY)

	addr := ":" + getenv("PORT", "8080")
	log.Printf("sandbox-service listening on %s control=%s", addr, a.controlURL)
	log.Fatal(http.ListenAndServe(addr, logRequests(mux)))
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, pageData{})
}

func (a *app) buildsPage(w http.ResponseWriter, r *http.Request) {
	data := buildsPageData{Now: time.Now().Format("15:04:05")}
	builds, err := a.listBuilds(r.Context())
	if err != nil {
		data.Error = err.Error()
	} else {
		data.Builds = builds
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := template.Must(template.New("builds").Parse(buildsHTML)).Execute(w, data); err != nil {
		log.Printf("render builds: %v", err)
	}
}

func (a *app) buildImage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	req := map[string]any{"image": strings.TrimSpace(r.FormValue("image"))}
	if req["image"] == "" {
		a.render(w, r, pageData{Error: "image is required"})
		return
	}
	if size := strings.TrimSpace(r.FormValue("rootfs_size_mb")); size != "" {
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			a.render(w, r, pageData{Error: "rootfs size must be a number"})
			return
		}
		req["rootfs_size_mb"] = n
	}
	out, err := a.postJSON(r.Context(), "/builds", req)
	if err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	jobID := anyString(out["id"])
	if jobID == "" {
		a.render(w, r, pageData{Error: "build started without a job id"})
		return
	}
	http.Redirect(w, r, "/?build_job="+url.QueryEscape(jobID), http.StatusSeeOther)
}

func (a *app) buildStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("id")
	if jobID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	raw, err := a.get(r.Context(), "/builds/"+url.PathEscape(jobID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (a *app) startEnv(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	req := envRequest(r)
	req["tty"] = true
	if req["image"] == "" && req["base_image_id"] == "" {
		a.render(w, r, pageData{Error: "image or base image id is required"})
		return
	}
	out, err := a.postJSON(r.Context(), "/startEnv", req)
	if err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	a.rememberEnvSession(out)
	http.Redirect(w, r, envURL(out), http.StatusSeeOther)
}

func (a *app) startTTY(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	req := envRequest(r)
	delete(req, "command")
	req["tty"] = true
	if req["image"] == "" && req["base_image_id"] == "" {
		a.render(w, r, pageData{Error: "image or base image id is required"})
		return
	}
	out, err := a.postJSON(r.Context(), "/startEnv", req)
	if err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	a.rememberEnvSession(out)
	http.Redirect(w, r, terminalURL(out), http.StatusSeeOther)
}

func (a *app) resumeEnv(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	req := envRequest(r)
	req["tty"] = true
	if req["env_id"] == "" {
		a.render(w, r, pageData{Error: "env id is required"})
		return
	}
	out, err := a.postJSON(r.Context(), "/resumeEnv", req)
	if err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	a.rememberEnvSession(out)
	http.Redirect(w, r, envURL(out), http.StatusSeeOther)
}

func (a *app) resumeTTY(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	req := envRequest(r)
	delete(req, "command")
	req["tty"] = true
	if req["env_id"] == "" {
		a.render(w, r, pageData{Error: "env id is required"})
		return
	}
	out, err := a.postJSON(r.Context(), "/resumeEnv", req)
	if err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	a.rememberEnvSession(out)
	http.Redirect(w, r, terminalURL(out), http.StatusSeeOther)
}

func (a *app) terminal(w http.ResponseWriter, r *http.Request) {
	a.env(w, r)
}

func (a *app) env(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	sessionID := r.URL.Query().Get("session_id")
	stored := a.envSession(sessionID)
	data := envPageData{
		EnvID:       r.URL.Query().Get("env_id"),
		SessionID:   sessionID,
		NodeID:      r.URL.Query().Get("node_id"),
		CPUCount:    withDefault(r.URL.Query().Get("cpu_count"), "1"),
		MemoryMiB:   withDefault(r.URL.Query().Get("memory_mib"), "128"),
		ImageRef:    stored.ImageRef,
		SnapshotID:  stored.SnapshotID,
		KeyTimings:  keyTimings(stored.Timings),
		AllTimings:  stored.Timings,
		HasRunStats: len(stored.Timings) > 0,
	}
	if err := template.Must(template.New("env").Parse(envHTML)).Execute(w, data); err != nil {
		log.Printf("render env: %v", err)
	}
}

func (a *app) rememberEnvSession(out map[string]any) {
	sessionID := anyString(out["session_id"])
	if sessionID == "" {
		return
	}
	timings, rawTimings := parseTimings(asString(out["firecracker_timings"]))
	_ = rawTimings
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[sessionID] = envSession{
		EnvID:      anyString(out["env_id"]),
		SessionID:  sessionID,
		NodeID:     anyString(out["node_id"]),
		ImageRef:   anyString(out["image_ref"]),
		SnapshotID: firstNonEmpty(anyString(out["snapshot_id"]), anyString(out["latest_snapshot_id"])),
		Timings:    timings,
	}
}

func (a *app) envSession(sessionID string) envSession {
	if sessionID == "" {
		return envSession{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[sessionID]
}

func keyTimings(timings []timing) []timing {
	want := []string{
		"attach_nbd_device",
		"sideload_orca_init",
		"run_firecracker",
		"request_to_first_user_output",
		"commit_snapshot",
	}
	byName := make(map[string]timing, len(timings))
	for _, timing := range timings {
		if _, ok := byName[timing.Name]; !ok {
			byName[timing.Name] = timing
		}
	}
	out := make([]timing, 0, len(want))
	for _, name := range want {
		if timing, ok := byName[name]; ok {
			out = append(out, timing)
		}
	}
	return out
}

func (a *app) ttyOutput(w http.ResponseWriter, r *http.Request) {
	path := fmt.Sprintf("/tty/%s/%s/output?offset=%s", r.URL.Query().Get("node_id"), r.URL.Query().Get("session_id"), r.URL.Query().Get("offset"))
	raw, err := a.get(r.Context(), path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (a *app) ttyInput(w http.ResponseWriter, r *http.Request) {
	a.ttyPostText(w, r, "input")
}

func (a *app) ttyStop(w http.ResponseWriter, r *http.Request) {
	a.ttyPostText(w, r, "stop")
}

func (a *app) ttyPostText(w http.ResponseWriter, r *http.Request, action string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := fmt.Sprintf("/tty/%s/%s/%s", r.FormValue("node_id"), r.FormValue("session_id"), action)
	if action == "stop" && r.FormValue("env_id") != "" {
		path += "?env_id=" + r.FormValue("env_id")
	}
	body := strings.NewReader(r.FormValue("input"))
	out, err := a.postRaw(r.Context(), path, body, "text/plain")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if action == "stop" {
		a.rememberTTYStop(r, out)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func (a *app) rememberTTYStop(r *http.Request, raw []byte) {
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return
	}
	sessionID := firstNonEmpty(anyString(out["session_id"]), r.FormValue("session_id"))
	if sessionID == "" {
		return
	}
	timings, _ := parseAnyTimings(out["firecracker_timings"])
	a.mu.Lock()
	defer a.mu.Unlock()
	stored := a.sessions[sessionID]
	stored.EnvID = firstNonEmpty(stored.EnvID, r.FormValue("env_id"), anyString(out["env_id"]))
	stored.SessionID = sessionID
	stored.NodeID = firstNonEmpty(stored.NodeID, r.FormValue("node_id"), anyString(out["node_id"]))
	stored.ImageRef = firstNonEmpty(stored.ImageRef, anyString(out["image_ref"]))
	stored.SnapshotID = firstNonEmpty(anyString(out["snapshot_id"]), anyString(out["latest_snapshot_id"]), stored.SnapshotID)
	if len(timings) > 0 {
		stored.Timings = timings
	}
	stored.Closed = true
	a.sessions[sessionID] = stored
}

func envRequest(r *http.Request) map[string]any {
	req := map[string]any{
		"image":         strings.TrimSpace(r.FormValue("image")),
		"base_image_id": strings.TrimSpace(r.FormValue("base_image_id")),
		"env_id":        strings.TrimSpace(r.FormValue("env_id")),
		"command":       strings.TrimSpace(r.FormValue("command")),
		"force_node":    strings.TrimSpace(r.FormValue("force_node")),
	}
	if n := positiveFormInt(r, "cpu_count"); n > 0 {
		req["cpu_count"] = n
	}
	if n := positiveFormInt(r, "memory_mib"); n > 0 {
		req["memory_mib"] = n
	}
	for k, v := range req {
		if v == "" {
			delete(req, k)
		}
	}
	return req
}

func positiveFormInt(r *http.Request, name string) int {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func (a *app) render(w http.ResponseWriter, r *http.Request, data pageData) {
	images, err := a.listImages(r.Context())
	if err != nil && data.Error == "" {
		data.Error = err.Error()
	}
	envs, envErr := a.listEnvs(r.Context())
	if envErr != nil && data.Error == "" {
		data.Error = envErr.Error()
	}
	nodes, nodeErr := a.listNodes(r.Context())
	if nodeErr != nil && data.Error == "" {
		data.Error = nodeErr.Error()
	}
	builds, buildsErr := a.listBuilds(r.Context())
	if buildsErr != nil && data.Error == "" {
		data.Error = buildsErr.Error()
	}
	if jobID := r.URL.Query().Get("build_job"); jobID != "" && data.BuildJob == nil {
		if job, err := a.getBuildJob(r.Context(), jobID); err == nil {
			data.BuildJob = job
		} else if data.Error == "" {
			data.Error = err.Error()
		}
	}
	data.Images = images
	data.Envs = envs
	data.Nodes = nodes
	data.Builds = builds
	data.ActiveSessions = a.activeSessions()
	data.Now = time.Now().Format("15:04:05")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.Execute(w, data); err != nil {
		log.Printf("render page: %v", err)
	}
}

func (a *app) listImages(ctx context.Context) ([]baseImage, error) {
	raw, err := a.get(ctx, "/images")
	if err != nil {
		return nil, err
	}
	var images []baseImage
	if err := json.Unmarshal(raw, &images); err != nil {
		return nil, err
	}
	return images, nil
}

func (a *app) listEnvs(ctx context.Context) ([]envRecord, error) {
	raw, err := a.get(ctx, "/envs")
	if err != nil {
		return nil, err
	}
	var envs []envRecord
	if err := json.Unmarshal(raw, &envs); err != nil {
		return nil, err
	}
	return envs, nil
}

func (a *app) listNodes(ctx context.Context) ([]nodeInfo, error) {
	raw, err := a.get(ctx, "/nodes")
	if err != nil {
		return nil, err
	}
	var nodes []nodeInfo
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

func (a *app) getBuildJob(ctx context.Context, id string) (*buildJobStatus, error) {
	raw, err := a.get(ctx, "/builds/"+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	var job buildJobStatus
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (a *app) listBuilds(ctx context.Context) ([]buildJobStatus, error) {
	raw, err := a.get(ctx, "/builds")
	if err != nil {
		return nil, err
	}
	var builds []buildJobStatus
	if err := json.Unmarshal(raw, &builds); err != nil {
		return nil, err
	}
	return builds, nil
}

func (a *app) activeSessions() []envSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]envSession, 0, len(a.sessions))
	for _, session := range a.sessions {
		if !session.Closed {
			out = append(out, session)
		}
	}
	return out
}

func (a *app) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.controlURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s failed: %s %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func (a *app) postJSON(ctx context.Context, path string, in any) (map[string]any, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.controlURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s failed: %s %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *app) postRaw(ctx context.Context, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.controlURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s failed: %s %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func terminalURL(out map[string]any) string {
	return envURL(out)
}

func envURL(out map[string]any) string {
	parts := []string{
		"env_id=" + urlQueryEscape(anyString(out["env_id"])),
		"session_id=" + urlQueryEscape(anyString(out["session_id"])),
		"node_id=" + urlQueryEscape(anyString(out["node_id"])),
	}
	if v := anyString(out["cpu_count"]); v != "" {
		parts = append(parts, "cpu_count="+urlQueryEscape(v))
	}
	if v := anyString(out["memory_mib"]); v != "" {
		parts = append(parts, "memory_mib="+urlQueryEscape(v))
	}
	return "/env?" + strings.Join(parts, "&")
}

func urlQueryEscape(v string) string {
	return url.QueryEscape(v)
}

func summarizeRun(title string, out map[string]any) *runResult {
	console := asString(out["firecracker_output"])
	stdout, stderr := splitGuestOutput(console)
	timings, rawTimings := parseTimings(asString(out["firecracker_timings"]))
	return &runResult{
		Title:      title,
		EnvID:      asString(out["env_id"]),
		SessionID:  asString(out["session_id"]),
		NodeID:     asString(out["node_id"]),
		ImageRef:   asString(out["image_ref"]),
		SnapshotID: asString(out["latest_snapshot_id"]),
		Stdout:     stdout,
		Stderr:     stderr,
		Console:    console,
		Timings:    timings,
		RawTimings: rawTimings,
	}
}

func summarizeBuild(out map[string]any) *buildResult {
	timings, rawTimings := parseAnyTimings(out["build_timings"])
	return &buildResult{
		BaseImageID:     asString(out["base_image_id"]),
		ImageRef:        asString(out["image_ref"]),
		ImageDigest:     asString(out["image_digest"]),
		VolumeID:        asString(out["volume_id"]),
		SnapshotID:      asString(out["snapshot_id"]),
		RootFSSizeBytes: asInt64(out["rootfs_size_bytes"]),
		DurationMS:      asInt64(out["duration_ms"]),
		Timings:         timings,
		RawTimings:      rawTimings,
	}
}

func splitGuestOutput(console string) (string, string) {
	var stdout []string
	var stderr []string
	for _, line := range strings.Split(console, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "orca-stdout:"):
			stdout = append(stdout, strings.TrimSpace(strings.TrimPrefix(line, "orca-stdout:")))
		case strings.HasPrefix(line, "orca-stderr:"):
			stderr = append(stderr, strings.TrimSpace(strings.TrimPrefix(line, "orca-stderr:")))
		}
	}
	return strings.Join(stdout, "\n"), strings.Join(stderr, "\n")
}

func parseTimings(raw string) ([]timing, string) {
	if raw == "" {
		return nil, ""
	}
	var timings []timing
	if err := json.Unmarshal([]byte(raw), &timings); err != nil {
		return nil, raw
	}
	return timings, raw
}

func parseAnyTimings(v any) ([]timing, string) {
	if v == nil {
		return nil, ""
	}
	if s, ok := v.(string); ok {
		return parseTimings(s)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Sprint(v)
	}
	timings, rawString := parseTimings(string(raw))
	if len(timings) == 0 && rawString == "" {
		return nil, string(raw)
	}
	return timings, rawString
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func anyString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}

func withDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		out, _ := n.Int64()
		return out
	default:
		return 0
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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

const pageHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Orca Sandbox</title>
  <style>
    :root { color-scheme: light; --line: #d8dee7; --muted: #667085; --ink: #111827; --bg: #f3f5f8; --panel: #fff; --accent: #1769aa; --bad: #a62929; --good: #176b3a; }
    * { box-sizing: border-box; }
    body { margin: 0; font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; color: var(--ink); background: var(--bg); }
    header { padding: 16px 24px; border-bottom: 1px solid var(--line); background: var(--panel); display: flex; justify-content: space-between; align-items: baseline; }
    h1 { margin: 0; font-size: 20px; }
    h2 { margin: 0 0 12px; font-size: 15px; }
    main { padding: 18px 24px 32px; display: grid; grid-template-columns: minmax(420px, 560px) 1fr; gap: 18px; align-items: start; }
    section { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 14px; }
    form { display: grid; gap: 10px; }
    label { display: block; margin: 0 0 5px; color: var(--muted); font-size: 12px; font-weight: 650; }
    input, textarea, select { width: 100%; border: 1px solid var(--line); border-radius: 6px; padding: 8px 9px; font: inherit; background: #fff; min-width: 0; }
    textarea { min-height: 96px; resize: vertical; font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace; }
    button { border: 0; border-radius: 6px; background: var(--accent); color: #fff; padding: 9px 12px; font-weight: 700; cursor: pointer; justify-self: start; }
    table { width: 100%; border-collapse: collapse; }
    th, td { border-bottom: 1px solid var(--line); padding: 7px 5px; text-align: left; vertical-align: top; }
    th { color: var(--muted); font-size: 12px; }
    code, pre { font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace; }
    pre { margin: 0; padding: 10px; background: #101820; color: #e8eef5; border-radius: 6px; overflow: auto; white-space: pre-wrap; }
    .stack, .forms { display: grid; gap: 14px; }
    .muted { color: var(--muted); }
    .notice { border-color: #8cc49e; color: var(--good); background: #f0faf3; }
    .error { border-color: #e0a0a0; color: var(--bad); background: #fff5f5; }
    .row { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
    .thirds { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 10px; }
    .meta { display: flex; flex-wrap: wrap; gap: 10px 14px; color: var(--muted); margin-bottom: 10px; }
    .result-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    .status-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; }
    .stat { border: 1px solid var(--line); border-radius: 8px; padding: 10px; background: #fbfcfe; }
    .stat strong { display: block; font-size: 20px; line-height: 1.1; }
    .inline-form { display: inline; }
    .secondary { background: #344054; }
    .ghost { background: #eef2f7; color: #475467; cursor: default; }
    .pill { display: inline-block; border: 1px solid var(--line); border-radius: 999px; padding: 2px 8px; color: var(--muted); font-size: 12px; background: #fbfcfe; }
    .build-line { margin-top: 8px; padding: 9px; border: 1px solid var(--line); border-radius: 6px; background: #fbfcfe; overflow-wrap: anywhere; }
    .nav { display: flex; gap: 12px; align-items: center; }
    a { color: var(--accent); }
    @media (max-width: 980px) { main, .result-grid, .thirds { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
  <header>
    <h1>Orca Sandbox</h1>
    <div class="nav"><a href="/builds">Builds</a><span class="muted">updated {{.Now}}</span></div>
  </header>
  <main>
    <div class="forms">
      {{if .Notice}}<section class="notice">{{.Notice}}</section>{{end}}
      {{if .Error}}<section class="error">{{.Error}}</section>{{end}}

      <section>
        <h2>Create Env</h2>
        <form method="post" action="/start">
          <label>Base image</label>
          <select name="base_image_id" {{if not .Images}}disabled{{end}}>
            {{range .Images}}<option value="{{.BaseImageID}}">{{.ImageRef}} · {{.BaseImageID}}</option>{{end}}
          </select>
          {{if not .Images}}<div class="muted">Build a base image first.</div>{{end}}
          <label>Orca run command</label>
          <textarea name="command">sleep infinity</textarea>
          <div class="thirds">
            <div>
              <label>vCPUs</label>
              <input name="cpu_count" type="number" min="1" value="1">
            </div>
            <div>
              <label>Memory MiB</label>
              <input name="memory_mib" type="number" min="64" step="64" value="128">
            </div>
            <div>
              <label>Node</label>
              <select name="force_node"><option value="">scheduler</option><option>node-1</option><option>node-2</option></select>
            </div>
          </div>
          <button type="submit">Create Env</button>
        </form>
      </section>

      <section>
        <h2>Resume Env</h2>
        <form method="post" action="/resume">
          <label>Env id</label>
          <input name="env_id" placeholder="env-...">
          <label>Orca run command</label>
          <textarea name="command">sleep infinity</textarea>
          <div class="thirds">
            <div>
              <label>vCPUs</label>
              <input name="cpu_count" type="number" min="1" value="1">
            </div>
            <div>
              <label>Memory MiB</label>
              <input name="memory_mib" type="number" min="64" step="64" value="128">
            </div>
            <div>
              <label>Node</label>
              <select name="force_node"><option value="">scheduler</option><option>node-1</option><option>node-2</option></select>
            </div>
          </div>
          <button type="submit">Resume Env</button>
        </form>
      </section>

      <section>
        <h2>Build Base Image</h2>
        <form method="post" action="/build" id="build-form">
          <div class="row">
            <div>
              <label>Image</label>
              <input name="image" list="known-images" value="alpine:3.22">
              <datalist id="known-images">
                {{range .Images}}<option value="{{.ImageRef}}"></option>{{end}}
                <option value="alpine:3.22"></option>
              </datalist>
            </div>
            <div>
              <label>Rootfs size MB</label>
              <input name="rootfs_size_mb" type="number" min="128" value="512">
            </div>
          </div>
          <button type="submit">Build</button>
        </form>
        <div class="muted" style="margin-top:8px">Builds run in the background; progress appears on this page.</div>
      </section>
    </div>

    <div class="stack">
      <section>
        <h2>Storage Status</h2>
        <div class="status-grid">
          <div class="stat"><strong>{{len .Images}}</strong><span class="muted">base images</span></div>
          <div class="stat"><strong>{{len .Envs}}</strong><span class="muted">envs</span></div>
          <div class="stat"><strong>{{len .ActiveSessions}}</strong><span class="muted">running here</span></div>
          <div class="stat"><strong>{{len .Builds}}</strong><span class="muted">build jobs</span></div>
        </div>
        <div class="meta" style="margin-top:10px;margin-bottom:0">
          {{range .Nodes}}<span class="pill">{{.NodeID}}</span>{{end}}
          <span class="pill">chunks immutable</span>
          <span class="pill">delete image disabled until chunk GC exists</span>
        </div>
      </section>

      <section>
        <h2>Builds</h2>
        {{if .Builds}}
        <table>
          <thead><tr><th>Image</th><th>State</th><th>Last line</th><th></th></tr></thead>
          <tbody>
          {{range .Builds}}
            <tr>
              <td>{{.Image}}</td>
              <td><code>{{.State}}</code></td>
              <td><code>{{.LastLine}}</code></td>
              <td><a href="/?build_job={{.ID}}">check</a></td>
            </tr>
          {{end}}
          </tbody>
        </table>
        <div class="muted" style="margin-top:8px"><a href="/builds">Open full build list</a></div>
        {{else}}<div class="muted">No builds started in this service process.</div>{{end}}
      </section>

      {{if .BuildJob}}
      <section id="build-status" data-build-id="{{.BuildJob.ID}}">
        <h2>Build Status</h2>
        <div class="meta">
          <span>job <code id="build-id">{{.BuildJob.ID}}</code></span>
          <span>image <code id="build-image">{{.BuildJob.Image}}</code></span>
          <span>state <code id="build-state">{{.BuildJob.State}}</code></span>
        </div>
        <div class="build-line"><code id="build-last-line">{{.BuildJob.LastLine}}</code></div>
        <div id="build-error" class="error" style="display:none;margin-top:10px"></div>
        <table style="margin-top:10px">
          <thead><tr><th>Step</th><th>Duration</th><th>Status</th></tr></thead>
          <tbody id="build-timing-body">
          {{range .BuildJob.Timings}}
            <tr><td>{{.Name}}</td><td>{{.DurationMS}}ms</td><td>{{.Status}}{{if .Error}} {{.Error}}{{end}}</td></tr>
          {{end}}
          </tbody>
        </table>
      </section>
      {{end}}

      <section>
        <h2>Current Envs</h2>
        {{if .ActiveSessions}}
        <table>
          <thead><tr><th>Env</th><th>Image</th><th>Node</th><th>Session</th><th></th></tr></thead>
          <tbody>
          {{range .ActiveSessions}}
            <tr>
              <td><code>{{.EnvID}}</code></td>
              <td>{{.ImageRef}}</td>
              <td><code>{{.NodeID}}</code></td>
              <td><code>{{.SessionID}}</code></td>
              <td><a href="/env?env_id={{.EnvID}}&session_id={{.SessionID}}&node_id={{.NodeID}}">open</a></td>
            </tr>
          {{end}}
          </tbody>
        </table>
        {{else}}<div class="muted">No running envs in this sandbox-service process.</div>{{end}}
      </section>

      <section>
        <h2>All Envs</h2>
        {{if .Envs}}
        <table>
          <thead><tr><th>Env</th><th>Image</th><th>Snapshot</th><th></th></tr></thead>
          <tbody>
          {{range .Envs}}
            <tr>
              <td><code>{{.EnvID}}</code></td>
              <td>{{.ImageRef}}</td>
              <td><code>{{.LatestSnapshotID}}</code></td>
              <td>
                <form class="inline-form" method="post" action="/resume">
                  <input type="hidden" name="env_id" value="{{.EnvID}}">
                  <input type="hidden" name="command" value="sleep infinity">
                  <button class="secondary" type="submit">Resume</button>
                </form>
              </td>
            </tr>
          {{end}}
          </tbody>
        </table>
        {{else}}<div class="muted">No envs created yet.</div>{{end}}
      </section>

      <section>
        <h2>Images</h2>
        {{if .Images}}
        <table>
          <thead><tr><th>Image</th><th>Base image id</th><th>Snapshot</th><th>Size</th><th></th></tr></thead>
          <tbody>
          {{range .Images}}
            <tr>
              <td>{{.ImageRef}}</td>
              <td><code>{{.BaseImageID}}</code></td>
              <td><code>{{.SnapshotID}}</code></td>
              <td>{{.RootFSSizeBytes}}</td>
              <td><button class="ghost" type="button" disabled>Delete</button></td>
            </tr>
          {{end}}
          </tbody>
        </table>
        {{else}}<div class="muted">No images built yet.</div>{{end}}
      </section>

      {{if .Result}}
      <section>
        <h2>{{.Result.Title}}</h2>
        <div class="meta">
          <span>env <code>{{.Result.EnvID}}</code></span>
          <span>session <code>{{.Result.SessionID}}</code></span>
          <span>node <code>{{.Result.NodeID}}</code></span>
          <span>snapshot <code>{{.Result.SnapshotID}}</code></span>
        </div>
        <div class="result-grid">
          <div>
            <h2>Stdout</h2>
            <pre>{{if .Result.Stdout}}{{.Result.Stdout}}{{else}}(empty){{end}}</pre>
          </div>
          <div>
            <h2>Stderr</h2>
            <pre>{{if .Result.Stderr}}{{.Result.Stderr}}{{else}}(empty){{end}}</pre>
          </div>
        </div>
      </section>

      <section>
        <h2>Timings</h2>
        {{if .Result.Timings}}
        <table>
          <thead><tr><th>Step</th><th>Duration</th><th>Status</th></tr></thead>
          <tbody>
          {{range .Result.Timings}}
            <tr><td>{{.Name}}</td><td>{{.DurationMS}}ms</td><td>{{.Status}}{{if .Error}} {{.Error}}{{end}}</td></tr>
          {{end}}
          </tbody>
        </table>
        {{else}}<pre>{{.Result.RawTimings}}</pre>{{end}}
      </section>

      <section>
        <h2>Console</h2>
        <pre>{{.Result.Console}}</pre>
      </section>
      {{end}}

      {{if .Build}}
      <section>
        <h2>Build Result</h2>
        <div class="meta">
          <span>image <code>{{.Build.ImageRef}}</code></span>
          <span>base <code>{{.Build.BaseImageID}}</code></span>
          <span>volume <code>{{.Build.VolumeID}}</code></span>
          <span>snapshot <code>{{.Build.SnapshotID}}</code></span>
          <span>total {{.Build.DurationMS}}ms</span>
        </div>
      </section>

      <section>
        <h2>Build Timings</h2>
        {{if .Build.Timings}}
        <table>
          <thead><tr><th>Step</th><th>Duration</th><th>Status</th></tr></thead>
          <tbody>
          {{range .Build.Timings}}
            <tr><td>{{.Name}}</td><td>{{.DurationMS}}ms</td><td>{{.Status}}{{if .Error}} {{.Error}}{{end}}</td></tr>
          {{end}}
          </tbody>
        </table>
        {{else}}<pre>{{.Build.RawTimings}}</pre>{{end}}
      </section>
      {{end}}
    </div>
  </main>
  <script>
    const buildStatus = document.getElementById("build-status");
    if (buildStatus) {
      const buildID = buildStatus.dataset.buildId;
      const stateEl = document.getElementById("build-state");
      const lineEl = document.getElementById("build-last-line");
      const errEl = document.getElementById("build-error");
      const timingBody = document.getElementById("build-timing-body");
      function renderBuildTimings(timings) {
        if (!Array.isArray(timings)) return;
        timingBody.replaceChildren();
        for (const timing of timings) {
          const tr = document.createElement("tr");
          const name = document.createElement("td");
          const duration = document.createElement("td");
          const status = document.createElement("td");
          name.textContent = timing.name || "";
          duration.textContent = String(timing.duration_ms || 0) + "ms";
          status.textContent = (timing.status || "") + (timing.error ? " " + timing.error : "");
          tr.appendChild(name);
          tr.appendChild(duration);
          tr.appendChild(status);
          timingBody.appendChild(tr);
        }
      }
      async function pollBuild() {
        try {
          const res = await fetch("/build/status?id=" + encodeURIComponent(buildID));
          if (!res.ok) throw new Error(await res.text());
          const job = await res.json();
          stateEl.textContent = job.state || "";
          lineEl.textContent = job.last_line || "";
          renderBuildTimings(job.build_timings);
          if (job.error) {
            errEl.style.display = "block";
            errEl.textContent = job.error;
          }
          if (job.state === "done" || job.state === "error") return;
        } catch (err) {
          stateEl.textContent = "poll failed";
          lineEl.textContent = err.message;
        }
        window.setTimeout(pollBuild, 1000);
      }
      pollBuild();
    }
  </script>
</body>
</html>`

const buildsHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Orca Builds</title>
  <style>
    :root { color-scheme: light; --line: #d8dee7; --muted: #667085; --ink: #111827; --bg: #f3f5f8; --panel: #fff; --accent: #1769aa; --bad: #a62929; --good: #176b3a; }
    * { box-sizing: border-box; }
    body { margin: 0; font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; color: var(--ink); background: var(--bg); }
    header { padding: 16px 24px; border-bottom: 1px solid var(--line); background: var(--panel); display: flex; justify-content: space-between; align-items: baseline; }
    main { padding: 18px 24px 32px; display: grid; gap: 14px; }
    section { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 14px; }
    h1 { margin: 0; font-size: 20px; }
    h2 { margin: 0 0 12px; font-size: 15px; }
    table { width: 100%; border-collapse: collapse; }
    th, td { border-bottom: 1px solid var(--line); padding: 8px 6px; text-align: left; vertical-align: top; }
    th { color: var(--muted); font-size: 12px; }
    code, pre { font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace; }
    pre { margin: 0; max-width: 760px; white-space: pre-wrap; overflow-wrap: anywhere; }
    a { color: var(--accent); }
    .muted { color: var(--muted); }
    .error { border-color: #e0a0a0; color: var(--bad); background: #fff5f5; }
    .state { display: inline-block; min-width: 72px; border: 1px solid var(--line); border-radius: 999px; padding: 2px 8px; text-align: center; font-size: 12px; background: #fbfcfe; }
    .running { color: #8a5a00; border-color: #e3b85c; background: #fff8e7; }
    .done { color: var(--good); border-color: #8cc49e; background: #f0faf3; }
    .error-state { color: var(--bad); border-color: #e0a0a0; background: #fff5f5; }
    .topline { display: flex; justify-content: space-between; gap: 12px; align-items: center; }
  </style>
</head>
<body>
  <header>
    <h1>Orca Builds</h1>
    <div><a href="/">Sandbox</a> <span class="muted">updated {{.Now}}</span></div>
  </header>
  <main>
    {{if .Error}}<section class="error">{{.Error}}</section>{{end}}
    <section>
      <div class="topline">
        <h2>Build Jobs</h2>
        <div class="muted">{{len .Builds}} jobs</div>
      </div>
      {{if .Builds}}
      <table>
        <thead><tr><th>Image</th><th>State</th><th>Started</th><th>Finished</th><th>Last line</th><th>Steps</th></tr></thead>
        <tbody>
        {{range .Builds}}
          <tr>
            <td><code>{{.Image}}</code><br><span class="muted">{{.ID}}</span></td>
            <td><span class="state {{.State}}{{if eq .State "error"}}-state{{end}}">{{.State}}</span></td>
            <td><code>{{.StartedAt}}</code></td>
            <td><code>{{.FinishedAt}}</code></td>
            <td><pre>{{.LastLine}}{{if .Error}}
{{.Error}}{{end}}</pre></td>
            <td>
              <details>
                <summary>{{len .Timings}} steps</summary>
                <table>
                  <tbody>
                  {{range .Timings}}
                    <tr><td>{{.Name}}</td><td>{{.DurationMS}}ms</td><td>{{.Status}}{{if .Error}} {{.Error}}{{end}}</td></tr>
                  {{end}}
                  </tbody>
                </table>
              </details>
            </td>
          </tr>
        {{end}}
        </tbody>
      </table>
      {{else}}<div class="muted">No builds started in this service process.</div>{{end}}
    </section>
  </main>
  <script>
    const hasRunning = Array.from(document.querySelectorAll(".state")).some((el) => {
      return el.textContent === "queued" || el.textContent === "running";
    });
    if (hasRunning) window.setTimeout(() => window.location.reload(), 2500);
  </script>
</body>
</html>`

const envHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Orca Env</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.css">
  <style>
    html, body { height: 100%; }
    body { margin: 0; font: 14px ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #0d1117; color: #dfe8f1; }
    header { display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 10px 12px; background: #151b23; border-bottom: 1px solid #30363d; }
    main { display: grid; grid-template-columns: 1fr 360px; height: calc(100vh - 43px); min-height: 0; }
    #terminal { min-height: 0; padding: 8px; overflow: hidden; }
    #terminal .xterm { height: 100%; }
    aside { display: grid; grid-auto-rows: max-content; gap: 12px; padding: 12px; border-left: 1px solid #30363d; background: #151b23; min-width: 0; overflow: auto; }
    #status { color: #8b949e; font: 12px system-ui, sans-serif; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .panel { display: grid; gap: 8px; }
    label { color: #8b949e; font-size: 12px; font-weight: 650; }
    textarea, input, select { width: 100%; border: 1px solid #30363d; border-radius: 6px; background: #0d1117; color: #dfe8f1; padding: 8px; font: 13px ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace; }
    textarea { min-height: 96px; resize: vertical; }
    button { font: 13px system-ui, sans-serif; color: #fff; background: #1769aa; border: 0; border-radius: 6px; padding: 8px 10px; cursor: pointer; }
    button.danger { background: #9b2f2f; }
    button:disabled { cursor: default; opacity: .55; }
    dl { display: grid; grid-template-columns: 72px 1fr; gap: 6px 8px; margin: 0; color: #8b949e; font-size: 12px; }
    dt { color: #6e7681; }
    dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
    .row { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
    h2 { margin: 0; font-size: 13px; color: #dfe8f1; }
    table { width: 100%; border-collapse: collapse; font-size: 12px; }
    th, td { padding: 5px 0; border-bottom: 1px solid #30363d; text-align: left; vertical-align: top; }
    th { color: #8b949e; font-weight: 650; }
    td:last-child, th:last-child { text-align: right; }
    details { color: #8b949e; }
    summary { cursor: pointer; font-size: 12px; }
    #env-logs { height: 220px; min-height: 160px; border: 1px solid #30363d; border-radius: 6px; padding: 6px; overflow: hidden; background: #0d1117; }
    #env-logs .xterm { height: 100%; }
    .ok { color: #7ee787; }
    .err { color: #ffb4ab; }
    code { color: #9fc7ee; }
    @media (max-width: 900px) { main { grid-template-columns: 1fr; grid-template-rows: minmax(420px, 1fr) auto; } aside { border-left: 0; border-top: 1px solid #30363d; } }
  </style>
</head>
<body>
  <header>
    <div>env <code>{{.EnvID}}</code></div>
    <a href="/" style="color:#9fc7ee">Sandbox</a>
  </header>
  <main>
    <div id="terminal"></div>
    <aside>
      <section class="panel">
        <div id="status">connecting</div>
        <dl>
          <dt>session</dt><dd><code>{{.SessionID}}</code></dd>
          <dt>node</dt><dd><code>{{.NodeID}}</code></dd>
          {{if .ImageRef}}<dt>image</dt><dd><code>{{.ImageRef}}</code></dd>{{end}}
          {{if .SnapshotID}}<dt>snapshot</dt><dd><code>{{.SnapshotID}}</code></dd>{{end}}
          <dt>vCPUs</dt><dd><code>{{.CPUCount}}</code></dd>
          <dt>memory</dt><dd><code>{{.MemoryMiB}} MiB</code></dd>
        </dl>
        <button type="button" class="danger" id="stop">Stop / Commit</button>
      </section>
      <section class="panel">
        <h2>Timings</h2>
        <table>
          <thead><tr><th>Step</th><th>Duration</th></tr></thead>
          <tbody id="key-timings-body">
          {{range .KeyTimings}}
            <tr><td>{{.Name}}{{if .Error}} <span class="err">{{.Error}}</span>{{end}}</td><td class="{{if eq .Status "ok"}}ok{{else}}err{{end}}">{{.DurationMS}}ms</td></tr>
          {{end}}
          </tbody>
        </table>
        <details>
          <summary>All timing steps</summary>
          <table>
            <tbody id="all-timings-body">
            {{range .AllTimings}}
              <tr><td>{{.Name}}</td><td class="{{if eq .Status "ok"}}ok{{else}}err{{end}}">{{.DurationMS}}ms</td></tr>
            {{end}}
            </tbody>
          </table>
        </details>
        <div id="timing-empty" style="color:#8b949e;font-size:12px;{{if .HasRunStats}}display:none{{end}}">Timings appear after creating or resuming an env from this page.</div>
      </section>
      <section class="panel">
        <h2>Env Logs</h2>
        <div id="env-logs"></div>
      </section>
      <form class="panel" method="post" action="/resume" id="resumeForm">
        <input type="hidden" name="env_id" value="{{.EnvID}}">
        <label>Orca run command</label>
        <textarea name="command">sleep infinity</textarea>
        <div class="row">
          <div>
            <label>vCPUs</label>
            <input name="cpu_count" type="number" min="1" value="{{.CPUCount}}">
          </div>
          <div>
            <label>Memory MiB</label>
            <input name="memory_mib" type="number" min="64" step="64" value="{{.MemoryMiB}}">
          </div>
        </div>
        <label>Node</label>
        <select name="force_node"><option value="">scheduler</option><option>node-1</option><option>node-2</option></select>
        <button type="submit" id="resumeButton">Resume Env</button>
      </form>
    </aside>
  </main>
  <script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.10.0/lib/addon-fit.js"></script>
  <script>
    const envID = "{{.EnvID}}";
    const sessionID = "{{.SessionID}}";
    const nodeID = "{{.NodeID}}";
    const status = document.getElementById("status");
    const stopButton = document.getElementById("stop");
    const resumeButton = document.getElementById("resumeButton");
    const keyTimingsBody = document.getElementById("key-timings-body");
    const allTimingsBody = document.getElementById("all-timings-body");
    const timingEmpty = document.getElementById("timing-empty");
    const envLogsElement = document.getElementById("env-logs");
    const keyTimingNames = [
      "attach_nbd_device",
      "sideload_orca_init",
      "run_firecracker",
      "request_to_first_user_output",
      "commit_snapshot"
    ];
    const term = new Terminal({
      cursorBlink: true,
      convertEol: false,
      fontFamily: 'ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace',
      fontSize: 14,
      scrollback: 5000,
      theme: {
        background: "#0d1117",
        foreground: "#dfe8f1",
        cursor: "#dfe8f1",
        selectionBackground: "#264f78"
      }
    });
    const fitAddon = new FitAddon.FitAddon();
    const logTerm = new Terminal({
      cursorBlink: false,
      cursorStyle: "bar",
      convertEol: true,
      disableStdin: true,
      fontFamily: 'ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace',
      fontSize: 12,
      scrollback: 3000,
      theme: {
        background: "#0d1117",
        foreground: "#dfe8f1",
        cursor: "#0d1117",
        selectionBackground: "#264f78"
      }
    });
    const logFitAddon = new FitAddon.FitAddon();
    const pollIntervalMS = 100;
    let offset = 0;
    let sessionClosed = !sessionID || !nodeID;
    let committed = !sessionID || !nodeID;

    term.loadAddon(fitAddon);
    term.open(document.getElementById("terminal"));
    fitAddon.fit();
    logTerm.loadAddon(logFitAddon);
    logTerm.open(envLogsElement);
    logFitAddon.fit();
    term.focus();
    window.addEventListener("resize", () => {
      fitAddon.fit();
      logFitAddon.fit();
    });

    function setStatus(text) {
      status.textContent = text;
    }
    function appendLogs(output) {
      if (!output) return;
      const lines = output.split(/\r?\n/).filter((line) =>
        line.includes("orca-init:") ||
        line.includes("orca-bg:") ||
        line.includes("Dock HTTP Api listening") ||
        line.includes("Workspace Server listening") ||
        line.includes("Published to JetBrains Relay") ||
        line.includes("Join this workspace using URL")
      );
      if (!lines.length) return;
      logTerm.write(lines.join("\r\n") + "\r\n");
    }
    function parseTimings(raw) {
      if (!raw) return [];
      if (Array.isArray(raw)) return raw;
      if (typeof raw === "string") {
        try {
          const parsed = JSON.parse(raw);
          return Array.isArray(parsed) ? parsed : [];
        } catch (_) {
          return [];
        }
      }
      return [];
    }
    function timingClass(timing) {
      return timing && timing.status === "ok" ? "ok" : "err";
    }
    function timingRow(timing) {
      const tr = document.createElement("tr");
      const name = document.createElement("td");
      name.textContent = timing.name || "";
      if (timing.error) {
        const err = document.createElement("span");
        err.className = "err";
        err.textContent = " " + timing.error;
        name.appendChild(err);
      }
      const duration = document.createElement("td");
      duration.className = timingClass(timing);
      duration.textContent = String(timing.duration_ms || 0) + "ms";
      tr.appendChild(name);
      tr.appendChild(duration);
      return tr;
    }
    function renderTimings(raw) {
      const timings = parseTimings(raw);
      if (!timings.length) return;
      const byName = new Map();
      for (const timing of timings) {
        if (timing && timing.name && !byName.has(timing.name)) byName.set(timing.name, timing);
      }
      keyTimingsBody.replaceChildren();
      for (const name of keyTimingNames) {
        if (byName.has(name)) keyTimingsBody.appendChild(timingRow(byName.get(name)));
      }
      allTimingsBody.replaceChildren();
      for (const timing of timings) allTimingsBody.appendChild(timingRow(timing));
      timingEmpty.style.display = "none";
    }
    async function postInput(data) {
      if (sessionClosed) return;
      const body = new URLSearchParams();
      body.set("env_id", envID);
      body.set("node_id", nodeID);
      body.set("session_id", sessionID);
      body.set("input", data);
      const res = await fetch("/tty/input", { method: "POST", body });
      if (!res.ok) {
        term.writeln("");
        term.writeln("[orca] " + await res.text());
      }
    }
    term.onData((data) => {
      postInput(data).catch((err) => {
        term.writeln("");
        term.writeln("[orca] input failed: " + err.message);
      });
    });

    async function poll() {
      if (committed || !sessionID || !nodeID) {
        setStatus("ready to resume");
        stopButton.disabled = true;
        resumeButton.disabled = false;
        return;
      }
      try {
        const res = await fetch("/tty/output?node_id=" + encodeURIComponent(nodeID) + "&session_id=" + encodeURIComponent(sessionID) + "&offset=" + offset);
        if (!res.ok) {
          sessionClosed = true;
          committed = true;
          stopButton.disabled = true;
          resumeButton.disabled = false;
          setStatus("ready to resume");
          term.writeln("");
          term.writeln("[orca] " + await res.text());
          return;
        }
        const out = await res.json();
        offset = out.offset || offset;
        if (out.output) {
          term.write(out.output);
          appendLogs(out.output);
        }
        if (out.done) {
          sessionClosed = true;
          setStatus("exited; commit before resume");
          stopButton.textContent = "Commit";
          resumeButton.disabled = true;
          return;
        }
        setStatus("connected");
        resumeButton.disabled = true;
      } catch (err) {
        setStatus("poll failed: " + err.message);
      }
      setTimeout(poll, pollIntervalMS);
    }
    async function stopSession() {
      if (committed) return;
      sessionClosed = true;
      committed = true;
      stopButton.disabled = true;
      resumeButton.disabled = true;
      setStatus("stopping");
      term.writeln("");
      term.writeln("[orca] stopping and committing...");
      const body = new URLSearchParams();
      body.set("env_id", envID);
      body.set("node_id", nodeID);
      body.set("session_id", sessionID);
      const res = await fetch("/tty/stop", { method: "POST", body });
      if (!res.ok) {
        committed = false;
        setStatus("stop failed");
        term.writeln("[orca] " + await res.text());
        stopButton.disabled = false;
        return;
      }
      const out = await res.json();
      setStatus("stopped snapshot " + (out.snapshot_id || ""));
      term.writeln("[orca] stopped snapshot " + (out.snapshot_id || ""));
      renderTimings(out.firecracker_timings);
      resumeButton.disabled = false;
    }
    stopButton.addEventListener("click", () => {
      stopSession().catch((err) => {
        setStatus("stop failed");
        term.writeln("[orca] stop failed: " + err.message);
      });
    });
    poll();
  </script>
</body>
</html>`
