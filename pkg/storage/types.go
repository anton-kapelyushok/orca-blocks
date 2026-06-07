package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

const (
	DefaultSizeBytes = int64(10 * 1024 * 1024 * 1024)
	DefaultChunkSize = int64(4 * 1024 * 1024)
)

var (
	ErrNotFound = errors.New("not found")
)

type Volume struct {
	VolumeID         string `json:"volume_id"`
	SizeBytes        int64  `json:"size_bytes"`
	ChunkSize        int64  `json:"chunk_size"`
	LatestSnapshotID string `json:"latest_snapshot_id,omitempty"`
	LastNode         string `json:"last_node,omitempty"`
}

type Snapshot struct {
	SnapshotID  string `json:"snapshot_id"`
	VolumeID    string `json:"volume_id"`
	ManifestKey string `json:"manifest_key"`
}

type BaseImage struct {
	BaseImageID     string   `json:"base_image_id"`
	ImageRef        string   `json:"image_ref"`
	ImageDigest     string   `json:"image_digest"`
	VolumeID        string   `json:"volume_id"`
	SnapshotID      string   `json:"snapshot_id"`
	RootFSSizeBytes int64    `json:"rootfs_size_bytes"`
	Env             []string `json:"env,omitempty"`
	WorkingDir      string   `json:"working_dir,omitempty"`
	Entrypoint      []string `json:"entrypoint,omitempty"`
	Cmd             []string `json:"cmd,omitempty"`
	User            string   `json:"user,omitempty"`
}

type Env struct {
	EnvID            string `json:"env_id"`
	BaseImageID      string `json:"base_image_id"`
	ImageRef         string `json:"image_ref"`
	VolumeID         string `json:"volume_id"`
	LatestSnapshotID string `json:"latest_snapshot_id"`
}

type ChunkRange struct {
	Index      int64
	ChunkStart int
	ReqStart   int64
	ReqEnd     int64
}

type Manifest map[int64]string

func HashChunk(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ZeroChunk(chunkSize int64) []byte {
	return make([]byte, int(chunkSize))
}

func ChunkIndexes(offset, length, chunkSize int64) ([]ChunkRange, error) {
	if offset < 0 {
		return nil, fmt.Errorf("offset must be >= 0")
	}
	if length < 0 {
		return nil, fmt.Errorf("length must be >= 0")
	}
	if chunkSize <= 0 {
		return nil, fmt.Errorf("chunk size must be > 0")
	}
	if length == 0 {
		return nil, nil
	}

	end := offset + length
	if end < offset {
		return nil, fmt.Errorf("range overflow")
	}

	first := offset / chunkSize
	last := (end - 1) / chunkSize
	out := make([]ChunkRange, 0, last-first+1)
	for idx := first; idx <= last; idx++ {
		chunkAbsStart := idx * chunkSize
		chunkAbsEnd := chunkAbsStart + chunkSize
		reqStart := max64(offset, chunkAbsStart)
		reqEnd := min64(end, chunkAbsEnd)
		out = append(out, ChunkRange{
			Index:      idx,
			ChunkStart: int(reqStart - chunkAbsStart),
			ReqStart:   reqStart,
			ReqEnd:     reqEnd,
		})
	}
	return out, nil
}

func ManifestToWire(m Manifest) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strconv.FormatInt(k, 10)] = v
	}
	return out
}

func ManifestFromWire(w map[string]string) (Manifest, error) {
	out := make(Manifest, len(w))
	for k, v := range w {
		idx, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid manifest chunk index %q: %w", k, err)
		}
		out[idx] = v
	}
	return out, nil
}

func CopyManifest(m Manifest) Manifest {
	out := make(Manifest, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func SortedManifestIndexes(m Manifest) []int64 {
	indexes := make([]int64, 0, len(m))
	for idx := range m {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	return indexes
}

type Repository interface {
	Init(ctx context.Context) error
	CreateVolume(ctx context.Context, volume Volume) (Volume, error)
	GetVolume(ctx context.Context, volumeID string) (Volume, error)
	UpdateLatestSnapshot(ctx context.Context, volumeID, snapshotID string) error
	UpdateLastNode(ctx context.Context, volumeID, nodeID string) error
	CreateSnapshot(ctx context.Context, snapshot Snapshot) error
	GetSnapshot(ctx context.Context, snapshotID string) (Snapshot, error)
	CreateBaseImage(ctx context.Context, image BaseImage) (BaseImage, error)
	GetBaseImage(ctx context.Context, baseImageID string) (BaseImage, error)
	GetBaseImageByRef(ctx context.Context, imageRef string) (BaseImage, error)
	ListBaseImages(ctx context.Context) ([]BaseImage, error)
	CreateEnv(ctx context.Context, env Env) (Env, error)
	GetEnv(ctx context.Context, envID string) (Env, error)
	UpdateEnvSnapshot(ctx context.Context, envID, snapshotID string) error
}

type ObjectStore interface {
	Bucket() string
	Put(ctx context.Context, key string, b []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Exists(ctx context.Context, key string) (bool, error)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
