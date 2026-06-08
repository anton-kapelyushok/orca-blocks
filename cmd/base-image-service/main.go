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
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anton-k/orca-blocks/pkg/storage"
	"github.com/google/uuid"
)

type app struct {
	backend          *storage.Backend
	repo             storage.Repository
	workDir          string
	containerRuntime string
	defaultRootFSMB  int64
	mu               sync.Mutex
	builds           map[string]*buildJob
}

type buildTiming struct {
	Name       string `json:"name"`
	StartedAt  string `json:"started_at"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

type buildLogger struct {
	image   string
	started time.Time
	steps   []buildTiming
	job     *buildJob
}

type buildJob struct {
	ID           string         `json:"id"`
	Image        string         `json:"image"`
	RootFSSizeMB int64          `json:"rootfs_size_mb"`
	State        string         `json:"state"`
	LastLine     string         `json:"last_line"`
	Logs         []string       `json:"logs,omitempty"`
	StartedAt    string         `json:"started_at"`
	FinishedAt   string         `json:"finished_at,omitempty"`
	Error        string         `json:"error,omitempty"`
	Result       map[string]any `json:"result,omitempty"`
	Timings      []buildTiming  `json:"build_timings,omitempty"`
}

func newBuildLogger(image string, job *buildJob) *buildLogger {
	b := &buildLogger{image: image, started: time.Now(), job: job}
	b.logf("buildImage image=%s starting", image)
	return b
}

func (b *buildLogger) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	log.Print(line)
	if b.job != nil {
		b.job.setLastLine(line)
	}
}

func (b *buildLogger) step(name string, fn func() error) error {
	started := time.Now()
	b.logf("buildImage image=%s step=%s starting", b.image, name)
	err := fn()
	timing := buildTiming{
		Name:       name,
		StartedAt:  started.UTC().Format(time.RFC3339Nano),
		DurationMS: time.Since(started).Milliseconds(),
		Status:     "ok",
	}
	if err != nil {
		timing.Status = "error"
		timing.Error = err.Error()
		b.logf("buildImage image=%s step=%s error duration_ms=%d err=%v", b.image, name, timing.DurationMS, err)
	} else {
		b.logf("buildImage image=%s step=%s ok duration_ms=%d", b.image, name, timing.DurationMS)
	}
	b.steps = append(b.steps, timing)
	if b.job != nil {
		b.job.setTimings(b.steps)
	}
	return err
}

func (j *buildJob) setLastLine(line string) {
	buildJobsMu.Lock()
	defer buildJobsMu.Unlock()
	j.LastLine = line
	j.Logs = append(j.Logs, time.Now().UTC().Format(time.RFC3339)+" "+line)
	if len(j.Logs) > 500 {
		j.Logs = append([]string(nil), j.Logs[len(j.Logs)-500:]...)
	}
}

func (j *buildJob) setTimings(timings []buildTiming) {
	buildJobsMu.Lock()
	defer buildJobsMu.Unlock()
	j.Timings = append([]buildTiming(nil), timings...)
}

var buildJobsMu sync.Mutex

func main() {
	ctx := context.Background()
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

	cache, err := storage.NewLocalCacheWithMemory(
		getenv("CACHE_DIR", "/cache"),
		mustInt64(getenv("CACHE_MAX_BYTES", "536870912")),
		mustInt64(getenv("CACHE_MEMORY_MAX_BYTES", "134217728")),
	)
	must(err)

	a := &app{
		backend:          storage.NewBackend(getenv("NODE_ID", "base-image-service"), repo, store, cache),
		repo:             repo,
		workDir:          getenv("WORK_DIR", "/work"),
		containerRuntime: getenv("CONTAINER_RUNTIME", "docker"),
		defaultRootFSMB:  mustInt64(getenv("ROOTFS_SIZE_MB", "2048")),
		builds:           map[string]*buildJob{},
	}
	must(os.MkdirAll(a.workDir, 0o755))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("POST /buildImage", a.buildImage)
	mux.HandleFunc("POST /builds", a.startBuild)
	mux.HandleFunc("GET /builds", a.listBuilds)
	mux.HandleFunc("GET /builds/{id}", a.getBuild)
	mux.HandleFunc("GET /getImageVolume", a.getImageVolume)

	addr := ":" + getenv("PORT", "8080")
	log.Printf("base-image-service listening on %s", addr)
	must(http.ListenAndServe(addr, logRequests(mux)))
}

type buildImageRequest struct {
	Image        string `json:"image"`
	RootFSSizeMB int64  `json:"rootfs_size_mb"`
	ChunkSize    int64  `json:"chunk_size"`
}

func (a *app) buildImage(w http.ResponseWriter, r *http.Request) {
	var req buildImageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := a.runBuild(r.Context(), req, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *app) startBuild(w http.ResponseWriter, r *http.Request) {
	var req buildImageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Image) == "" {
		http.Error(w, "image is required", http.StatusBadRequest)
		return
	}
	if req.RootFSSizeMB == 0 {
		req.RootFSSizeMB = a.defaultRootFSMB
	}
	job := &buildJob{
		ID:           "build-" + uuid.NewString(),
		Image:        req.Image,
		RootFSSizeMB: req.RootFSSizeMB,
		State:        "queued",
		LastLine:     "queued",
		StartedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	a.mu.Lock()
	a.builds[job.ID] = job
	a.mu.Unlock()

	go func() {
		buildJobsMu.Lock()
		job.State = "running"
		job.LastLine = "starting build"
		buildJobsMu.Unlock()
		out, err := a.runBuild(context.Background(), req, job)
		buildJobsMu.Lock()
		defer buildJobsMu.Unlock()
		job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err != nil {
			job.State = "error"
			job.Error = err.Error()
			job.LastLine = err.Error()
			return
		}
		job.State = "done"
		job.Result = out
		job.LastLine = fmt.Sprintf("built %s as %s", out["image_ref"], out["base_image_id"])
	}()

	writeJSON(w, http.StatusAccepted, jobSnapshot(job))
}

func (a *app) getBuild(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	job := a.builds[r.PathValue("id")]
	a.mu.Unlock()
	if job == nil {
		http.Error(w, "build not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, jobSnapshot(job))
}

func (a *app) listBuilds(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	jobs := make([]*buildJob, 0, len(a.builds))
	for _, job := range a.builds {
		jobs = append(jobs, job)
	}
	a.mu.Unlock()
	out := make([]buildJob, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, jobSnapshot(job))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	writeJSON(w, http.StatusOK, out)
}

func jobSnapshot(job *buildJob) buildJob {
	buildJobsMu.Lock()
	defer buildJobsMu.Unlock()
	out := *job
	if job.Result != nil {
		out.Result = make(map[string]any, len(job.Result))
		for k, v := range job.Result {
			out.Result[k] = v
		}
	}
	out.Timings = append([]buildTiming(nil), job.Timings...)
	out.Logs = append([]string(nil), job.Logs...)
	return out
}

func (a *app) runBuild(ctx context.Context, req buildImageRequest, job *buildJob) (map[string]any, error) {
	if req.Image == "" {
		return nil, fmt.Errorf("image is required")
	}
	if req.RootFSSizeMB == 0 {
		req.RootFSSizeMB = a.defaultRootFSMB
	}

	build := newBuildLogger(req.Image, job)
	result, err := a.materializeImage(ctx, req.Image, req.RootFSSizeMB, build)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(result.dir)

	file, err := os.Open(result.rootfsPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	baseImageID := "base-" + uuid.NewString()
	volumeID := "base-vol-" + uuid.NewString()
	var volume storage.Volume
	if err := build.step("create_volume", func() error {
		var err error
		volume, err = a.backend.CreateVolume(ctx, volumeID, info.Size(), req.ChunkSize)
		return err
	}); err != nil {
		return nil, err
	}
	var snapshot storage.Snapshot
	if err := build.step("import_rootfs_snapshot", func() error {
		var err error
		snapshot, err = a.importFile(ctx, volume, file, build)
		return err
	}); err != nil {
		return nil, err
	}
	var baseImage storage.BaseImage
	if err := build.step("record_base_image", func() error {
		var err error
		baseImage, err = a.repo.CreateBaseImage(ctx, storage.BaseImage{
			BaseImageID:     baseImageID,
			ImageRef:        req.Image,
			ImageDigest:     result.digest,
			VolumeID:        volume.VolumeID,
			SnapshotID:      snapshot.SnapshotID,
			RootFSSizeBytes: info.Size(),
			Env:             result.config.Env,
			WorkingDir:      result.config.WorkingDir,
			Entrypoint:      result.config.Entrypoint,
			Cmd:             result.config.Cmd,
			User:            result.config.User,
		})
		return err
	}); err != nil {
		return nil, err
	}
	totalMS := time.Since(build.started).Milliseconds()
	build.logf("buildImage image=%s complete base_image_id=%s volume_id=%s snapshot_id=%s duration_ms=%d", req.Image, baseImage.BaseImageID, baseImage.VolumeID, baseImage.SnapshotID, totalMS)

	return map[string]any{
		"base_image_id":     baseImage.BaseImageID,
		"image_ref":         baseImage.ImageRef,
		"image_digest":      baseImage.ImageDigest,
		"volume_id":         baseImage.VolumeID,
		"snapshot_id":       baseImage.SnapshotID,
		"rootfs_size_bytes": baseImage.RootFSSizeBytes,
		"env":               baseImage.Env,
		"working_dir":       baseImage.WorkingDir,
		"entrypoint":        baseImage.Entrypoint,
		"cmd":               baseImage.Cmd,
		"user":              baseImage.User,
		"duration_ms":       totalMS,
		"build_timings":     build.steps,
	}, nil
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

type materializedImage struct {
	dir        string
	rootfsPath string
	digest     string
	config     imageConfig
}

type imageConfig struct {
	Env        []string `json:"env,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	User       string   `json:"user,omitempty"`
}

func (a *app) materializeImage(ctx context.Context, image string, rootFSSizeMB int64, build *buildLogger) (materializedImage, error) {
	var dir string
	if err := build.step("prepare_workdir", func() error {
		var err error
		dir, err = os.MkdirTemp(a.workDir, "image-build-*")
		return err
	}); err != nil {
		return materializedImage{}, err
	}
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = os.RemoveAll(dir)
		}
	}()

	rootfsTar := filepath.Join(dir, "rootfs.tar")
	inspectPath := filepath.Join(dir, "image-inspect.json")
	rootfsPath := filepath.Join(dir, "rootfs.ext4")
	mountDir := filepath.Join(dir, "mnt")

	if err := build.step("pull_image", func() error {
		if _, err := output(ctx, a.containerRuntime, "image", "inspect", image); err == nil {
			build.logf("buildImage image=%s already available locally", image)
			return nil
		}
		return run(ctx, a.containerRuntime, "pull", image)
	}); err != nil {
		return materializedImage{}, err
	}
	var inspect []byte
	if err := build.step("inspect_image", func() error {
		var err error
		inspect, err = output(ctx, a.containerRuntime, "image", "inspect", image)
		return err
	}); err != nil {
		return materializedImage{}, err
	}
	if err := build.step("write_image_inspect", func() error {
		return os.WriteFile(inspectPath, inspect, 0o644)
	}); err != nil {
		return materializedImage{}, err
	}
	digest := imageDigest(inspect)
	config := dockerImageConfig(inspect)

	var cidRaw []byte
	if err := build.step("create_container", func() error {
		var err error
		cidRaw, err = output(ctx, a.containerRuntime, "create", "--entrypoint", "", image, "true")
		return err
	}); err != nil {
		return materializedImage{}, err
	}
	cid := strings.TrimSpace(string(cidRaw))
	defer func() {
		if cid != "" {
			_ = run(context.Background(), a.containerRuntime, "rm", "-f", cid)
		}
	}()
	if err := build.step("export_rootfs_tar", func() error {
		return runToFileWithProgress(ctx, rootfsTar, build, "export_rootfs_tar", a.containerRuntime, "export", cid)
	}); err != nil {
		return materializedImage{}, err
	}
	if err := build.step("remove_container", func() error {
		return run(ctx, a.containerRuntime, "rm", "-f", cid)
	}); err != nil {
		return materializedImage{}, err
	}
	cid = ""

	if err := build.step("create_rootfs_file", func() error {
		return truncateFile(rootfsPath, rootFSSizeMB*1024*1024)
	}); err != nil {
		return materializedImage{}, err
	}
	if err := build.step("mkfs_ext4", func() error {
		return run(ctx, "mkfs.ext4", "-F", rootfsPath)
	}); err != nil {
		return materializedImage{}, err
	}
	if err := build.step("prepare_mount_dir", func() error {
		return os.MkdirAll(mountDir, 0o755)
	}); err != nil {
		return materializedImage{}, err
	}
	if err := build.step("mount_rootfs", func() error {
		return run(ctx, "mount", "-o", "loop", rootfsPath, mountDir)
	}); err != nil {
		return materializedImage{}, err
	}
	mounted := true
	defer func() {
		if mounted {
			_ = run(context.Background(), "umount", mountDir)
		}
	}()

	if err := build.step("unpack_rootfs_tar", func() error {
		return run(ctx, "tar", "--numeric-owner", "-xf", rootfsTar, "-C", mountDir)
	}); err != nil {
		return materializedImage{}, err
	}
	if err := build.step("prepare_guest_dirs", func() error {
		for _, dir := range []string{"dev", "proc", "sys", "run", "tmp", "etc", "orca"} {
			if err := os.MkdirAll(filepath.Join(mountDir, dir), 0o755); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return materializedImage{}, err
	}
	if err := build.step("write_guest_image_inspect", func() error {
		return copyFile(inspectPath, filepath.Join(mountDir, "etc", "orca-image-inspect.json"), 0o644)
	}); err != nil {
		return materializedImage{}, err
	}
	if err := build.step("write_guest_image_ref", func() error {
		return os.WriteFile(filepath.Join(mountDir, "etc", "orca-image-ref"), []byte(image+"\n"), 0o644)
	}); err != nil {
		return materializedImage{}, err
	}
	meta := fmt.Sprintf("image=%s\nrootfs_size_mb=%d\ncontainer_runtime=%s\nworkdir=%s\nuser=%s\n", image, rootFSSizeMB, a.containerRuntime, config.WorkingDir, config.User)
	if err := build.step("write_guest_rootfs_meta", func() error {
		return os.WriteFile(filepath.Join(mountDir, "etc", "orca-rootfs-from-image"), []byte(meta), 0o644)
	}); err != nil {
		return materializedImage{}, err
	}

	if err := build.step("unmount_rootfs", func() error {
		return run(ctx, "umount", mountDir)
	}); err != nil {
		return materializedImage{}, err
	}
	mounted = false
	cleanupOnError = false
	return materializedImage{dir: dir, rootfsPath: rootfsPath, digest: digest, config: config}, nil
}

func (a *app) importFile(ctx context.Context, volume storage.Volume, file *os.File, build *buildLogger) (storage.Snapshot, error) {
	session, err := a.backend.StartSession(ctx, volume.VolumeID)
	if err != nil {
		return storage.Snapshot{}, err
	}
	defer func() { _ = a.backend.Stop(session.ID) }()

	buf := make([]byte, int(volume.ChunkSize))
	var offset int64
	var chunks int64
	lastLog := time.Now()
	build.logf("buildImage import_rootfs_snapshot volume_id=%s session_id=%s starting chunk_size=%d", volume.VolumeID, session.ID, volume.ChunkSize)
	for {
		n, readErr := io.ReadFull(file, buf)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			return storage.Snapshot{}, readErr
		}
		if n > 0 {
			if err := a.backend.Write(ctx, session.ID, offset, buf[:n]); err != nil {
				return storage.Snapshot{}, err
			}
			offset += int64(n)
			chunks++
			if chunks%16 == 0 || time.Since(lastLog) > 2*time.Second {
				build.logf("buildImage import_rootfs_snapshot volume_id=%s session_id=%s progress chunks=%d bytes=%d", volume.VolumeID, session.ID, chunks, offset)
				lastLog = time.Now()
			}
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
	}
	build.logf("buildImage import_rootfs_snapshot volume_id=%s session_id=%s committing chunks=%d bytes=%d", volume.VolumeID, session.ID, chunks, offset)
	lastCommitLog := time.Now()
	snapshot, err := a.backend.CommitWithOptions(ctx, session.ID, storage.CommitOptions{
		OnProgress: func(p storage.CommitProgress) {
			if p.Phase == "chunks" && time.Since(lastCommitLog) < 2*time.Second && p.DoneChunks < p.TotalChunks {
				return
			}
			lastCommitLog = time.Now()
			build.logf("buildImage import_rootfs_snapshot volume_id=%s session_id=%s commit phase=%s chunks=%d/%d uploaded=%d skipped=%d bytes=%d manifest_items=%d snapshot_id=%s",
				volume.VolumeID, session.ID, p.Phase, p.DoneChunks, p.TotalChunks, p.Uploaded, p.Skipped, p.Bytes, p.ManifestItems, p.SnapshotID)
		},
	})
	if err != nil {
		return storage.Snapshot{}, err
	}
	build.logf("buildImage import_rootfs_snapshot volume_id=%s session_id=%s committed snapshot_id=%s chunks=%d bytes=%d", volume.VolumeID, session.ID, snapshot.SnapshotID, chunks, offset)
	return snapshot, nil
}

func imageDigest(inspect []byte) string {
	var images []struct {
		ID          string   `json:"Id"`
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := json.Unmarshal(inspect, &images); err != nil || len(images) == 0 {
		return ""
	}
	for _, digest := range images[0].RepoDigests {
		if i := strings.LastIndex(digest, "@"); i >= 0 {
			return digest[i+1:]
		}
	}
	return images[0].ID
}

func dockerImageConfig(inspect []byte) imageConfig {
	var images []struct {
		Config struct {
			Env        []string `json:"Env"`
			WorkingDir string   `json:"WorkingDir"`
			Entrypoint []string `json:"Entrypoint"`
			Cmd        []string `json:"Cmd"`
			User       string   `json:"User"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(inspect, &images); err != nil || len(images) == 0 {
		return imageConfig{}
	}
	cfg := images[0].Config
	return imageConfig{
		Env:        append([]string(nil), cfg.Env...),
		WorkingDir: cfg.WorkingDir,
		Entrypoint: append([]string(nil), cfg.Entrypoint...),
		Cmd:        append([]string(nil), cfg.Cmd...),
		User:       cfg.User,
	}
}

func truncateFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = log.Writer()
	cmd.Stderr = log.Writer()
	return cmd.Run()
}

func output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = log.Writer()
	return cmd.Output()
}

func runToFile(ctx context.Context, dst, name string, args ...string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = out
	cmd.Stderr = log.Writer()
	return cmd.Run()
}

func runToFileWithProgress(ctx context.Context, dst string, build *buildLogger, stepName, name string, args ...string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = out
	cmd.Stderr = log.Writer()

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if info, err := os.Stat(dst); err == nil {
					build.logf("buildImage step=%s progress file=%s bytes=%d", stepName, filepath.Base(dst), info.Size())
				}
			}
		}
	}()

	err = cmd.Run()
	if info, statErr := os.Stat(dst); statErr == nil {
		build.logf("buildImage step=%s complete file=%s bytes=%d", stepName, filepath.Base(dst), info.Size())
	}
	return err
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

func mustInt64(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Fatal(err)
	}
	return n
}
