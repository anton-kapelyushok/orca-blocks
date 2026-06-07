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
	"strconv"
	"strings"
	"time"

	"github.com/anton-k/orca-blocks/pkg/storage"
	"github.com/google/uuid"
)

type app struct {
	backend          *storage.Backend
	repo             storage.Repository
	workDir          string
	orcaInitPath     string
	containerRuntime string
	defaultRootFSMB  int64
}

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

	cache, err := storage.NewLocalCache(getenv("CACHE_DIR", "/cache"), mustInt64(getenv("CACHE_MAX_BYTES", "536870912")))
	must(err)

	a := &app{
		backend:          storage.NewBackend(getenv("NODE_ID", "base-image-service"), repo, store, cache),
		repo:             repo,
		workDir:          getenv("WORK_DIR", "/work"),
		orcaInitPath:     getenv("ORCA_INIT_BIN", "/orca-init"),
		containerRuntime: getenv("CONTAINER_RUNTIME", "docker"),
		defaultRootFSMB:  mustInt64(getenv("ROOTFS_SIZE_MB", "2048")),
	}
	must(os.MkdirAll(a.workDir, 0o755))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("POST /buildImage", a.buildImage)
	mux.HandleFunc("GET /getImageVolume", a.getImageVolume)

	addr := ":" + getenv("PORT", "8080")
	log.Printf("base-image-service listening on %s", addr)
	must(http.ListenAndServe(addr, logRequests(mux)))
}

func (a *app) buildImage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image        string `json:"image"`
		RootFSSizeMB int64  `json:"rootfs_size_mb"`
		ChunkSize    int64  `json:"chunk_size"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Image == "" {
		http.Error(w, "image is required", http.StatusBadRequest)
		return
	}
	if req.RootFSSizeMB == 0 {
		req.RootFSSizeMB = a.defaultRootFSMB
	}

	started := time.Now()
	result, err := a.materializeImage(r.Context(), req.Image, req.RootFSSizeMB)
	if err != nil {
		writeError(w, err)
		return
	}
	defer os.RemoveAll(result.dir)

	file, err := os.Open(result.rootfsPath)
	if err != nil {
		writeError(w, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, err)
		return
	}

	baseImageID := "base-" + uuid.NewString()
	volumeID := "base-vol-" + uuid.NewString()
	volume, err := a.backend.CreateVolume(r.Context(), volumeID, info.Size(), req.ChunkSize)
	if err != nil {
		writeError(w, err)
		return
	}
	snapshot, err := a.importFile(r.Context(), volume, file)
	if err != nil {
		writeError(w, err)
		return
	}
	baseImage, err := a.repo.CreateBaseImage(r.Context(), storage.BaseImage{
		BaseImageID:     baseImageID,
		ImageRef:        req.Image,
		ImageDigest:     result.digest,
		VolumeID:        volume.VolumeID,
		SnapshotID:      snapshot.SnapshotID,
		RootFSSizeBytes: info.Size(),
	})
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"base_image_id":     baseImage.BaseImageID,
		"image_ref":         baseImage.ImageRef,
		"image_digest":      baseImage.ImageDigest,
		"volume_id":         baseImage.VolumeID,
		"snapshot_id":       baseImage.SnapshotID,
		"rootfs_size_bytes": baseImage.RootFSSizeBytes,
		"duration_ms":       time.Since(started).Milliseconds(),
	})
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
}

func (a *app) materializeImage(ctx context.Context, image string, rootFSSizeMB int64) (materializedImage, error) {
	dir, err := os.MkdirTemp(a.workDir, "image-build-*")
	if err != nil {
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

	if err := run(ctx, a.containerRuntime, "pull", image); err != nil {
		return materializedImage{}, err
	}
	inspect, err := output(ctx, a.containerRuntime, "image", "inspect", image)
	if err != nil {
		return materializedImage{}, err
	}
	if err := os.WriteFile(inspectPath, inspect, 0o644); err != nil {
		return materializedImage{}, err
	}
	digest := imageDigest(inspect)

	cidRaw, err := output(ctx, a.containerRuntime, "create", "--entrypoint", "", image, "true")
	if err != nil {
		return materializedImage{}, err
	}
	cid := strings.TrimSpace(string(cidRaw))
	defer func() {
		if cid != "" {
			_ = run(context.Background(), a.containerRuntime, "rm", "-f", cid)
		}
	}()
	if err := runToFile(ctx, rootfsTar, a.containerRuntime, "export", cid); err != nil {
		return materializedImage{}, err
	}
	if err := run(ctx, a.containerRuntime, "rm", "-f", cid); err != nil {
		return materializedImage{}, err
	}
	cid = ""

	if err := truncateFile(rootfsPath, rootFSSizeMB*1024*1024); err != nil {
		return materializedImage{}, err
	}
	if err := run(ctx, "mkfs.ext4", "-F", rootfsPath); err != nil {
		return materializedImage{}, err
	}
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return materializedImage{}, err
	}
	if err := run(ctx, "mount", "-o", "loop", rootfsPath, mountDir); err != nil {
		return materializedImage{}, err
	}
	mounted := true
	defer func() {
		if mounted {
			_ = run(context.Background(), "umount", mountDir)
		}
	}()

	if err := run(ctx, "tar", "--numeric-owner", "-xf", rootfsTar, "-C", mountDir); err != nil {
		return materializedImage{}, err
	}
	for _, dir := range []string{"dev", "proc", "sys", "run", "tmp", "etc", "orca"} {
		if err := os.MkdirAll(filepath.Join(mountDir, dir), 0o755); err != nil {
			return materializedImage{}, err
		}
	}
	if err := copyFile(a.orcaInitPath, filepath.Join(mountDir, "init"), 0o755); err != nil {
		return materializedImage{}, err
	}
	if err := copyFile(inspectPath, filepath.Join(mountDir, "etc", "orca-image-inspect.json"), 0o644); err != nil {
		return materializedImage{}, err
	}
	if err := os.WriteFile(filepath.Join(mountDir, "etc", "orca-image-ref"), []byte(image+"\n"), 0o644); err != nil {
		return materializedImage{}, err
	}
	meta := fmt.Sprintf("image=%s\nrootfs_size_mb=%d\ncontainer_runtime=%s\n", image, rootFSSizeMB, a.containerRuntime)
	if err := os.WriteFile(filepath.Join(mountDir, "etc", "orca-rootfs-from-image"), []byte(meta), 0o644); err != nil {
		return materializedImage{}, err
	}

	if err := run(ctx, "umount", mountDir); err != nil {
		return materializedImage{}, err
	}
	mounted = false
	cleanupOnError = false
	return materializedImage{dir: dir, rootfsPath: rootfsPath, digest: digest}, nil
}

func (a *app) importFile(ctx context.Context, volume storage.Volume, file *os.File) (storage.Snapshot, error) {
	session, err := a.backend.StartSession(ctx, volume.VolumeID)
	if err != nil {
		return storage.Snapshot{}, err
	}
	defer func() { _ = a.backend.Stop(session.ID) }()

	buf := make([]byte, int(volume.ChunkSize))
	var offset int64
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
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
	}
	return a.backend.Commit(ctx, session.ID)
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
