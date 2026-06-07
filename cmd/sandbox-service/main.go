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
	"os"
	"strconv"
	"strings"
	"time"
)

type app struct {
	controlURL string
	client     *http.Client
	tmpl       *template.Template
}

type baseImage struct {
	BaseImageID     string `json:"base_image_id"`
	ImageRef        string `json:"image_ref"`
	ImageDigest     string `json:"image_digest"`
	VolumeID        string `json:"volume_id"`
	SnapshotID      string `json:"snapshot_id"`
	RootFSSizeBytes int64  `json:"rootfs_size_bytes"`
}

type pageData struct {
	Images []baseImage
	Result *runResult
	Build  *buildResult
	Notice string
	Error  string
	Now    string
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("GET /", a.index)
	mux.HandleFunc("POST /build", a.buildImage)
	mux.HandleFunc("POST /start", a.startEnv)
	mux.HandleFunc("POST /resume", a.resumeEnv)

	addr := ":" + getenv("PORT", "8080")
	log.Printf("sandbox-service listening on %s control=%s", addr, a.controlURL)
	log.Fatal(http.ListenAndServe(addr, logRequests(mux)))
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, pageData{})
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
	out, err := a.postJSON(r.Context(), "/buildImage", req)
	if err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	a.render(w, r, pageData{
		Notice: fmt.Sprintf("built %s as %s", out["image_ref"], out["base_image_id"]),
		Build:  summarizeBuild(out),
	})
}

func (a *app) startEnv(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	req := envRequest(r)
	if req["command"] == "" {
		a.render(w, r, pageData{Error: "command is required"})
		return
	}
	if req["image"] == "" && req["base_image_id"] == "" {
		a.render(w, r, pageData{Error: "image or base image id is required"})
		return
	}
	out, err := a.postJSON(r.Context(), "/startEnv", req)
	if err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	a.render(w, r, pageData{Result: summarizeRun("startEnv", out)})
}

func (a *app) resumeEnv(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	req := envRequest(r)
	if req["env_id"] == "" {
		a.render(w, r, pageData{Error: "env id is required"})
		return
	}
	if req["command"] == "" {
		a.render(w, r, pageData{Error: "command is required"})
		return
	}
	out, err := a.postJSON(r.Context(), "/resumeEnv", req)
	if err != nil {
		a.render(w, r, pageData{Error: err.Error()})
		return
	}
	a.render(w, r, pageData{Result: summarizeRun("resumeEnv", out)})
}

func envRequest(r *http.Request) map[string]any {
	req := map[string]any{
		"image":         strings.TrimSpace(r.FormValue("image")),
		"base_image_id": strings.TrimSpace(r.FormValue("base_image_id")),
		"env_id":        strings.TrimSpace(r.FormValue("env_id")),
		"command":       strings.TrimSpace(r.FormValue("command")),
		"force_node":    strings.TrimSpace(r.FormValue("force_node")),
	}
	for k, v := range req {
		if v == "" {
			delete(req, k)
		}
	}
	return req
}

func (a *app) render(w http.ResponseWriter, r *http.Request, data pageData) {
	images, err := a.listImages(r.Context())
	if err != nil && data.Error == "" {
		data.Error = err.Error()
	}
	data.Images = images
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
    :root { color-scheme: light; --line: #d7dde5; --muted: #5f6b7a; --ink: #17202a; --bg: #f7f8fa; --panel: #fff; --accent: #1769aa; --bad: #a62929; --good: #176b3a; }
    * { box-sizing: border-box; }
    body { margin: 0; font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; color: var(--ink); background: var(--bg); }
    header { padding: 18px 24px; border-bottom: 1px solid var(--line); background: var(--panel); display: flex; justify-content: space-between; align-items: baseline; }
    h1 { margin: 0; font-size: 20px; }
    h2 { margin: 0 0 12px; font-size: 15px; }
    main { padding: 18px 24px 32px; display: grid; grid-template-columns: minmax(320px, 420px) 1fr; gap: 18px; }
    section { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 14px; }
    label { display: block; margin: 10px 0 5px; color: var(--muted); font-size: 12px; font-weight: 650; }
    input, textarea, select { width: 100%; border: 1px solid var(--line); border-radius: 6px; padding: 8px 9px; font: inherit; background: #fff; }
    textarea { min-height: 86px; resize: vertical; font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace; }
    button { margin-top: 10px; border: 0; border-radius: 6px; background: var(--accent); color: #fff; padding: 8px 11px; font-weight: 700; cursor: pointer; }
    table { width: 100%; border-collapse: collapse; }
    th, td { border-bottom: 1px solid var(--line); padding: 7px 5px; text-align: left; vertical-align: top; }
    th { color: var(--muted); font-size: 12px; }
    code, pre { font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace; }
    pre { margin: 0; padding: 10px; background: #101820; color: #e8eef5; border-radius: 6px; overflow: auto; white-space: pre-wrap; }
    .stack { display: grid; gap: 14px; }
    .forms { display: grid; gap: 14px; }
    .muted { color: var(--muted); }
    .notice { border-color: #8cc49e; color: var(--good); background: #f0faf3; }
    .error { border-color: #e0a0a0; color: var(--bad); background: #fff5f5; }
    .row { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
    .meta { display: flex; flex-wrap: wrap; gap: 10px 14px; color: var(--muted); margin-bottom: 10px; }
    .result-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    @media (max-width: 900px) { main, .result-grid { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
  <header>
    <h1>Orca Sandbox</h1>
    <div class="muted">updated {{.Now}}</div>
  </header>
  <main>
    <div class="forms">
      {{if .Notice}}<section class="notice">{{.Notice}}</section>{{end}}
      {{if .Error}}<section class="error">{{.Error}}</section>{{end}}

      <section>
        <h2>Build Image</h2>
        <form method="post" action="/build">
          <label>Image</label>
          <input name="image" value="alpine:3.22">
          <label>Rootfs size MB</label>
          <input name="rootfs_size_mb" value="512">
          <button type="submit">Build</button>
        </form>
      </section>

      <section>
        <h2>Run Env</h2>
        <form method="post" action="/start">
          <label>Image</label>
          <input name="image" placeholder="alpine:3.22">
          <label>Base image id</label>
          <input name="base_image_id" placeholder="optional">
          <label>Command</label>
          <textarea name="command">echo hello from orca</textarea>
          <label>Node</label>
          <select name="force_node"><option value="">scheduler</option><option>node-1</option><option>node-2</option></select>
          <button type="submit">Run</button>
        </form>
      </section>

      <section>
        <h2>Resume Env</h2>
        <form method="post" action="/resume">
          <label>Env id</label>
          <input name="env_id" placeholder="env-...">
          <label>Command</label>
          <textarea name="command">cat /etc/orca-image-ref</textarea>
          <label>Node</label>
          <select name="force_node"><option value="">scheduler</option><option>node-1</option><option>node-2</option></select>
          <button type="submit">Resume</button>
        </form>
      </section>
    </div>

    <div class="stack">
      <section>
        <h2>Images</h2>
        {{if .Images}}
        <table>
          <thead><tr><th>Image</th><th>Base image id</th><th>Snapshot</th><th>Size</th></tr></thead>
          <tbody>
          {{range .Images}}
            <tr><td>{{.ImageRef}}</td><td><code>{{.BaseImageID}}</code></td><td><code>{{.SnapshotID}}</code></td><td>{{.RootFSSizeBytes}}</td></tr>
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
</body>
</html>`
