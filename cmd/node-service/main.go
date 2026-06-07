package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anton-k/orca-blocks/pkg/nbd"
	"github.com/anton-k/orca-blocks/pkg/storage"
	"github.com/creack/pty"
)

type app struct {
	backend             *storage.Backend
	nbdExports          map[string]*nbd.StorageDevice
	nbdMu               sync.RWMutex
	nbdAddr             string
	nbdPublicAddr       string
	nbdCommitBatch      int
	nbdDefaultCommit    bool
	nbdDeviceStart      int
	nbdDeviceCount      int
	requireNBD          bool
	kvmDevice           string
	requireKVM          bool
	firecrackerBin      string
	firecrackerKernel   string
	firecrackerRootFS   string
	firecrackerInitrd   string
	orcaInitPath        string
	firecrackerBootMode string
	firecrackerWorkDir  string
	firecrackerTimeout  time.Duration
	firecrackerNetwork  bool
	firecrackerGuestDNS string
	mountRoot           string
	nbdDevicePrefix     string
	mountMu             sync.Mutex
	mounts              map[string]*mountedSession
	usedNBDDevices      map[string]struct{}
	ttyMu               sync.Mutex
	ttySessions         map[string]*firecrackerTTYSession
}

type mountedSession struct {
	SessionID string
	NBDDevice string
	MountPath string
}

type firecrackerRunRequest struct {
	Mode             string
	Payload          string
	VCPUCount        int
	MemoryMiB        int
	ImageEnv         []string
	ImageWorkingDir  string
	ImageUser        string
	RequestStartedAt time.Time
	CommitAfterRun   *bool
	SaveMemory       bool
	RestoreMemory    string
	RestoreVMState   string
	RestoreDevice    string
}

type firecrackerTTYSession struct {
	sessionID string
	process   *firecrackerProcess
	stopFn    func() (storage.Snapshot, error)
	timingsFn func() string
	mu        sync.Mutex
	stopped   bool
}

type firecrackerNetwork struct {
	TapName   string
	HostCIDR  string
	GuestCIDR string
	GatewayIP string
	GuestIP   string
	GuestMAC  string
	DNS       string
}

type firecrackerStepTiming struct {
	Name       string `json:"name"`
	StartedAt  string `json:"started_at"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
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
		backend:             storage.NewBackend(nodeID, repo, store, cache),
		nbdExports:          map[string]*nbd.StorageDevice{},
		nbdAddr:             getenv("NBD_ADDR", ""),
		nbdPublicAddr:       getenv("NBD_PUBLIC_ADDR", ""),
		nbdCommitBatch:      int(mustInt64(getenv("NBD_COMMIT_BATCH_CHUNKS", "16"))),
		nbdDefaultCommit:    getenv("NBD_COMMIT_ON_DISCONNECT", "false") == "true",
		nbdDeviceStart:      int(mustInt64(getenv("NBD_DEVICE_START", "0"))),
		nbdDeviceCount:      int(mustInt64(getenv("NBD_DEVICE_COUNT", "16"))),
		requireNBD:          getenv("REQUIRE_NBD_DEVICES", "false") == "true",
		kvmDevice:           getenv("KVM_DEVICE", "/dev/kvm"),
		requireKVM:          getenv("REQUIRE_KVM", "false") == "true",
		firecrackerBin:      getenv("FIRECRACKER_BIN", "/firecracker-assets/firecracker"),
		firecrackerKernel:   getenv("FIRECRACKER_KERNEL", "/firecracker-assets/vmlinux"),
		firecrackerRootFS:   getenv("FIRECRACKER_ROOTFS", "/firecracker-assets/rootfs.ext4"),
		firecrackerInitrd:   getenv("FIRECRACKER_INITRD", "/firecracker-assets/initramfs.cpio.gz"),
		orcaInitPath:        getenv("ORCA_INIT_BIN", "/orca-init"),
		firecrackerBootMode: getenv("FIRECRACKER_BOOT_MODE", "initramfs"),
		firecrackerWorkDir:  getenv("FIRECRACKER_WORK_DIR", "/tmp/orca-firecracker"),
		firecrackerTimeout:  time.Duration(mustInt64(getenv("FIRECRACKER_TIMEOUT_SECONDS", "30"))) * time.Second,
		firecrackerNetwork:  getenv("FIRECRACKER_NETWORK", "true") == "true",
		firecrackerGuestDNS: getenv("FIRECRACKER_GUEST_DNS", "1.1.1.1"),
		mountRoot:           getenv("MOUNT_ROOT", "/mnt/orca-sessions"),
		nbdDevicePrefix:     getenv("NBD_DEVICE_PREFIX", "/dev/nbd"),
		mounts:              map[string]*mountedSession{},
		usedNBDDevices:      map[string]struct{}{},
		ttySessions:         map[string]*firecrackerTTYSession{},
	}
	if err := a.preflightKVM(); err != nil {
		if a.requireKVM {
			log.Fatal(err)
		}
		log.Printf("KVM preflight warning: %v", err)
	}
	if nbdAddr := a.nbdAddr; nbdAddr != "" {
		if err := runCommand("modprobe", "nbd", "max_part=8"); err != nil {
			log.Printf("NBD module load warning: %v", err)
		}
		if err := a.preflightNBDDevices(); err != nil {
			if a.requireNBD {
				log.Fatal(err)
			}
			log.Printf("NBD device preflight warning: %v", err)
		}
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
	mux.HandleFunc("GET /sessions/{id}/tty/output", a.ttyOutput)
	mux.HandleFunc("POST /sessions/{id}/tty/input", a.ttyInput)
	mux.HandleFunc("POST /sessions/{id}/tty/stop", a.ttyStop)

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
	requestStartedAt := time.Now()
	var req struct {
		Runtime            string   `json:"runtime"`
		VolumeID           string   `json:"volume_id"`
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
		ImageUser          string   `json:"image_user"`
		CommitAfterRun     *bool    `json:"commit_after_run"`
		SaveMemory         bool     `json:"save_memory_snapshot"`
		RestoreMemory      string   `json:"restore_memory_snapshot_path"`
		RestoreVMState     string   `json:"restore_vmstate_snapshot_path"`
		RestoreDevice      string   `json:"restore_firecracker_device"`
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
	runtime := normalizeRuntime(req.Runtime, req.Frontend)
	resp["runtime"] = runtime
	switch runtime {
	case "http-block":
	case "nbd-export-test":
		if a.nbdPublicAddr == "" {
			_ = a.backend.Stop(session.ID)
			writeError(w, fmt.Errorf("NBD test export is not enabled on this node"))
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
	case "mounted-fs":
		if err := a.startMountedSession(session, req.Format, req.FSType); err != nil {
			a.unregisterNBDExport(session.ID)
			_ = a.backend.Stop(session.ID)
			writeError(w, err)
			return
		}
		a.mountMu.Lock()
		mounted := a.mounts[session.ID]
		a.mountMu.Unlock()
		resp["mount_path"] = mounted.MountPath
		resp["nbd_device"] = mounted.NBDDevice
	case "firecracker":
		result, err := a.runFirecrackerSession(r.Context(), session, firecrackerRunRequest{
			Mode:             req.FirecrackerMode,
			Payload:          req.FirecrackerPayload,
			VCPUCount:        req.VCPUCount,
			MemoryMiB:        req.MemoryMiB,
			ImageEnv:         req.ImageEnv,
			ImageWorkingDir:  req.ImageWorkingDir,
			ImageUser:        req.ImageUser,
			RequestStartedAt: requestStartedAt,
			CommitAfterRun:   req.CommitAfterRun,
			SaveMemory:       req.SaveMemory,
			RestoreMemory:    req.RestoreMemory,
			RestoreVMState:   req.RestoreVMState,
			RestoreDevice:    req.RestoreDevice,
		})
		if err != nil {
			a.unregisterNBDExport(session.ID)
			_ = a.backend.Stop(session.ID)
			writeError(w, err)
			return
		}
		for k, v := range result {
			resp[k] = v
		}
	default:
		_ = a.backend.Stop(session.ID)
		writeError(w, fmt.Errorf("unsupported runtime %q", runtime))
		return
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
	sessionID := r.PathValue("id")
	if err := a.detachMountedSession(sessionID); err != nil {
		writeError(w, err)
		return
	}
	snapshot, err := a.backend.Commit(r.Context(), sessionID)
	if err != nil {
		writeError(w, err)
		return
	}
	a.unregisterNBDExport(sessionID)
	writeJSON(w, http.StatusCreated, snapshot)
}

func (a *app) stop(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if err := a.detachMountedSession(sessionID); err != nil {
		writeError(w, err)
		return
	}
	a.unregisterNBDExport(sessionID)
	if err := a.backend.Stop(sessionID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"stopped": sessionID})
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

func (a *app) startMountedSession(session *storage.Session, format bool, fsType string) error {
	if a.nbdAddr == "" {
		return fmt.Errorf("mounted-fs runtime is not enabled on this node")
	}
	if err := a.preflightNBDDevices(); err != nil {
		return err
	}
	if fsType == "" {
		fsType = "ext4"
	}
	if fsType != "ext4" {
		return fmt.Errorf("unsupported fs_type %q", fsType)
	}
	if err := os.MkdirAll(a.mountRoot, 0o755); err != nil {
		return err
	}
	device, err := a.allocateNBDDevice()
	if err != nil {
		return err
	}
	mountPath := filepath.Join(a.mountRoot, session.ID)
	cleanup := true
	defer func() {
		if cleanup {
			a.unregisterNBDExport(session.ID)
			_ = runCommand("nbd-client", "-d", device)
			a.releaseNBDDevice(device)
			_ = os.RemoveAll(mountPath)
		}
	}()

	a.registerNBDExport(session.ID, &nbd.StorageDevice{
		Backend:            a.backend,
		SessionID:          session.ID,
		SizeBytes:          session.Volume.SizeBytes,
		CommitOnDisconnect: false,
		CommitOptions: storage.CommitOptions{
			UploadBatchChunks: a.nbdCommitBatch,
		},
	})
	nbdHost, nbdPort := localNBDClientTarget(a.nbdAddr)
	if err := runCommand("nbd-client", nbdHost, nbdPort, device, "-N", session.ID); err != nil {
		a.unregisterNBDExport(session.ID)
		return err
	}
	if format {
		if err := runCommand("mkfs.ext4", "-F", device); err != nil {
			a.unregisterNBDExport(session.ID)
			return err
		}
	}
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		a.unregisterNBDExport(session.ID)
		return err
	}
	if err := runCommand("mount", device, mountPath); err != nil {
		a.unregisterNBDExport(session.ID)
		return err
	}

	a.mountMu.Lock()
	a.mounts[session.ID] = &mountedSession{
		SessionID: session.ID,
		NBDDevice: device,
		MountPath: mountPath,
	}
	a.mountMu.Unlock()
	cleanup = false
	return nil
}

func (a *app) runFirecrackerSession(ctx context.Context, session *storage.Session, req firecrackerRunRequest) (map[string]string, error) {
	timings := []firecrackerStepTiming{}
	record := func(name string, started time.Time, err error) {
		timing := firecrackerStepTiming{
			Name:       name,
			StartedAt:  started.UTC().Format(time.RFC3339Nano),
			DurationMS: time.Since(started).Milliseconds(),
			Status:     "ok",
		}
		if err != nil {
			timing.Status = "error"
			timing.Error = err.Error()
		}
		timings = append(timings, timing)
	}
	recordStatus := func(name string, started time.Time, ended time.Time, status string, err error) {
		timing := firecrackerStepTiming{
			Name:       name,
			StartedAt:  started.UTC().Format(time.RFC3339Nano),
			DurationMS: ended.Sub(started).Milliseconds(),
			Status:     status,
		}
		if err != nil {
			timing.Error = err.Error()
		}
		timings = append(timings, timing)
	}
	writeTimings := func(path string) {
		raw, err := json.MarshalIndent(timings, "", "  ")
		if err != nil {
			log.Printf("marshal firecracker timings failed: %v", err)
			return
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			log.Printf("write firecracker timings failed: %v", err)
		}
	}
	timingsText := func() string {
		return timingsJSON(timings)
	}

	mode := req.Mode
	if mode == "" {
		mode = "smoke"
	}
	if mode != "smoke" && mode != "write" && mode != "read" && mode != "restore" && mode != "docker-smoke" && mode != "docker-read" && mode != "image-rootfs-smoke" && mode != "image-rootfs-run" && mode != "image-rootfs-tty" {
		return nil, fmt.Errorf("unsupported firecracker_mode %q", mode)
	}
	if mode == "restore" && (req.RestoreMemory == "" || req.RestoreVMState == "" || req.RestoreDevice == "") {
		return nil, fmt.Errorf("firecracker restore requires restore_memory_snapshot_path, restore_vmstate_snapshot_path, and restore_firecracker_device")
	}
	payload := req.Payload
	if payload == "" {
		payload = "hello-from-firecracker"
	}
	vcpuCount := req.VCPUCount
	if vcpuCount <= 0 {
		vcpuCount = 1
	}
	memoryMiB := req.MemoryMiB
	if memoryMiB <= 0 {
		memoryMiB = 128
	}
	commitAfterRun := firecrackerModeWritesVolume(mode)
	if req.CommitAfterRun != nil {
		commitAfterRun = *req.CommitAfterRun
	}
	started := time.Now()
	if err := a.preflightFirecracker(); err != nil {
		record("preflight_firecracker", started, err)
		return nil, err
	}
	record("preflight_firecracker", started, nil)
	if a.nbdAddr == "" {
		return nil, fmt.Errorf("firecracker runtime requires NBD_ADDR to attach the Orca volume")
	}
	started = time.Now()
	if err := a.preflightNBDDevices(); err != nil {
		record("preflight_nbd", started, err)
		return nil, err
	}
	record("preflight_nbd", started, nil)
	started = time.Now()
	device, err := a.allocateNBDDevicePrefer(req.RestoreDevice)
	if err != nil {
		record("allocate_nbd_device", started, err)
		return nil, err
	}
	record("allocate_nbd_device", started, nil)
	workDir := filepath.Join(a.firecrackerWorkDir, session.ID)
	rootfsCopy := filepath.Join(workDir, "rootfs.ext4")
	initrdCopy := filepath.Join(workDir, "initramfs.cpio.gz")
	socketPath := filepath.Join(workDir, "firecracker.sock")
	configPath := filepath.Join(workDir, "firecracker.json")
	logPath := filepath.Join(workDir, "firecracker.log")
	serialPath := filepath.Join(workDir, "serial.log")
	timingsPath := filepath.Join(workDir, "timings.json")
	vmStateSnapshotPath := filepath.Join(workDir, "vmstate.snap")
	memSnapshotPath := filepath.Join(workDir, "memory.snap")
	var fcNet *firecrackerNetwork
	detached := false
	detach := func() {
		if detached {
			return
		}
		started := time.Now()
		err := runCommand("nbd-client", "-d", device)
		record("detach_nbd_device", started, err)
		a.unregisterNBDExport(session.ID)
		a.releaseNBDDevice(device)
		if fcNet != nil {
			started = time.Now()
			err := cleanupFirecrackerNetwork(*fcNet)
			record("cleanup_firecracker_network", started, err)
		}
		detached = true
	}
	cleanupOnReturn := true
	defer func() {
		if cleanupOnReturn {
			detach()
			writeTimings(timingsPath)
		}
	}()

	started = time.Now()
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		record("prepare_workdir", started, err)
		return nil, err
	}
	record("prepare_workdir", started, nil)

	firecrackerBootMode := a.firecrackerBootMode
	var guestDataDevice string
	var rootfsPath string
	var initrdPath string
	rootfsReadOnly := false
	rootfsCopied := false
	if firecrackerModeUsesSessionRootFS(mode) {
		firecrackerBootMode = "image-rootfs"
		guestDataDevice = "/dev/vda"
	} else {
		switch firecrackerBootMode {
		case "rootfs":
			guestDataDevice = "/dev/vdb"
			if mode != "restore" {
				started = time.Now()
				if err := copyFile(a.firecrackerRootFS, rootfsCopy); err != nil {
					record("copy_rootfs", started, err)
					return nil, fmt.Errorf("copy firecracker rootfs: %w", err)
				}
				record("copy_rootfs", started, nil)
				rootfsPath = rootfsCopy
				rootfsCopied = true
			}
		case "initramfs":
			guestDataDevice = "/dev/vda"
			if mode != "restore" {
				started = time.Now()
				if err := copyFile(a.firecrackerInitrd, initrdCopy); err != nil {
					record("copy_initramfs", started, err)
					return nil, fmt.Errorf("copy firecracker initramfs: %w", err)
				}
				record("copy_initramfs", started, nil)
				initrdPath = initrdCopy
			}
		default:
			return nil, fmt.Errorf("unsupported FIRECRACKER_BOOT_MODE %q", firecrackerBootMode)
		}
	}

	started = time.Now()
	a.registerNBDExport(session.ID, &nbd.StorageDevice{
		Backend:            a.backend,
		SessionID:          session.ID,
		SizeBytes:          session.Volume.SizeBytes,
		CommitOnDisconnect: false,
		CommitOptions: storage.CommitOptions{
			UploadBatchChunks: a.nbdCommitBatch,
		},
	})
	record("register_nbd_export", started, nil)
	nbdHost, nbdPort := localNBDClientTarget(a.nbdAddr)
	started = time.Now()
	if err := runCommand("nbd-client", nbdHost, nbdPort, device, "-N", session.ID); err != nil {
		record("attach_nbd_device", started, err)
		return nil, err
	}
	record("attach_nbd_device", started, nil)

	if firecrackerModeUsesSessionRootFS(mode) {
		started = time.Now()
		if err := a.sideloadOrcaInit(device, filepath.Join(workDir, "sideload-root")); err != nil {
			record("sideload_orca_init", started, err)
			return nil, err
		}
		record("sideload_orca_init", started, nil)
	}

	if mode == "restore" {
		resp, err := a.restoreFirecrackerSession(ctx, session, device, workDir, socketPath, configPath, logPath, serialPath, timingsPath, req, record, detach, writeTimings, timingsText)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}

	createMemorySnapshot := firecrackerModeWritesVolume(mode) && commitAfterRun && req.SaveMemory
	bootArgs := firecrackerBootArgs(firecrackerBootMode, mode, payload, guestDataDevice, createMemorySnapshot)
	successMarker := "orca-init: " + mode + " ok"
	if firecrackerModeUsesSessionRootFS(mode) {
		rootfsPath = device
		initrdPath = ""
		guestDataDevice = ""
		rootfsReadOnly = mode == "image-rootfs-smoke"
		if a.firecrackerNetwork {
			started = time.Now()
			network, err := setupFirecrackerNetwork(session.ID, a.firecrackerGuestDNS)
			record("setup_firecracker_network", started, err)
			if err != nil {
				return nil, err
			}
			fcNet = &network
		}
		bootArgs = imageRootFSBootArgs(payload, rootfsReadOnly, mode == "image-rootfs-tty", req.ImageWorkingDir, req.ImageEnv, req.ImageUser, fcNet)
		successMarker = "orca-init: image-rootfs ok"
		if mode == "image-rootfs-tty" {
			successMarker = "orca-init: tty ready"
		}
	}
	started = time.Now()
	if err := writeFirecrackerConfig(configPath, a.firecrackerKernel, rootfsPath, rootfsReadOnly, initrdPath, guestDataDevice, false, logPath, bootArgs, vcpuCount, memoryMiB, fcNet); err != nil {
		record("write_firecracker_config", started, err)
		return nil, err
	}
	record("write_firecracker_config", started, nil)

	var runCtx context.Context
	var cancel context.CancelFunc
	if mode == "image-rootfs-tty" {
		runCtx, cancel = context.WithCancel(context.Background())
	} else {
		runCtx, cancel = context.WithTimeout(ctx, a.firecrackerTimeout)
		defer cancel()
	}
	log.Printf("running firecracker session=%s mode=%s device=%s", session.ID, mode, device)
	cmd := exec.CommandContext(runCtx, a.firecrackerBin, "--api-sock", socketPath, "--config-file", configPath)
	started = time.Now()
	run, err := startFirecrackerProcess(cmd, serialPath, successMarker)
	if err != nil {
		record("run_firecracker", started, err)
		return nil, err
	}
	if err := run.waitForMarkerOrExit(runCtx); err != nil {
		serial := run.output()
		record("run_firecracker", started, err)
		run.stop()
		return nil, fmt.Errorf("%w: %s", err, tail(serial, 4096))
	}
	record("run_firecracker", started, nil)
	if firstOutputAt, ok := run.firstUserOutputAt(); ok && !req.RequestStartedAt.IsZero() {
		recordStatus("request_to_first_user_output", req.RequestStartedAt, firstOutputAt, "ok", nil)
	} else if !req.RequestStartedAt.IsZero() {
		recordStatus("request_to_first_user_output", req.RequestStartedAt, time.Now(), "not_observed", nil)
	}
	if mode == "image-rootfs-tty" {
		cleanupOnReturn = false
		ttySession := &firecrackerTTYSession{
			sessionID: session.ID,
			process:   run,
			timingsFn: func() string {
				return timingsJSON(timings)
			},
		}
		ttySession.stopFn = func() (storage.Snapshot, error) {
			_ = run.writeInput("sync\nexit\n")
			if err := run.waitTimeout(3 * time.Second); err != nil {
				run.stop()
			}
			cancel()
			started := time.Now()
			if err := runCommand("sync"); err != nil {
				record("sync_host", started, err)
				writeTimings(timingsPath)
				return storage.Snapshot{}, err
			}
			record("sync_host", started, nil)
			started = time.Now()
			if err := runCommand("blockdev", "--flushbufs", device); err != nil {
				record("flush_nbd_device", started, err)
				writeTimings(timingsPath)
				return storage.Snapshot{}, err
			}
			record("flush_nbd_device", started, nil)
			detach()
			started = time.Now()
			snapshot, err := a.backend.CommitWithOptions(context.Background(), session.ID, storage.CommitOptions{
				UploadBatchChunks: a.nbdCommitBatch,
			})
			record("commit_snapshot", started, err)
			_ = a.backend.Stop(session.ID)
			writeTimings(timingsPath)
			return snapshot, err
		}
		a.ttyMu.Lock()
		a.ttySessions[session.ID] = ttySession
		a.ttyMu.Unlock()
		resp := map[string]string{
			"firecracker_mode":      mode,
			"firecracker_boot_mode": firecrackerBootMode,
			"firecracker_device":    device,
			"firecracker_output":    orcaInitLines(run.output()),
			"firecracker_work_dir":  workDir,
			"cpu_count":             strconv.Itoa(vcpuCount),
			"memory_mib":            strconv.Itoa(memoryMiB),
			"tty":                   "true",
		}
		resp["firecracker_timings"] = timingsJSON(timings)
		writeTimings(timingsPath)
		return resp, nil
	}
	if createMemorySnapshot {
		started = time.Now()
		if err := waitForPath(socketPath, 2*time.Second); err != nil {
			record("wait_firecracker_api", started, err)
			run.stop()
			return nil, err
		}
		record("wait_firecracker_api", started, nil)
		started = time.Now()
		if err := pauseFirecracker(socketPath); err != nil {
			record("pause_firecracker", started, err)
			run.stop()
			return nil, err
		}
		record("pause_firecracker", started, nil)
		started = time.Now()
		if err := createFirecrackerSnapshot(socketPath, vmStateSnapshotPath, memSnapshotPath); err != nil {
			record("create_memory_snapshot", started, err)
			run.stop()
			return nil, err
		}
		record("create_memory_snapshot", started, nil)
		run.stop()
	} else if err := run.wait(); err != nil {
		serial := run.output()
		record("wait_firecracker_exit", started, err)
		return nil, fmt.Errorf("firecracker failed after guest success: %w: %s", err, tail(serial, 4096))
	}
	serial := run.output()
	if rootfsCopied && rootfsPath != "" && !createMemorySnapshot {
		started = time.Now()
		if err := os.Remove(rootfsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			record("remove_rootfs_copy", started, err)
			return nil, err
		}
		record("remove_rootfs_copy", started, nil)
		rootfsPath = ""
	}
	if firecrackerModeWritesVolume(mode) {
		started = time.Now()
		if err := runCommand("sync"); err != nil {
			record("sync_host", started, err)
			return nil, err
		}
		record("sync_host", started, nil)
		started = time.Now()
		if err := runCommand("blockdev", "--flushbufs", device); err != nil {
			record("flush_nbd_device", started, err)
			return nil, err
		}
		record("flush_nbd_device", started, nil)
	}

	resp := map[string]string{
		"firecracker_mode":      mode,
		"firecracker_boot_mode": firecrackerBootMode,
		"firecracker_device":    device,
		"firecracker_output":    orcaInitLines(serial),
		"firecracker_work_dir":  workDir,
		"cpu_count":             strconv.Itoa(vcpuCount),
		"memory_mib":            strconv.Itoa(memoryMiB),
	}
	if createMemorySnapshot {
		resp["memory_snapshot_path"] = memSnapshotPath
		resp["vmstate_snapshot_path"] = vmStateSnapshotPath
	}
	detach()
	resp["firecracker_timings"] = timingsJSON(timings)
	if commitAfterRun {
		started = time.Now()
		snapshot, err := a.backend.CommitWithOptions(ctx, session.ID, storage.CommitOptions{
			UploadBatchChunks: a.nbdCommitBatch,
		})
		if err != nil {
			record("commit_snapshot", started, err)
			resp["firecracker_timings"] = timingsJSON(timings)
			return nil, err
		}
		record("commit_snapshot", started, nil)
		resp["snapshot_id"] = snapshot.SnapshotID
		resp["manifest_key"] = snapshot.ManifestKey
		resp["firecracker_timings"] = timingsJSON(timings)
	}
	return resp, nil
}

func firecrackerModeWritesVolume(mode string) bool {
	return mode == "write" || mode == "docker-smoke" || mode == "image-rootfs-run"
}

func firecrackerModeUsesSessionRootFS(mode string) bool {
	return mode == "image-rootfs-smoke" || mode == "image-rootfs-run" || mode == "image-rootfs-tty"
}

func (a *app) sideloadOrcaInit(device, mountPath string) error {
	if a.orcaInitPath == "" {
		return fmt.Errorf("ORCA_INIT_BIN is empty")
	}
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		return err
	}
	log.Printf("sideloading orca-init from %s onto %s", a.orcaInitPath, device)
	if err := runCommand("mount", device, mountPath); err != nil {
		return err
	}
	mounted := true
	defer func() {
		if mounted {
			_ = runCommand("umount", mountPath)
		}
	}()
	initPath := filepath.Join(mountPath, "init")
	if err := os.Remove(initPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := copyFileMode(a.orcaInitPath, initPath, 0o755); err != nil {
		return err
	}
	if err := runCommand("sync"); err != nil {
		return err
	}
	if err := runCommand("umount", mountPath); err != nil {
		return err
	}
	mounted = false
	if err := runCommand("blockdev", "--flushbufs", device); err != nil {
		return err
	}
	return os.RemoveAll(mountPath)
}

func (a *app) detachMountedSession(sessionID string) error {
	a.mountMu.Lock()
	mounted, ok := a.mounts[sessionID]
	a.mountMu.Unlock()
	if !ok {
		return nil
	}
	if err := runCommand("sync"); err != nil {
		return err
	}
	if err := runCommand("umount", mounted.MountPath); err != nil {
		return err
	}
	if err := runCommand("nbd-client", "-d", mounted.NBDDevice); err != nil {
		return err
	}
	a.releaseNBDDevice(mounted.NBDDevice)
	a.mountMu.Lock()
	delete(a.mounts, sessionID)
	a.mountMu.Unlock()
	return os.RemoveAll(mounted.MountPath)
}

func (a *app) restoreFirecrackerSession(
	ctx context.Context,
	session *storage.Session,
	device string,
	workDir string,
	socketPath string,
	configPath string,
	logPath string,
	serialPath string,
	timingsPath string,
	req firecrackerRunRequest,
	record func(string, time.Time, error),
	detach func(),
	writeTimings func(string),
	timingsText func() string,
) (map[string]string, error) {
	started := time.Now()
	if err := writeFirecrackerRestoreConfig(configPath, req.RestoreVMState, req.RestoreMemory, device, logPath); err != nil {
		record("write_firecracker_restore_config", started, err)
		return nil, err
	}
	record("write_firecracker_restore_config", started, nil)
	runCtx, cancel := context.WithTimeout(ctx, a.firecrackerTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, a.firecrackerBin, "--api-sock", socketPath)
	started = time.Now()
	run, err := startFirecrackerProcess(cmd, serialPath, "orca-restore-never")
	if err != nil {
		record("start_firecracker_restore", started, err)
		return nil, err
	}
	record("start_firecracker_restore", started, nil)
	defer func() {
		run.stop()
		detach()
		writeTimings(timingsPath)
	}()

	started = time.Now()
	if err := waitForPath(socketPath, 2*time.Second); err != nil {
		record("wait_firecracker_api", started, err)
		return nil, err
	}
	record("wait_firecracker_api", started, nil)

	started = time.Now()
	if err := configureFirecrackerLogger(socketPath, logPath); err != nil {
		record("configure_firecracker_logger", started, err)
		return nil, err
	}
	record("configure_firecracker_logger", started, nil)

	started = time.Now()
	if err := loadFirecrackerSnapshot(socketPath, req.RestoreVMState, req.RestoreMemory, true); err != nil {
		record("restore_memory_snapshot", started, err)
		return nil, err
	}
	record("restore_memory_snapshot", started, nil)

	time.Sleep(100 * time.Millisecond)
	resp := map[string]string{
		"firecracker_mode":          "restore",
		"firecracker_boot_mode":     a.firecrackerBootMode,
		"firecracker_device":        device,
		"firecracker_output":        orcaInitLines(run.output()),
		"firecracker_work_dir":      workDir,
		"memory_snapshot_path":      req.RestoreMemory,
		"vmstate_snapshot_path":     req.RestoreVMState,
		"restored_memory_snapshot":  req.RestoreMemory,
		"restored_vmstate_snapshot": req.RestoreVMState,
	}
	resp["firecracker_timings"] = timingsText()
	return resp, nil
}

type firecrackerProcess struct {
	cmd                *exec.Cmd
	stdin              io.WriteCloser
	marker             string
	markerCh           chan struct{}
	doneCh             chan struct{}
	doneErr            error
	closeFile          func()
	markerOnce         sync.Once
	stopOnce           sync.Once
	userOutputOnce     sync.Once
	mu                 sync.Mutex
	outputBuf          bytes.Buffer
	firstUserOutput    time.Time
	hasFirstUserOutput bool
}

func startFirecrackerProcess(cmd *exec.Cmd, serialPath, marker string) (*firecrackerProcess, error) {
	serialFile, err := os.OpenFile(serialPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	ttyFile, err := pty.Start(cmd)
	if err != nil {
		_ = serialFile.Close()
		return nil, err
	}
	p := &firecrackerProcess{
		cmd:      cmd,
		stdin:    ttyFile,
		marker:   marker,
		markerCh: make(chan struct{}),
		doneCh:   make(chan struct{}),
		closeFile: func() {
			_ = ttyFile.Close()
			_ = serialFile.Close()
		},
	}
	collect := func(r io.Reader) {
		buf := make([]byte, 4096)
		var pendingLine strings.Builder
		capturingLegacyUserOutput := false
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				p.appendOutput(serialFile, chunk)
				p.signalMarkerIfOutputContainsMarker()
				pendingLine.WriteString(chunk)
				for {
					lineBuf := pendingLine.String()
					idx := strings.IndexByte(lineBuf, '\n')
					if idx < 0 {
						break
					}
					line := lineBuf[:idx+1]
					pendingLine.Reset()
					pendingLine.WriteString(lineBuf[idx+1:])
					userOutput := isUserOutputLine(line, capturingLegacyUserOutput)
					switch {
					case strings.Contains(line, "orca-init: started image-rootfs command="):
						capturingLegacyUserOutput = true
					case strings.Contains(line, "orca-init: command ok") || strings.Contains(line, "orca-init: command failed"):
						capturingLegacyUserOutput = false
					}
					if userOutput {
						p.signalUserOutput(time.Now())
					}
				}
			}
			if err != nil {
				return
			}
		}
	}
	go collect(ttyFile)
	go func() {
		err := cmd.Wait()
		p.signalMarkerIfOutputContainsMarker()
		_ = serialFile.Sync()
		_ = serialFile.Close()
		p.mu.Lock()
		p.doneErr = err
		p.mu.Unlock()
		close(p.doneCh)
	}()
	return p, nil
}

func (p *firecrackerProcess) appendOutput(file *os.File, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.outputBuf.WriteString(line)
	_, _ = file.WriteString(line)
}

func (p *firecrackerProcess) signalMarkerIfOutputContainsMarker() {
	p.mu.Lock()
	contains := strings.Contains(p.outputBuf.String(), p.marker)
	p.mu.Unlock()
	if contains {
		p.signalMarker()
	}
}

func (p *firecrackerProcess) signalMarker() {
	p.markerOnce.Do(func() {
		close(p.markerCh)
	})
}

func (p *firecrackerProcess) signalUserOutput(at time.Time) {
	p.userOutputOnce.Do(func() {
		p.mu.Lock()
		p.firstUserOutput = at
		p.hasFirstUserOutput = true
		p.mu.Unlock()
	})
}

func isUserOutputLine(line string, capturingLegacyUserOutput bool) bool {
	if strings.Contains(line, "orca-stdout:") || strings.Contains(line, "orca-stderr:") {
		return true
	}
	return capturingLegacyUserOutput && !strings.Contains(line, "orca-init:")
}

func (p *firecrackerProcess) waitForMarkerOrExit(ctx context.Context) error {
	select {
	case <-p.markerCh:
		return nil
	case <-p.doneCh:
		err := p.err()
		if err != nil {
			return fmt.Errorf("firecracker exited before guest success marker: %w", err)
		}
		if !strings.Contains(p.output(), p.marker) {
			return fmt.Errorf("firecracker exited without guest success marker %q", p.marker)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *firecrackerProcess) wait() error {
	<-p.doneCh
	p.stopOnce.Do(p.closeFile)
	return p.err()
}

func (p *firecrackerProcess) waitTimeout(timeout time.Duration) error {
	select {
	case <-p.doneCh:
		p.stopOnce.Do(p.closeFile)
		return p.err()
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for firecracker process")
	}
}

func (p *firecrackerProcess) stop() {
	p.stopOnce.Do(func() {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		<-p.doneCh
		p.closeFile()
	})
}

func (p *firecrackerProcess) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.doneErr
}

func (p *firecrackerProcess) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outputBuf.String()
}

func (p *firecrackerProcess) outputSince(offset int) (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.outputBuf.String()
	if offset < 0 {
		offset = 0
	}
	if offset > len(s) {
		offset = len(s)
	}
	return s[offset:], len(s)
}

func (p *firecrackerProcess) writeInput(input string) error {
	select {
	case <-p.doneCh:
		return fmt.Errorf("firecracker process has exited")
	default:
	}
	if p.stdin == nil {
		return fmt.Errorf("firecracker stdin is not available")
	}
	_, err := io.WriteString(p.stdin, input)
	return err
}

func (p *firecrackerProcess) isDone() bool {
	select {
	case <-p.doneCh:
		return true
	default:
		return false
	}
}

func (p *firecrackerProcess) firstUserOutputAt() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstUserOutput, p.hasFirstUserOutput
}

func (a *app) allocateNBDDevice() (string, error) {
	return a.allocateNBDDevicePrefer("")
}

func (a *app) allocateNBDDevicePrefer(preferred string) (string, error) {
	a.mountMu.Lock()
	defer a.mountMu.Unlock()
	if preferred != "" {
		info, err := os.Stat(preferred)
		if err != nil {
			return "", fmt.Errorf("preferred NBD device %s is not visible: %w", preferred, err)
		}
		if info.Mode()&os.ModeDevice == 0 {
			return "", fmt.Errorf("preferred NBD device %s exists but is not a device", preferred)
		}
		if _, used := a.usedNBDDevices[preferred]; used {
			return "", fmt.Errorf("preferred NBD device %s is already allocated", preferred)
		}
		a.usedNBDDevices[preferred] = struct{}{}
		return preferred, nil
	}
	visible := 0
	for i := a.nbdDeviceStart; i < a.nbdDeviceStart+a.nbdDeviceCount; i++ {
		device := fmt.Sprintf("%s%d", a.nbdDevicePrefix, i)
		if _, err := os.Stat(device); err != nil {
			continue
		}
		visible++
		if _, used := a.usedNBDDevices[device]; used {
			continue
		}
		a.usedNBDDevices[device] = struct{}{}
		return device, nil
	}
	if visible == 0 {
		return "", fmt.Errorf("no NBD devices visible at %s[%d..%d); load the host nbd module with remote-setup and run the node with privileged/device access", a.nbdDevicePrefix, a.nbdDeviceStart, a.nbdDeviceStart+a.nbdDeviceCount)
	}
	return "", fmt.Errorf("all %d visible NBD devices are already allocated on this node", visible)
}

func (a *app) releaseNBDDevice(device string) {
	a.mountMu.Lock()
	defer a.mountMu.Unlock()
	delete(a.usedNBDDevices, device)
}

func runCommand(name string, args ...string) error {
	log.Printf("running: %s %s", name, strings.Join(args, " "))
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	if len(out) > 0 {
		log.Printf("%s output: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *app) getTTYSession(sessionID string) (*firecrackerTTYSession, bool) {
	a.ttyMu.Lock()
	defer a.ttyMu.Unlock()
	s, ok := a.ttySessions[sessionID]
	return s, ok
}

func (a *app) ttyOutput(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	tty, ok := a.getTTYSession(sessionID)
	if !ok {
		writeError(w, storage.ErrNotFound)
		return
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			http.Error(w, "offset must be a non-negative integer", http.StatusBadRequest)
			return
		}
		offset = n
	}
	output, next := tty.process.outputSince(offset)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"offset":     next,
		"output":     output,
		"done":       tty.process.isDone(),
	})
}

func (a *app) ttyInput(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	tty, ok := a.getTTYSession(sessionID)
	if !ok {
		writeError(w, storage.ErrNotFound)
		return
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := tty.process.writeInput(string(raw)); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (a *app) ttyStop(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	tty, ok := a.getTTYSession(sessionID)
	if !ok {
		writeError(w, storage.ErrNotFound)
		return
	}
	tty.mu.Lock()
	if tty.stopped {
		tty.mu.Unlock()
		http.Error(w, "tty session already stopped", http.StatusConflict)
		return
	}
	tty.stopped = true
	tty.mu.Unlock()

	snapshot, err := tty.stopFn()
	a.ttyMu.Lock()
	delete(a.ttySessions, sessionID)
	a.ttyMu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"session_id":          sessionID,
		"snapshot_id":         snapshot.SnapshotID,
		"latest_snapshot_id":  snapshot.SnapshotID,
		"manifest_key":        snapshot.ManifestKey,
		"firecracker_output":  orcaInitLines(tty.process.output()),
		"firecracker_timings": tty.timingsFn(),
	})
}

func pauseFirecracker(socketPath string) error {
	return firecrackerAPI(socketPath, http.MethodPatch, "/vm", map[string]string{
		"state": "Paused",
	})
}

func configureFirecrackerLogger(socketPath, logPath string) error {
	return firecrackerAPI(socketPath, http.MethodPut, "/logger", map[string]any{
		"log_path":        logPath,
		"level":           "Info",
		"show_level":      true,
		"show_log_origin": true,
	})
}

func createFirecrackerSnapshot(socketPath, vmStatePath, memPath string) error {
	return firecrackerAPI(socketPath, http.MethodPut, "/snapshot/create", map[string]string{
		"snapshot_type": "Full",
		"snapshot_path": vmStatePath,
		"mem_file_path": memPath,
	})
}

func loadFirecrackerSnapshot(socketPath, vmStatePath, memPath string, resume bool) error {
	return firecrackerAPI(socketPath, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": vmStatePath,
		"mem_file_path": memPath,
		"resume_vm":     resume,
	})
}

func firecrackerAPI(socketPath, method, path string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	req, err := http.NewRequest(method, "http://firecracker"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker api %s %s failed with %s: %s", method, path, resp.Status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func waitForPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (a *app) preflightKVM() error {
	if a.kvmDevice == "" {
		return nil
	}
	info, err := os.Stat(a.kvmDevice)
	if err != nil {
		return fmt.Errorf("KVM device %s is not visible in the node container; enable host KVM and pass /dev/kvm before using firecracker runtime: %w", a.kvmDevice, err)
	}
	if info.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("KVM path %s exists but is not a device", a.kvmDevice)
	}
	log.Printf("KVM device visible: %s", a.kvmDevice)
	return nil
}

func (a *app) preflightFirecracker() error {
	if err := a.preflightKVM(); err != nil {
		return err
	}
	required := map[string]string{
		"firecracker binary": a.firecrackerBin,
		"firecracker kernel": a.firecrackerKernel,
	}
	switch a.firecrackerBootMode {
	case "rootfs":
		required["firecracker rootfs"] = a.firecrackerRootFS
	case "initramfs":
		required["firecracker initramfs"] = a.firecrackerInitrd
	default:
		return fmt.Errorf("unsupported FIRECRACKER_BOOT_MODE %q", a.firecrackerBootMode)
	}
	for name, path := range required {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s %s is not visible in the node container; build Firecracker assets and mount firecracker-assets: %w", name, path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s %s is a directory, want file", name, path)
		}
	}
	return nil
}

func (a *app) preflightNBDDevices() error {
	for _, command := range []string{"blockdev", "nbd-client", "mkfs.ext4", "mount", "umount"} {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("mounted-fs runtime requires %q in the node image: %w", command, err)
		}
	}
	devices := a.visibleNBDDevices()
	if len(devices) == 0 {
		return fmt.Errorf("mounted-fs runtime requires host NBD block devices, but none are visible at %s[%d..%d); run remote-setup to load nbd and ensure the node container has privileged/device access", a.nbdDevicePrefix, a.nbdDeviceStart, a.nbdDeviceStart+a.nbdDeviceCount)
	}
	log.Printf("NBD devices visible: %s", strings.Join(devices, ", "))
	return nil
}

func (a *app) visibleNBDDevices() []string {
	var devices []string
	for i := a.nbdDeviceStart; i < a.nbdDeviceStart+a.nbdDeviceCount; i++ {
		device := fmt.Sprintf("%s%d", a.nbdDevicePrefix, i)
		info, err := os.Stat(device)
		if err != nil || info.Mode()&os.ModeDevice == 0 {
			continue
		}
		devices = append(devices, device)
	}
	return devices
}

func copyFile(src, dst string) error {
	return copyFileMode(src, dst, 0o644)
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(mode); err != nil {
		return err
	}
	return out.Sync()
}

func firecrackerBootArgs(bootMode, mode, payload, dataDevice string, waitForHost bool) string {
	afterOK := "reboot"
	if waitForHost {
		afterOK = "wait"
	}
	base := fmt.Sprintf(
		"console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.mode=%s orca.payload_b64=%s orca.data_dev=%s orca.after_ok=%s",
		mode,
		base64.StdEncoding.EncodeToString([]byte(payload)),
		dataDevice,
		afterOK,
	)
	if bootMode == "rootfs" {
		return "root=/dev/vda rw " + base
	}
	return base
}

func imageRootFSBootArgs(command string, readOnly bool, tty bool, workingDir string, env []string, user string, network *firecrackerNetwork) string {
	if command == "" {
		command = "cat /etc/orca-image-ref"
	}
	rootMode := "rw"
	if readOnly {
		rootMode = "ro"
	}
	ttyArg := ""
	if tty {
		ttyArg = " orca.tty=1"
	}
	workdirArg := ""
	if workingDir != "" {
		workdirArg = " orca.workdir_b64=" + base64.StdEncoding.EncodeToString([]byte(workingDir))
	}
	envArg := ""
	if len(env) > 0 {
		envArg = " orca.env_b64=" + base64.StdEncoding.EncodeToString([]byte(strings.Join(env, "\n")))
	}
	userArg := ""
	if user != "" {
		userArg = " orca.user_b64=" + base64.StdEncoding.EncodeToString([]byte(user))
	}
	netArg := ""
	if network != nil {
		netArg = fmt.Sprintf(
			" orca.net_ip=%s orca.net_gateway=%s orca.net_dns=%s",
			network.GuestCIDR,
			network.GatewayIP,
			network.DNS,
		)
	}
	return fmt.Sprintf(
		"root=/dev/vda %s console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.command_b64=%s%s%s%s%s%s",
		rootMode,
		base64.StdEncoding.EncodeToString([]byte(command)),
		ttyArg,
		workdirArg,
		envArg,
		userArg,
		netArg,
	)
}

func setupFirecrackerNetwork(sessionID, dns string) (firecrackerNetwork, error) {
	network := firecrackerSessionNetwork(sessionID, dns)
	_ = cleanupFirecrackerNetwork(network)
	script := fmt.Sprintf(`
set -eu
ip tuntap add dev %[1]s mode tap
ip addr add %[2]s dev %[1]s
ip link set dev %[1]s up
sysctl -w net.ipv4.ip_forward=1 >/dev/null
iptables -t nat -C POSTROUTING -s %[3]s -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s %[3]s -j MASQUERADE
iptables -C FORWARD -i %[1]s -j ACCEPT 2>/dev/null || iptables -A FORWARD -i %[1]s -j ACCEPT
iptables -C FORWARD -o %[1]s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || iptables -A FORWARD -o %[1]s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
`, network.TapName, network.HostCIDR, network.GuestCIDR)
	if err := runCommand("sh", "-lc", script); err != nil {
		_ = cleanupFirecrackerNetwork(network)
		return firecrackerNetwork{}, err
	}
	return network, nil
}

func cleanupFirecrackerNetwork(network firecrackerNetwork) error {
	script := fmt.Sprintf(`
iptables -t nat -D POSTROUTING -s %[2]s -j MASQUERADE 2>/dev/null || true
iptables -D FORWARD -i %[1]s -j ACCEPT 2>/dev/null || true
iptables -D FORWARD -o %[1]s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
ip link del %[1]s 2>/dev/null || true
`, network.TapName, network.GuestCIDR)
	return runCommand("sh", "-lc", script)
}

func firecrackerSessionNetwork(sessionID, dns string) firecrackerNetwork {
	if dns == "" {
		dns = "1.1.1.1"
	}
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(sessionID))
	n := sum.Sum32()
	third := 16 + int(n%200)
	base := 4 + int((n/200)%60)*4
	hostIP := fmt.Sprintf("172.30.%d.%d", third, base+1)
	guestIP := fmt.Sprintf("172.30.%d.%d", third, base+2)
	tapSuffix := strings.ReplaceAll(sessionID, "-", "")
	if len(tapSuffix) > 10 {
		tapSuffix = tapSuffix[:10]
	}
	if tapSuffix == "" {
		tapSuffix = fmt.Sprintf("%08x", n)
	}
	return firecrackerNetwork{
		TapName:   "tap" + tapSuffix,
		HostCIDR:  hostIP + "/30",
		GuestCIDR: guestIP + "/30",
		GatewayIP: hostIP,
		GuestIP:   guestIP,
		GuestMAC:  fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", byte(n>>24), byte(n>>16), byte(n>>8), byte(n)),
		DNS:       dns,
	}
}

func writeFirecrackerConfig(path, kernelPath, rootfsPath string, rootfsReadOnly bool, initrdPath, dataDevice string, dataReadOnly bool, logPath, bootArgs string, vcpuCount, memoryMiB int, network *firecrackerNetwork) error {
	bootSource := map[string]any{
		"kernel_image_path": kernelPath,
		"boot_args":         bootArgs,
	}
	if initrdPath != "" {
		bootSource["initrd_path"] = initrdPath
	}
	drives := []map[string]any{}
	if rootfsPath != "" {
		drives = append(drives, map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   rootfsPath,
			"is_root_device": true,
			"is_read_only":   rootfsReadOnly,
		})
	}
	if dataDevice != "" {
		drives = append(drives, map[string]any{
			"drive_id":       "orca",
			"path_on_host":   dataDevice,
			"is_root_device": false,
			"is_read_only":   dataReadOnly,
		})
	}
	cfg := map[string]any{
		"boot-source": bootSource,
		"drives":      drives,
		"machine-config": map[string]any{
			"vcpu_count":        vcpuCount,
			"mem_size_mib":      memoryMiB,
			"track_dirty_pages": false,
		},
		"logger": map[string]any{
			"log_path":        logPath,
			"level":           "Info",
			"show_level":      true,
			"show_log_origin": true,
		},
	}
	if network != nil {
		cfg["network-interfaces"] = []map[string]any{
			{
				"iface_id":      "eth0",
				"guest_mac":     network.GuestMAC,
				"host_dev_name": network.TapName,
			},
		}
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func writeFirecrackerRestoreConfig(path, vmStatePath, memPath, dataDevice, logPath string) error {
	cfg := map[string]any{
		"restore": map[string]any{
			"snapshot_path": vmStatePath,
			"mem_file_path": memPath,
			"resume_vm":     true,
		},
		"orca": map[string]any{
			"data_device": dataDevice,
		},
		"logger": map[string]any{
			"log_path":        logPath,
			"level":           "Info",
			"show_level":      true,
			"show_log_origin": true,
		},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func orcaInitLines(s string) string {
	var lines []string
	capturingCommandOutput := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, "orca-init: started image-rootfs command="):
			capturingCommandOutput = true
			lines = append(lines, line)
		case strings.Contains(line, "orca-init: command ok") || strings.Contains(line, "orca-init: command failed"):
			capturingCommandOutput = false
			lines = append(lines, line)
		case strings.Contains(line, "orca-init:") || strings.Contains(line, "orca-stdout:") || strings.Contains(line, "orca-stderr:") || strings.Contains(line, "orca-timing:"):
			lines = append(lines, line)
		case capturingCommandOutput:
			lines = append(lines, "orca-stdout: "+line)
		}
	}
	return strings.Join(lines, "\n")
}

func timingsJSON(timings []firecrackerStepTiming) string {
	raw, err := json.Marshal(timings)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func localNBDClientTarget(listenAddr string) (string, string) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "localhost", strings.TrimPrefix(listenAddr, ":")
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "localhost"
	}
	return host, port
}

func normalizeRuntime(runtime, legacyFrontend string) string {
	if runtime != "" {
		return runtime
	}
	switch legacyFrontend {
	case "":
		return "http-block"
	case "mount":
		return "mounted-fs"
	case "nbd":
		return "nbd-export-test"
	default:
		return legacyFrontend
	}
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
